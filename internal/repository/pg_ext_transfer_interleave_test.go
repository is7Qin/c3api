// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// pg_ext_transfer_interleave_test.go C1 收口（真实 PG）：把转属屏障打在**仓储内部**
// 的查询边界上——不再只依赖服务入口屏障。覆盖：
//   - GetOwnedAccountExt：owner 判定与 ext 读取必须落在**同一条 SQL**（旧实现先
//     执行 accounts owner id 集、再以物化旧 id 集独立查 ext，两条语句之间转属即
//     读到他人凭据）。
//   - FindAccountExtByCodexKey（组合键查重，经 accountExtQuery）：同上。
//   - TryInsertAccountExt：owner 判定与 INSERT 必须落在**同一 owner 行锁边界**内
//     （旧实现无锁 Exist 通过后转属并提交，随后 INSERT 不复核 owner）。
//
// 屏障机制：包裹 dialect.Driver，在指定查询**发送前**阻塞（无 sleep 的确定性交错）。
// 屏障在旧形状命中的“第二条语句”处触发，在新形状命中的“唯一一条语句”处触发——两者
// 都在同一文本点命中，故新实现恒返回正确结果、旧的转属窗口无法再制造越权读/写。
//
// 跑法：TEST_DATABASE_URL=... go test -race -count=1 -p 1 ./internal/repository/ \
//      -run 'PGExtTransferBarrier' -v

import (
	"context"
	"strings"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// queryBarrier 一次性查询屏障：匹配到目标查询时在**发送前**阻塞，直到 release。
type queryBarrier struct {
	match   func(query string) bool
	reached chan struct{}
	release chan struct{}

	mu          sync.Mutex
	fired       bool
	releaseOnce sync.Once
}

func newQueryBarrier(match func(query string) bool) *queryBarrier {
	return &queryBarrier{match: match, reached: make(chan struct{}), release: make(chan struct{})}
}

// releaseAll 幂等放行（test 失败走 Goexit 时 defer 仍会调用，避免把仓储协程留在
// 屏障里、进而拖住共享 schema 清理）。
func (b *queryBarrier) releaseAll() { b.releaseOnce.Do(func() { close(b.release) }) }

// block 在**执行前**对匹配查询阻塞（仅首次）。非匹配查询直接放行。
func (b *queryBarrier) block(query string) {
	if !b.match(query) {
		return
	}
	b.mu.Lock()
	if b.fired {
		b.mu.Unlock()
		return
	}
	b.fired = true
	b.mu.Unlock()
	close(b.reached)
	<-b.release
}

// barrierDriver 包裹 dialect.Driver/Tx，把每条语句先过一遍屏障。
type barrierDriver struct {
	dialect.Driver
	barrier *queryBarrier
}

func (d *barrierDriver) Exec(ctx context.Context, query string, args, v any) error {
	d.barrier.block(query)
	return d.Driver.Exec(ctx, query, args, v)
}

func (d *barrierDriver) Query(ctx context.Context, query string, args, v any) error {
	d.barrier.block(query)
	return d.Driver.Query(ctx, query, args, v)
}

func (d *barrierDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return &barrierTx{Tx: tx, barrier: d.barrier}, nil
}

type barrierTx struct {
	dialect.Tx
	barrier *queryBarrier
}

func (t *barrierTx) Exec(ctx context.Context, query string, args, v any) error {
	t.barrier.block(query)
	return t.Tx.Exec(ctx, query, args, v)
}

func (t *barrierTx) Query(ctx context.Context, query string, args, v any) error {
	t.barrier.block(query)
	return t.Tx.Query(ctx, query, args, v)
}

// newBarrierRepos 用屏障驱动在本测试的私有 clone 上构造独立仓储实例（不触发 migrate）。
// pgtest.Clone 按测试幂等 ⇒ 与同测试内的 newPGReposShared 共享同一库（见同包 pg_shared_test.go）。
func newBarrierRepos(t *testing.T, bar *queryBarrier) *repository.Repository {
	t.Helper()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	drv := entsql.OpenDB(dialect.Postgres, db)
	brepos, err := repository.NewWithPG(context.Background(), &barrierDriver{Driver: drv, barrier: bar}, false, pool)
	require.NoError(t, err)
	return brepos
}

func matchesAccountExts(query string) bool { return strings.Contains(query, "account_exts") }

// TestPGExtOwnedReadTransferBarrier 在 GetOwnedAccountExt 的 ext 查询边界打转属屏障：
// 旧实现此处已取到 owner id 集（第一条语句），转属后第二条 ext 查询仍按旧 id 集命中
// 他人凭据；修复后 owner 判定与 ext 读取同一条 SQL ⇒ 转属后 404。
func TestPGExtOwnedReadTransferBarrier(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "ext-xfer-read-owner@example.com")
	target := seedSupplierUser(t, repos, "ext-xfer-read-target@example.com")
	acc := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "ext-xfer-read-acc")
	seedPATExtPG(t, repos, acc.ID, "owner-pat")

	bar := newQueryBarrier(matchesAccountExts)
	defer bar.releaseAll()
	brepos := newBarrierRepos(t, bar)

	var (
		wg  sync.WaitGroup
		got *domain.AccountExt
		err error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		got, err = brepos.GetOwnedAccountExt(scopeCtx(owner.ID), acc.ID)
	}()

	<-bar.reached // 仓储已到 ext 查询边界（旧实现此处已物化 owner id 集）
	_, xerr := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{SupplierUserID: &target.ID})
	require.NoError(t, xerr)
	bar.releaseAll()
	wg.Wait()

	require.ErrorIs(t, err, repository.ErrNotFound, "转属交错下 ext 单读必须 404（不得返回他人凭据）")
	require.Nil(t, got)

	// 管理面仍读得到（证明过滤来自作用域而非数据缺失）。
	mgr, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, "owner-pat", *mgr.CodexPATKey)
}

// TestPGExtCodexKeyTransferBarrier 在 accountExtQuery 的 ext 查询边界打转属屏障：
// 组合键查重在转属交错下必须 ErrNotFound（不得按旧 id 集命中他人行）。
func TestPGExtCodexKeyTransferBarrier(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "ext-xfer-key-owner@example.com")
	target := seedSupplierUser(t, repos, "ext-xfer-key-target@example.com")
	acc := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "ext-xfer-key-acc")

	const email, acctID = "xfer-key@example.com", "xfer-key-1"
	seedCodexKeyExtPG(t, repos, acc.ID, email, acctID, "owner-pat")

	bar := newQueryBarrier(matchesAccountExts)
	defer bar.releaseAll()
	brepos := newBarrierRepos(t, bar)

	var (
		wg  sync.WaitGroup
		got *domain.AccountExt
		err error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		got, err = brepos.FindAccountExtByCodexKey(scopeCtx(owner.ID), email, acctID)
	}()

	<-bar.reached
	_, xerr := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{SupplierUserID: &target.ID})
	require.NoError(t, xerr)
	bar.releaseAll()
	wg.Wait()

	require.ErrorIs(t, err, repository.ErrNotFound, "转属交错下组合键查重必须 ErrNotFound（不得命中他人行）")
	require.Nil(t, got)
}

// TestPGExtTryInsertTransferBarrier 在 TryInsertAccountExt 的 INSERT 边界打转属屏障：
// 修复后事务已持 owner 行锁 ⇒ 并发转属（UPDATE accounts）阻塞在行锁上超时；旧实现无锁
// ⇒ 转属可提交，随后 INSERT 给他人账号落 ext 行。
func TestPGExtTryInsertTransferBarrier(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "ext-xfer-ti-owner@example.com")
	target := seedSupplierUser(t, repos, "ext-xfer-ti-target@example.com")
	acc := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "ext-xfer-ti-acc")
	// 无 ext 行 ⇒ 走首写路径。

	bar := newQueryBarrier(func(q string) bool {
		return strings.Contains(q, "account_exts") && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)), "INSERT")
	})
	defer bar.releaseAll()
	brepos := newBarrierRepos(t, bar)

	const iid = "11111111-2222-3333-4444-555555555555"
	var (
		wg       sync.WaitGroup
		inserted bool
		err      error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		inserted, err = brepos.AccountExts.TryInsertAccountExt(scopeCtx(owner.ID), &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexPAT,
			CodexIdentity: &domain.CodexIdentity{InstallationID: iid}, CodexPATKey: strPtrPG("owner-pat"),
		})
	}()

	<-bar.reached // 仓储已持 owner 行锁、即将 INSERT
	// 并发转属（独立连接）必须在 owner 行锁上阻塞：短 lock_timeout ⇒ 确定性失败。
	conn := pgSharedConn(t)
	_, xerr := conn.Exec(ctx, "SET lock_timeout='300ms'")
	require.NoError(t, xerr)
	_, xerr = conn.Exec(ctx, "UPDATE accounts SET supplier_user_id=$2 WHERE id=$1", acc.ID, target.ID)
	require.Error(t, xerr, "TryInsert 期间账号行必须被 owner 锁住（并发转属阻塞/超时）")
	bar.releaseAll()
	wg.Wait()

	require.NoError(t, err)
	require.True(t, inserted, "owner 账号首写应成功")
	// 转属未在锁窗口内提交：账号仍归 owner；ext 行落在 owner 账号上。
	after, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, owner.ID, after.SupplierUserID)
	e, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, "owner-pat", *e.CodexPATKey)
}
