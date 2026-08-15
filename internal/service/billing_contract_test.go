// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
)

// 本文件覆盖 Phase 5 T4 service 层契约校验：组倍率范围（万分数）、
// service_tier policy 设置值域、用户余额负值。用户倍率按组（T3.5 修正）经
// SetGroupAssignments 校验（见 assignment 测试）。

// intPtr *int 构造（service 包既有 ptr 为 string 专用）。
func intPtr(v int) *int { return &v }

func TestGroupMultiplierValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	// 创建：nil = 未指定 → ×1（service 归一 10000 恒写入）；20000 显式；
	// 显式 0 = 免费组（T3.5 修正：API 边界 nullable 可表达，service 不再把 0
	// 当未指定）
	g, err := svc.CreateGroup(ctx, "g0", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 10000, g.PriceMultiplier, "nil = 未指定 → ×1")
	g, err = svc.CreateGroup(ctx, "g1", domain.GroupVisibilityPublic, intPtr(20000), nil)
	require.NoError(t, err)
	require.Equal(t, 20000, g.PriceMultiplier)
	g, err = svc.CreateGroup(ctx, "g-free", domain.GroupVisibilityPublic, intPtr(0), nil)
	require.NoError(t, err)
	require.Equal(t, 0, g.PriceMultiplier, "显式 0 = 免费组（恒写入）")

	// 创建/更新超界 → 400
	_, err = svc.CreateGroup(ctx, "g2", domain.GroupVisibilityPublic, intPtr(-1), nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateGroup(ctx, "g3", domain.GroupVisibilityPublic, intPtr(100001), nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	g.PriceMultiplier = 100001
	_, err = svc.UpdateGroup(ctx, g)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestServiceTierPolicySettingValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	// 三值均可
	for _, v := range []string{"passthrough", "strip", "reject"} {
		_, err := svc.UpdateSetting(ctx, "service_tier_policy_priority", v)
		require.NoError(t, err, "value=%s", v)
		_, err = svc.UpdateSetting(ctx, "service_tier_policy_flex", v)
		require.NoError(t, err, "value=%s", v)
		_, err = svc.UpdateSetting(ctx, "service_tier_policy_fast", v)
		require.NoError(t, err, "value=%s", v)
	}
	// 非法 → 400
	for _, key := range []string{"service_tier_policy_priority", "service_tier_policy_flex", "service_tier_policy_fast"} {
		_, err := svc.UpdateSetting(ctx, key, "bogus")
		require.ErrorIs(t, err, ErrInvalidInput, "key=%s", key)
	}
	// 未知 key → 400（注册表）
	_, err := svc.UpdateSetting(ctx, "service_tier_policy_bogus", "passthrough")
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestServiceTierPolicy service_tier 转发策略读取：priority/flex/fast 三档分别
// 按各自 key 取策略；缺失/未知值 → passthrough 默认（TierFast 之前走
// default=passthrough，新增显式分支行为兼容）。
func TestServiceTierPolicy(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	// 重置三个 policy key（清空后快照回归默认 passthrough）。
	resetPolicy := func() {
		for _, k := range []string{"service_tier_policy_priority", "service_tier_policy_flex", "service_tier_policy_fast"} {
			delete(fs.settings, k)
		}
		svc.reloadSettings(ctx)
	}
	// 设置：合法值走 UpdateSetting 全路径；bogus 绕过校验直种快照（读路径兜底）。
	setPolicy := func(key, value string) {
		t.Helper()
		if value == "bogus" {
			fs.settings[key] = &domain.Setting{Key: key, Type: domain.SettingTypeString, Value: value}
			svc.reloadSettings(ctx)
			return
		}
		_, err := svc.UpdateSetting(ctx, key, value)
		require.NoError(t, err)
	}

	cases := []struct {
		name  string
		tier  billing.Tier
		key   string // 空 = 缺失（测默认）
		value string
		want  billing.TierPolicyMode
	}{
		{"fast strip", billing.TierFast, "service_tier_policy_fast", "strip", billing.TierPolicyStrip},
		{"fast reject", billing.TierFast, "service_tier_policy_fast", "reject", billing.TierPolicyReject},
		{"fast 缺失 → passthrough", billing.TierFast, "", "", billing.TierPolicyPassthrough},
		{"fast 未知值 → passthrough", billing.TierFast, "service_tier_policy_fast", "bogus", billing.TierPolicyPassthrough},
		// 回归：priority/flex 仍按各自 key
		{"priority strip", billing.TierPriority, "service_tier_policy_priority", "strip", billing.TierPolicyStrip},
		{"flex reject", billing.TierFlex, "service_tier_policy_flex", "reject", billing.TierPolicyReject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetPolicy()
			if tc.key != "" {
				setPolicy(tc.key, tc.value)
			}
			require.Equal(t, tc.want, svc.ServiceTierPolicy(tc.tier))
		})
	}
}
