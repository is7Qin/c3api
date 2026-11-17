// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGListAccountsGroupFilter 是 `GET /accounts?group_id=`（及供应商面同参）在
// **生产谓词**上的覆盖（spec 2026-11-17 §3.1）：handler 用例只打到内存替身，
// 替身自实现过滤 ⇒ 生产谓词被删/被写成两态时不会有任何用例失败。
//
// 三态语义（唯一判据点是 account_repo.ListAccounts 的 pred）：
//   - nil   = 不过滤（区别于 &0——这是本次最容易被「0 = 不过滤」旧先例带偏的一处）
//   - &0    = 未分组（没有任何分组）
//   - &n>0  = 命中该分组（多分组账号 A+B 在两个分组下都应命中）
//
// 分页与 total 必须同谓词：limit=1 时 total 仍是过滤后的总数（不是页内条数）。
func TestPGListAccountsGroupFilter(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	gA := seedPGGroup(t, repos, "group-a")
	gB := seedPGGroup(t, repos, "group-b")
	gC := seedPGGroup(t, repos, "group-c") // 存在但无成员：与「不存在的 id」是两件事

	onlyA := seedPGAccount(t, repos, tpl.ID, "only-a")
	onlyB := seedPGAccount(t, repos, tpl.ID, "only-b")
	both := seedPGAccount(t, repos, tpl.ID, "a-and-b")
	none := seedPGAccount(t, repos, tpl.ID, "no-group")

	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, onlyA.ID, []int64{gA.ID}))
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, onlyB.ID, []int64{gB.ID}))
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, both.ID, []int64{gA.ID, gB.ID}))
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, none.ID, []int64{}))

	list := func(q repository.ListQuery) ([]string, int64, int) {
		rows, total, err := repos.Accounts.ListAccounts(ctx, q)
		require.NoError(t, err)
		names := make([]string, 0, len(rows))
		for _, a := range rows {
			names = append(names, a.Name)
		}
		return names, total, len(rows)
	}

	t.Run("positive group id hits members only", func(t *testing.T) {
		names, total, n := list(repository.ListQuery{AccountGroupID: new(gA.ID), Limit: 200})
		require.ElementsMatch(t, []string{"only-a", "a-and-b"}, names, "&A ⇒ A 与 A+B")
		require.Equal(t, int64(2), total, "total 与页内一致")
		require.Equal(t, 2, n)
	})

	t.Run("zero means ungrouped", func(t *testing.T) {
		zero := int64(0)
		names, total, n := list(repository.ListQuery{AccountGroupID: &zero, Limit: 200})
		require.Equal(t, []string{"no-group"}, names, "&0 ⇒ 只命中无分组账号（不是「不过滤」）")
		require.Equal(t, int64(1), total)
		require.Equal(t, 1, n)
	})

	t.Run("nil means no filter", func(t *testing.T) {
		names, total, n := list(repository.ListQuery{Limit: 200})
		require.ElementsMatch(t, []string{"only-a", "only-b", "a-and-b", "no-group"}, names, "nil ⇒ 全部四行")
		require.Equal(t, int64(4), total)
		require.Equal(t, 4, n)
	})

	t.Run("unknown group id is empty not error", func(t *testing.T) {
		missing := int64(999_999)
		names, total, n := list(repository.ListQuery{AccountGroupID: &missing})
		require.Empty(t, names, "不存在的分组 id ⇒ 0 行（不是 400）")
		require.Zero(t, total)
		require.Zero(t, n)
	})

	t.Run("total counts the whole filter, not the page", func(t *testing.T) {
		names, total, n := list(repository.ListQuery{AccountGroupID: new(gA.ID), Limit: 1, Offset: 0})
		require.Equal(t, int64(2), total, "limit=1 时 total 仍为过滤后的总数")
		require.Equal(t, 1, n, "页内只返回一条")
		require.Len(t, names, 1)
	})

	t.Run("composable with other predicates", func(t *testing.T) {
		names, total, _ := list(repository.ListQuery{AccountGroupID: new(gA.ID), Name: "a-and"})
		require.Equal(t, []string{"a-and-b"}, names, "分组谓词与名称模糊匹配叠加")
		require.Equal(t, int64(1), total)
	})

	// 分组**存在但没有任何成员**：0 行不是错误，也不是「不过滤」——与上面
	// 「不存在的分组 id」是两件不同的事（后者连行都没有）。
	t.Run("existing group without members is empty", func(t *testing.T) {
		names, total, n := list(repository.ListQuery{AccountGroupID: new(gC.ID), Limit: 200})
		require.Empty(t, names, "空分组 ⇒ 0 行")
		require.Zero(t, total, "空分组的 total 也是 0")
		require.Zero(t, n)
	})

	// 作用域 × 分组（spec 2026-11-17 §3.3）：EXISTS 子查询不得绕过归属谓词——
	// 供应商 B 拿 A 的分组 id 去筛，命中 0 行；同一分组 A 自己筛则命中。
	t.Run("scope does not expose other owners' members", func(t *testing.T) {
		ownerA := seedSupplierUser(t, repos, "gf-owner-a@example.com")
		ownerB := seedSupplierUser(t, repos, "gf-owner-b@example.com")
		gOwned := seedPGGroup(t, repos, "group-owned")
		mine := seedOwnedAccount(t, repos, tpl.ID, ownerA.ID, "owned-a")
		theirs := seedOwnedAccount(t, repos, tpl.ID, ownerB.ID, "owned-b")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, mine.ID, []int64{gOwned.ID}))
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, theirs.ID, []int64{gOwned.ID}))

		// 管理面（无作用域）：同一分组的两个归属都可见（证明数据确实在）。
		adminRows, adminTotal, err := repos.Accounts.ListAccounts(ctx,
			repository.ListQuery{AccountGroupID: new(gOwned.ID), Limit: 200})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"owned-a", "owned-b"}, namesOf(adminRows))
		require.Equal(t, int64(2), adminTotal)

		// 供应商 B 筛同一分组：只看到自己的那条（A 的账号被归属谓词滤掉）。
		rowsB, totalB, err := repos.Accounts.ListAccounts(scopeCtx(ownerB.ID),
			repository.ListQuery{AccountGroupID: new(gOwned.ID), Limit: 200})
		require.NoError(t, err)
		require.Equal(t, []string{"owned-b"}, namesOf(rowsB), "越域账号不得因 EXISTS 子查询漏出")
		require.Equal(t, int64(1), totalB, "total 同样受作用域约束")

		// 供应商 A 筛同一分组：只看到自己的那条。
		rowsA, totalA, err := repos.Accounts.ListAccounts(scopeCtx(ownerA.ID),
			repository.ListQuery{AccountGroupID: new(gOwned.ID), Limit: 200})
		require.NoError(t, err)
		require.Equal(t, []string{"owned-a"}, namesOf(rowsA))
		require.Equal(t, int64(1), totalA)
	})
}

// namesOf 账号名列表（作用域 subtest 的报告可读性用）。
func namesOf(accs []*domain.Account) []string {
	out := make([]string, 0, len(accs))
	for _, a := range accs {
		out = append(out, a.Name)
	}
	return out
}
