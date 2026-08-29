// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGGroupMultiplierDefault 组倍率默认 10000（T3.5）：Create 缺省（service
// 归一 10000 恒写入）→ ×1；LoadGroupMultipliers 全量读出。
func TestPGGroupMultiplierDefault(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "def-mult", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000})
	require.NoError(t, err)
	require.Equal(t, 10000, g.PriceMultiplier, "Create 缺省 ×1")

	got, err := repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, 10000, got.PriceMultiplier, "roundtrip：读回默认倍率")

	mults, err := repos.Groups.LoadGroupMultipliers(ctx)
	require.NoError(t, err)
	require.Equal(t, 10000, mults[g.ID], "快照含该组默认倍率")
}

// TestPGGroupMultiplierSetUpdate 组倍率显式设置 + 更新（T3.5）：Create 设
// 15000 → 读回；Update 显式 0 = 免费组 → 读回 0；再改 20000 → 读回。
func TestPGGroupMultiplierSetUpdate(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "mult-g", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 15000,
	})
	require.NoError(t, err)
	require.Equal(t, 15000, g.PriceMultiplier, "Create 显式设倍率")

	got, err := repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, 15000, got.PriceMultiplier, "roundtrip：显式倍率读回")

	// Update 恒写入：显式 0 = 免费组
	g.PriceMultiplier = 0
	updated, err := repos.Groups.UpdateGroup(ctx, g)
	require.NoError(t, err)
	require.Equal(t, 0, updated.PriceMultiplier, "Update 显式 0 = 免费组")

	g.PriceMultiplier = 20000
	updated, err = repos.Groups.UpdateGroup(ctx, g)
	require.NoError(t, err)
	require.Equal(t, 20000, updated.PriceMultiplier, "Update 改倍率")
}

// TestPGAssignmentMultiplierRoundtrip 用户-组专属倍率 roundtrip（T3.5 修正：
// 按组挂载）：设置/读回/清除（nil）/再设置；同用户不同组倍率互不干扰。
func TestPGAssignmentMultiplierRoundtrip(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	g1, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "am-g1", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000})
	require.NoError(t, err)
	g2, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "am-g2", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000})
	require.NoError(t, err)
	u := seedPGUser(t, repos, "am@example.com")
	require.NoError(t, repos.GrantGroup(ctx, g1.ID, u.ID))
	require.NoError(t, repos.GrantGroup(ctx, g2.ID, u.ID))

	// 初始：未设置（nil）
	as, err := repos.ListAssignmentsByGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.Len(t, as, 1)
	require.Nil(t, as[0].PriceMultiplier, "Create 未设置 → nil")

	// 设置专属倍率 15000
	require.NoError(t, repos.SetAssignmentMultiplier(ctx, g1.ID, u.ID, intPtr(15000)))
	as, err = repos.ListAssignmentsByGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.NotNil(t, as[0].PriceMultiplier)
	require.Equal(t, 15000, *as[0].PriceMultiplier, "设置读回")

	// 同用户另一组独立（未设置）
	as2, err := repos.ListAssignmentsByGroup(ctx, g2.ID)
	require.NoError(t, err)
	require.Nil(t, as2[0].PriceMultiplier, "同用户不同组倍率独立（按组）")

	// 清除为未设置（nil）
	require.NoError(t, repos.SetAssignmentMultiplier(ctx, g1.ID, u.ID, nil))
	as, err = repos.ListAssignmentsByGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.Nil(t, as[0].PriceMultiplier, "nil 清除为未设置")

	// 设置 0 = 免费
	require.NoError(t, repos.SetAssignmentMultiplier(ctx, g1.ID, u.ID, intPtr(0)))
	as, err = repos.ListAssignmentsByGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.Equal(t, 0, *as[0].PriceMultiplier, "0 = 免费是已设置值")

	// 授予行缺失 → ErrNotFound
	require.ErrorIs(t, repos.SetAssignmentMultiplier(ctx, g1.ID, 999999, intPtr(10000)), repository.ErrNotFound)
}

// TestPGLoadAssignmentMultipliers LoadAssignmentMultipliers：仅 price_multiplier
// 非 NULL 行进入 map（存在 = 已设置；缺失 = 未设置 → 用组倍率）；撤销后行消失。
func TestPGLoadAssignmentMultipliers(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	g1, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "lam-g1", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000})
	require.NoError(t, err)
	g2, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "lam-g2", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000})
	require.NoError(t, err)
	u1 := seedPGUser(t, repos, "lam1@example.com")
	u2 := seedPGUser(t, repos, "lam2@example.com")
	require.NoError(t, repos.GrantGroup(ctx, g1.ID, u1.ID))
	require.NoError(t, repos.GrantGroup(ctx, g1.ID, u2.ID))
	require.NoError(t, repos.GrantGroup(ctx, g2.ID, u1.ID))
	// u1 在 g1 设 0（免费，合法值必须入 map）；u1 在 g2 设 20000；u2 未设置
	require.NoError(t, repos.SetAssignmentMultiplier(ctx, g1.ID, u1.ID, intPtr(0)))
	require.NoError(t, repos.SetAssignmentMultiplier(ctx, g2.ID, u1.ID, intPtr(20000)))

	mults, err := repos.Groups.LoadAssignmentMultipliers(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, mults[billing.AssignmentKey{UserID: u1.ID, GroupID: g1.ID}], "倍率 0（免费）是已设置值，必须入 map")
	require.Equal(t, 20000, mults[billing.AssignmentKey{UserID: u1.ID, GroupID: g2.ID}])
	_, has := mults[billing.AssignmentKey{UserID: u2.ID, GroupID: g1.ID}]
	require.False(t, has, "未设置专属倍率的授予不入 map（缺失 = 用组倍率）")

	// 撤销 u1 于 g1 → 行删除 → map 消失
	require.NoError(t, repos.RevokeGroup(ctx, g1.ID, u1.ID))
	mults, err = repos.Groups.LoadAssignmentMultipliers(ctx)
	require.NoError(t, err)
	_, has = mults[billing.AssignmentKey{UserID: u1.ID, GroupID: g1.ID}]
	require.False(t, has, "撤销后专属倍率行随授予删除")
	require.Equal(t, 20000, mults[billing.AssignmentKey{UserID: u1.ID, GroupID: g2.ID}], "其他组不受影响")
}

// TestPGLoadBalances LoadBalances（T3.5 修正后仅余额——用户倍率按组挂载，
// 由 LoadAssignmentMultipliers 单独加载）。
func TestPGLoadBalances(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	u1 := seedPGUser(t, repos, "lb1@example.com")
	u2 := seedPGUser(t, repos, "lb2@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u1.ID, 50000))
	require.NoError(t, repos.UpdateUserBalance(ctx, u2.ID, 70000))

	bals, err := repos.LoadBalances(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(50000), bals[u1.ID])
	require.Equal(t, int64(70000), bals[u2.ID])
}

func intPtr(v int) *int { return &v }
