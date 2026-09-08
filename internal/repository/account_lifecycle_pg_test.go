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

func TestPGAccountCostDefaults(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cost-default", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8})
	require.NoError(t, err)
	require.Equal(t, int64(1), a.LifecycleRevision, "revision starts at 1")
	require.True(t, a.Enabled, "enabled defaults true")
	require.Equal(t, 10000, a.UpstreamCostMultiplierBp, "multiplier defaults 10000")
	require.Nil(t, a.CacheDomain, "cache_domain defaults nil")
	require.Nil(t, a.FailureSource)
}

func TestPGGetAccountLoadsTemplateForLifecycleFencing(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	account, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "fencing-template", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8})
	require.NoError(t, err)

	got, err := repos.Accounts.GetAccountWithTemplate(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Template, "lifecycle fencing needs the template to recompute candidate identity")
	require.Equal(t, tpl.ID, got.Template.ID)
}

func TestPGAccountCostValidation(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	// Negative multiplier should be rejected via service validation, but repo also should allow? Test at repo level: direct repo create with negative should still write (service guards), but we test service path separately.
	// Here test that cost 0, 10000, high succeed via CAS update.
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cost", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8})
	require.NoError(t, err)
	require.NoError(t, repos.Accounts.UpdateAccountCostMultiplierCAS(ctx, a.ID, a.LifecycleRevision, 0))
	got, err := repos.Accounts.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 0, got.UpstreamCostMultiplierBp)
	require.Equal(t, int64(2), got.LifecycleRevision)

	require.NoError(t, repos.Accounts.UpdateAccountCostMultiplierCAS(ctx, got.ID, got.LifecycleRevision, 10000))
	got2, _ := repos.Accounts.GetAccount(ctx, got.ID)
	require.Equal(t, 10000, got2.UpstreamCostMultiplierBp)
	require.Equal(t, int64(3), got2.LifecycleRevision)

	require.NoError(t, repos.Accounts.UpdateAccountCostMultiplierCAS(ctx, got2.ID, got2.LifecycleRevision, 50000))
	got3, _ := repos.Accounts.GetAccount(ctx, got2.ID)
	require.Equal(t, 50000, got3.UpstreamCostMultiplierBp)
}

func TestPGAccountCacheDomain(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cache", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8})
	require.NoError(t, err)
	shared := "cache.example.com"
	require.NoError(t, repos.Accounts.UpdateAccountCacheDomainCAS(ctx, a.ID, a.LifecycleRevision, &shared))
	got, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.NotNil(t, got.CacheDomain)
	require.Equal(t, shared, *got.CacheDomain)

	// empty (nil) domain = private per-account
	require.NoError(t, repos.Accounts.UpdateAccountCacheDomainCAS(ctx, got.ID, got.LifecycleRevision, nil))
	got2, _ := repos.Accounts.GetAccount(ctx, got.ID)
	require.Nil(t, got2.CacheDomain)
}

func TestPGAccountLifecycleRevision(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "lifecycle", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8})
	require.NoError(t, err)
	rev1 := a.LifecycleRevision
	require.Equal(t, int64(1), rev1)

	// Fail CAS increments
	failedAt := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, a.ID, rev1, "rule", failedAt, "boom"))
	got, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, int64(2), got.LifecycleRevision)
	require.NotNil(t, got.FailedAt)
	require.NotNil(t, got.FailureSource)
	require.Equal(t, "rule", *got.FailureSource)

	// Recover CAS increments
	require.NoError(t, repos.Accounts.RecoverAccountCAS(ctx, a.ID, got.LifecycleRevision))
	got2, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, int64(3), got2.LifecycleRevision)
	require.Nil(t, got2.FailedAt)
	require.Nil(t, got2.FailureSource)

	// Enable CAS increments and does not clear failure (enable must not silently clear)
	// first fail again
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, a.ID, got2.LifecycleRevision, "sdk", failedAt, "again"))
	got3, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.NotNil(t, got3.FailedAt)
	revBeforeEnable := got3.LifecycleRevision
	require.NoError(t, repos.Accounts.SetAccountEnabledCAS(ctx, a.ID, revBeforeEnable, false))
	got4, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, revBeforeEnable+1, got4.LifecycleRevision)
	require.False(t, got4.Enabled)
	require.NotNil(t, got4.FailedAt, "enable must not clear failure")
}

func TestPGAccountRevisionStaleReject(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "stale", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8})
	require.NoError(t, err)
	rev1 := a.LifecycleRevision
	failedAt := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, a.ID, rev1, "rule", failedAt, "first"))
	// stale Fail with old revision should fail
	err = repos.Accounts.FailAccountCAS(ctx, a.ID, rev1, "rule", failedAt, "stale")
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict)

	// stale Recover with old revision should fail
	got, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, int64(2), got.LifecycleRevision)
	err = repos.Accounts.RecoverAccountCAS(ctx, a.ID, rev1)
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict)

	// correct Recover with current revision succeeds
	require.NoError(t, repos.Accounts.RecoverAccountCAS(ctx, a.ID, got.LifecycleRevision))
	got2, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Nil(t, got2.FailedAt)

	// stale enable with old revision
	err = repos.Accounts.SetAccountEnabledCAS(ctx, a.ID, rev1, false)
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict)

	// credential CAS stale
	err = repos.Accounts.ReplaceAccountCredentialCAS(ctx, a.ID, rev1, "sk-new", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict)

	// cost CAS stale
	err = repos.Accounts.UpdateAccountCostMultiplierCAS(ctx, a.ID, rev1, 123)
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict)

	// cache CAS stale
	dom := "a.example.com"
	err = repos.Accounts.UpdateAccountCacheDomainCAS(ctx, a.ID, rev1, &dom)
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict)
}

func TestPGAccountBatchAndImport(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a1 := seedPGAccount(t, repos, tpl.ID, "b1")
	a2 := seedPGAccount(t, repos, tpl.ID, "b2")

	// batch update cost/cache/enabled
	enabled := false
	cost := 25000
	domainStr := "batch.example.com"
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID}, repository.AccountPatch{Enabled: &enabled, UpstreamCostMultiplierBp: &cost, CacheDomain: &domainStr}))
	for _, id := range []int64{a1.ID, a2.ID} {
		got, err := repos.Accounts.GetAccount(ctx, id)
		require.NoError(t, err)
		require.False(t, got.Enabled)
		require.Equal(t, 25000, got.UpstreamCostMultiplierBp)
		require.NotNil(t, got.CacheDomain)
		require.Equal(t, domainStr, *got.CacheDomain)
	}
	// clear cache domain via batch empty string
	empty := ""
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{CacheDomain: &empty}))
	got, _ := repos.Accounts.GetAccount(ctx, a1.ID)
	require.Nil(t, got.CacheDomain)
}
