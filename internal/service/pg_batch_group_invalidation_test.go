// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// batchGroupPGTestSchema 本文件 PG 测试专用 schema（与其它 PG 测试隔离）。
const batchGroupPGTestSchema = "batch_group_invalidation_test"

// newBatchGroupPG 独立 schema 上的服务：注入记录失效与发布的假件，调用方可
// 断言事务提交后的失效动作面。未设置 TEST_DATABASE_URL 则跳过。
func newBatchGroupPG(t *testing.T) (*Service, *repository.Repository, *invRecorder, *pubRecorder) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + batchGroupPGTestSchema
	} else {
		dsn += "?search_path=" + batchGroupPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+batchGroupPGTestSchema+` CASCADE; CREATE SCHEMA `+batchGroupPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.NewWithPG(t.Context(), entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	rec, pr := &invRecorder{}, &pubRecorder{}
	svc := New(repos, nil, rec, pr, nil, nil, nil, ServiceDeps{EmailCodeStore: testEmailCodes})
	return svc, repos, rec, pr
}

// seedBatchGroupPG 批量组失效测试的模板与分组种子：一个模板、三个分组。
func seedBatchGroupPG(t *testing.T, svc *Service) (tpl *domain.Template, gA, gB, gC *domain.Group) {
	t.Helper()
	ctx := context.Background()
	tpl, err := svc.CreateTemplate(ctx, &domain.Template{
		Name: "batch-inv-tpl", BaseURL: "https://batch-inv.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	gA, err = svc.CreateGroup(ctx, "batch-inv-a", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	gB, err = svc.CreateGroup(ctx, "batch-inv-b", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	gC, err = svc.CreateGroup(ctx, "batch-inv-c", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	return tpl, gA, gB, gC
}

// createBatchGroupAccount 在指定分组建账号（创建期的失效记录会被调用方清空，
// 只保留被测写入的记录）。
func createBatchGroupAccount(t *testing.T, svc *Service, tpl *domain.Template, name string, gids []int64) *domain.Account {
	t.Helper()
	ctx := context.Background()
	acc, err := svc.CreateAccount(ctx, repository.AccountPatch{
		Name:           strPtr(name),
		TemplateID:     int64Ptr(tpl.ID),
		UpstreamKey:    strPtr("sk-" + name),
		MaxConcurrency: intPtr(8),
		GroupIDs:       &gids,
	})
	require.NoError(t, err)
	return acc
}

// accountCalls 取记录器中账号失效调用的分组集合与调用次数。
func accountCalls(rec *invRecorder) (gids [][]int64, n int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, c := range rec.calls {
		if c.kind == "accounts" {
			n++
			gids = append(gids, append([]int64(nil), c.gids...))
		}
	}
	return gids, n
}

// TestBatchGroupInvalidationPG 钉住批量写入的组级失效派生与提交后执行：账号
// 跳到两个组后，事务提交后对旧组并集与新组执行组级 reload 与失效发布；回滚
// 时不执行。失效只在写成功之后调用，回滚路径天然不留半套失效。
func TestBatchGroupInvalidationPG(t *testing.T) {
	svc, _, rec, pr := newBatchGroupPG(t)
	ctx := context.Background()
	tpl, gA, gB, gC := seedBatchGroupPG(t, svc)

	a1 := createBatchGroupAccount(t, svc, tpl, "batch-inv-1", []int64{gA.ID})
	a2 := createBatchGroupAccount(t, svc, tpl, "batch-inv-2", []int64{gB.ID})
	rec.mu.Lock()
	rec.calls = nil
	rec.mu.Unlock()
	pr.mu.Lock()
	pr.calls = nil
	pr.mu.Unlock()

	name := "batch-inv-renamed"
	_, err := svc.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID}, repository.AccountPatch{
		Name:     &name,
		GroupIDs: &[]int64{gB.ID, gC.ID},
	})
	require.NoError(t, err)

	gids, n := accountCalls(rec)
	require.Equal(t, 1, n, "一次批量写入只触发一次组级失效")
	require.Len(t, gids, 1)
	require.ElementsMatch(t, []int64{gA.ID, gB.ID, gB.ID, gC.ID}, gids[0],
		"旧组并集（两账号旧组）与新组（目标两组）都要刷新")

	got := pr.last()
	require.NotNil(t, got, "批量写入提交后必须发布 NOTIFY")
	require.ElementsMatch(t, []int64{gA.ID, gB.ID, gB.ID, gC.ID}, got.Groups,
		"NOTIFY 携带旧组并集与新组")
	require.False(t, got.Clients, "纯配置写入不触客户端缓存失效")
}

// TestSingleAccountMoveGroupInvalidationPG 钉住单账号移组：A→B 后 A、B 两组
// 都被刷新（旧组与新组各一次定向）。
func TestSingleAccountMoveGroupInvalidationPG(t *testing.T) {
	svc, _, rec, pr := newBatchGroupPG(t)
	ctx := context.Background()
	tpl, gA, gB, _ := seedBatchGroupPG(t, svc)

	acc := createBatchGroupAccount(t, svc, tpl, "move-inv-1", []int64{gA.ID})
	rec.mu.Lock()
	rec.calls = nil
	rec.mu.Unlock()
	pr.mu.Lock()
	pr.calls = nil
	pr.mu.Unlock()

	name := "move-inv-renamed"
	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{
		Name:     &name,
		GroupIDs: &[]int64{gB.ID},
	}, nil)
	require.NoError(t, err)

	gids, n := accountCalls(rec)
	require.Equal(t, 1, n, "一次单账号写入只触发一次组级失效")
	require.Len(t, gids, 1)
	require.ElementsMatch(t, []int64{gA.ID, gB.ID}, gids[0], "移组 A→B：旧组与新组都被刷新")

	got := pr.last()
	require.NotNil(t, got, "单账号写入提交后必须发布 NOTIFY")
	require.ElementsMatch(t, []int64{gA.ID, gB.ID}, got.Groups, "NOTIFY 携带旧组与新组")
}

// failingBatchStore 让批量写失败（模拟事务回滚），其余能力全部委托给真实
// PG 仓库：只替换批量写动词，故断言的是"写失败后服务层的失效动作面"。
// 照抄同包回滚测试的写法（写失败路径不得留下半套失效与发布）。
type failingBatchStore struct {
	Store
	err error
}

func (f *failingBatchStore) UpdateAccountsBatch(context.Context, []int64, repository.AccountPatch) ([]repository.AccountWriteResult, error) {
	return nil, f.err
}

// TestBatchGroupInvalidationRollbackPG 钉住回滚零失效：批量写失败（事务回
// 滚）时，组级重载与 NOTIFY 都不得执行。
func TestBatchGroupInvalidationRollbackPG(t *testing.T) {
	svc, repos, _, _ := newBatchGroupPG(t)
	ctx := context.Background()
	tpl, gA, _, _ := seedBatchGroupPG(t, svc)

	acc := createBatchGroupAccount(t, svc, tpl, "rollback-inv-1", []int64{gA.ID})

	rec, pr := &invRecorder{}, &pubRecorder{}
	failing := &Service{store: &failingBatchStore{Store: repos, err: repository.ErrNotFound}, inv: rec, pub: pr, log: nil}

	name := "rollback-inv-renamed"
	_, err := failing.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Name: &name})
	require.Error(t, err)

	require.Zero(t, rec.total(), "回滚的批量写不得触发任何快照失效")
	require.Zero(t, pr.total(), "回滚的批量写不得发布 NOTIFY")
}
