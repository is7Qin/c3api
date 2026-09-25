// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
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

// TestQueryStatsTrend_zoneHorizon 成本上限裁决（Admit step 3，保留期无关纯常量）：
// 非 cube 精确时区（:30 偏移或窗口含 DST 跳变）走原始行 → 跨度 > 8d → 400
// （宁 400 不静默残缺）；cube 精确时区（UTC / Shanghai 恒整点无 DST / NY 无跳变
// 整窗）受 90d 上限约束故合法。本用例零 Retention（coverage 步整体跳过——覆盖率
// 由 TestAdmit_* / stat_admit_test.go 单独钉死）。
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
	require.Equal(t, ny, fs.lastTrendExec.Zone, "时区经 Exec 原样透传至 store")
	require.Equal(t, domain.StatsStorageCube, fs.lastTrendExec.Storage, "整点界 + 恒整点时区 → cube")
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
	require.Equal(t, ist, fs.lastEntityTrendExec.Zone, "钉死身份不得吞掉请求时区")
	require.Equal(t, domain.StatsStorageRaw, fs.lastEntityTrendExec.Storage, ":30 偏移 → 原始行")
}

// TestOverview_zoneThreadingAndHorizon Overview 把请求时区透传 summary/trend
// 两查询；跨 DST 跳变的长窗（days=20，超出分组原始行成本上限 8d）→ ErrInvalidInput；
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
	require.Equal(t, ny, fs.lastSummaryExec.Zone)
	require.Equal(t, ny, fs.lastDaysExec.Zone)
	fs.mu.Unlock()

	_, err = svc.Overview(context.Background(), day, 20, 0, ny)
	require.ErrorIs(t, err, ErrInvalidInput, "跨 DST 跳变 20d 窗超 raw horizon → 400")

	_, err = svc.Overview(context.Background(), day, 30, 0, cst)
	require.NoError(t, err, "恒整点时区 30d 合法（cube 路径）")
}

// TestQueryStatsTrend_windowAlignmentHorizon 窗口界对齐 = 精确性谓词的第一条
// 前提（domain.ZoneCubeVerdict）：**界不齐不再强制降级**——Admit 第 2 条把双界
// 各自向后对齐（每端 <1h）后若能精确重组即用 cube（WindowShifted），这正是
// R4 事故的修复（旧实现"界不齐 ⇒ 100% 落原始行 ⇒ 超闸门 400"）。
func TestQueryStatsTrend_windowAlignmentHorizon(t *testing.T) {
	aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		from     time.Time
		to       time.Time
		wantFrom time.Time // 实际读的窗口下界（对齐后）
		wantTo   time.Time
	}{
		{"UTC 偏 30 分界 30d → 对齐后 cube（旧实现 400）", aug.Add(30 * time.Minute), aug.Add(30*24*time.Hour + 30*time.Minute),
			aug.Add(time.Hour), aug.Add(30*24*time.Hour + time.Hour)},
		{"UTC 偏 30 分界 24h → 对齐后 cube", aug.Add(30 * time.Minute), aug.Add(24*time.Hour + 30*time.Minute),
			aug.Add(time.Hour), aug.Add(24*time.Hour + time.Hour)},
		{"界带 1ms 尾数 30d → 对齐后 cube", aug, aug.Add(30*24*time.Hour + time.Millisecond),
			aug, aug.Add(30*24*time.Hour + time.Hour)},
		{"双界齐 30d → cube 原窗口不变", aug, aug.Add(30 * 24 * time.Hour),
			aug, aug.Add(30 * 24 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := statsTestSvc(fs)
			_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
				From: tc.from, To: tc.to, Granularity: "day", Zone: time.UTC,
			})
			require.NoError(t, err)
			fs.mu.Lock()
			defer fs.mu.Unlock()
			require.Equal(t, domain.StatsStorageCube, fs.lastTrendExec.Storage)
			require.True(t, fs.lastTrendExec.From.Equal(tc.wantFrom), "生效下界 %v（want %v）", fs.lastTrendExec.From, tc.wantFrom)
			require.True(t, fs.lastTrendExec.To.Equal(tc.wantTo), "生效上界 %v（want %v）", fs.lastTrendExec.To, tc.wantTo)
		})
	}
}

// TestQueryStatsTrend_coverageCutoff coverage 步（Admit step 4）：跨度上限只防
// "窗太宽"，不防"窗太老"——生效窗口起点早于 Tables 中表的最保守 floor
// （now−retentionDays 的 UTC 日界截断，与 retention worker DROP 逐位同形）即 400，
// 行已被分区 DROP 时宁 400 不静默缺行。cutoff 由注入的 Retention + 固定 statsNow
// 决定（**不用墙钟**）；tables 全关闭保留期（Days <= 0）⇒ coverage 整体跳过；
// cube 路径按 usage_stats 的 Retention.Stats 独立判定（Stats=0 ⇒ 该路径守卫关闭）。
func TestQueryStatsTrend_coverageCutoff(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata") // :30 偏移 → 恒 raw 路径
	require.NoError(t, err)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) // Retention{Log:7} → cutoff = 2026-08-27T00:00Z
	cutoff := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	raw7 := domain.Retention{Log: 7, ErrLog: 7}
	cases := []struct {
		name      string
		zone      *time.Location
		retention domain.Retention
		from      time.Time
		wantErr   bool
	}{
		{"起点早于 cutoff 的短窗 → 400（跨度上限放不下的洞）", ist, raw7, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), true},
		{"起点 == cutoff → 合法（保守日界，Before 才拒）", ist, raw7, cutoff, false},
		{"起点晚于 cutoff 一天 → 合法", ist, raw7, cutoff.Add(day), false},
		{"未来窗（to 越过 now）不拒", ist, raw7, now.Add(2 * day), false},
		{"保留期禁用（Days <= 0）→ coverage 整体跳过：2020 起点亦合法", ist, domain.Retention{}, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"只关一表（ErrLog=0）仍按剩余表判定：Log=7 生效", ist, domain.Retention{Log: 7}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), true},
		{"cube 精确路径（UTC 整点界）不受 raw 保留期约束", time.UTC, raw7, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(newFakeStore(), nil, &invRecorder{}, nil, nil, nil, nil,
				ServiceDeps{EmailCodeStore: testEmailCodes, Retention: tc.retention})
			svc.statsNow = func() time.Time { return now }
			_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
				From: tc.from, To: tc.from.Add(day), Granularity: "day", Zone: tc.zone,
			})
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidInput)
			swe := assertStatWindowReject(t, err, domain.StatsRejectRawHorizon)
			require.Equal(t, 7, swe.RetentionDays)
			require.True(t, swe.Cutoff.Equal(cutoff), "cutoff %v（want %v）", swe.Cutoff, cutoff)
		})
	}

	// cube 侧同一条 floor 规则（独立 basis：Retention.Stats）——整点对齐 1h 窗
	// 起点早于 now−Stats 日界 → cube_horizon。
	t.Run("cube coverage：Retention.Stats 生效 → cube_horizon", func(t *testing.T) {
		svc := New(newFakeStore(), nil, &invRecorder{}, nil, nil, nil, nil,
			ServiceDeps{EmailCodeStore: testEmailCodes, Retention: domain.Retention{Stats: 7}})
		svc.statsNow = func() time.Time { return now }
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: cutoff.Add(-day), To: cutoff, Granularity: "day", Zone: time.UTC,
		})
		require.ErrorIs(t, err, ErrInvalidInput)
		swe := assertStatWindowReject(t, err, domain.StatsRejectCubeHorizon)
		require.Equal(t, 7, swe.RetentionDays)
	})
}
