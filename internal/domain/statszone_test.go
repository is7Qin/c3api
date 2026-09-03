// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ZoneCubeExact 路由谓词金标准（request-browser-timezone-stats）：窗口双界恒
// UTC 整点且窗口内零偏移跳变 → cube 重组精确（true）；界劈开小时行、窗口含
// DST 跳变或 :30/:45 偏移 → 必须原始行精确聚合（false）。

func TestZoneCubeExact(t *testing.T) {
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	npt, err := time.LoadLocation("Asia/Kathmandu") // +5:45
	require.NoError(t, err)
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	lon, err := time.LoadLocation("Europe/London")
	require.NoError(t, err)
	perth, err := time.LoadLocation("Australia/Perth") // +8 无 DST
	require.NoError(t, err)

	aug := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	spring := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)  // NY 春进 3/8 07:00Z
	autumn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // NY/London 秋退窗

	cases := []struct {
		name        string
		loc         *time.Location
		from, to    time.Time
		wantExactly bool
	}{
		{"nil = UTC", nil, aug, aug.Add(90 * 24 * time.Hour), true},
		{"UTC", time.UTC, aug, aug.Add(90 * 24 * time.Hour), true},
		{"Shanghai 恒 +8 长窗", cst, aug, aug.Add(90 * 24 * time.Hour), true},
		{"Perth 无 DST 整点", perth, aug, aug.Add(30 * 24 * time.Hour), true},
		{"Kolkata 恒 +5:30 半小时 → raw", ist, aug, aug.Add(time.Hour), false},
		{"Kathmandu +5:45 → raw", npt, aug, aug.Add(time.Hour), false},
		{"NY 夏窗（无跳变、整点恒偏移）→ cube", ny, spring.Add(20 * 24 * time.Hour), spring.Add(50 * 24 * time.Hour), true},
		{"NY 春进窗 → raw", ny, spring, spring.Add(3 * 24 * time.Hour), false},
		{"NY 秋退窗 → raw", ny, autumn, autumn.Add(3 * 24 * time.Hour), false},
		{"London 秋退窗（10-25 跳变在窗内）→ raw", lon, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), false},
		{"跳变落在窗尾边界（to 端采样捕获）→ raw", ny, spring.Add(6 * time.Hour), spring.Add(6*time.Hour + time.Hour), false},
		{"UTC 窗界偏 30 分 → raw（cube 小时行被窗界劈开）", time.UTC, aug.Add(30 * time.Minute), aug.Add(24*time.Hour + 30*time.Minute), false},
		{"UTC to 带 1s → raw", time.UTC, aug, aug.Add(24 * time.Hour).Add(time.Second), false},
		{"UTC from 带 1ms → raw", time.UTC, aug.Add(time.Millisecond), aug.Add(24 * time.Hour), false},
		{"恒偏移区 from 不齐 to 齐 → raw（两界独立把关）", cst, aug.Add(15 * time.Minute), aug.Add(24 * time.Hour), false},
		{"恒偏移区双界齐整点 → cube", cst, aug.Add(time.Hour), aug.Add(25 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantExactly, ZoneCubeExact(tc.loc, tc.from, tc.to))
		})
	}
}
