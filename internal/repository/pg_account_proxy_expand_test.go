// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// pg_account_proxy_expand_test.go 账号**读路径**的出口展开矩阵（spec-proxy-pool
// §3.6，M1e）：`Account.Proxy` 由仓库批量装配，装配谓词必须随调用方作用域收窄——
// 这是本阶段最容易做错的一处（把「管理员看得见」当成「读路径一律展开」，供应商面
// 就会拿到别人代理的名字）。
//
// 跑法：TEST_DATABASE_URL=... go test -count=1 -p 1 ./internal/repository/ \
//      -run 'PGAccountReadExpandsProxy' -v

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// bindProxy 把账号绑定到代理（走真实批量写面的三态入口，不直接写列）。
func bindProxy(t *testing.T, repos *repository.Repository, accID, proxyID int64) {
	t.Helper()
	_, err := repos.Accounts.UpdateAccountsBatch(context.Background(), []int64{accID},
		repository.AccountPatch{ProxyID: &proxyID})
	require.NoError(t, err)
}

// TestPGAccountReadExpandsProxy 展开矩阵：
//
//	主体 / 绑定目标          | ProxyID | Proxy  | 说明
//	管理员 + 平台代理         |  非 nil | 非 nil | 指向的行存在就展开
//	管理员 + 私池代理         |  非 nil | 非 nil |
//	供应商本人 + 自己的代理   |  非 nil | 非 nil |
//	供应商本人 + 平台代理     |  非 nil | nil    | 平台池不是公共池：看不见就是看不见
//	任意 + 已软删的代理       |  非 nil | nil    | 绑定缺失（既有语义）
//	一页多账号共用同一代理    |  非 nil | 各自非 nil |
//
// 同时覆盖三条读入口（GetAccount / GetAccountWithTemplate / ListAccounts——列表视图
// `ListAccountViews` 就走 ListAccounts）。
func TestPGAccountReadExpandsProxy(t *testing.T) {
	repos := newPGReposShared(t)
	adminCtx := context.Background()
	tpl := seedPGTemplateNamed(t, repos, "tpl-proxy-expand")
	supA := seedSupplierUser(t, repos, "expand-a@example.com")
	supB := seedSupplierUser(t, repos, "expand-b@example.com")
	ctxA := scopeCtx(supA.ID)

	newAccount := func(name string, owner int64) int64 {
		acc, err := repos.Accounts.CreateAccount(adminCtx, &domain.Account{
			Name: name, TemplateID: tpl.ID, UpstreamKey: "sk-" + name,
			MaxConcurrency: 8, Enabled: true, SupplierUserID: owner,
		})
		require.NoError(t, err)
		return acc.ID
	}

	platformProxy := seedPGProxyNamed(t, repos, "expand-platform-proxy")
	privateProxyA := seedPGProxyOwned(t, repos, supA.ID, "expand-private-a")

	platformBound := newAccount("expand-platform-bound", 0)
	bindProxy(t, repos, platformBound, platformProxy)

	supOwnProxy := newAccount("expand-sup-own", supA.ID)
	bindProxy(t, repos, supOwnProxy, privateProxyA)

	// 管理员把平台代理绑到**供应商 A 的账号**上：绑定是合法写（§3.6 平台代理 →
	// 任意账号），但 A 自己读这一行时看不见那个代理。
	supPlatformBound := newAccount("expand-sup-platform", supA.ID)
	bindProxy(t, repos, supPlatformBound, platformProxy)

	// 另一个供应商的账号也绑同一个私池代理（谓词禁止新绑定，但历史/管理员夹具要
	// 覆盖「同一代理被一页里多个账号共用」的展开约定）。
	shared := newAccount("expand-shared-1", supA.ID)
	bindProxy(t, repos, shared, privateProxyA)
	shared2 := newAccount("expand-shared-2", supA.ID)
	bindProxy(t, repos, shared2, privateProxyA)

	deletedProxy := seedPGProxyNamed(t, repos, "expand-deleted-proxy")
	deletedBound := newAccount("expand-deleted-bound", 0)
	bindProxy(t, repos, deletedBound, deletedProxy)
	require.NoError(t, repos.Proxies.DeleteProxy(adminCtx, deletedProxy), "软删（同事务解绑所有引用）")
	// 解绑连坐会把 proxy_id 清空 ⇒ 重新造一条「绑定指向已软删行」的历史状态：直接
	// 通过写面落值（仓库只落值，不校验存在性——校验在 service 谓词）。
	_, err := repos.Accounts.UpdateAccountsBatch(adminCtx, []int64{deletedBound},
		repository.AccountPatch{ProxyID: &deletedProxy})
	require.NoError(t, err)

	t.Run("admin expands the bound row", func(t *testing.T) {
		got, err := repos.Accounts.GetAccount(adminCtx, platformBound)
		require.NoError(t, err)
		require.NotNil(t, got.Proxy, "管理员：指向的行存在就展开")
		require.Equal(t, platformProxy, got.Proxy.ID)
		require.Equal(t, "expand-platform-proxy", got.Proxy.Name)
		require.Nil(t, got.Proxy.OwnerUID, "平台代理 owner 为 NULL")
	})

	t.Run("admin expands a private proxy it did not own", func(t *testing.T) {
		got, err := repos.Accounts.GetAccount(adminCtx, supOwnProxy)
		require.NoError(t, err)
		require.NotNil(t, got.Proxy)
		require.Equal(t, privateProxyA, got.Proxy.ID)
		require.NotNil(t, got.Proxy.OwnerUID)
		require.Equal(t, supA.ID, *got.Proxy.OwnerUID)
	})

	t.Run("supplier expands its own proxy", func(t *testing.T) {
		got, err := repos.Accounts.GetAccount(ctxA, supOwnProxy)
		require.NoError(t, err)
		require.NotNil(t, got.Proxy, "本人私池 ⇒ 展开")
		require.Equal(t, privateProxyA, got.Proxy.ID)
		require.Equal(t, "expand-private-a", got.Proxy.Name)
	})

	t.Run("supplier does not expand a platform proxy bound by an admin", func(t *testing.T) {
		got, err := repos.Accounts.GetAccount(ctxA, supPlatformBound)
		require.NoError(t, err)
		require.NotNil(t, got.ProxyID, "绑定事实（proxy_id 列）不被可见性改写")
		require.Equal(t, platformProxy, *got.ProxyID)
		require.Nil(t, got.Proxy, "平台代理对供应商不可见 ⇒ 不展开（看不见就是看不见）")
	})

	t.Run("another supplier cannot see A's private proxy", func(t *testing.T) {
		got, err := repos.Accounts.GetAccount(scopeCtx(supB.ID), supOwnProxy)
		require.ErrorIs(t, err, repository.ErrNotFound, "越域账号本身就读不到（归属谓词）")
		require.Nil(t, got)
	})

	t.Run("soft-deleted proxy stays unexpanded", func(t *testing.T) {
		got, err := repos.Accounts.GetAccount(adminCtx, deletedBound)
		require.NoError(t, err)
		require.NotNil(t, got.ProxyID, "绑定列仍是那个 id（历史状态）")
		require.Nil(t, got.Proxy, "已软删 ⇒ 不留条目（绑定缺失）")
	})

	t.Run("GetAccountWithTemplate expands too", func(t *testing.T) {
		got, err := repos.Accounts.GetAccountWithTemplate(adminCtx, platformBound)
		require.NoError(t, err)
		require.NotNil(t, got.Proxy)
		require.Equal(t, platformProxy, got.Proxy.ID)
	})

	t.Run("list page expands every row sharing one proxy", func(t *testing.T) {
		rows, _, err := repos.Accounts.ListAccounts(adminCtx, repository.ListQuery{Name: "expand-shared", Limit: 50})
		require.NoError(t, err)
		require.Len(t, rows, 2, "两条共用同一个代理的账号必须都在这一页")
		for _, row := range rows {
			require.NotNil(t, row.Proxy, "共用同一代理的每一行各自展开（不许只填一行）")
			require.Equal(t, privateProxyA, row.Proxy.ID)
			require.Equal(t, "expand-private-a", row.Proxy.Name)
		}
		require.NotSame(t, rows[0].Proxy, rows[1].Proxy, "每行各持一份指针：共用同一对象会让一行的就地改写泄漏成另一行的事实（可变对象别名，不是指针身份判等）")
	})

	t.Run("list page is scope-aware per row", func(t *testing.T) {
		rows, _, err := repos.Accounts.ListAccounts(ctxA, repository.ListQuery{Name: "expand-sup", Limit: 50})
		require.NoError(t, err)
		byName := make(map[string]*domain.Account, len(rows))
		for _, row := range rows {
			byName[row.Name] = row
		}
		own := byName["expand-sup-own"]
		require.NotNil(t, own)
		require.NotNil(t, own.Proxy, "自己的代理：展开")
		platform := byName["expand-sup-platform"]
		require.NotNil(t, platform)
		require.NotNil(t, platform.ProxyID)
		require.Nil(t, platform.Proxy, "平台代理：同一页里对供应商不展开")
	})

	t.Run("unbound account has no proxy", func(t *testing.T) {
		plain := newAccount("expand-unbound", 0)
		got, err := repos.Accounts.GetAccount(adminCtx, plain)
		require.NoError(t, err)
		require.Nil(t, got.ProxyID)
		require.Nil(t, got.Proxy)
	})

	t.Run("create echoes the expansion", func(t *testing.T) {
		acc, err := repos.Accounts.CreateAccount(adminCtx, &domain.Account{
			Name: "expand-on-create", TemplateID: tpl.ID, UpstreamKey: "sk-expand-on-create",
			MaxConcurrency: 8, Enabled: true, ProxyID: &platformProxy,
		})
		require.NoError(t, err)
		require.NotNil(t, acc.Proxy, "创建响应与读路径同形（否则新建即回显 id 但名字为 null）")
		require.Equal(t, "expand-platform-proxy", acc.Proxy.Name)
	})
}
