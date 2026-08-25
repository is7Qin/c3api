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

// TestUpdateSettingNumberRange A-P2-11 值域护栏：注册表 Min/Max 是单一事实源——
// 低于 Min → 400（含负值直达新注册用户的攻击形态 default_user_balance=-500）；
// Min 恰好命中（边界值）→ 接受；合法正数 → 接受。消费端零改动（护栏前置）。
func TestUpdateSettingNumberRange(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		key   string
		below string // 低于 Min → 400
		atMin string // 恰好命中 Min → 接受
	}{
		{"default_user_max_concurrency", "default_user_max_concurrency", "-1", "0"},
		{"default_user_balance", "default_user_balance", "-500", "0"},
		{"default_user_temp_balance", "default_user_temp_balance", "-1", "0"},
		{"default_user_temp_balance_ttl_days", "default_user_temp_balance_ttl_days", "-1", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
			_, err := svc.UpdateSetting(ctx, tc.key, tc.below)
			require.ErrorIs(t, err, ErrInvalidInput, "低于 Min → 400: %s=%s", tc.key, tc.below)
			_, err = svc.UpdateSetting(ctx, tc.key, tc.atMin)
			require.NoError(t, err, "Min 边界命中 → 接受: %s=%s", tc.key, tc.atMin)
			_, err = svc.UpdateSetting(ctx, tc.key, "12345")
			require.NoError(t, err, "合法正数 → 接受: %s", tc.key)
		})
	}
}

// TestUpdateSettingNumberNoMax 无 Max 条目（注册表未声明上界）：大数不受上界
// 护栏限制（仅下界生效）。
func TestUpdateSettingNumberNoMax(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	_, err := svc.UpdateSetting(ctx, "default_user_max_concurrency", "1000000000000")
	require.NoError(t, err, "Max nil（无上界）→ 不越界")
}

// TestServiceTierPolicyKeysDerived P3-7：serviceTierPolicyKeys 从注册表
// PolicyValues 枚举域派生（消双处同步）——注册表是唯一事实源，派生表与注册表
// 一一对应（无残留手写 key）。
func TestServiceTierPolicyKeysDerived(t *testing.T) {
	want := map[string][]string{
		"service_tier_policy_priority": []string{"passthrough", "strip", "reject"},
		"service_tier_policy_flex":     []string{"passthrough", "strip", "reject"},
		"service_tier_policy_fast":     []string{"passthrough", "strip", "reject"},
	}

	for key, vals := range want {
		got, ok := serviceTierPolicyKeys[key]
		require.True(t, ok, "派生表含 %s", key)
		require.ElementsMatch(t, vals, got, "%s 值域随注册表", key)
	}
	// 派生表与注册表 PolicyValues 条目一一对应
	registered := 0
	for _, d := range domain.DefaultSettings {
		if len(d.PolicyValues) > 0 {
			registered++
			require.Contains(t, serviceTierPolicyKeys, d.Key, "注册表条目 %s 在派生表中", d.Key)
		}
	}
	require.Len(t, serviceTierPolicyKeys, registered, "派生表无注册表之外的 key")
}
