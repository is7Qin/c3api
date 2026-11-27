// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestAccountValidationCostCache(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	// need template
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)

	// negative multiplier -> invalid
	_, err = svc.CreateAccount(context.Background(), repository.AccountPatch{Name: new("a"), TemplateID: new(int64(1)), UpstreamKey: new("sk-x"), UpstreamCostMultiplierBp: new(-1)})
	require.ErrorIs(t, err, ErrInvalidInput)

	// invalid domain -> invalid
	bad := "not a domain!"
	_, err = svc.CreateAccount(context.Background(), repository.AccountPatch{Name: new("a2"), TemplateID: new(int64(1)), UpstreamKey: new("sk-x"), CacheDomain: &bad})
	require.ErrorIs(t, err, ErrInvalidInput)

	// valid shared domain
	good := "shared.example.com"
	acc, err := svc.CreateAccount(context.Background(), repository.AccountPatch{Name: new("a3"), TemplateID: new(int64(1)), UpstreamKey: new("sk-x"), CacheDomain: &good, UpstreamCostMultiplierBp: new(25000)})
	require.NoError(t, err)
	require.Equal(t, 25000, domain.MultBp(acc.UpstreamCostMultiplierBp))

	// empty domain (nil) allowed - private
	acc2, err := svc.CreateAccount(context.Background(), repository.AccountPatch{Name: new("a4"), TemplateID: new(int64(1)), UpstreamKey: new("sk-x")})
	require.NoError(t, err)
	require.NotNil(t, acc2)

	// batch patch validation: negative cost
	costNeg := -5
	_, err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &costNeg})
	require.ErrorIs(t, err, ErrInvalidInput)

	// batch invalid domain
	badDom := "bad_domain!"
	_, err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{CacheDomain: &badDom})
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestCreateAccountExplicitZeroMultiplier 创建即落 ×0（免费）：0 与"未提供"必须
// 可区分——补丁指针非 nil → 精确落值；未提供 → 创建默认 ×1。
func TestCreateAccountExplicitZeroMultiplier(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)

	free, err := svc.CreateAccount(context.Background(), repository.AccountPatch{
		Name: new("free"), TemplateID: new(int64(1)), UpstreamKey: new("sk-x"),
		UpstreamCostMultiplierBp: new(0),
	})
	require.NoError(t, err)
	require.Equal(t, 0, domain.MultBp(free.UpstreamCostMultiplierBp), "显式 ×0（免费）不得被创建默认吞掉")

	def, err := svc.CreateAccount(context.Background(), repository.AccountPatch{
		Name: new("unset"), TemplateID: new(int64(1)), UpstreamKey: new("sk-y"),
	})
	require.NoError(t, err)
	require.Equal(t, 10000, domain.MultBp(def.UpstreamCostMultiplierBp), "未提供 → 创建默认 ×1")
}

func TestCodexImport(t *testing.T) {
	// Verify the codex import path remains functional.
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}}
	// need templates of codex type
	tplOauth, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "tpl-oauth", CredentialType: "codex-oauth", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}})
	require.NoError(t, err)
	// import oauth
	items := []domain.CodexOAuthImportItem{{CodexEmail: "a@example.com", CodexAccountID: "acc1", CodexOAuthToken: "tok", CodexOAuthRefreshToken: "rt"}}
	res, err := svc.ImportCodexOAuthAccounts(context.Background(), items, &tplOauth.ID, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
}

// probeCall 记录 recover→PROBING 健康写入（recover-side prober 契约面）。

// fakeRecoverLatch latch 显式释放记录面。
type fakeRecoverLatch struct{ cleared []int64 }

func (l *fakeRecoverLatch) Clear(accountID int64) { l.cleared = append(l.cleared, accountID) }

// fakeRecoverHealthClear 健康记录显式清除记录面。
type fakeRecoverHealthClear struct {
	cleared []int64
	err     error
}

func (c *fakeRecoverHealthClear) ClearAccount(_ context.Context, accountID int64) (int64, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.cleared = append(c.cleared, accountID)
	return 1, nil
}

// seedLifecycleAccount 建模板 + 一个 rev=5 已失效账号（failed_at/failure_source/
// last_error 置位，enabled=true，倍率 25000，共享缓存域）。
func seedLifecycleAccount(t *testing.T, fs *fakeStore) *domain.Account {
	t.Helper()
	ctx := context.Background()
	_, err := fs.CreateTemplate(ctx, &domain.Template{ID: 1, Name: "tpl", CredentialType: "api_key"})
	require.NoError(t, err)
	failed := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	src := "rule"
	reason := "fatal"
	dom := "shared.example.com"
	acc, err := fs.CreateAccount(ctx, &domain.Account{
		Name: "a", TemplateID: 1, UpstreamKey: "sk-a",
		Enabled: true, FailedAt: &failed, FailureSource: &src, LastError: &reason,
		LifecycleRevision: 5, IdentityRevision: 3, UpstreamCostMultiplierBp: new(25000), CacheDomain: &dom,
	})
	require.NoError(t, err)
	return acc
}

// TestRecoverAccountFenced recover 校验 revision：正确 revision → 清失效三字段 +
// revision+1 + 清健康记录 + 组级失效；stale revision → ErrConflict（409）
// 且不清失效；缺 id → ErrNotFound。
func TestRecoverAccountFenced(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	latch := &fakeRecoverLatch{}
	healthClear := &fakeRecoverHealthClear{}
	inv := &invRecorder{}
	svc := &Service{store: fs, inv: inv, recoverLatch: latch, recoverHealthClear: healthClear}

	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err)
	require.Nil(t, got.FailedAt)
	require.Nil(t, got.FailureSource)
	require.Nil(t, got.LastError)
	require.Equal(t, int64(6), got.LifecycleRevision)
	require.Equal(t, []int64{acc.ID}, latch.cleared, "recover 必须显式释放 latch")
	// 恢复的健康面动作**只是清除**：不写任何新记录。写记录（尤其 PROBING）等于
	// 让账号的可选性依赖探针环——而探针环是可选的。清空即「无记录 = READY」。
	require.Equal(t, []int64{acc.ID}, healthClear.cleared, "recover 必须显式清除该账号健康记录")
	require.Len(t, inv.calls, 1, "recover 后必须触发组级失效")

	// stale revision（重放同一 expected_revision）→ 409，不再清健康
	_, err = svc.RecoverAccount(ctx, acc.ID, 5)
	require.ErrorIs(t, err, ErrConflict)
	require.Len(t, latch.cleared, 1, "409 不得释放 latch")
	require.Len(t, healthClear.cleared, 1, "409 不得清除健康记录")

	_, err = svc.RecoverAccount(ctx, 999, 1)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRecoverAccountDoesNotDependOnProber 钉住恢复链路的**结构约束**：
// recover 不再接受任何「探针/健康写入」依赖（RecoverProber 接口已删除，类型
// 不存在即编译期证明）。恢复只做持久 CAS + latch 释放 + 健康清除。
//
// 为什么这条约束承重：健康面里 PROBING 是唯一必须由探针推进才能离开的状态。
// 只要恢复链路能创建它，账号的「可选性」就依赖一个可选组件是否在跑——那是
// 无界等待。这里用「清空后 EffectiveState 必为 READY」把语义钉在接口之外。
func TestRecoverAccountDoesNotDependOnProber(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	latch := &fakeRecoverLatch{}
	healthClear := &fakeRecoverHealthClear{}
	inv := &invRecorder{}
	svc := &Service{store: fs, inv: inv, recoverLatch: latch, recoverHealthClear: healthClear}

	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err)
	require.Nil(t, got.FailedAt)
	require.Equal(t, int64(6), got.LifecycleRevision, "CAS 已提交，C 必须推进")
	require.Equal(t, []int64{acc.ID}, latch.cleared)
	require.Equal(t, []int64{acc.ID}, healthClear.cleared)
	require.Len(t, inv.calls, 1, "恢复后必须触发组级失效")
}

// TestRecoverAccountWithoutClearers 未装配 latch/健康清除面时 recover 仍完成持久
// 恢复（nil = 跳过，不阻断）。
func TestRecoverAccountWithoutClearers(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}
	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err)
	require.Nil(t, got.FailedAt)
	require.Equal(t, int64(6), got.LifecycleRevision)
}

// TestRecoverAccountClearsHealthWithoutFailure 钉住「UI 敢给冷却账号放恢复按钮」的
// 后端前提：recover 不要求 failed_at 置位。只在冷却（health 有记录、failed_at 为空）
// 的账号同样要执行 latch 释放 + 健康清除——否则页面上的按钮会变成
// 「点了只 bump revision，冷却纹丝不动」。
func TestRecoverAccountClearsHealthWithoutFailure(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	_, err := fs.CreateTemplate(ctx, &domain.Template{ID: 1, Name: "tpl", CredentialType: "api_key"})
	require.NoError(t, err)
	acc, err := fs.CreateAccount(ctx, &domain.Account{
		Name: "cooling", TemplateID: 1, UpstreamKey: "sk-a", Enabled: true,
		LifecycleRevision: 5, IdentityRevision: 3, UpstreamCostMultiplierBp: new(10000),
	})
	require.NoError(t, err)
	require.Nil(t, acc.FailedAt, "前提：只在运行时冷却、没有终态失效")

	latch := &fakeRecoverLatch{}
	healthClear := &fakeRecoverHealthClear{}
	svc := &Service{store: fs, inv: &invRecorder{}, recoverLatch: latch, recoverHealthClear: healthClear}

	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err)
	require.Equal(t, int64(6), got.LifecycleRevision, "恢复同样推进 C")
	require.Equal(t, []int64{acc.ID}, healthClear.cleared, "failed_at 为空也必须清冷却记录")
	require.Equal(t, []int64{acc.ID}, latch.cleared)
}

// TestRecoverAccountHealthClearFailureIsBestEffort 钉住健康清除失败的语义：
// CAS 已提交（失效字段已清、C 已 +1），故不回滚、不上抛；latch 释放仍要执行。
// 代价是「200 成功但冷却记录还在」——契约描述必须如实写成 best-effort，
// 调用方可重试或等 TTL/Sync 收敛。
func TestRecoverAccountHealthClearFailureIsBestEffort(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	latch := &fakeRecoverLatch{}
	healthClear := &fakeRecoverHealthClear{err: errors.New("redis down")}
	inv := &invRecorder{}
	svc := &Service{store: fs, inv: inv, recoverLatch: latch, recoverHealthClear: healthClear}

	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err, "健康清除失败不得回滚已提交的 CAS")
	require.Equal(t, int64(6), got.LifecycleRevision)
	require.Nil(t, got.FailedAt)
	require.Equal(t, []int64{acc.ID}, latch.cleared, "健康清除失败不得跳过 latch 释放")
	require.Len(t, inv.calls, 1)
}

// TestPatchAccountEnabled 账号启用/禁用经唯一写点：落值 + 无条件推进 C；
// disable 不清失效字段（失效恢复唯一入口 = RecoverAccount）。
func TestPatchAccountEnabled(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	off := false
	got, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{Enabled: &off}, nil)
	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.Equal(t, int64(6), got.LifecycleRevision, "配置写入无条件推进 C")
	require.NotNil(t, got.FailedAt, "disable 不清失效字段")

	on := true
	got, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{Enabled: &on}, nil)
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, int64(7), got.LifecycleRevision)
}

// TestPatchAccountCostMultiplier 采购倍率经唯一写点：合法 bp 落库 + 推进 C；
// 越界（<0 / >×10）→ ErrInvalidInput。
func TestPatchAccountCostMultiplier(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	bp := 15000
	got, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamCostMultiplierBp: &bp}, nil)
	require.NoError(t, err)
	require.Equal(t, 15000, domain.MultBp(got.UpstreamCostMultiplierBp))
	require.Equal(t, int64(6), got.LifecycleRevision)

	neg := -1
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamCostMultiplierBp: &neg}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	over := 100001
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamCostMultiplierBp: &over}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestPatchAccountCacheDomain 缓存域经唯一写点：设置 + 推进 C；空串 = 清空
// （回账号私有域）；非法域 → ErrInvalidInput。
func TestPatchAccountCacheDomain(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	other := "other.example.com"
	got, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{CacheDomain: &other}, nil)
	require.NoError(t, err)
	require.Equal(t, other, *got.CacheDomain)
	require.Equal(t, int64(6), got.LifecycleRevision)

	empty := ""
	got, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{CacheDomain: &empty}, nil)
	require.NoError(t, err)
	require.Nil(t, got.CacheDomain, "空串 = 清空回私有域")
	require.Equal(t, int64(7), got.LifecycleRevision)

	bad := "BAD domain!"
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{CacheDomain: &bad}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestPatchAccountRejectsEmptyPatch 空补丁（无任何字段）→ 400：写面不接受无字段
// 请求（否则等于一次纯 C 推进的空写）。判定在 service 收口，handler 不重复。
func TestPatchAccountRejectsEmptyPatch(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}
	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}
