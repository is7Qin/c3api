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
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "fence-import", TemplateID: tpl.ID, UpstreamKey: "sk-1", Weight: 100, MaxConcurrency: 25})
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
	// Admin path should increment
	// Use CAS method if available, otherwise this will fail before fix
	err = repos.Accounts.ReplaceAccountCredentialCAS(ctx, acc.ID, 1, "sk-admin", nil)
	require.NoError(t, err, "admin credential CAS should succeed")
	afterAdmin, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterAdmin.LifecycleRevision)
	// stale should fail
	err = repos.Accounts.ReplaceAccountCredentialCAS(ctx, acc.ID, 1, "sk-stale", nil)
	require.ErrorIs(t, err, repository.ErrConflict)
}

func strPtr(s string) *string { return &s }

// TestAdminFencingCostRevision tests cost CAS
func TestAdminFencingCostRevision(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cost-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 1, MaxConcurrency: 8})
	require.Equal(t, int64(1), acc.LifecycleRevision)
	require.NoError(t, repos.Accounts.UpdateAccountCostMultiplierCAS(ctx, acc.ID, 1, 0))
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), after.LifecycleRevision)
	require.Equal(t, 0, after.UpstreamCostMultiplierBp)
	// stale
	err := repos.Accounts.UpdateAccountCostMultiplierCAS(ctx, acc.ID, 1, 100)
	require.ErrorIs(t, err, repository.ErrConflict)
}

// TestAdminFencingCacheRevision similar
func TestAdminFencingCacheRevision(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cache-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 1, MaxConcurrency: 8})
	dom := "example.com"
	require.NoError(t, repos.Accounts.UpdateAccountCacheDomainCAS(ctx, acc.ID, 1, &dom))
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), after.LifecycleRevision)
	require.Equal(t, dom, *after.CacheDomain)
}

// TestRecoverFencing tests status active recovery CAS
func TestRecoverFencing(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "recover-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 1, MaxConcurrency: 8})
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
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "enable-fence", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 1, MaxConcurrency: 8})
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, acc.ID, 1, "sdk", time.Now(), "err"))
	afterFail, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterFail.LifecycleRevision)
	require.NoError(t, repos.Accounts.SetAccountEnabledCAS(ctx, acc.ID, 2, false))
	afterDis, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(3), afterDis.LifecycleRevision)
	require.NotNil(t, afterDis.FailedAt, "enable must not clear failure")
	require.False(t, afterDis.Enabled)
}

func TestBatchFencingCredential(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a1, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "b1", TemplateID: tpl.ID, UpstreamKey: "sk-1", Weight: 1, MaxConcurrency: 8})
	a2, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "b2", TemplateID: tpl.ID, UpstreamKey: "sk-2", Weight: 1, MaxConcurrency: 8})
	// batch update with UpstreamKey should CAS per account
	newKey := "sk-new"
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID}, repository.AccountPatch{UpstreamKey: &newKey}))
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
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "stale-first-ext", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 1, MaxConcurrency: 8})
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

func TestBatchStaleSecondLeavesFirstUnchanged(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a1, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "batch-stale-1", TemplateID: tpl.ID, UpstreamKey: "sk-1", Weight: 1, MaxConcurrency: 8})
	a2, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "batch-stale-2", TemplateID: tpl.ID, UpstreamKey: "sk-2", Weight: 1, MaxConcurrency: 8})
	require.Equal(t, int64(1), a1.LifecycleRevision)
	require.Equal(t, int64(1), a2.LifecycleRevision)
	// Set hook to make second stale: after first account's Save, concurrently increment second's revision
	repository.BatchHook = func(id int64) {
		if id == a1.ID {
			// Increment a2's revision via direct CAS to make its expected stale
			_ = repos.Accounts.ReplaceAccountCredentialCAS(context.Background(), a2.ID, 1, "sk-concurrent", nil)
		}
	}
	defer func() { repository.BatchHook = nil }()
	newKey := "sk-batch-new"
	err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID}, repository.AccountPatch{UpstreamKey: &newKey})
	require.ErrorIs(t, err, repository.ErrConflict, "second stale should make batch fail")
	// First must remain unchanged (rolled back)
	got1, _ := repos.Accounts.GetAccount(ctx, a1.ID)
	require.Equal(t, "sk-1", got1.UpstreamKey, "first must remain unchanged due to atomic rollback")
	require.Equal(t, int64(1), got1.LifecycleRevision)
	got2, _ := repos.Accounts.GetAccount(ctx, a2.ID)
	require.Equal(t, "sk-concurrent", got2.UpstreamKey, "second should be from concurrent increment, not batch")
	require.Equal(t, int64(2), got2.LifecycleRevision)
}

func TestCombinedCredentialRecoverySingleIncrement(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "combined", TemplateID: tpl.ID, UpstreamKey: "sk-old", Weight: 1, MaxConcurrency: 8})
	// fail to make recovery needed
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, acc.ID, 1, "rule", time.Now(), "boom"))
	afterFail, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterFail.LifecycleRevision)
	require.NotNil(t, afterFail.FailedAt)
	// Combined credential+recovery in one batch should increment exactly once (2->3)
	newKey := "sk-new-combined"
	statusActive := domain.StatusActive
	err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{UpstreamKey: &newKey, Status: &statusActive})
	require.NoError(t, err)
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(3), after.LifecycleRevision, "combined must increment exactly once, not twice")
	require.Equal(t, "sk-new-combined", after.UpstreamKey)
	require.Nil(t, after.FailedAt, "recovery should clear")
	require.Nil(t, after.FailureSource)
}

func TestAdminStaleDoesNotUpdateExt(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	acc, _ := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "stale-ext", TemplateID: tpl.ID, UpstreamKey: "sk-x", Weight: 1, MaxConcurrency: 8})
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
