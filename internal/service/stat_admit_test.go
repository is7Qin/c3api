// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// Admit 的 service 侧接线用例（spec §8）：A5（Warn 三类断言）、A6（Warn 不重复 +
// 精确路径零 Warn）、A13（cube coverage 两条恒定路径 + 缓存之前）、A13b（cost 与
// 保留期解耦）、A14（四条 zone-free 原始行路径 coverage + cost 独立）、A20（节流）。
//
// **全部用例自建带 logger 与 Retention 的 Service**：statsTestSvc 无 logger、无
// Retention，复用即假绿（spec §9.8）。时间一律注入固定 statsNow，零墙钟依赖。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/stretchr/testify/require"
)

// statsAdmitNow 固定判定时钟（coverage 的 cutoff 由它 + Retention 决定）。
var statsAdmitNow = time.Date(2026, 9, 25, 15, 38, 21, 0, time.UTC)

// statsLogSink 捕获 Warn（logx 唯一日志面 + 真实文件编码——与既有
// mailer_worker_privacy_test 同款：不引入测试专用日志后门）。
type statsLogSink struct {
	log  *logx.Logger
	path string
}

// out 读回全部日志文本（文件不存在 = 零日志）。
func (s statsLogSink) out(t *testing.T) string {
	t.Helper()
	require.NoError(t, s.log.Sync())
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

// count 出现次数（Warn 计数断言：每条 Warn 一行 JSON）。
func (s statsLogSink) count(t *testing.T, msg string) int {
	t.Helper()
	return strings.Count(s.out(t), msg)
}

// newStatsAdmitSvc 带 logger + Retention 的 Service（判定时钟固定注入）。
func newStatsAdmitSvc(t *testing.T, fs *fakeStore, ret domain.Retention, now time.Time) (*Service, statsLogSink) {
	t.Helper()
	dir, err := os.MkdirTemp("", "stats-admit-log-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "stats.json")
	logger, err := logx.New("warn", path)
	require.NoError(t, err)
	svc := New(fs, nil, &invRecorder{}, nil, nil, nil, logger,
		ServiceDeps{EmailCodeStore: testEmailCodes, Retention: ret})
	svc.statsNow = func() time.Time { return now }
	return svc, statsLogSink{log: logger, path: path}
}

// —— A5：Warn 存在、字段集正确、拒绝路径零 Warn ——

// TestAdmitWarn_fallbackFields A5① / A5②：无 logger 不得 panic（statsTestSvc
// 构造的 Service 无 logger——全量既有用例即覆盖）；有 logger 时 raw 路径恰好
// 一条降级 Warn，字段集取 §4.3（reason/kind/timezone/from/to/span_seconds）。
func TestAdmitWarn_fallbackFields(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	t.Run("无 logger 的 Service 不得 panic（s.log 判空）", func(t *testing.T) {
		svc := statsTestSvc(newFakeStore())
		require.Nil(t, svc.log)
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: to, Granularity: "day", Zone: ist})
		require.NoError(t, err)
	})

	t.Run("raw 降级恰好一条 Warn 且字段集齐", func(t *testing.T) {
		svc, sink := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{}, statsAdmitNow)
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: to, Granularity: "day", Zone: ist})
		require.NoError(t, err)
		out := sink.out(t)
		require.Equal(t, 1, strings.Count(out, "stats: falling back to raw rows"))
		require.Contains(t, out, `"reason":"offset"`, ":30 偏移 → reason=offset")
		require.Contains(t, out, `"kind":"trend"`)
		require.Contains(t, out, `"timezone":"Asia/Kolkata"`)
		require.Contains(t, out, `"from":"2026-09-25T00:00:00Z"`)
		require.Contains(t, out, `"to":"2026-09-26T00:00:00Z"`)
		require.Contains(t, out, `"span_seconds":86400`)
	})

	t.Run("窗口位移恰好一条 Warn 且字段集齐", func(t *testing.T) {
		svc, sink := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{}, statsAdmitNow)
		// 整点时区 + 界不齐（跨度 2h）→ 第 2 条判定命中：cube/WindowShifted。
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from.Add(30 * time.Minute), To: to.Add(30 * time.Minute), Granularity: "day", Zone: time.UTC})
		require.NoError(t, err)
		out := sink.out(t)
		require.Equal(t, 1, strings.Count(out, "stats: window shifted to hour boundary"))
		require.Contains(t, out, `"kind":"trend"`)
		require.Contains(t, out, `"timezone":"UTC"`)
		require.Contains(t, out, `"requested_from":"2026-09-25T00:30:00Z"`)
		require.Contains(t, out, `"effective_from":"2026-09-25T01:00:00Z"`)
		require.Contains(t, out, `"shift_seconds":1800`)
		require.Zero(t, strings.Count(out, "stats: falling back to raw rows"), "位移不产生降级 Warn")
	})
}

// TestAdmitWarn_rejectPathSilent A5④：被 Admit 拒绝时**零 Warn**（可观测性由 400
// 机读字段承载；拒绝路径不产生日志噪声）。
func TestAdmitWarn_rejectPathSilent(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

	t.Run("cost 拒绝", func(t *testing.T) {
		svc, sink := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{Log: 7, ErrLog: 7}, statsAdmitNow)
		// ttft_exact 成本上限 168h：8d 必拒（保留期无关）。
		_, err := svc.QueryStatsTTFT(context.Background(), TTFTQuery{
			From: from, To: from.Add(8 * 24 * time.Hour), EntityType: "account", EntityID: 7})
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectWindowTooLong)
		require.Zero(t, sink.count(t, "stats:"), "拒绝路径零日志（body=%q）", sink.out(t))
	})

	t.Run("coverage 拒绝", func(t *testing.T) {
		ist, err := time.LoadLocation("Asia/Kolkata") // :30 偏移 → 原始行路径
		require.NoError(t, err)
		svc, sink := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{Log: 7, ErrLog: 7}, statsAdmitNow)
		// cutoff = 2026-09-18T00:00Z（now−7d 日界）；起点早一小时 → raw_horizon。
		cutoff := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
		_, err = svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: cutoff.Add(-time.Hour), To: cutoff, Granularity: "day", Zone: ist})
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectRawHorizon)
		require.Zero(t, sink.count(t, "stats:"), "拒绝路径零日志（body=%q）", sink.out(t))
	})
}

// TestAdmitWarn_notPerSQL A6：同一请求只记一条（不是每条 SQL 一条——raw 分组
// 实际下发 usage_logs + err_logs 两条 SQL）；cube 精确路径零 Warn。
func TestAdmitWarn_notPerSQL(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

	t.Run("raw 双源 SQL 只有一条 Warn", func(t *testing.T) {
		svc, sink := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{}, statsAdmitNow)
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: from.Add(24 * time.Hour), Granularity: "day", Zone: ist})
		require.NoError(t, err)
		require.Equal(t, 1, sink.count(t, "stats: falling back to raw rows"))
	})

	t.Run("cube 精确路径零 Warn", func(t *testing.T) {
		svc, sink := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{}, statsAdmitNow)
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: from.Add(24 * time.Hour), Granularity: "day", Zone: time.UTC})
		require.NoError(t, err)
		require.Zero(t, sink.count(t, "stats:"))
	})
}

// —— A13：cube coverage（两条 storage 恒定的 cube 读路径各断言一次 + 缓存之前） ——

// TestAdmitCubeCoverage A13：Retention{Stats: 30} 下整点对齐 90d 窗口 → 400
// cube_horizon（/stats/trend 与 storage 恒定的 /stats/top 各一次）；Stats=180
// → 200；且 TTFT 的判定必须先于 statsTTFTC（同键先 200 后注入收紧 → 400，
// 证明未被旧缓存短路）。
func TestAdmitCubeCoverage(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(domain.MaxCubeSpan) // 整点对齐 90d
	// now − 30d 日界 = 2026-08-26T00:00Z > from ⇒ cube_horizon。
	tight := domain.Retention{Stats: 30}
	loose := domain.Retention{Stats: 180}

	t.Run("trend 90d cube 窗口 → 400 cube_horizon", func(t *testing.T) {
		svc, _ := newStatsAdmitSvc(t, newFakeStore(), tight, statsAdmitNow)
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: to, Granularity: "day", Zone: time.UTC})
		require.ErrorIs(t, err, ErrInvalidInput)
		swe := assertStatWindowReject(t, err, domain.StatsRejectCubeHorizon)
		require.Equal(t, 30, swe.RetentionDays)
		require.Equal(t, domain.StatsStorageCube, swe.Storage)
		require.True(t, swe.Cutoff.Equal(time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)), "cutoff %v", swe.Cutoff)
	})

	t.Run("top 90d cube 窗口 → 400 cube_horizon（storage 恒定路径最易漏）", func(t *testing.T) {
		svc, _ := newStatsAdmitSvc(t, newFakeStore(), tight, statsAdmitNow)
		_, err := svc.QueryStatsTop(context.Background(), TopQuery{
			From: from, To: to, EntityType: "account", By: "cost"})
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectCubeHorizon)
	})

	t.Run("Stats=180 → 两者皆 200", func(t *testing.T) {
		svc, _ := newStatsAdmitSvc(t, newFakeStore(), loose, statsAdmitNow)
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: to, Granularity: "day", Zone: time.UTC})
		require.NoError(t, err)
		_, err = svc.QueryStatsTop(context.Background(), TopQuery{
			From: from, To: to, EntityType: "account", By: "cost"})
		require.NoError(t, err)
	})

	t.Run("TTFT 判定先于 statsTTFTC（改注入后同键不得命中旧缓存）", func(t *testing.T) {
		fs := newFakeStore()
		seedTTFT(fs)
		svc, _ := newStatsAdmitSvc(t, fs, loose, statsAdmitNow)
		q := TTFTQuery{From: from, To: to} // sketch 分支（无实体）= cube 读路径
		sum, err := svc.QueryStatsTTFT(context.Background(), q)
		require.NoError(t, err)
		require.Equal(t, int64(5), sum.Count, "首次 200 并写入 statsTTFTC")

		svc.retention = tight // 同键再请求：覆盖率已收紧 → 必须 400，不得回旧缓存值
		_, err = svc.QueryStatsTTFT(context.Background(), q)
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectCubeHorizon)
	})
}

// TestAdmitCostIndependentOfRetention A13b：cost cap 是**保留期无关的纯常量**——
// /accounts/usage 在 Retention{Log: 0}（coverage 整体关闭）与 {Log: 30} 两种注入
// 下结局相同：91d → 400 window_too_long、90d → 200。
func TestAdmitCostIndependentOfRetention(t *testing.T) {
	// 起点取 Retention{Log:30} 的 cutoff（2026-08-26T00:00Z）——两种注入下
	// coverage 步都放行，故结局差异只可能来自 cost。
	from := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	for _, ret := range []domain.Retention{{Log: 0}, {Log: 30}} {
		t.Run("Retention{Log:"+itoa(ret.Log)+"}", func(t *testing.T) {
			svc, _ := newStatsAdmitSvc(t, newFakeStore(), ret, statsAdmitNow)
			_, err := svc.AccountsGatewayUsage(context.Background(), []int64{1}, from, from.Add(91*24*time.Hour))
			require.ErrorIs(t, err, ErrInvalidInput, "91d 超 usage_agg 的 90d 常量上限")
			swe := assertStatWindowReject(t, err, domain.StatsRejectWindowTooLong)
			require.Equal(t, int64((90 * 24 * time.Hour / time.Second)), swe.LimitSeconds)
			require.Zero(t, swe.RetentionDays, "cost 拒绝与保留期无关")

			_, err = svc.AccountsGatewayUsage(context.Background(), []int64{1}, from, from.Add(90*24*time.Hour))
			require.NoError(t, err, "90d 恰在上限内")
		})
	}
}

// —— A14：四条 zone-free 原始行读路径的 coverage + cost 独立性 ——

// TestAdmitZoneFreeCoverage A14：/stats/ttft(exact)、/accounts/usage、/usage_logs、
// /err_logs（后两者含用户面同源变体——同 service 方法）各自 cutoff−1h → 400
// raw_horizon、cutoff → 200；对 cutoff+1h 起点 + 6d 跨度 → 200（四者无 cube
// 备选、6d 落在各自 cost cap 之内）；ttft_exact 8d → 400 window_too_long（cost
// 独立于保留期）。basis 各不相同：err_logs 取 Retention.ErrLog，其余取 Log。
func TestAdmitZoneFreeCoverage(t *testing.T) {
	// now = 2026-09-25T15:38:21Z；Retention{Log:7, ErrLog:3} →
	// usage_logs cutoff = 09-18T00:00Z、err_logs cutoff = 09-22T00:00Z。
	logCutoff := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	errCutoff := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	ret := domain.Retention{Log: 7, ErrLog: 3, Stats: 180}

	cases := []struct {
		name   string
		cutoff time.Time
		call   func(t *testing.T, svc *Service, from, to time.Time) error
	}{
		{"ttft_exact（/stats/ttft 与 /api/user/stats/ttft）", logCutoff,
			func(t *testing.T, svc *Service, from, to time.Time) error {
				_, err := svc.QueryStatsTTFT(context.Background(), TTFTQuery{From: from, To: to, EntityType: "user", EntityID: 7})
				return err
			}},
		{"usage_agg（/accounts/usage）", logCutoff,
			func(t *testing.T, svc *Service, from, to time.Time) error {
				_, err := svc.AccountsGatewayUsage(context.Background(), []int64{1}, from, to)
				return err
			}},
		{"usage_list（/usage_logs 与 /api/user/usage_logs）", logCutoff,
			func(t *testing.T, svc *Service, from, to time.Time) error {
				f, l := from, to
				_, err := svc.QueryUsages(context.Background(), repository.UsageQuery{From: &f, To: &l})
				return err
			}},
		{"errlog_list（/err_logs 与 /api/user/err_logs）", errCutoff,
			func(t *testing.T, svc *Service, from, to time.Time) error {
				f, l := from, to
				_, err := svc.QueryErrLogs(context.Background(), repository.ErrLogQuery{From: &f, To: &l})
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newStatsAdmitSvc(t, newFakeStore(), ret, statsAdmitNow)

			err := tc.call(t, svc, tc.cutoff.Add(-time.Hour), tc.cutoff)
			require.ErrorIs(t, err, ErrInvalidInput, "起点早于 cutoff 1h（跨度仅 1h，cost 不可能参与）")
			swe := assertStatWindowReject(t, err, domain.StatsRejectRawHorizon)
			require.True(t, swe.Cutoff.Equal(tc.cutoff), "cutoff %v（want %v）", swe.Cutoff, tc.cutoff)

			require.NoError(t, tc.call(t, svc, tc.cutoff, tc.cutoff.Add(time.Hour)), "起点 == cutoff 恰合法")
			require.NoError(t, tc.call(t, svc, tc.cutoff.Add(time.Hour), tc.cutoff.Add(time.Hour+6*24*time.Hour)),
				"无 cube 备选 + 6d 跨度（落在各自 cost cap 内）合法")
		})
	}

	t.Run("ttft_exact 8d → 400 window_too_long（cost 与保留期无关）", func(t *testing.T) {
		svc, _ := newStatsAdmitSvc(t, newFakeStore(), ret, statsAdmitNow)
		_, err := svc.QueryStatsTTFT(context.Background(), TTFTQuery{
			From: logCutoff, To: logCutoff.Add(8 * 24 * time.Hour), EntityType: "user", EntityID: 7})
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectWindowTooLong)
	})
}

// —— A20：Warn 节流 ——

// TestAdmitWarnThrottle A20：同 (kind, zone 名, reason) 每分钟至多一条——
// 连续 5 次请求只 1 条；statsNow 推进 61s 后第 2 条；不同 zone 名各自独立。
func TestAdmitWarnThrottle(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	npt, err := time.LoadLocation("Asia/Kathmandu") // +5:45（同类 reason=offset，不同 zone 名）
	require.NoError(t, err)
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)

	now := statsAdmitNow
	svc, sink := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{}, now)
	svc.statsNow = func() time.Time { return now } // 可推进的注入时钟（零墙钟）
	call := func(zone *time.Location) {
		t.Helper()
		_, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: from.Add(24 * time.Hour), Granularity: "day", Zone: zone})
		require.NoError(t, err)
	}

	for i := 0; i < 5; i++ {
		call(ist)
	}
	require.Equal(t, 1, sink.count(t, "stats: falling back to raw rows"), "同键 5 次只 1 条")

	call(npt) // 不同 zone 名 → 独立计数（不互相抑制）
	require.Equal(t, 2, sink.count(t, "stats: falling back to raw rows"))

	now = now.Add(61 * time.Second) // 推进过一节流窗口
	call(ist)
	require.Equal(t, 3, sink.count(t, "stats: falling back to raw rows"), "61s 后可再发一条")
}

// TestAdmitLogsWindow_nilWindowRejected 钉死明细分页面的"无窗口"不再是一条
// 静默路径（评审 F1）：from/to 契约必填，nil 只可能来自内部/测试调用；
// 它必须走 domain.Admit 的 step 1（零值 → window_invalid），而不是跳过判定。
func TestAdmitLogsWindow_nilWindowRejected(t *testing.T) {
	svc, _ := newStatsAdmitSvc(t, newFakeStore(), domain.Retention{Log: 7, ErrLog: 3, Stats: 180}, statsAdmitNow)

	from := time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)

	t.Run("QueryUsages 两端 nil", func(t *testing.T) {
		_, err := svc.QueryUsages(context.Background(), repository.UsageQuery{})
		require.ErrorIs(t, err, ErrInvalidInput, "nil 窗口必须被拒，不得静默放行")
		assertStatWindowReject(t, err, domain.StatsRejectWindowInvalid)
	})
	t.Run("QueryErrLogs 两端 nil", func(t *testing.T) {
		_, err := svc.QueryErrLogs(context.Background(), repository.ErrLogQuery{})
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectWindowInvalid)
	})
	t.Run("QueryUsages 仅 to 为 nil（半窗口也是无效窗口）", func(t *testing.T) {
		_, err := svc.QueryUsages(context.Background(), repository.UsageQuery{From: &from})
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectWindowInvalid)
	})
	t.Run("QueryUsages 仅 from 为 nil", func(t *testing.T) {
		_, err := svc.QueryUsages(context.Background(), repository.UsageQuery{To: &from})
		require.ErrorIs(t, err, ErrInvalidInput)
		assertStatWindowReject(t, err, domain.StatsRejectWindowInvalid)
	})
}

// itoa 局部小工具（避免引入 strconv 仅为用例名）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
