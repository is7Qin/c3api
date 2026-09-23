// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// A3（含 A2 的 config 子句）：routing.observation_retention_days 地板。
// 地板 = 正确性回看下界（domain.BaselineLookback 24h）+ 24h 边际 = 2 天，
// 故 0/1 启动失败、2 通过；缺省（无 [routing] 段）= 7。
func TestRoutingObservationRetentionFloor(t *testing.T) {
	setenvRequired(t)
	for _, tc := range []struct {
		days int
		ok   bool
	}{
		{0, false},
		{1, false},
		{2, true},
		{7, true},
	} {
		t.Run(fmt.Sprintf("days=%d", tc.days), func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf("[routing]\nobservation_retention_days = %d\n", tc.days))
			c, err := Load(path)
			if !tc.ok {
				require.Error(t, err, "retention below the correctness floor must fail fast at startup")
				require.ErrorContains(t, err, "routing.observation_retention_days")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.days, c.Routing.ObservationRetentionDays)
		})
	}

	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, 7, c.Routing.ObservationRetentionDays, "缺省观测保留 = 7 天")
}

// A12：config.example.toml 的"智能路由无独立配置段"断言已重写，且 [routing] 段
// 存在（策略参数仍不可配，观测保留深度必须可配——两件事分开写清楚）。
func TestConfigExampleRoutingSection(t *testing.T) {
	setenvRequired(t)
	raw, err := os.ReadFile("../../config.example.toml")
	require.NoError(t, err)
	text := string(raw)
	require.Contains(t, text, "[routing]", "example 必须给出 [routing] 段")
	require.Contains(t, text, "observation_retention_days = 7")
	require.NotContains(t, text, "智能路由（intelligent routing）无独立配置段",
		"旧断言（智能路由无独立配置段）已作废，不得留在 example 里")
	require.Contains(t, text, "观测保留深度是运维/存储参数", "重写后的断言必须说明保留深度为何可配")

	c, err := Load("../../config.example.toml")
	require.NoError(t, err)
	require.Equal(t, 7, c.Routing.ObservationRetentionDays, "example 的 [routing] 段必须真的加载进配置")
}
