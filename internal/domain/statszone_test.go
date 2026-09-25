// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ZoneCubeVerdict 金标准：request-browser-timezone-stats 谓词的双前提
// （UTC 整点界 + 窗口内偏移恒整点无跳变）——cube 可精确重组 ⇒ ZoneCubeReusable；
// 界不齐/半小时偏移/DST 跳变 ⇒ 精确的降级原因（不是布尔：原因进 Warn 字段，
// 见 spec §4.3）。三类各至少一例（界/偏移/漂移）。

func TestZoneCubeVerdict(t *testing.T) {
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
	autumn := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // NY/London 秋退

	cases := []struct {
		name     string
		loc      *time.Location
		from, to time.Time
		want     ZoneCubeReason
	}{
		{"nil = UTC", nil, aug, aug.Add(90 * 24 * time.Hour), ZoneCubeReusable},
		{"UTC", time.UTC, aug, aug.Add(90 * 24 * time.Hour), ZoneCubeReusable},
		{"Shanghai · +8 恒整点", cst, aug, aug.Add(90 * 24 * time.Hour), ZoneCubeReusable},
		{"Perth · 无 DST 恒整点", perth, aug, aug.Add(30 * 24 * time.Hour), ZoneCubeReusable},
		{"Kolkata · +5:30 非整小时", ist, aug, aug.Add(time.Hour), ZoneCubeOffset},
		{"Kathmandu +5:45 非整小时", npt, aug, aug.Add(time.Hour), ZoneCubeOffset},
		{"NY 夏季长窗（恒偏移）", ny, spring.Add(20 * 24 * time.Hour), spring.Add(50 * 24 * time.Hour), ZoneCubeReusable},
		{"NY 春进跳变在窗内", ny, spring, spring.Add(3 * 24 * time.Hour), ZoneCubeDST},
		{"NY 秋退跳变在窗内", ny, autumn, autumn.Add(3 * 24 * time.Hour), ZoneCubeDST},
		{"London 秋退周（10-25 跳变在窗内）", lon, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), ZoneCubeDST},
		{"春进日尾边界（to 端即跳变时刻）", ny, spring.Add(6 * time.Hour), spring.Add(7 * time.Hour), ZoneCubeDST},
		{"UTC 双界各偏 30 分（界不齐）", time.UTC, aug.Add(30 * time.Minute), aug.Add(24*time.Hour + 30*time.Minute), ZoneCubeUnaligned},
		{"UTC to 偏 1s", time.UTC, aug, aug.Add(24 * time.Hour).Add(time.Second), ZoneCubeUnaligned},
		{"UTC from 偏 1ms", time.UTC, aug.Add(time.Millisecond), aug.Add(24 * time.Hour), ZoneCubeUnaligned},
		{"左偏右齐（from 偏 15 分）", cst, aug.Add(15 * time.Minute), aug.Add(24 * time.Hour), ZoneCubeUnaligned},
		{"双界齐（cst）", cst, aug.Add(time.Hour), aug.Add(25 * time.Hour), ZoneCubeReusable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ZoneCubeVerdict(tc.loc, tc.from, tc.to))
		})
	}
}
