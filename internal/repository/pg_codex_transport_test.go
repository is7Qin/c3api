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
// codex responses 出站传输开关（spec-codex-outbound-transport §4.1–§4.4）真实 PG
// 测试：列读写三态 + 全列替换清空 + 快照合并矩阵（含独立 if 门控——strip=NULL
// 行必须也合并）+ 陈旧 ext 行不合并。
// 基座见 pg_account_groups_test.go 的 newPGReposShared（逐测试克隆库）。
// ---------------------------------------------------------------------------

// seedPGCodexTemplate 建 codex 类型模板（merge 只对 codex 类型生效）。
func seedPGCodexTemplate(t *testing.T, repos *repository.Repository, name string) *domain.Template {
	t.Helper()
	tpl, err := repos.Templates.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	return tpl
}

// snapshotTransportOf 从 LoadGroupsAccounts 全量快照取账号模板合并后的出站传输。
func snapshotTransportOf(t *testing.T, repos *repository.Repository, groupID, accountID int64) (domain.CodexResponsesTransport, bool) {
	t.Helper()
	m, err := repos.Groups.LoadGroupsAccounts(context.Background())
	require.NoError(t, err)
	for _, a := range m[groupID] {
		if a.ID == accountID && a.Template != nil {
			return a.Template.CodexResponsesTransport, true
		}
	}
	return domain.CodexResponsesTransportDefault, false
}

// TestTemplateExtCodexTransportPG 列读写：""/"http"/"ws" 三态 roundtrip +
// 全列替换语义（缺省 / 空串 ⇒ 落 ""，非可空列不 Clear）+ 幂等 upsert 单行。
func TestTemplateExtCodexTransportPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGCodexTemplate(t, repos, "t-transport")

	// 未配置 = 零值 ""（NOT NULL DEFAULT ''：插入路径不显式 Set 也不为 NULL）
	saved := seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
	})
	require.Equal(t, domain.CodexResponsesTransportDefault, saved.CodexResponsesTransport)

	for _, want := range []domain.CodexResponsesTransport{
		domain.CodexResponsesTransportHTTP,
		domain.CodexResponsesTransportWS,
		domain.CodexResponsesTransportDefault,
	} {
		saved = seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: want,
		})
		require.Equal(t, want, saved.CodexResponsesTransport, "upsert 返回值必须反映落库值")

		got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
		require.NoError(t, err)
		require.Equal(t, want, got.CodexResponsesTransport, "回读必须与落库值一致")
	}

	rows, err := repos.Client.TemplateExt.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, rows, "同 template_id 幂等 upsert 恒单行")
}

// TestPGCodexTransportSnapshotMerge 快照合并矩阵：
//   - strip=NULL, transport=ws  ⇒ 合并 ws（**独立 if** 门控：塞进 strip 守卫里这行必红）
//   - strip=true, transport=ws  ⇒ 合并 ws + strip 保持 true（两列互不干扰）
//   - strip_image_tools 先写 true 再清 NULL，transport 仍生效（守卫与开关解耦）
func TestPGCodexTransportSnapshotMerge(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	t.Run("strip NULL transport ws merges", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-null")
		g := seedPGGroup(t, repos, "g-null")
		acc := seedPGAccount(t, repos, tpl.ID, "a-null")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		// 本行要的是「列缺失」这条路径的机制：**不传** StripImageTools ⇒ 仓库里
		// 的 SetNillableStripImageTools(nil) 是 no-op ⇒ strip_image_tools 列根本
		// 不进入 INSERT 列表 ⇒ 落 DB 默认值 NULL。**不要**为了"可读性"补成
		// new(false)：那是显式 false，走的是另一条路径（列在 INSERT 里、值非
		// NULL），本用例就退化成下面那条 strip=true 的近亲，独立 if 的门控再也
		// 测不到。
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok, "快照必须含该账号")
		require.Equal(t, domain.CodexResponsesTransportWS, got,
			"strip=NULL 行也必须合并出站传输（合并是独立 if，不得挂在 strip 守卫内）")

		members, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
		require.NoError(t, err)
		require.Len(t, members, 1)
		require.Equal(t, domain.CodexResponsesTransportWS, members[0].Template.CodexResponsesTransport,
			"组级重载数据源同语义")
	})

	t.Run("strip true transport ws merges and strip preserved", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-strip")
		g := seedPGGroup(t, repos, "g-strip")
		acc := seedPGAccount(t, repos, tpl.ID, "a-strip")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			StripImageTools:         new(true),
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		m, err := repos.Groups.LoadGroupsAccounts(ctx)
		require.NoError(t, err)
		var found *domain.Template
		for _, a := range m[g.ID] {
			if a.ID == acc.ID {
				found = a.Template
			}
		}
		require.NotNil(t, found)
		require.True(t, found.StripImageTools, "strip 列照旧合并")
		require.Equal(t, domain.CodexResponsesTransportWS, found.CodexResponsesTransport,
			"两列互不干扰：strip 与出站传输同一次合并各自独立")
	})

	t.Run("strip cleared later keeps transport", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-clear")
		g := seedPGGroup(t, repos, "g-clear")
		acc := seedPGAccount(t, repos, tpl.ID, "a-clear")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			StripImageTools:         new(true),
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})
		// 清 strip（nil ⇒ NULL），transport 原样重写：守卫为假时出站传输仍必须合并
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexOAuth,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.Equal(t, domain.CodexResponsesTransportWS, got)
	})
}

// TestPGCodexTransportMergeGating 三重门控的两个否定行：
//   - ext 行类型 ≠ 模板类型（模板改过类型、陈旧 ext 行仍带 ws）⇒ 不合并
//   - 模板不是 codex 类型（如 custom 挂着陈旧 ws 行）⇒ 不合并
func TestPGCodexTransportMergeGating(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	t.Run("stale ext row type mismatch not merged", func(t *testing.T) {
		tpl := seedPGCodexTemplate(t, repos, "t-stale")
		g := seedPGGroup(t, repos, "g-stale")
		acc := seedPGAccount(t, repos, tpl.ID, "a-stale")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		// 陈旧行：ext.credential_type=codex-pat 而模板是 codex-oauth
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCodexPAT,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.Equal(t, domain.CodexResponsesTransportDefault, got,
			"ext 行类型与模板类型不一致 ⇒ 不得合并（否则类型改回 codex 时 ws 突然生效）")
	})

	t.Run("non codex template not merged", func(t *testing.T) {
		tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: "t-custom", BaseURL: "",
			CredentialType:   credential.TypeCustom,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			ModelMapping:     domain.ModelMapping{},
		})
		require.NoError(t, err)
		g := seedPGGroup(t, repos, "g-custom")
		acc := seedPGAccount(t, repos, tpl.ID, "a-custom")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}))

		// 脏行：custom 模板的 ext 行带着 ws（写面会 400，这里模拟 DB 直改）
		seedTemplateExt(t, repos, &domain.TemplateExt{
			TemplateID: tpl.ID, CredentialType: credential.TypeCustom,
			CodexResponsesTransport: domain.CodexResponsesTransportWS,
		})

		got, ok := snapshotTransportOf(t, repos, g.ID, acc.ID)
		require.True(t, ok)
		require.Equal(t, domain.CodexResponsesTransportDefault, got,
			"非 codex 类型不经过 responses 出站面 ⇒ 不合并")
	})
}
