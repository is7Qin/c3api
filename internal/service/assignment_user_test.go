// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestGetGroupAssignments 组维度读取（GET /groups/{id}/assignments）：缺失组 →
// 404；ids 全量 + mults 只含有专属倍率的用户（nil/缺省 = 未设置省略）。
func TestGetGroupAssignments(t *testing.T) {
	svc, fs, _ := newUserGroupService()
	ctx := context.Background()

	u1, err := fs.CreateUser(ctx, &domain.User{Email: "ga1@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	u2, err := fs.CreateUser(ctx, &domain.User{Email: "ga2@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "g", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)

	// 缺失组 → 404
	_, _, err = svc.GetGroupAssignments(ctx, 99999)
	require.ErrorIs(t, err, ErrNotFound)

	// 未授予 → 空列表 + nil mults
	ids, mults, err := svc.GetGroupAssignments(ctx, g.ID)
	require.NoError(t, err)
	require.Empty(t, ids)
	require.Nil(t, mults)

	// u1 有专属倍率、u2 无 → ids 全量、mults 只含 u1
	require.NoError(t, fs.GrantGroup(ctx, g.ID, u1.ID))
	require.NoError(t, fs.GrantGroup(ctx, g.ID, u2.ID))
	require.NoError(t, fs.SetAssignmentMultiplier(ctx, g.ID, u1.ID, intPtr(5000)))
	ids, mults, err = svc.GetGroupAssignments(ctx, g.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{u1.ID, u2.ID}, ids)
	require.Equal(t, map[int64]*int{u1.ID: intPtr(5000)}, mults, "无专属倍率的用户不出现")
}

// TestGetUserGroups 用户维度读取（GET /users/{id}/groups）：缺失用户 → 404；
// 与 TestGetGroupAssignments 对称。
func TestGetUserGroups(t *testing.T) {
	svc, fs, _ := newUserGroupService()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "gu@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g1, err := fs.CreateGroup(ctx, &domain.Group{Name: "g1", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)
	g2, err := fs.CreateGroup(ctx, &domain.Group{Name: "g2", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)

	// 缺失用户 → 404
	_, _, err = svc.GetUserGroups(ctx, 99999)
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, fs.GrantGroup(ctx, g1.ID, u.ID))
	require.NoError(t, fs.GrantGroup(ctx, g2.ID, u.ID))
	require.NoError(t, fs.SetAssignmentMultiplier(ctx, g1.ID, u.ID, intPtr(20000)))
	ids, mults, err := svc.GetUserGroups(ctx, u.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{g1.ID, g2.ID}, ids)
	require.Equal(t, map[int64]*int{g1.ID: intPtr(20000)}, mults, "无专属倍率的组不出现")
}

// TestSetUserGroups 用户维度写（PUT /users/{id}/groups）：
// - 替换语义：未列出组撤销、空数组清空；multipliers 键 ∈ group_ids
// - 倍率：设置/清除（null）；未在 mults 的组沿用当前值；组内其他用户不受影响
// - 与组维度 GetGroupAssignments 交叉验证
// - 表驱动：非法/重复/缺失/越界 → 400/404
func TestSetUserGroups(t *testing.T) {
	svc, fs, _ := newUserGroupService()
	ctx := context.Background()

	u1, err := fs.CreateUser(ctx, &domain.User{Email: "su1@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	u2, err := fs.CreateUser(ctx, &domain.User{Email: "su2@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g1, err := fs.CreateGroup(ctx, &domain.Group{Name: "g1", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)
	g2, err := fs.CreateGroup(ctx, &domain.Group{Name: "g2", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)

	// 授予 g1+g2，g1 专属倍率 1.5（15000）→ 响应回显 + 组维度读取交叉验证
	applied, post, err := svc.SetUserGroups(ctx, u1.ID, []int64{g1.ID, g2.ID}, map[int64]*int{g1.ID: intPtr(15000)})
	require.NoError(t, err)
	require.Equal(t, []int64{g1.ID, g2.ID}, applied)
	require.Equal(t, map[int64]*int{g1.ID: intPtr(15000), g2.ID: nil}, post, "g2 未设倍率 → nil")
	ids, mults, err := svc.GetGroupAssignments(ctx, g1.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{u1.ID}, ids)
	require.Equal(t, map[int64]*int{u1.ID: intPtr(15000)}, mults, "用户维度写入 ↔ 组维度读取一致")

	// 未列出的组撤销：只留 g1（g2 撤销）；mults 不含 g1 → 倍率沿用当前值
	applied, post, err = svc.SetUserGroups(ctx, u1.ID, []int64{g1.ID}, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{g1.ID}, applied)
	require.Equal(t, map[int64]*int{g1.ID: intPtr(15000)}, post, "未在 mults 的组沿用当前倍率")
	rows, err := fs.ListAssignmentsByUser(ctx, u1.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, g1.ID, rows[0].GroupID)

	// null 清除专属倍率 → 回退组倍率
	_, post, err = svc.SetUserGroups(ctx, u1.ID, []int64{g1.ID}, map[int64]*int{g1.ID: nil})
	require.NoError(t, err)
	require.Equal(t, map[int64]*int{g1.ID: nil}, post)
	rows, err = fs.ListAssignmentsByGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.Nil(t, rows[0].PriceMultiplier, "null = 清除为未设置")

	// 空数组 = 清空（撤销全部当前授予组）
	applied, post, err = svc.SetUserGroups(ctx, u1.ID, nil, nil)
	require.NoError(t, err)
	require.Empty(t, applied)
	require.Empty(t, post)
	rows, err = fs.ListAssignmentsByUser(ctx, u1.ID)
	require.NoError(t, err)
	require.Empty(t, rows)

	// 组内其他用户不受影响：u2 在 g1 有专属倍率，u1 的写入不改动它
	require.NoError(t, fs.GrantGroup(ctx, g1.ID, u2.ID))
	require.NoError(t, fs.SetAssignmentMultiplier(ctx, g1.ID, u2.ID, intPtr(9000)))
	_, _, err = svc.SetUserGroups(ctx, u1.ID, []int64{g1.ID}, map[int64]*int{g1.ID: intPtr(20000)})
	require.NoError(t, err)
	rows, err = fs.ListAssignmentsByGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "组内其他用户保留")
	for _, a := range rows {
		if a.UserID == u2.ID {
			require.Equal(t, intPtr(9000), a.PriceMultiplier, "其他用户专属倍率不受影响")
		}
	}

	// 表驱动校验：非法/重复/缺失/越界
	tests := []struct {
		name     string
		groupIDs []int64
		mults    map[int64]*int
		wantErr  error
	}{
		{"重复 group_id", []int64{g1.ID, g1.ID}, nil, ErrInvalidInput},
		{"非法 id 0", []int64{0}, nil, ErrInvalidInput},
		{"组缺失", []int64{99999}, nil, ErrNotFound},
		{"multipliers key 不在 group_ids", []int64{g1.ID}, map[int64]*int{g2.ID: intPtr(5000)}, ErrInvalidInput},
		{"倍率越界", []int64{g1.ID}, map[int64]*int{g1.ID: intPtr(100001)}, ErrInvalidInput},
		{"组数超限", make([]int64, 101), nil, ErrInvalidInput},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.SetUserGroups(ctx, u1.ID, tc.groupIDs, tc.mults)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
	// 用户缺失 → 404
	_, _, err = svc.SetUserGroups(ctx, 99999, []int64{g1.ID}, nil)
	require.ErrorIs(t, err, ErrNotFound)
}
