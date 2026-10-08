// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// account_scope_pg_test.go 账号作用域（spec 2026-10-09 §2.5 / A13① / A14③）在真实
// PostgreSQL 上的越域拒绝证据：C1 修复后 usage 聚合与 ext 单读都必须把 owner 谓词
// AND 进 SQL，越域 id 在**数据库层**被过滤，应用层拿不到他人数据。
//
// 跑法：TEST_DATABASE_URL=... go test -count=1 -p 1 ./internal/repository/ \
//      -run 'PGAccountScope|PGUsageAggScope' -v

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// scopeCtx 注入供应商面行层作用域（镜像 handler.SupplierScopeInject）。
func scopeCtx(uid int64) context.Context {
	return domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(uid))
}

// seedPATExtPG 为账号写入 pat ext 行（凭据面越域断言用）。
func seedPATExtPG(t *testing.T, repos *repository.Repository, accountID int64, pat string) {
	t.Helper()
	_, err := repos.AccountExts.UpsertAccountExt(context.Background(), &domain.AccountExt{
		AccountID: accountID, CredentialType: credential.TypeCodexPAT,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "11111111-2222-3333-4444-555555555555"},
		CodexPATKey:   strPtrPG(pat),
	})
	require.NoError(t, err)
}

// seedOwnedAccount 建一个归属指定供应商的账号（SupplierUserID>0 走 §2.5 值域校验）。
func seedOwnedAccount(t *testing.T, repos *repository.Repository, tplID, uid int64, name string) *domain.Account {
	t.Helper()
	a, err := repos.Accounts.CreateAccount(context.Background(), &domain.Account{
		Name: name, TemplateID: tplID, UpstreamKey: "sk-" + name,
		MaxConcurrency: 8, Enabled: true, SupplierUserID: uid})
	require.NoError(t, err)
	require.Equal(t, uid, a.SupplierUserID)
	return a
}

// seedCodexKeyExtPG 为账号写入带组合幂等键（codex_email, codex_account_id）的
// pat ext 行——跨归属同键隔离断言用（该组合键有全局唯一索引）。
func seedCodexKeyExtPG(t *testing.T, repos *repository.Repository, accountID int64, email, codexAccountID, pat string) {
	t.Helper()
	_, err := repos.AccountExts.UpsertAccountExt(context.Background(), &domain.AccountExt{
		AccountID: accountID, CredentialType: credential.TypeCodexPAT,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "11111111-2222-3333-4444-555555555555"},
		CodexEmail:    strPtrPG(email), CodexAccountID: strPtrPG(codexAccountID), CodexPATKey: strPtrPG(pat),
	})
	require.NoError(t, err)
}

// TestPGAccountLifecycleLockOrder spec I1/A13⑤：普通 `{enabled:true}` 不得复活
// 「已禁用供应商」名下账号——批量更新按 users → accounts 协议锁**当前与目标
// owner**，并复核最终归属用户仍 active/可达。
func TestPGAccountLifecycleLockOrder(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "lifecycle-owner@example.com")
	other := seedSupplierUser(t, repos, "lifecycle-other@example.com")

	acc := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "lifecycle-acc")

	// ① 正常态：普通配置写（enabled=true 幂等）通过。
	enabled := true
	_, err := repos.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Enabled: &enabled})
	require.NoError(t, err, "归属供应商 active 时照常可写")

	// ② 禁用归属供应商（连带停其名下账号：user_repo 同事务 SetEnabled(false)）。
	disabled := domain.UserStatusDisabled
	_, err = repos.UpdateUser(ctx, &repository.UserPatch{ID: owner.ID, Status: &disabled})
	require.NoError(t, err)
	after, err := repos.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.False(t, after.Enabled, "禁用供应商 ⇒ 名下账号连带停用")

	// ③ **重新启用** ⇒ 拒绝（否则即「disabled 供应商的 enabled 账号」复活）。
	_, err = repos.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Enabled: &enabled})
	require.ErrorIs(t, err, repository.ErrInvalidInput,
		"禁用供应商名下的账号不得被重新启用（spec I1）")
	require.Contains(t, err.Error(), "active supplier-surface user")

	// ④ 事务回滚：账号仍为 disabled（拒绝发生在写之前）。
	after, err = repos.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.False(t, after.Enabled, "拒绝路径不得留下半套写入")

	// ⑤ 转属给**有效**供应商仍需锁定当前（失效）owner 并复核目标：本条断言
	// 「目标校验照旧生效」（active 目标可分配）。
	_, err = repos.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{SupplierUserID: &other.ID})
	require.NoError(t, err, "转属给 active 供应商应成功（当前 owner 失效不阻塞转出）")
	moved, err := repos.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, other.ID, moved.SupplierUserID)
}

// TestPGAccountScopeOwnershipPredicate A14③：同一批 id 在管理面（无作用域）与供应商
// 面（作用域）下返回不同的可见集——越域 id 0 行（ErrNotFound），不泄漏存在性。
func TestPGAccountScopeOwnershipPredicate(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "scope-owner@example.com")
	other := seedSupplierUser(t, repos, "scope-other@example.com")

	mine := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "scope-mine")
	theirs := seedOwnedAccount(t, repos, tpl.ID, other.ID, "scope-theirs")

	// 管理面全量：两条都可见。
	got, err := repos.Accounts.GetAccount(ctx, theirs.ID)
	require.NoError(t, err, "管理面（无作用域）不受归属限制")
	require.Equal(t, other.ID, got.SupplierUserID)

	// 供应商面：本人可见。
	got, err = repos.Accounts.GetAccount(scopeCtx(owner.ID), mine.ID)
	require.NoError(t, err)
	require.Equal(t, owner.ID, got.SupplierUserID)

	// 供应商面：他人 id ⇒ ErrNotFound（0 行，非 403——不泄漏存在性）。
	_, err = repos.Accounts.GetAccount(scopeCtx(owner.ID), theirs.ID)
	require.ErrorIs(t, err, repository.ErrNotFound, "越域单读必须 ErrNotFound")

	// 整批作用域校验：全部本属 ⇒ 无缺失；混入他人 ⇒ 报出该 id。
	_, missing, err := repos.FindMissingOwnedAccountID(scopeCtx(owner.ID), []int64{mine.ID})
	require.NoError(t, err)
	require.False(t, missing)

	badID, missing, err := repos.FindMissingOwnedAccountID(scopeCtx(owner.ID), []int64{mine.ID, theirs.ID})
	require.NoError(t, err)
	require.True(t, missing, "混入他人 id 必须报缺失")
	require.Equal(t, theirs.ID, badID)

	// 管理面缺省作用域 ⇒ 恒无缺失（既有语义不变）。
	_, missing, err = repos.FindMissingOwnedAccountID(ctx, []int64{theirs.ID})
	require.NoError(t, err)
	require.False(t, missing, "管理面无作用域 ⇒ 恒放行")
}

// TestPGUsageAggScopeC1 C1 回归（真实 SQL）：供应商面 ScanUsageAgg 必须**在 SQL 层**
// 过滤他人 usage_logs——即使调用方传入他人 account_id，也拿不到任何行；且管理面同一
// 查询仍能读到（证明过滤来自作用域而非数据缺失）。
func TestPGUsageAggScopeC1(t *testing.T) {
	repos := newPGReposShared(t)
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "usage-scope-owner@example.com")
	other := seedSupplierUser(t, repos, "usage-scope-other@example.com")

	mine := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "usage-scope-mine")
	theirs := seedOwnedAccount(t, repos, tpl.ID, other.ID, "usage-scope-theirs")

	from := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	to := from.Add(2 * time.Hour)
	logs := []*domain.UsageLog{
		{RequestID: "scope-mine-1", AccountID: mine.ID, Model: "m", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 100, RawCost: 200, CreatedAt: from},
		{RequestID: "scope-theirs-1", AccountID: theirs.ID, Model: "m", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 99, Cost: 9999, RawCost: 9999, CreatedAt: from},
	}
	require.NoError(t, repos.Usages.InsertBatch(context.Background(), logs))

	// 供应商面：**传入他人 id** 也拿不到（SQL 层 owner 过滤）。
	aggs, err := repos.ScanUsageAgg(scopeCtx(owner.ID), []int64{theirs.ID}, from, to)
	require.NoError(t, err)
	require.Empty(t, aggs, "越域 account_id 的聚合结果必须为空（C1：WHERE 必须 AND 归属）")

	// 供应商面：本属 id 正常返回。
	aggs, err = repos.ScanUsageAgg(scopeCtx(owner.ID), []int64{mine.ID}, from, to)
	require.NoError(t, err)
	require.Len(t, aggs, 1)
	require.Equal(t, int64(100), aggs[mine.ID].Cost)

	// 顺带：越域 + 本属混批时，他人行被过滤而本属行保留（证明是行级过滤而非整批失败）。
	aggs, err = repos.ScanUsageAgg(scopeCtx(owner.ID), []int64{mine.ID, theirs.ID}, from, to)
	require.NoError(t, err)
	require.Len(t, aggs, 1, "SQL 侧仅保留本属账号行")
	require.NotNil(t, aggs[mine.ID])
	require.Nil(t, aggs[theirs.ID])

	// 管理面（无作用域）：两条都可读（既有语义不变；过滤确由作用域引入）。
	aggs, err = repos.ScanUsageAgg(context.Background(), []int64{mine.ID, theirs.ID}, from, to)
	require.NoError(t, err)
	require.Len(t, aggs, 2, "管理面无作用域 ⇒ 全量可见")
}

// TestPGCodexKeyScopeIsolation 跨归属同组合键（spec §2.5「跨归属同键 ⇒ 行级 failed，
// 绝不得越权更新他人账号」）在真实 PG 上成立：作用域查重查不到他人行（不是「先全局
// 命中再应用层比归属」），插入撞全局唯一索引，**他人行与其凭据保持原值**。
func TestPGCodexKeyScopeIsolation(t *testing.T) {
	repos := newPGReposShared(t)
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "key-scope-owner@example.com")
	other := seedSupplierUser(t, repos, "key-scope-other@example.com")

	const email, acctID = "shared@example.com", "shared-acct"
	a := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "key-scope-a")
	seedCodexKeyExtPG(t, repos, a.ID, email, acctID, "owner-pat")

	otherCtx := scopeCtx(other.ID)
	// ① 供应商面（他人）：同组合键查重**查不到**（作用域谓词 AND 进 SQL）。
	_, err := repos.AccountExts.FindAccountExtByCodexKey(otherCtx, email, acctID)
	require.ErrorIs(t, err, repository.ErrNotFound,
		"越域同键不得被全局查重命中（否则即 spec 禁止的 TOCTOU 形态）")

	// 管理面（无作用域）：同键可见（证明过滤确由作用域引入）。
	got, err := repos.AccountExts.FindAccountExtByCodexKey(context.Background(), email, acctID)
	require.NoError(t, err)
	require.Equal(t, a.ID, got.AccountID)

	// ② 他人导入同键（新账号 + 同 ext 键）⇒ 撞全局唯一索引，事务回滚，无孤儿。
	b := seedOwnedAccount(t, repos, tpl.ID, other.ID, "key-scope-b")
	_, err = repos.AccountExts.UpsertAccountExt(context.Background(), &domain.AccountExt{
		AccountID: b.ID, CredentialType: credential.TypeCodexPAT,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "11111111-2222-3333-4444-555555555555"},
		CodexEmail:    strPtrPG(email), CodexAccountID: strPtrPG(acctID), CodexPATKey: strPtrPG("other-pat"),
	})
	require.Error(t, err, "跨归属同组合键必须撞唯一索引（row-level failed，不是改他人行）")

	// ③ 他人行与其凭据**未被改动**。
	cur, err := repos.AccountExts.GetAccountExt(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "owner-pat", *cur.CodexPATKey, "跨归属导入不得改他人凭据")
	// ④ 他人账号仍只归原主（作用域下他人仍不可见）。
	_, err = repos.Accounts.GetAccount(otherCtx, a.ID)
	require.ErrorIs(t, err, repository.ErrNotFound)

	// ⑤ 凭据 CAS 也不得越域写（§2.5「更新语句必须 AND 供应商作用域」）：他人带
	// **正确 revision** 轮转凭据 ⇒ 影响 0 行（ErrStaleRevision），他人 ext 不变。
	accA, err := repos.Accounts.GetAccount(context.Background(), a.ID)
	require.NoError(t, err)
	err = repos.AccountExts.AdminWritePATKeyCAS(otherCtx, a.ID, accA.LifecycleRevision, "attacker-pat")
	require.ErrorIs(t, err, repository.ErrStaleRevision, "越域 CAS 必须 0 行拒绝")
	cur, err = repos.AccountExts.GetAccountExt(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "owner-pat", *cur.CodexPATKey, "越域 CAS 不得改动他人凭据")
}

// TestPGAccountScopeOwnedExtRead C1 回归（ext 面）：GetOwnedAccountExt 越域不读 ext
// ——他人账号即使有 ext 行，供应商面也拿 ErrNotFound（不泄漏凭据、不探上游）。
func TestPGAccountScopeOwnedExtRead(t *testing.T) {
	repos := newPGReposShared(t)
	tpl := seedPGTemplate(t, repos)
	owner := seedSupplierUser(t, repos, "ext-scope-owner@example.com")
	other := seedSupplierUser(t, repos, "ext-scope-other@example.com")

	mine := seedOwnedAccount(t, repos, tpl.ID, owner.ID, "ext-scope-mine")
	theirs := seedOwnedAccount(t, repos, tpl.ID, other.ID, "ext-scope-theirs")

	seedPATExtPG(t, repos, mine.ID, "scope-mine-pat")
	seedPATExtPG(t, repos, theirs.ID, "scope-theirs-pat")

	// 供应商面：本人 ext 可读。
	e, err := repos.GetOwnedAccountExt(scopeCtx(owner.ID), mine.ID)
	require.NoError(t, err)
	require.NotNil(t, e.CodexPATKey)
	require.Equal(t, "scope-mine-pat", *e.CodexPATKey)

	// 供应商面：他人 ext ⇒ ErrNotFound（**不读 ext**）。
	_, err = repos.GetOwnedAccountExt(scopeCtx(owner.ID), theirs.ID)
	require.ErrorIs(t, err, repository.ErrNotFound, "越域 ext 单读必须 ErrNotFound（C1）")

	// 管理面：他人 ext 照常可读（既有语义不变）。
	e, err = repos.GetOwnedAccountExt(context.Background(), theirs.ID)
	require.NoError(t, err)
	require.Equal(t, "scope-theirs-pat", *e.CodexPATKey)
}
