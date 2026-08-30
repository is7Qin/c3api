// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// W4 模板快照扩展真实 PG 测试：StripImageTools 从 template_ext 合并进调度器
// 快照（LoadGroupsAccounts / LoadGroupAccounts）——热路径零 DB 的数据源。
// 基座见 pg_account_groups_test.go 的 newPGRepos（DROP SCHEMA 重建）。
// ---------------------------------------------------------------------------

// snapshotStripOf 从 LoadGroupsAccounts 全量快照取账号的模板 StripImageTools。
func snapshotStripOf(t *testing.T, repos *repository.Repository, groupID, accountID int64) (bool, bool) {
	t.Helper()
	m, err := repos.Groups.LoadGroupsAccounts(context.Background())
	require.NoError(t, err)
	for _, a := range m[groupID] {
		if a.ID == accountID && a.Template != nil {
			return a.Template.StripImageTools, true
		}
	}
	return false, false
}

// TestPGStripSnapshotLoad 模板 ext 开关 → 调度器快照 StripImageTools 合并：
// 全量（LoadGroupsAccounts）与组级（LoadGroupAccounts）两条数据源一致；
// 无 ext 行 → false（未配置 = 关闭）；ext 更新后重载反映新值（热路径零 DB，
// 配置经快照重载生效）。
func TestPGStripSnapshotLoad(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	g := seedPGGroup(t, repos, "g")
	acc := seedPGAccount(t, repos, tpl.ID, "a1")
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

	t.Run("no ext row defaults off", func(t *testing.T) {
		strip, ok := snapshotStripOf(t, repos, g.ID, acc.ID)
		require.True(t, ok, "快照必须含该账号")
		require.False(t, strip, "无 ext 行 = 未配置 = 关闭")
		// 组级数据源同语义
		members, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, err)
		require.Len(t, members, 1)
		require.False(t, members[0].Template.StripImageTools)
	})

	t.Run("ext strip true merged into snapshot", func(t *testing.T) {
		_, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeResponsesSpecial,
			StripImageTools: boolPtrPG(true),
		})
		require.NoError(t, err)
		strip, ok := snapshotStripOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.True(t, strip, "ext strip_image_tools=true 必须合并进全量快照")
		members, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, err)
		require.True(t, members[0].Template.StripImageTools, "组级重载数据源同语义")
	})

	t.Run("ext update false after reload", func(t *testing.T) {
		_, err := repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeResponsesSpecial,
			StripImageTools: boolPtrPG(false),
		})
		require.NoError(t, err)
		strip, ok := snapshotStripOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.False(t, strip, "ext 更新后重载快照必须反映新值（false）")
	})

	t.Run("other ext columns untouched", func(t *testing.T) {
		// 非 special 类型 ext 行（oauth）不带 strip：快照保持 false
		tpl2, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "t2", BaseURL: "https://u/v1",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			ModelMapping:     domain.ModelMapping{},
		})
		require.NoError(t, err)
		g2 := seedPGGroup(t, repos, "g2")
		acc2 := seedPGAccount(t, repos, tpl2.ID, "a2")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc2.ID, []int64{g2.ID}))
		_, err = repos.TemplateExts.UpsertTemplateExt(ctx, &domain.TemplateExt{
			TemplateID: tpl2.ID, CredentialType: credential.TypeCodexOAuth,
		})
		require.NoError(t, err)
		strip, ok := snapshotStripOf(t, repos, g2.ID, acc2.ID)
		require.True(t, ok)
		require.False(t, strip, "oauth 类型无 strip 配置 → 快照 false")
	})
}
