// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 装载展开的四状态矩阵（spec-proxy-pool §4.1 / §9.2 第 1 行）：**两个装载器**都必须
// 按 `proxy_id` 把 `Proxy` 展开好，且**都不决定 `Routable`**。
//
//	状态              | ProxyID | Proxy            | Routable（本层恒 false，派生在 scheduler）
//	未绑定            | nil     | nil              | false
//	绑定且可用        | id      | 非 nil,Enabled=T | false
//	绑定但禁用        | id      | 非 nil,Enabled=F | false
//	绑定但缺失/已软删 | id      | nil              | false
//
// `Routable=false` 不是"装载器认为不可用"，而是"装载器不碰这个位"——它唯一的派生
// 点在 scheduler 的 `newSnapshotStatic`（brief §1）。这条断言正是把"别在装载器里
// 顺手算一个"钉死：一旦有人在本层写它，四个状态里至少一个会变 true。
func TestPGLoadAccountsExpandsProxy(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplateNamed(t, repos, "tpl-proxy-expand")
	g := seedPGGroup(t, repos, "grp-proxy-expand")

	// ① 绑定且可用：凭据一并落库，展开必须带出来（M3 的 Dial 要用）。
	boundProxy, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "expand-bound", Scheme: domain.ProxySchemeSOCKS5H, Host: "10.9.9.9", Port: 1080,
		Username: "u-exp", Password: "p-exp", Enabled: true,
	})
	require.NoError(t, err)
	// ② 绑定但 enabled=false：行仍存在 ⇒ Proxy 非 nil（回显/排障用），只是不可用。
	disabledProxy, err := repos.Proxies.CreateProxy(ctx, domain.Proxy{
		Name: "expand-disabled", Scheme: domain.ProxySchemeHTTP, Host: "10.9.9.8", Port: 3128,
		Enabled: true,
	})
	require.NoError(t, err)
	cur, err := repos.Proxies.GetProxy(ctx, disabledProxy)
	require.NoError(t, err)
	cur.Enabled = false
	require.NoError(t, repos.Proxies.UpdateProxy(ctx, cur, repository.ProxyCredential{}))
	// ③ 绑定但缺失：行被软删**而绑定未清除**。仓库的 DeleteProxy 会在同事务里
	// 解绑，所以这个状态只能由"删除解绑失败/残留窗口"（spec L6/L8）产生——测试
	// 直接改 ent 行伪造该窗口（不经 DeleteProxy，才能留住 proxy_id）。
	missingProxy := seedPGProxyNamed(t, repos, "expand-missing")

	direct := seedPGAccount(t, repos, tpl.ID, "expand-direct")
	bound := seedPGAccount(t, repos, tpl.ID, "expand-bound-acc")
	disabledBound := seedPGAccount(t, repos, tpl.ID, "expand-disabled-acc")
	missingBound := seedPGAccount(t, repos, tpl.ID, "expand-missing-acc")

	bind := func(accID, proxyID int64) {
		t.Helper()
		_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{accID}, repository.AccountPatch{ProxyID: &proxyID})
		require.NoError(t, err)
	}
	bind(bound.ID, boundProxy)
	bind(disabledBound.ID, disabledProxy)
	bind(missingBound.ID, missingProxy)
	for _, id := range []int64{direct.ID, bound.ID, disabledBound.ID, missingBound.ID} {
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, id, []int64{g.ID}))
	}
	require.NoError(t, repos.Client.Proxy.UpdateOneID(missingProxy).SetDeletedAt(time.Now()).Exec(ctx))

	// 两个数据源：全量（LoadGroupsAccounts）与组级（LoadGroupAccounts）。
	loadFull := func(t *testing.T) map[int64]*domain.Account {
		t.Helper()
		m, err := repos.Groups.LoadGroupsAccounts(ctx)
		require.NoError(t, err)
		out := make(map[int64]*domain.Account, len(m[g.ID]))
		for _, a := range m[g.ID] {
			out[a.ID] = a
		}
		return out
	}
	loadGroup := func(t *testing.T) map[int64]*domain.Account {
		t.Helper()
		members, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, err)
		out := make(map[int64]*domain.Account, len(members))
		for _, a := range members {
			out[a.ID] = a
		}
		return out
	}

	for _, tc := range []struct {
		loader string
		load   func(*testing.T) map[int64]*domain.Account
	}{
		{"LoadGroupsAccounts", loadFull},
		{"LoadGroupAccounts", loadGroup},
	} {
		t.Run(tc.loader, func(t *testing.T) {
			byID := tc.load(t)
			require.Len(t, byID, 4, "四个账号都要在快照里")

			// ① 未绑定 ⇒ ProxyID nil、Proxy nil。
			a := byID[direct.ID]
			require.Nil(t, a.ProxyID, "未绑定：proxy_id 为 NULL")
			require.Nil(t, a.Proxy, "未绑定：不展开")
			require.False(t, a.Routable, "装载器不得决定 Routable")

			// ② 绑定且可用 ⇒ 展开全字段（含凭据）。
			a = byID[bound.ID]
			require.NotNil(t, a.ProxyID)
			require.Equal(t, boundProxy, *a.ProxyID)
			require.NotNil(t, a.Proxy, "绑定且可用：必须展开")
			require.Equal(t, boundProxy, a.Proxy.ID)
			require.Equal(t, domain.ProxySchemeSOCKS5H, a.Proxy.Scheme)
			require.Equal(t, "10.9.9.9", a.Proxy.Host)
			require.Equal(t, 1080, a.Proxy.Port)
			require.Equal(t, "u-exp", a.Proxy.Username)
			require.Equal(t, "p-exp", a.Proxy.Password, "展开必须带凭据（M3 的 Dial 用它）")
			require.True(t, a.Proxy.Enabled)
			require.False(t, a.Routable, "装载器不得决定 Routable")

			// ③ 绑定但禁用 ⇒ 行仍展开（enabled=false 是可判定的回显事实）。
			a = byID[disabledBound.ID]
			require.NotNil(t, a.ProxyID)
			require.NotNil(t, a.Proxy, "禁用 ≠ 缺失：行还在，必须保留回显")
			require.Equal(t, disabledProxy, a.Proxy.ID)
			require.False(t, a.Proxy.Enabled)
			require.False(t, a.Routable, "装载器不得决定 Routable（禁用由 Routable 派生）")

			// ④ 绑定但已软删 ⇒ Proxy 留 nil（"绑定缺失"的可判定形态）。
			a = byID[missingBound.ID]
			require.NotNil(t, a.ProxyID, "残留窗口里 proxy_id 仍非空")
			require.Equal(t, missingProxy, *a.ProxyID)
			require.Nil(t, a.Proxy, "已软删 ⇒ 不展开（缺失，与未绑定靠 ProxyID 区分）")
			require.False(t, a.Routable)
		})
	}
}

// TestPGLoadAccountsProxySharedInstance 钉住展开后的 `*domain.Proxy` 是**同一个
// 指针**（一次全表扫描 → 一份索引 → 按 id 复用）：每个账号各自 new 一个代理对象
// 会让后续"编译事实按摘要比较"退化为按地址比较，且白白多 N 次分配。
func TestPGLoadAccountsProxySharedInstance(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplateNamed(t, repos, "tpl-proxy-share")
	g := seedPGGroup(t, repos, "grp-proxy-share")
	proxyID := seedPGProxyNamed(t, repos, "share-one")

	a1 := seedPGAccount(t, repos, tpl.ID, "share-a1")
	a2 := seedPGAccount(t, repos, tpl.ID, "share-a2")
	for _, id := range []int64{a1.ID, a2.ID} {
		_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{id}, repository.AccountPatch{ProxyID: &proxyID})
		require.NoError(t, err)
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, id, []int64{g.ID}))
	}

	m, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	byID := map[int64]*domain.Account{}
	for _, a := range m[g.ID] {
		byID[a.ID] = a
	}
	require.Len(t, byID, 2)
	require.NotNil(t, byID[a1.ID].Proxy)
	require.Same(t, byID[a1.ID].Proxy, byID[a2.ID].Proxy, "同一代理必须共享同一个展开对象")
}
