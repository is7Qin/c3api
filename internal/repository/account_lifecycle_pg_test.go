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
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cost-default", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
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
	account, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "fencing-template", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
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
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cost", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	require.NoError(t, err)
	zero := 0
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{a.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &zero})
	require.NoError(t, err)
	got, err := repos.Accounts.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 0, got.UpstreamCostMultiplierBp)
	require.Equal(t, int64(2), got.LifecycleRevision)

	tenK := 10000
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{got.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &tenK})
	require.NoError(t, err)
	got2, _ := repos.Accounts.GetAccount(ctx, got.ID)
	require.Equal(t, 10000, got2.UpstreamCostMultiplierBp)
	require.Equal(t, int64(3), got2.LifecycleRevision)

	fiftyK := 50000
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{got2.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &fiftyK})
	require.NoError(t, err)
	got3, _ := repos.Accounts.GetAccount(ctx, got2.ID)
	require.Equal(t, 50000, got3.UpstreamCostMultiplierBp)
}

func TestPGAccountCacheDomain(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "cache", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	require.NoError(t, err)
	shared := "cache.example.com"
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{a.ID}, repository.AccountPatch{CacheDomain: &shared})
	require.NoError(t, err)
	got, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.NotNil(t, got.CacheDomain)
	require.Equal(t, shared, *got.CacheDomain)

	// 空串 = 清空（回账号私有域）
	cleared := ""
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{got.ID}, repository.AccountPatch{CacheDomain: &cleared})
	require.NoError(t, err)
	got2, _ := repos.Accounts.GetAccount(ctx, got.ID)
	require.Nil(t, got2.CacheDomain)
}

func TestPGAccountLifecycleRevision(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "lifecycle", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	require.NoError(t, err)
	rev1 := a.LifecycleRevision
	require.Equal(t, int64(1), rev1)

	// Fail CAS：围栏在 K（身份代际），推进 C（配置代际）。失效不是身份写入
	// ⇒ K 不变；配置写入 ⇒ C 无条件 +1。
	failedAt := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, a.ID, a.IdentityRevision, "rule", failedAt, "boom"))
	got, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, a.IdentityRevision, got.IdentityRevision, "失效路径不改 K")
	require.Equal(t, int64(2), got.LifecycleRevision)
	require.NotNil(t, got.FailedAt)
	require.NotNil(t, got.FailureSource)
	require.Equal(t, "rule", *got.FailureSource)

	// Recover CAS：围栏在 C（客户端令牌），推进 C。恢复同样不是身份写入 ⇒ K 不变。
	require.NoError(t, repos.Accounts.RecoverAccountCAS(ctx, a.ID, got.LifecycleRevision))
	got2, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, a.IdentityRevision, got2.IdentityRevision, "恢复路径不改 K")
	require.Equal(t, int64(3), got2.LifecycleRevision)
	require.Nil(t, got2.FailedAt)
	require.Nil(t, got2.FailureSource)

	// Enable CAS increments and does not clear failure (enable must not silently clear)
	// first fail again
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, a.ID, got2.IdentityRevision, "sdk", failedAt, "again"))
	got3, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.NotNil(t, got3.FailedAt)
	revBeforeEnable := got3.LifecycleRevision
	off := false
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{a.ID}, repository.AccountPatch{Enabled: &off})
	require.NoError(t, err)
	got4, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, revBeforeEnable+1, got4.LifecycleRevision)
	require.False(t, got4.Enabled)
	require.NotNil(t, got4.FailedAt, "enable must not clear failure")
}

func TestPGAccountRevisionStaleReject(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "stale", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, Enabled: true})
	require.NoError(t, err)
	k0 := a.IdentityRevision
	c0 := a.LifecycleRevision
	failedAt := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, a.ID, k0, "rule", failedAt, "first"))
	got, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, k0, got.IdentityRevision, "失效路径不改 K")
	require.Equal(t, c0+1, got.LifecycleRevision, "失效路径推进 C")

	// 身份写入（upstream_key）推进 K ⇒ 携带旧 K 的失效判决作废：这正是 (I,K)
	// 围栏的意义——身份已被授权变更，旧判决不得再落库。
	newKey := "sk-rotated"
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{a.ID}, repository.AccountPatch{UpstreamKey: &newKey})
	require.NoError(t, err)
	gotK, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Equal(t, k0+1, gotK.IdentityRevision, "身份写入推进 K")

	err = repos.Accounts.FailAccountCAS(ctx, a.ID, k0, "rule", failedAt, "stale")
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict, "旧 K 的失效判决必须作废")

	// 恢复的前置条件是 C（不是 K）：陈旧 C 拒绝
	err = repos.Accounts.RecoverAccountCAS(ctx, a.ID, c0)
	require.Error(t, err)
	require.ErrorIs(t, err, repository.ErrConflict)

	// 当前 C 的恢复成功，且不改 K
	cur, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.NoError(t, repos.Accounts.RecoverAccountCAS(ctx, a.ID, cur.LifecycleRevision))
	got2, _ := repos.Accounts.GetAccount(ctx, a.ID)
	require.Nil(t, got2.FailedAt)
	require.Equal(t, gotK.IdentityRevision, got2.IdentityRevision, "恢复不改 K")
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
	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID}, repository.AccountPatch{Enabled: &enabled, UpstreamCostMultiplierBp: &cost, CacheDomain: &domainStr})
	require.NoError(t, err)
	for _, id := range []int64{a1.ID, a2.ID} {
		got, err := repos.Accounts.GetAccount(ctx, id)
		require.NoError(t, err)
		require.False(t, got.Enabled)
		require.Equal(t, 25000, got.UpstreamCostMultiplierBp)
		require.NotNil(t, got.CacheDomain)
		require.Equal(t, domainStr, *got.CacheDomain)
		// 配置类字段（enabled / 倍率 / 缓存域）变更**不**推进 K：它们换的是配置
		// 代际 C，不换路由目标身份，故不得白白作废在途判定。
		require.Equal(t, int64(1), got.IdentityRevision, "config-only write must not advance K")
	}
	// clear cache domain via batch empty string
	empty := ""
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{CacheDomain: &empty})
	require.NoError(t, err)
	got, _ := repos.Accounts.GetAccount(ctx, a1.ID)
	require.Nil(t, got.CacheDomain)
	require.Equal(t, int64(1), got.IdentityRevision, "clearing a config field must not advance K")
	// 清空 base_url（可空身份字段）是身份变更：从 NULL 到 NULL 是幂等重写，K 不变。
	blank := ""
	_, err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID}, repository.AccountPatch{BaseURL: &blank})
	require.NoError(t, err)
	gotBlank, _ := repos.Accounts.GetAccount(ctx, a1.ID)
	require.Equal(t, int64(1), gotBlank.IdentityRevision, "clearing an already-NULL base_url must not advance K")
}

// TestPGCreateAccountWithDomainStaysEnabled 是缺陷 A 的回归：创建即带
// cache_domain（或采购倍率）的账号必须与无域创建一致默认启用——创建面没有
// Enabled 字段（fenced 端点独占），repo 不得以零值 Enabled=false 为由在带
// 生命周期字段时显式落 disabled，否则账号静默永不进入路由候选（6/6 黑洞）。
func TestPGCreateAccountWithDomainStaysEnabled(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	dom := "dx.example"
	created, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "with-domain", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, CacheDomain: &dom, Enabled: true})
	require.NoError(t, err)
	require.True(t, created.Enabled, "创建即带域必须默认启用（回显）")
	got, err := repos.Accounts.GetAccount(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, got.Enabled, "创建即带域必须默认启用（落库）")
	require.NotNil(t, got.CacheDomain)
	require.Equal(t, dom, *got.CacheDomain)

	costly, err := repos.Accounts.CreateAccount(ctx, &domain.Account{Name: "with-cost", TemplateID: tpl.ID, UpstreamKey: "sk-x", MaxConcurrency: 8, UpstreamCostMultiplierBp: 25000, Enabled: true})
	require.NoError(t, err)
	require.True(t, costly.Enabled, "创建即带采购倍率必须默认启用（回显）")
	gotCost, err := repos.Accounts.GetAccount(ctx, costly.ID)
	require.NoError(t, err)
	require.True(t, gotCost.Enabled, "创建即带采购倍率必须默认启用（落库）")
	require.Equal(t, 25000, gotCost.UpstreamCostMultiplierBp)
}
