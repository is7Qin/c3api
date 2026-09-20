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

// TestPGListAccountsEnabledFilter 是 `GET /accounts` 的 enabled 三态过滤在**生产
// 谓词**上的覆盖：handler 用例只打到内存替身，替身自实现过滤 ⇒ 生产谓词
// （account_repo.go 的 EnabledEQ）被删除时不会有任何用例失败。本用例直连真实
// repository，故谓词缺失即失败。
//
// 三态语义：true = 仅启用；false = 仅禁用；nil = 不过滤。过滤的是管理面启停列
// （enabled），与运行时失效 failed_at 无关——故这里另加一个"已失效但仍启用"的
// 账号，证明它仍被 true 过滤命中。
func TestPGListAccountsEnabledFilter(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	seedPGAccount(t, repos, tpl.ID, "enabled-a")
	enabledB := seedPGAccount(t, repos, tpl.ID, "enabled-b")

	disabled, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "disabled-a", TemplateID: tpl.ID, UpstreamKey: "sk-disabled-a", MaxConcurrency: 8, Enabled: false,
	})
	require.NoError(t, err)
	require.False(t, disabled.Enabled, "fixture must actually be disabled")

	// 运行时失效但管理面仍启用：enabled 过滤不得把它当成 disabled。
	failedAt := time.Now()
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, enabledB.ID, enabledB.IdentityRevision, "rule", failedAt, "boom"))

	names := func(q repository.ListQuery) []string {
		rows, _, err := repos.Accounts.ListAccounts(ctx, q)
		require.NoError(t, err)
		out := make([]string, 0, len(rows))
		for _, a := range rows {
			out = append(out, a.Name)
		}
		return out
	}

	all := names(repository.ListQuery{})
	require.ElementsMatch(t, []string{"enabled-a", "enabled-b", "disabled-a"}, all, "nil = 不过滤")

	onlyEnabled := names(repository.ListQuery{Enabled: boolPtr(true)})
	require.ElementsMatch(t, []string{"enabled-a", "enabled-b"}, onlyEnabled,
		"true = 仅启用（运行时失效的账号管理面仍启用，故仍在列）")

	onlyDisabled := names(repository.ListQuery{Enabled: boolPtr(false)})
	require.Equal(t, []string{"disabled-a"}, onlyDisabled, "false = 仅禁用")

	// 与其它谓词叠加：启用 + 名称过滤。
	combined := names(repository.ListQuery{Enabled: boolPtr(true), Name: "enabled-a"})
	require.Equal(t, []string{"enabled-a"}, combined)
}
