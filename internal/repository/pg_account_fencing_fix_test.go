// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestAdminFencingImportRevision tests admin re-import must CAS and increment.
func TestAdminFencingImportRevision(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	// seed via direct repo for simplicity: create template and group
	tpl := seedPGTemplate(t, repos)
	// Simulate import path: use repository directly to mimic admin vs SDK
	// Create account with ext via Upsert
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "fence-import", TemplateID: tpl.ID, UpstreamKey: "sk-1", MaxConcurrency: 25, Enabled: true})
	require.NoError(t, err)
	require.Equal(t, int64(1), acc.LifecycleRevision)
	// create ext
	ext := &domain.AccountExt{AccountID: acc.ID, CredentialType: "codex-oauth", CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1", SessionID: "sess-1", ThreadID: "sess-1", WindowID: "sess-1:0"}, CodexOAuthToken: strPtr("tok1"), CodexOAuthRefreshToken: strPtr("rt1"), CodexEmail: strPtr("a@example.com"), CodexAccountID: strPtr("acc1")}
	_, err = repos.AccountExts.UpsertAccountExt(ctx, ext)
	require.NoError(t, err)
	// Admin rotation via WriteOAuthRotationWithRevision should increment, but currently WriteOAuthRotation does not
	// Test SDK path does NOT increment
	before, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(1), before.LifecycleRevision)
	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "tok-sdk", "rt-sdk", nil))
	afterSDK, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(1), afterSDK.LifecycleRevision, "SDK refresh must not increment")
	// 管理员写入经唯一写点 → 推进 C
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{UpstreamKey: strPtr("sk-admin")})
	require.NoError(t, err)
	afterAdmin, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterAdmin.LifecycleRevision)
}

func strPtr(s string) *string { return &s }

// TestAdminFencingCostRevision tests cost CAS
func TestAdminFencingCostRevision(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cost-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	require.Equal(t, int64(1), acc.LifecycleRevision)
	zero := 0
	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &zero})
	require.NoError(t, err)
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), after.LifecycleRevision)
	require.Equal(t, 0, domain.MultBp(after.UpstreamCostMultiplierBp))
}

// TestAdminFencingCacheRevision similar
func TestAdminFencingCacheRevision(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cache-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	dom := "example.com"
	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{CacheDomain: &dom})
	require.NoError(t, err)
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), after.LifecycleRevision)
	require.Equal(t, dom, *after.CacheDomain)
}

// TestRecoverFencing tests status active recovery CAS
func TestRecoverFencing(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "recover-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	// fail via CAS
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, acc.ID, 1, "rule", time.Now(), "boom"))
	afterFail, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NotNil(t, afterFail.FailedAt)
	require.Equal(t, int64(2), afterFail.LifecycleRevision)
	// recover via CAS
	require.NoError(t, repos.Accounts.RecoverAccountCAS(ctx, acc.ID, 2))
	afterRec, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Nil(t, afterRec.FailedAt)
	require.Equal(t, int64(3), afterRec.LifecycleRevision)
	// stale recover should fail
	err := repos.Accounts.RecoverAccountCAS(ctx, acc.ID, 2)
	require.ErrorIs(t, err, repository.ErrConflict)
}

// TestFencedEnableNotClear tests enable does not clear failure
func TestFencedEnableNotClear(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "enable-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, acc.ID, 1, "sdk", time.Now(), "err"))
	afterFail, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterFail.LifecycleRevision)
	off := false
	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{Enabled: &off})
	require.NoError(t, err)
	afterDis, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(3), afterDis.LifecycleRevision)
	require.NotNil(t, afterDis.FailedAt, "enable must not clear failure")
	require.False(t, afterDis.Enabled)
}

func TestBatchFencingCredential(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a1, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "b1", TemplateID: tpl.ID, UpstreamKey: "sk-1", MaxConcurrency: 8, Enabled: true})
	a2, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "b2", TemplateID: tpl.ID, UpstreamKey: "sk-2", MaxConcurrency: 8, Enabled: true})
	// batch update with UpstreamKey should CAS per account
	newKey := "sk-new"
	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID}, repository.AccountPatch{UpstreamKey: &newKey})
	require.NoError(t, err)
	// Check that revisions incremented? Before fix, batch does not CAS, so revisions stay 1
	got1, _ := repos.Accounts.GetAccount(ctx, a1.ID)
	got2, _ := repos.Accounts.GetAccount(ctx, a2.ID)
	// This test will fail before fix if batch does not increment
	require.Equal(t, int64(2), got1.LifecycleRevision, "batch credential should increment")
	require.Equal(t, int64(2), got2.LifecycleRevision)
}

func TestStaleFirstExtPutLeavesNoExt(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "stale-first-ext", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	require.Equal(t, int64(1), acc.LifecycleRevision)
	ext := &domain.AccountExt{AccountID: acc.ID, CredentialType: "codex-oauth", CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1", SessionID: "sess-1", ThreadID: "sess-1", WindowID: "sess-1:0"}, CodexOAuthToken: strPtr("tok1"), CodexOAuthRefreshToken: strPtr("rt1"), CodexEmail: strPtr("e1@example.com"), CodexAccountID: strPtr("a1")}
	// stale expected 999 should fail and leave no ext
	_, err := repos.AccountExts.AdminUpsertAccountExtCAS(ctx, ext, 999)
	require.ErrorIs(t, err, repository.ErrConflict)
	_, err = repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.ErrorIs(t, err, repository.ErrNotFound, "stale first PUT must leave no ext")
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(1), after.LifecycleRevision, "stale must not increment")
	// correct expected should succeed
	_, err = repos.AccountExts.AdminUpsertAccountExtCAS(ctx, ext, 1)
	require.NoError(t, err)
	after2, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), after2.LifecycleRevision)
	got, _ := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.Equal(t, "tok1", *got.CodexOAuthToken)
}

// TestBatchMissingIDRollsBackFirst 批量原子性钉：批内任一 id 缺失 → 整批失败，
// 先写账号的凭据/revision 全部回滚。单事务 + FOR UPDATE 预锁（922c2cd）使
// "批中途被并发 CAS 变 stale" 在锁语义下不可能发生——旧 BatchHook 交错注入面
// 随之删除，原子性由回滚场景直接锁定。
func TestBatchMissingIDRollsBackFirst(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a1, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "batch-rb-1", TemplateID: tpl.ID, UpstreamKey: "sk-1", MaxConcurrency: 8, Enabled: true})
	require.Equal(t, int64(1), a1.LifecycleRevision)
	newKey := "sk-batch-new"
	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, 999999999}, repository.AccountPatch{UpstreamKey: &newKey})
	require.Error(t, err, "缺 id → 整批失败")
	got1, _ := repos.Accounts.GetAccount(ctx, a1.ID)
	require.Equal(t, "sk-1", got1.UpstreamKey, "first must remain unchanged due to atomic rollback")
	require.Equal(t, int64(1), got1.LifecycleRevision)
}

func TestAdminStaleDoesNotUpdateExt(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "stale-ext", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	ext := &domain.AccountExt{AccountID: acc.ID, CredentialType: "codex-oauth", CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1", SessionID: "sess-1", ThreadID: "sess-1", WindowID: "sess-1:0"}, CodexOAuthToken: strPtr("tok1"), CodexOAuthRefreshToken: strPtr("rt1"), CodexEmail: strPtr("e@example.com"), CodexAccountID: strPtr("a1")}
	_, _ = repos.AccountExts.UpsertAccountExt(ctx, ext)
	require.NoError(t, repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, 1, "tok2", "rt2", nil))
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), after.LifecycleRevision)
	ext2, _ := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.Equal(t, "tok2", *ext2.CodexOAuthToken)
	// stale with expected 1 should fail and not change ext nor revision
	err := repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, 1, "tok-stale", "rt-stale", nil)
	require.ErrorIs(t, err, repository.ErrConflict)
	afterStale, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterStale.LifecycleRevision, "stale must not increment")
	ext3, _ := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.Equal(t, "tok2", *ext3.CodexOAuthToken, "stale must not update ext")
	// SDK refresh must not increment but must update ext
	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "tok-sdk", "rt-sdk", nil))
	afterSDK, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterSDK.LifecycleRevision, "SDK refresh must not increment")
	extSDK, _ := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.Equal(t, "tok-sdk", *extSDK.CodexOAuthToken)
}
