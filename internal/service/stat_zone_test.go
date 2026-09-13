// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 统计请求时区契约测试（request-browser-timezone-stats 2026-09-03）：
// ResolveTimeZone 边界解析、原始行路径 horizon、时区经 query 结构透传至
// store、UserStats 钉死身份不吞时区。fake 记录 zone（分组正确性由
// repository PG 测试钉死）。

func TestResolveTimeZone(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want *time.Location
	}{
		{"缺省空串回落 UTC（兼容）", "", time.UTC},
		{"UTC 显式", "UTC", time.UTC},
		{"合法 IANA", "Asia/Shanghai", nil},
		{"带 DST 的合法 IANA", "America/New_York", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTimeZone(tc.raw)
			require.NoError(t, err)
			require.NotNil(t, got)
			if tc.want != nil {
				require.Equal(t, tc.want, got)
			}
		})
	}
	for _, bad := range []string{"Mars/Olympus_Mons", "Local", "local", "../etc", "上海", "Asia", "+08:00"} {
		got, err := ResolveTimeZone(bad)
		require.ErrorIs(t, err, ErrInvalidInput, "未知名/特殊名必须 400：%q", bad)
		require.Nil(t, got)
	}
}

// TestQueryStatsTrend_zoneHorizon horizon 裁决：非 cube 精确时区（:30 偏移或
// 窗口含 DST 跳变）窗口 > MaxStatsRawSpan → ErrInvalidInput（宁 400 不静默
// 残缺）；cube 精确时区（UTC / Shanghai 恒整点无 DST / NY 无跳变整窗）合法。
func TestQueryStatsTrend_zoneHorizon(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC) // NY 秋退（11-01 06:00Z）在窗内

	cases := []struct {
		name    string
		zone    *time.Location
		from    time.Time
		to      time.Time
		wantErr bool
	}{
		{"UTC 90d 合法", time.UTC, from, from.Add(90 * 24 * time.Hour), false},
		{"Shanghai 90d 合法（整点无 DST → cube）", cst, from, from.Add(90 * 24 * time.Hour), false},
		{"NY 无跳变夏窗 90d 合法（恒偏移 → cube 精确）", ny, from, from.Add(90 * 24 * time.Hour), false},
		{"NY 跨秋退 8d 合法（raw 含 DST 余量）", ny, oct, oct.Add(8 * 24 * time.Hour), false},
		{"NY 跨秋退 9d → 400", ny, oct, oct.Add(9*24*time.Hour + time.Minute), true},
		{"Kolkata 30d → 400（:30 偏移 → raw horizon）", ist, from, from.Add(30 * 24 * time.Hour), true},
		{"Kolkata 24h 合法", ist, from, from.Add(24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := statsTestSvc(newFakeStore())
			_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
				From: tc.from, To: tc.to, Granularity: "day", Zone: tc.zone,
			})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidInput)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestQueryStatsTrend_zonePassthrough nil zone 归一语义：service 不擅自塞
// UTC（repo 入口 locOrUTC 兜底）——store 收到什么由 query 决定；显式 zone
// 原样透传。
func TestQueryStatsTrend_zonePassthrough(t *testing.T) {
	fs := newFakeStore()
	svc := statsTestSvc(fs)
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	_, err = svc.QueryStatsTrend(context.Background(), TrendQuery{
		From: from, To: from.Add(time.Hour), Granularity: "hour", Zone: ny,
	})
	require.NoError(t, err)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	require.Equal(t, ny, fs.lastTrendZone)
}

// TestUserStats_pinningPreservesZone 用户台钉死（JWT 身份 = 唯一过滤条件）
// 与请求时区共存：caller 伪造 EntityType/EntityID 被覆写为 self，Zone 原样
// 到达 store。
func TestUserStats_pinningPreservesZone(t *testing.T) {
	fs := newFakeStore()
	svc := statsTestSvc(fs)
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	from := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	seedEntities(fs, 3)
	_, err = svc.UserStats(context.Background(), 42, EntityTrendQuery{
		EntityType: "account", EntityID: 999,
		From: from, To: from.Add(time.Hour), Granularity: "hour", Zone: ist,
	})
	require.NoError(t, err)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	require.Equal(t, ist, fs.lastEntityTrendZone, "钉死身份不得吞掉请求时区")
}

// TestOverview_zoneThreadingAndHorizon Overview 把请求时区透传 summary/trend
// 两查询；跨 DST 跳变的窗口 > MaxStatsRawSpan（days=20）→ ErrInvalidInput；
// 含秋退日的 7d 窗（+1h DST ≤ 8d 余量）合法；恒整点时区 30d 走 cube 放行。
func TestOverview_zoneThreadingAndHorizon(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	fs := newFakeStore()
	svc := statsTestSvc(fs)
	day := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC) // NY 秋退日本地零点
	_, err = svc.Overview(context.Background(), day, 7, 0, ny)
	require.NoError(t, err)
	fs.mu.Lock()
	require.Equal(t, ny, fs.lastSummaryZone)
	require.Equal(t, ny, fs.lastDaysZone)
	fs.mu.Unlock()

	_, err = svc.Overview(context.Background(), day, 20, 0, ny)
	require.ErrorIs(t, err, ErrInvalidInput, "跨 DST 跳变 20d 窗超 raw horizon → 400")

	_, err = svc.Overview(context.Background(), day, 30, 0, cst)
	require.NoError(t, err, "恒整点时区 30d 合法（cube 路径）")
}

// TestQueryStatsTrend_windowAlignmentHorizon 窗口界对齐 = 共享精确性谓词的
// 另一半（domain.ZoneCubeExact 前提 1）：界非 UTC 整点时连 UTC 都走 raw——
// service horizon 校验与 repository 路由用同一谓词，绝不放行"repo 会 raw
// 但 service 按 cube 上限校验"的判岐窗口。
func TestQueryStatsTrend_windowAlignmentHorizon(t *testing.T) {
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		from    time.Time
		to      time.Time
		wantErr bool
	}{
		{"UTC 偏 30 分界 30d → 400（raw horizon，非 cube 90d）", aug.Add(30 * time.Minute), aug.Add(30*24*time.Hour + 30*time.Minute), true},
		{"UTC 偏 30 分界 24h → 合法（raw 精确）", aug.Add(30 * time.Minute), aug.Add(24*time.Hour + 30*time.Minute), false},
		{"界带 1ms 尾数 30d → 400", aug, aug.Add(30*24*time.Hour + time.Millisecond), true},
		{"双界齐 30d → 合法（cube 放行）", aug, aug.Add(30 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := statsTestSvc(newFakeStore())
			_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
				From: tc.from, To: tc.to, Granularity: "day", Zone: time.UTC,
			})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidInput)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestSetStatsRawSpan horizon 随配置换算（"绝不比配置更乐观"）：retention 7d
// → 8d 缺省同值；3d → 4d（更短窗 400 提前）；0/负 → 不限（保留禁用无固定
// horizon，跨 DST 长窗放行）。
func TestSetStatsRawSpan(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	oct := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC) // NY 秋退在窗内 → raw
	cases := []struct {
		name      string
		retention int
		span      time.Duration
		wantErr   bool
	}{
		{"retention 7d：8d 内合法", 7, 8 * 24 * time.Hour, false},
		{"retention 7d：8d+1s → 400", 7, 8*24*time.Hour + time.Second, true},
		{"retention 3d：5d → 400（horizon 收紧到 4d，不谎称 8d）", 3, 5 * 24 * time.Hour, true},
		{"retention 3d：4d 内合法", 3, 4 * 24 * time.Hour, false},
		{"retention 0（禁用删除）→ 不限：30d 合法", 0, 30 * 24 * time.Hour, false},
		{"retention 负 → 同 0 不限", -1, 60 * 24 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := statsTestSvc(newFakeStore())
			svc.SetStatsRawSpan(tc.retention)
			// 固定时钟到窗口起点：本测钉的是跨度换算，保留兜底（cutoff =
			// now−days 日界）恰好不构成拒绝——兜底自身由
			// TestQueryStatsTrend_rawRetentionCutoff 单独钉死。
			svc.statsNow = func() time.Time { return oct }
			_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
				From: oct, To: oct.Add(tc.span), Granularity: "day", Zone: ny,
			})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidInput)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestQueryStatsTrend_rawRetentionCutoff 保留兜底：跨度闸门只防"窗太宽"，
// 不防"窗太老"——raw 表起点早于保证存留的 UTC 分区 cutoff（now−retentionDays
// 日界截断，与 retention worker DROP 同语义）的窗口即使 ≤ statsRawSpan，
// 行也可能已被分区 DROP，宁 400 不静默缺行。cutoff 只裁决 from；to 越过
// now 的未来窗不因此拒绝；保留禁用（0）跳过兜底；cube 精确路径不受影响。
func TestQueryStatsTrend_rawRetentionCutoff(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata") // :30 偏移 → 恒 raw 路径
	require.NoError(t, err)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) // retention 7d → cutoff = 2026-08-27T00:00Z
	cutoff := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name      string
		zone      *time.Location
		retention int
		from      time.Time
		wantErr   bool
	}{
		{"起点早于 cutoff 的短窗 → 400（跨度闸门放不下的洞）", ist, 7, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), true},
		{"起点 == cutoff → 合法（保守日界，Before 才拒）", ist, 7, cutoff, false},
		{"起点晚于 cutoff 一天 → 合法", ist, 7, cutoff.Add(day), false},
		{"未来窗（to 越过 now）不拒", ist, 7, now.Add(2 * day), false},
		{"retention 0（禁用）→ 2020 起点亦合法", ist, 0, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"cube 精确路径（UTC 整点界）不受兜底约束", time.UTC, 7, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := statsTestSvc(newFakeStore())
			svc.SetStatsRawSpan(tc.retention)
			svc.statsNow = func() time.Time { return now }
			_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
				From: tc.from, To: tc.from.Add(day), Granularity: "day", Zone: tc.zone,
			})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidInput)
				return
			}
			require.NoError(t, err)
		})
	}
}
