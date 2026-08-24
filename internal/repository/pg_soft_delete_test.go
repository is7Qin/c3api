// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// #29 软删除真实 PostgreSQL 语义锚（基座同 pg_auth_test：newPGRepos 每测试
// 重建 schema）。覆盖计划 §5：列表过滤（含 count）/GET 单个可见 + deleted_at/
// 唯一约束占位/消费路径过滤/批量事务原子/组删除级联。
// ---------------------------------------------------------------------------

// TestPGSoftDeleteListFilters 软删后列表过滤（含 count）+ GET 单个可见（含
// deleted_at）——5 实体全覆盖。
func TestPGSoftDeleteListFilters(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "sd-list@example.com")

	// 存活基准：各实体建一活项，列表应可见
	tpl := seedPGTemplate(t, repos)
	g := seedPGGroup(t, repos, "sd-live-g")
	acc := seedPGAccount(t, repos, tpl.ID, "sd-live-a")
	k, err := repos.Keys.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "sd-live-k",
		KeyRaw: "sd-live-raw", Status: domain.KeyStatusActive,
	})
	require.NoError(t, err)
	ruleID, err := repos.CreateRule(ctx, domain.Rule{Name: "sd-live-r", Enabled: true, Priority: 1000})
	require.NoError(t, err)

	// 逐个软删：列表过滤（count 同谓词）；GET 单个仍可查且 deleted_at 非空
	require.NoError(t, repos.DeleteTemplate(ctx, tpl.ID))
	got, err := repos.GetTemplate(ctx, tpl.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DeletedAt, "软删模板 GET 单个可见（含 deleted_at）")
	rows, total, err := repos.ListTemplates(ctx, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(0), total, "模板列表过滤已删（count 同谓词）")
	require.Empty(t, rows)

	require.NoError(t, repos.DeleteGroup(ctx, g.ID))
	gotG, err := repos.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.NotNil(t, gotG.DeletedAt)
	groups, gTotal, err := repos.ListGroups(ctx, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(0), gTotal, "组列表过滤已删")
	require.Empty(t, groups)

	require.NoError(t, repos.DeleteAccount(ctx, acc.ID))
	gotA, err := repos.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, gotA.DeletedAt)
	accs, aTotal, err := repos.ListAccounts(ctx, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(0), aTotal, "账号列表过滤已删")
	require.Empty(t, accs)

	require.NoError(t, repos.DeleteKey(ctx, k.ID))
	gotK, err := repos.GetKey(ctx, k.ID)
	require.NoError(t, err)
	require.NotNil(t, gotK.DeletedAt)
	keys, kTotal, err := repos.ListKeysByUser(ctx, u.ID, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(0), kTotal, "key 列表过滤已删")
	require.Empty(t, keys)

	require.NoError(t, repos.DeleteRule(ctx, ruleID))
	rules, err := repos.ListRules(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, rules, "已删规则不进列表（规则引擎 Reload 消费同路径）")
}

// TestPGSoftDeleteUniqueHeld 软删项仍占唯一约束：同名/同明文重建 → ErrConflict
// （审计优先：唯一检查不加 deleted_at 过滤，与 DB 约束一致；同名重建走运维 SQL）。
func TestPGSoftDeleteUniqueHeld(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "sd-uniq@example.com")

	t.Run("group name", func(t *testing.T) {
		g := seedPGGroup(t, repos, "sd-uniq-g")
		require.NoError(t, repos.DeleteGroup(ctx, g.ID))
		_, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "sd-uniq-g", Visibility: domain.GroupVisibilityPublic})
		require.ErrorIs(t, err, repository.ErrConflict, "软删组名仍占唯一约束 → 同名重建 409")
	})

	t.Run("template name", func(t *testing.T) {
		tpl := seedPGTemplate(t, repos)
		require.NoError(t, repos.DeleteTemplate(ctx, tpl.ID))
		_, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
			Name: tpl.Name, BaseURL: "https://u2",
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		})
		require.ErrorIs(t, err, repository.ErrConflict, "软删模板名仍占唯一约束 → 同名重建 409")
	})

	t.Run("rule priority", func(t *testing.T) {
		ruleID, err := repos.CreateRule(ctx, domain.Rule{Name: "sd-uniq-r", Enabled: true, Priority: 2000})
		require.NoError(t, err)
		require.NoError(t, repos.DeleteRule(ctx, ruleID))
		_, err = repos.CreateRule(ctx, domain.Rule{Name: "sd-uniq-r2", Enabled: true, Priority: 2000})
		require.ErrorIs(t, err, repository.ErrConflict, "软删规则 priority 仍占唯一约束 → 重建 409")
	})

	t.Run("key raw", func(t *testing.T) {
		g := seedPGGroup(t, repos, "sd-uniq-kg")
		k, err := repos.Keys.CreateKey(ctx, &domain.Key{
			UserID: u.ID, GroupID: g.ID, Name: "sd-uniq-k",
			KeyRaw: "sd-uniq-raw", Status: domain.KeyStatusActive,
		})
		require.NoError(t, err)
		require.NoError(t, repos.DeleteKey(ctx, k.ID))
		_, err = repos.Keys.CreateKey(ctx, &domain.Key{
			UserID: u.ID, GroupID: g.ID, Name: "sd-uniq-k2",
			KeyRaw: k.KeyRaw, Status: domain.KeyStatusActive,
		})
		require.ErrorIs(t, err, repository.ErrConflict, "软删 key_raw 仍占唯一约束 → 重建 409")
	})
}

// TestPGSoftDeleteConsumptionFilters 消费路径过滤已删：LoadKeys（鉴权快照）、
// GetKeyByRaw（鉴权按未找到拒绝）、LoadGroupsAccounts（调度器快照）、
// ListRules（规则引擎 Reload 数据源——ListFilters 已锚）。
func TestPGSoftDeleteConsumptionFilters(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "sd-consum@example.com")
	tpl := seedPGTemplate(t, repos)
	g := seedPGGroup(t, repos, "sd-consum-g")
	acc := seedPGAccount(t, repos, tpl.ID, "sd-consum-a")
	require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g.ID}), "账号入组（快照断言前提）")
	k, err := repos.Keys.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "sd-consum-k",
		KeyRaw: "sd-consum-raw", Status: domain.KeyStatusActive,
	})
	require.NoError(t, err)

	// 删除前快照含全部
	m, err := repos.Keys.LoadKeys(ctx)
	require.NoError(t, err)
	require.Contains(t, m, k.KeyRaw, "活 key 在鉴权快照")
	snap, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	require.Contains(t, snap, g.ID, "活组在调度器快照")
	require.Contains(t, snap[g.ID][0].Template.Name, tpl.Name, "账号带模板")

	// 软删后消费路径全部过滤
	require.NoError(t, repos.DeleteKey(ctx, k.ID))
	m2, err := repos.Keys.LoadKeys(ctx)
	require.NoError(t, err)
	require.NotContains(t, m2, k.KeyRaw, "已软删 key 不进鉴权快照（鉴权拒绝）")
	gone, err := repos.GetKeyByRaw(ctx, k.KeyRaw)
	require.NoError(t, err)
	require.Nil(t, gone, "已软删 key 按未找到处理")

	require.NoError(t, repos.DeleteGroup(ctx, g.ID))
	require.NoError(t, repos.DeleteAccount(ctx, acc.ID))
	snap2, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	require.NotContains(t, snap2, g.ID, "已软删组不进调度器快照")
	single, err := repos.Groups.LoadGroupAccounts(ctx, g.ID)
	require.NoError(t, err)
	require.Empty(t, single, "已软删账号不进组内账号快照")
}

// TestPGSoftDeleteBatchAtomic 批量软删：存在性检查（缺失 id → ErrNotFound）+
// 事务原子（任一失败整体回滚——缺失 id 场景下存活项不被软删）。
func TestPGSoftDeleteBatchAtomic(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t1 := seedPGTemplate(t, repos)
	t2, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "sd-batch-2", BaseURL: "https://u2",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)

	// 全成：批量软删两模板 → 均带 deleted_at、列表过滤
	require.NoError(t, repos.DeleteTemplatesBatch(ctx, []int64{t1.ID, t2.ID}))
	got1, err := repos.GetTemplate(ctx, t1.ID)
	require.NoError(t, err)
	require.NotNil(t, got1.DeletedAt, "批量软删生效")
	rows, total, err := repos.ListTemplates(ctx, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(0), total, "批量软删后列表过滤")
	require.Empty(t, rows)

	// 原子：t3 存活 + 缺失 id → ErrNotFound → t3 保持存活（全成或全败）。
	// 注意 t3 必须唯一名：软删的 t1 仍占 "t" 唯一约束（本测试即软删占位语义）。
	t3, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "sd-batch-3", BaseURL: "https://u3",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	err = repos.DeleteTemplatesBatch(ctx, []int64{t3.ID, 999999})
	require.ErrorIs(t, err, repository.ErrNotFound, "批量存在性检查缺 id → ErrNotFound")
	got3, err := repos.GetTemplate(ctx, t3.ID)
	require.NoError(t, err)
	require.Nil(t, got3.DeletedAt, "整体回滚：存活项未被软删")
}

// TestPGSoftDeleteGroupCascade 组删除级联：组内 key 一并软删（service 调用链
// DeleteKeysByGroup → DeleteGroup 的仓库语义）。
func TestPGSoftDeleteGroupCascade(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "sd-cascade@example.com")
	g := seedPGGroup(t, repos, "sd-cascade-g")
	k1, err := repos.Keys.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "sd-c1",
		KeyRaw: "sd-c1-raw", Status: domain.KeyStatusActive,
	})
	require.NoError(t, err)
	k2, err := repos.Keys.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "sd-c2",
		KeyRaw: "sd-c2-raw", Status: domain.KeyStatusActive,
	})
	require.NoError(t, err)

	// 删组前置清理（service.DeleteGroup 调用链）→ 组内 key 软删 + 返回明文
	raws, err := repos.DeleteKeysByGroup(ctx, g.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{k1.KeyRaw, k2.KeyRaw}, raws, "返回本次被删明文列表")
	for _, k := range []int64{k1.ID, k2.ID} {
		got, err := repos.GetKey(ctx, k)
		require.NoError(t, err)
		require.NotNil(t, got.DeletedAt, "级联软删：组内 key 带 deleted_at")
	}
	require.NoError(t, repos.DeleteGroup(ctx, g.ID))
	gotG, err := repos.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.NotNil(t, gotG.DeletedAt, "组软删")
	// 消费路径全过滤
	snap, err := repos.Groups.LoadGroupsAccounts(ctx)
	require.NoError(t, err)
	require.NotContains(t, snap, g.ID)
	auth, err := repos.Keys.LoadKeys(ctx)
	require.NoError(t, err)
	require.NotContains(t, auth, k1.KeyRaw)
	require.NotContains(t, auth, k2.KeyRaw)
}
