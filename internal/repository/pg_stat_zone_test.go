// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package repository_test

// 浏览器请求时区统计读真实 PG 测试套件（request-browser-timezone-stats 2026-09-03）：
// 时区经读取方法参数注入（无装配级 setter——并发不同区天然互不污染）。
//   - Kolkata :30：半小时偏移时区的 hour/day 分组走原始行精确聚合——本地小时
//     界 = UTC :30 分界（卷积表恒 UTC 整点界，重组必错）；同数据切 UTC 回到
//     整点桶（对照证明绑定参数真驱动分组）；
//   - Shanghai 整点：恒整点无 DST 时区由 cube 重组，日界 = UTC 16:00；cube 路
//     与原始行暴力参照逐桶等值（语义保持的交叉验证）；
//   - NY DST：fall-back 墙钟重复小时（05:30Z EDT 与 06:30Z EST 都显示 01:30）
//     产出两个不同绝对起点桶（不塌缩合并——选定精确行为，契约以 offset 区分）；
//     spring-forward 缺失墙钟小时无桶；
//   - 会话 TimeZone 中毒：NY 会话池上绑定 $zone 支配分组（cube 与 raw 两路径
//     同断言）；
//   - 原始行语义与 cube 写侧逐位一致：usage none/abort 全测量 + err_logs
//     非 abort 计数补充（abort 行排除防双计、豁免 none 行计 rc）；
//   - SummarizeStats 绝对区间：数值与时区无关（UTC 界窗内 raw 路径与 cube
//     路径等值）。
//
// 基座同 pg_stat_test.go（共享 PID schema；本包 PG 测试串行——无 t.Parallel）。

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// —— 断言辅助 ——

// bucketClocks 桶时刻（UTC 分/时组合键）→ request_count 摘要。
func bucketClocks(bs []*domain.StatBucket) map[string]int64 {
	m := map[string]int64{}
	for _, b := range bs {
		m[b.BucketTime.UTC().Format("15:04")] += b.RequestCount
	}
	return m
}

// seedRawRows 造数：usage_logs + err_logs 行落盘（分区预建经 seedAggWindow）。
func seedRawRows(t *testing.T, repos *repository.Repository, usages, errs []*domain.UsageLog) {
	t.Helper()
	ctx := context.Background()
	if len(usages) > 0 {
		require.NoError(t, repos.Usages.InsertBatch(ctx, usages))
	}
	if len(errs) > 0 {
		require.NoError(t, repos.ErrLogs.InsertBatch(ctx, errs))
	}
}

// TestPGRawKolkataHourAndDay 半小时偏移时区原始行精确分组：Asia/Kolkata
// （+5:30，本地小时界 = UTC :30）三 usage 行 09:15Z/09:45Z/10:15Z + 一 err 5xx
// 行 10:45Z（IST 16:15）——hour 分组桶界 {08:30:1, 09:30:2, 10:30:1}（err 补充
// 落 10:30 桶），day 分组单桶 IST Aug14（绝对起点 Aug13 18:30Z）rc=4/ec=1；
// 同数据 zone=UTC → 整点界 {09:00:2, 10:00:2}（对照证明请求时区真驱动分组）。
func TestPGRawKolkataHourAndDay(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	base := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, base, base.Add(8*time.Hour))
	seedRawRows(t, repos,
		[]*domain.UsageLog{
			usageLogRow("tz-u1", time.Date(2026, 8, 14, 9, 15, 0, 0, time.UTC), domain.ErrNone, 1, 5, 42, 9, "gpt-4o", 1, 1, 0, 0, 10, 10, 0, nil),
			usageLogRow("tz-u2", time.Date(2026, 8, 14, 9, 45, 0, 0, time.UTC), domain.ErrNone, 1, 5, 42, 9, "gpt-4o", 1, 1, 0, 0, 10, 10, 0, nil),
			usageLogRow("tz-u3", time.Date(2026, 8, 14, 10, 15, 0, 0, time.UTC), domain.ErrNone, 1, 5, 42, 9, "gpt-4o", 1, 1, 0, 0, 10, 10, 0, nil),
		},
		[]*domain.UsageLog{
			errLogRow("tz-e1", time.Date(2026, 8, 14, 10, 45, 0, 0, time.UTC), domain.Err5xx, 1, 5, 42, 9, "gpt-4o"),
		})

	hourly, err := repos.Stats.StatsTrend(ctx, base, base.Add(4*time.Hour), "hour", 0, "", ist)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"08:30": 1, "09:30": 2, "10:30": 1}, bucketClocks(hourly),
		"hour 桶界 = IST 本地整点（:30 分界劈开 UTC 小时行——raw 精确形态）")
	for _, b := range hourly {
		require.Equal(t, 0, b.BucketTime.In(ist).Minute(), "本地墙钟分 = 00")
	}
	require.Equal(t, int64(1), hourly[len(hourly)-1].ErrorCount, "err_logs 补充行入其本地小时桶")

	daily, err := repos.Stats.StatsTrend(ctx, base.Add(-time.Hour), base.Add(8*time.Hour), "day", 0, "", ist)
	require.NoError(t, err)
	require.Len(t, daily, 1, "IST Aug14 全天单桶")
	require.True(t, daily[0].BucketTime.Equal(time.Date(2026, 8, 13, 18, 30, 0, 0, time.UTC)),
		"IST 日界绝对起点 = Aug13 18:30Z（%v）", daily[0].BucketTime)
	require.Equal(t, "2026-08-14", daily[0].BucketTime.Format("2006-01-02"), "墙钟分量 = 请求时区日期")
	require.Equal(t, int64(4), daily[0].RequestCount)
	require.Equal(t, int64(1), daily[0].ErrorCount)

	// 对照：同数据入 cube 后 zone=UTC → 整点界（证明 IST :30 界是请求时区驱动
	// 的 raw 精确形态，非恒 UTC 假象）。
	cube, entity, _, err := repos.Stats.LoadAggRange(ctx, base, base.Add(4*time.Hour))
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, base, base.Add(4*time.Hour), base.Add(4*time.Hour), cube, entity))
	hourlyUTC, err := repos.Stats.StatsTrend(ctx, base, base.Add(4*time.Hour), "hour", 0, "", time.UTC)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"09:00": 2, "10:00": 2}, hourKey(hourlyUTC),
		"UTC 下同数据落整点桶（对照 IST :30 界）")
}

// overviewCubeBucket 日分组测试用 cube 桶（model 固定，group=7）。
func overviewCubeBucket(bt time.Time, req int64) *domain.StatBucket {
	return &domain.StatBucket{BucketTime: bt, GroupID: 7, Model: "m", RequestCount: req, TTFTHist: make([]int64, 10)}
}

// TestPGRawNYDSTFallBackNoCollapse DST fall-back 精确行为（选定语义，契约
// 文档化）：America/New_York 2026-11-01 02:00 EDT→01:00 EST——墙钟 01:30 出现
// 两次（05:30Z EDT / 06:30Z EST）。hour 分组产出两个不同绝对起点桶（05:00Z
// 与 06:00Z），计数不塌缩合并；日分组（本地日界不受重复小时影响）单桶聚合
// 两小时。spring-forward 窗（03-08 02:00→03:00）：墙钟 02:xx 无行、01:xx 与
// 03:xx 各自成桶（缺失小时自然无桶）。
func TestPGRawNYDSTFallBackNoCollapse(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	base := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, base, base.Add(8*time.Hour))
	seedRawRows(t, repos, []*domain.UsageLog{
		usageLogRow("fb-1", time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), domain.ErrNone, 0, 0, 42, 0, "m", 1, 1, 0, 0, 1, 1, 0, nil), // 01:30 EDT
		usageLogRow("fb-2", time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), domain.ErrNone, 0, 0, 42, 0, "m", 1, 1, 0, 0, 1, 1, 0, nil), // 01:30 EST（重复墙钟）
	}, nil)
	hourly, err := repos.Stats.StatsTrend(ctx, base, base.Add(4*time.Hour), "hour", 0, "", ny)
	require.NoError(t, err)
	require.Len(t, hourly, 2, "重复墙钟小时 = 两个独立绝对桶（不静默塌缩）")
	require.True(t, hourly[0].BucketTime.Equal(time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)))
	require.True(t, hourly[1].BucketTime.Equal(time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)))
	o1, _ := hourly[0].BucketTime.In(ny).Zone()
	o2, _ := hourly[1].BucketTime.In(ny).Zone()
	require.NotEqual(t, o1, o2, "两桶 offset 不同（EDT/EST）——RFC3339 offset 分量可表达重复墙钟标签")
	require.Equal(t, hourly[0].BucketTime.In(ny).Format("15:04"), hourly[1].BucketTime.In(ny).Format("15:04"), "墙钟标签相同")

	daily, err := repos.Stats.StatsTrend(ctx, base, base.Add(4*time.Hour), "day", 0, "", ny)
	require.NoError(t, err)
	require.Len(t, daily, 1, "fall-back 日本地日界唯一")
	require.True(t, daily[0].BucketTime.Equal(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)), "Nov1 本地零点 EDT 04:00Z")
	require.Equal(t, int64(2), daily[0].RequestCount, "重复小时两行都计入本地日（25h 日本地计数完整）")

	// spring-forward：06:30Z（01:30 EST）与 07:30Z（03:30 EDT）；墙钟 02:xx 缺失。
	sb := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, sb, sb.Add(8*time.Hour))
	seedRawRows(t, repos, []*domain.UsageLog{
		usageLogRow("sf-1", time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC), domain.ErrNone, 0, 0, 42, 0, "m", 1, 1, 0, 0, 1, 1, 0, nil),
		usageLogRow("sf-2", time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC), domain.ErrNone, 0, 0, 42, 0, "m", 1, 1, 0, 0, 1, 1, 0, nil),
	}, nil)
	sh, err := repos.Stats.StatsTrend(ctx, sb, sb.Add(4*time.Hour), "hour", 0, "", ny)
	require.NoError(t, err)
	require.Len(t, sh, 2)
	require.Equal(t, "01:00", sh[0].BucketTime.In(ny).Format("15:04"))
	require.Equal(t, "03:00", sh[1].BucketTime.In(ny).Format("15:04"), "02:00 本地小时不存在——无桶（缺失自然表达）")
}

// TestPGShanghaiCubeAndEntityDayGrouping 恒整点无 DST（Asia/Shanghai）cube
// 重组路径：日界 = UTC 16:00；cube 桶 / 实体 cube 桶按请求时区分组 + 返回
// 桶墙钟分量 = 请求时区日期；zone=nil（UTC）对照单桶。
func TestPGShanghaiCubeAndEntityDayGrouping(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	dayStartUTC := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, dayStartUTC, dayStartUTC.Add(48*time.Hour))
	b1 := overviewCubeBucket(dayStartUTC.Add(15*time.Hour), 3)
	b2 := overviewCubeBucket(dayStartUTC.Add(16*time.Hour), 5)
	require.NoError(t, repos.Stats.AggregateRange(ctx, dayStartUTC, dayStartUTC.Add(48*time.Hour),
		dayStartUTC.Add(17*time.Hour), []*domain.StatBucket{b1, b2},
		[]*domain.EntityStatBucket{
			{BucketTime: dayStartUTC.Add(15 * time.Hour), EntityType: "user", EntityID: 42, Model: "m", RequestCount: 3},
			{BucketTime: dayStartUTC.Add(16 * time.Hour), EntityType: "user", EntityID: 42, Model: "m", RequestCount: 5},
		}))
	from, to := dayStartUTC.Add(-24*time.Hour), dayStartUTC.Add(48*time.Hour)

	// UTC 缺省（cube 现状路径）。
	tr, err := repos.Stats.ScanStatsDays(ctx, from, to, 0, nil)
	require.NoError(t, err)
	require.Len(t, tr, 1, "UTC 日界：15:00Z 与 16:00Z 同属 Aug 14")
	require.Equal(t, int64(8), tr[0].Requests)

	// Shanghai 请求：分两桶（绝对起点 = 本地日零点，墙钟 = 请求时区日期）。
	tr, err = repos.Stats.ScanStatsDays(ctx, from, to, 0, cst)
	require.NoError(t, err)
	require.Len(t, tr, 2, "请求时区日界分桶（Asia/Shanghai）")
	require.True(t, tr[0].Date.Equal(dayStartUTC.Add(-8*time.Hour)), "桶 1 = 本地 Aug14 零点（UTC Aug13 16:00）（%v）", tr[0].Date)
	require.Equal(t, "2026-08-14", tr[0].Date.Format("2006-01-02"))
	require.Equal(t, int64(3), tr[0].Requests)
	require.True(t, tr[1].Date.Equal(dayStartUTC.Add(16*time.Hour)), "桶 2 = 本地 Aug15 零点（%v）", tr[1].Date)
	require.Equal(t, "2026-08-15", tr[1].Date.Format("2006-01-02"))
	require.Equal(t, int64(5), tr[1].Requests)

	tb, err := repos.Stats.StatsTrend(ctx, from, to, "day", 0, "", cst)
	require.NoError(t, err)
	require.Len(t, tb, 2)
	require.Equal(t, "2026-08-14", tb[0].BucketTime.Format("2006-01-02"))
	require.Equal(t, int64(3), tb[0].RequestCount)
	require.Equal(t, int64(5), tb[1].RequestCount)

	eb, err := repos.Stats.StatsEntityTrend(ctx, from, to, "day", "user", 42, "", cst)
	require.NoError(t, err)
	require.Len(t, eb, 2)
	require.True(t, eb[0].BucketTime.Equal(dayStartUTC.Add(-8*time.Hour)))
	require.Equal(t, "2026-08-14", eb[0].BucketTime.Format("2006-01-02"))
	require.Equal(t, int64(3), eb[0].RequestCount)
	require.Equal(t, int64(5), eb[1].RequestCount)
}

// TestPGRawMatchesBruteForce 原始行路径 vs Go 侧独立暴力参照（语义保持金标
// 准）：混造 usage（none/abort/5xx 排除）+ err（非 abort 补充、含豁免 none 行、
// abort 行排除）跨两个 Kolkata 本地日——StatsTrend/ScanStatsDays/StatsEntityTrend/
// SummarizeStats 的桶归属、rc/ec/tokens/cost/call/TTFT 全与逐行手工分组期望等值。
func TestPGRawMatchesBruteForce(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, base, base.Add(12*time.Hour))
	ttft150 := int64(150)
	usages := []*domain.UsageLog{
		usageLogRow("bf-u1", base, domain.ErrNone, 7, 5, 42, 9, "m", 10, 20, 0, 0, 100, 300, 2, &ttft150),
		usageLogRow("bf-u2", base.Add(6*time.Hour), domain.ErrAbort, 7, 5, 42, 9, "m", 1, 1, 0, 0, 10, 30, 1, nil),    // abort：rc+ec 双计 + tokens 全量
		usageLogRow("bf-u3", base.Add(7*time.Hour), domain.ErrNone, 8, 5, 43, 9, "other", 5, 5, 0, 0, 50, 50, 1, nil), // 组 8 / model other——base+7h = 19:00Z = IST Aug15 00:30（跨本地日界）
		usageLogRow("bf-u4", base.Add(6*time.Hour), domain.Err5xx, 7, 5, 42, 9, "m", 1, 1, 0, 0, 1, 1, 0, nil),        // usage 5xx 行：写侧口径排除
	}
	errs := []*domain.UsageLog{
		errLogRow("bf-e1", base.Add(30*time.Minute), domain.Err5xx, 7, 5, 42, 9, "m"),
		errLogRow("bf-e2", base.Add(30*time.Minute), domain.ErrNone, 7, 5, 42, 9, "m"), // 豁免非错误行：计 rc 不计 ec
	}
	seedRawRows(t, repos, usages, errs)

	// Kolkata 本地日界：base 12:00Z = IST 17:30（Aug14）；base+6h = IST 23:30
	// （Aug14）；base+7h = IST 00:30（Aug15，日界起点 = Aug14 18:30Z）。跨界归属
	// 与逐行手工分组期望一致（独立参照，不经被测 SQL）。
	days, err := repos.Stats.ScanStatsDays(ctx, base.Add(-time.Hour), base.Add(12*time.Hour), 0, ist)
	require.NoError(t, err)
	require.Len(t, days, 2)
	require.True(t, days[0].Date.Equal(base.Add(-17*time.Hour-30*time.Minute)), "Aug14 本地日桶 = IST Aug14 零点绝对时刻（Aug13 18:30Z）（%v）", days[0].Date)
	require.Equal(t, "2026-08-14", days[0].Date.Format("2006-01-02"))
	require.Equal(t, int64(4), days[0].Requests, "u1 u2 e1 e2（u3 明日、u4 排除、err abort 无）")
	require.Equal(t, int64(2), days[0].Errors, "u2 abort + e1 5xx")
	require.Equal(t, int64(32), days[0].Tokens, "u1 30 + u2 2 + e 行 0（tokens 仅 usage 源）")
	require.Equal(t, "2026-08-15", days[1].Date.Format("2006-01-02"))
	require.Equal(t, int64(1), days[1].Requests)
	require.Equal(t, int64(10), days[1].Tokens)

	// 组过滤 + 绝对区间：组 8 只有 u3（Aug15 IST，桶起点 Aug14 18:30Z）。
	tb, err := repos.Stats.StatsTrend(ctx, base.Add(-time.Hour), base.Add(12*time.Hour), "day", 8, "", ist)
	require.NoError(t, err)
	require.Len(t, tb, 1, "组 8 只有 u3（Aug15 IST）")
	require.Equal(t, int64(1), tb[0].RequestCount)
	require.True(t, tb[0].BucketTime.Equal(base.Add(6*time.Hour+30*time.Minute)),
		"桶起点 = IST Aug15 零点（绝对时刻 %v）", tb[0].BucketTime)

	// 实体趋势：user 42（u1 u2 + e1 e2 → 4 rc / 2 ec 两桶）+ model 过滤。
	eb, err := repos.Stats.StatsEntityTrend(ctx, base.Add(-time.Hour), base.Add(12*time.Hour), "hour", "user", 42, "m", ist)
	require.NoError(t, err)
	var rc, ec int64
	for _, b := range eb {
		rc += b.RequestCount
		ec += b.ErrorCount
	}
	require.Equal(t, int64(4), rc, "entity 过滤 + err 补充（豁免 none 计 rc）")
	require.Equal(t, int64(2), ec, "abort + 5xx")

	// 绝对区间单行 sum：请求时区不改变区间数值——UTC-cube 式期望。
	su, err := repos.Stats.SummarizeStats(ctx, base.Add(-time.Hour), base.Add(12*time.Hour), 0, ist)
	require.NoError(t, err)
	require.Equal(t, int64(5), su.Requests, "u1 u2 u3 + e1 e2（u4 排除——写侧口径）")
	require.Equal(t, int64(2), su.Errors)
	require.Equal(t, int64(42), su.TotalTokens)
	require.Equal(t, int64(160), su.Cost)
	require.Equal(t, int64(380), su.RawCost)
	require.Equal(t, int64(1), su.TTFTCount)
	require.Equal(t, int64(150), su.TTFTTotalMS)
}

// TestPGRawEntityNullDropped 实体路径 IS NOT NULL 语义（raw 与 cube 写侧
// 同口径）：无 user_id 的 usage/err 行不入实体趋势；key/account 各自成组。
func TestPGRawEntityNullDropped(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	ist, err := time.LoadLocation("Asia/Kolkata") // :30 恒偏移 → raw 路径
	require.NoError(t, err)
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, base, base.Add(4*time.Hour))
	seedRawRows(t, repos, []*domain.UsageLog{
		usageLogRow("n-u1", base, domain.ErrNone, 0, 0, 42, 0, "m", 1, 1, 0, 0, 1, 1, 0, nil),
		// user_id=0（InsertBatch 不写列 → NULL）：不得被实体钻取计入
	}, []*domain.UsageLog{
		errLogRow("n-e1", base.Add(time.Hour), domain.Err5xx, 0, 0, 0, 0, "m"),
	})
	eb, err := repos.Stats.StatsEntityTrend(ctx, base.Add(-time.Hour), base.Add(4*time.Hour), "hour", "user", 42, "", ist)
	require.NoError(t, err)
	rc := int64(0)
	for _, b := range eb {
		rc += b.RequestCount
	}
	require.Equal(t, int64(1), rc, "只计 user=42 行；NULL 归属行丢弃（实体无 ID=0）")
	eb0, err := repos.Stats.StatsEntityTrend(ctx, base.Add(-time.Hour), base.Add(4*time.Hour), "hour", "user", 0, "", ist)
	require.NoError(t, err)
	require.Empty(t, eb0, "无主行不入任何实体桶")
}

// TestPGStatsZoneSessionPoison 会话 TimeZone 无关性：America/New_York 会话池
// （DSN options 注入）上跑绑定 $zone 的读——cube 路径（Shanghai）与 raw 路径
// （Kolkata）结果都与默认会话池逐桶相等；未指定（UTC）不得被会话 TZ 移动。
func TestPGStatsZoneSessionPoison(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	dayStartUTC := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, dayStartUTC, dayStartUTC.Add(24*time.Hour))
	require.NoError(t, repos.Stats.AggregateRange(ctx, dayStartUTC, dayStartUTC.Add(24*time.Hour),
		dayStartUTC.Add(17*time.Hour),
		[]*domain.StatBucket{overviewCubeBucket(dayStartUTC.Add(16*time.Hour), 4)}, nil))
	seedRawRows(t, repos, []*domain.UsageLog{
		usageLogRow("sp-u1", dayStartUTC.Add(13*time.Hour), domain.ErrNone, 1, 5, 42, 9, "m", 1, 1, 0, 0, 2, 4, 0, nil),
	}, nil)

	// 基线（默认会话）：cube Shanghai 日界 + raw Kolkata 小时界 + UTC。
	baseCube, err := repos.Stats.ScanStatsDays(ctx, dayStartUTC, dayStartUTC.Add(24*time.Hour), 0, cst)
	require.NoError(t, err)
	require.Len(t, baseCube, 1)
	require.True(t, baseCube[0].Date.Equal(dayStartUTC.Add(16*time.Hour)), "基线 = 本地 Aug16 零点")
	baseRaw, err := repos.Stats.StatsTrend(ctx, dayStartUTC, dayStartUTC.Add(24*time.Hour), "hour", 0, "", ist)
	require.NoError(t, err)
	require.Len(t, baseRaw, 1)
	require.True(t, baseRaw[0].BucketTime.Equal(dayStartUTC.Add(12*time.Hour+30*time.Minute)),
		"13:00Z = IST 18:30 → Kolkata 本地 18:00 界 = UTC 12:30", baseRaw[0].BucketTime)

	// NY 会话池（DSN options 注入 TimeZone）指向同一共享 schema。
	dsn := os.Getenv("TEST_DATABASE_URL")
	require.NotEqual(t, "", dsn)
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + sharedPGSchema()
	} else {
		dsn += "?search_path=" + sharedPGSchema()
	}
	dsn += "&options=-c%20TimeZone%3DAmerica%2FNew_York"
	nyPool, err := repository.OpenPG(ctx, dsn, 2)
	require.NoError(t, err)
	t.Cleanup(nyPool.Close)
	conn, err := nyPool.Acquire(ctx)
	require.NoError(t, err)
	var tz string
	require.NoError(t, conn.QueryRow(ctx, `SHOW TimeZone`).Scan(&tz))
	require.Equal(t, "America/New_York", tz, "会话 TZ 必须生效（否则本测试真空）")
	conn.Release()
	nyDB := stdlib.OpenDBFromPool(nyPool)
	t.Cleanup(func() { _ = nyDB.Close() })
	nyRepos, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, nyDB), false, nyPool)
	require.NoError(t, err)

	// NY 会话 + Shanghai（cube 路径）→ 与基线逐桶相等（绑定参数压过会话 TZ）。
	nyCube, err := nyRepos.Stats.ScanStatsDays(ctx, dayStartUTC, dayStartUTC.Add(24*time.Hour), 0, cst)
	require.NoError(t, err)
	require.Len(t, nyCube, 1)
	require.True(t, nyCube[0].Date.Equal(baseCube[0].Date), "NY 会话下仍按请求时区分桶（%s ≠ %s）", nyCube[0].Date, baseCube[0].Date)
	require.Equal(t, "2026-08-16", nyCube[0].Date.Format("2006-01-02"))

	// NY 会话 + Kolkata（raw 路径）→ 与基线逐桶相等。
	nyRaw, err := nyRepos.Stats.StatsTrend(ctx, dayStartUTC, dayStartUTC.Add(24*time.Hour), "hour", 0, "", ist)
	require.NoError(t, err)
	require.Len(t, nyRaw, 1)
	require.True(t, nyRaw[0].BucketTime.Equal(baseRaw[0].BucketTime), "raw 路径同样会话无关（%v ≠ %v）", nyRaw[0].BucketTime, baseRaw[0].BucketTime)

	// NY 会话 + 缺省 UTC（cube 路径）→ UTC 小时界（会话 TZ 既不漏进分组也不
	// 压掉请求时区——16:00Z 桶来自 cube，与默认会话基线同形）。
	nyUTC, err := nyRepos.Stats.StatsTrend(ctx, dayStartUTC, dayStartUTC.Add(24*time.Hour), "hour", 0, "", time.UTC)
	require.NoError(t, err)
	require.Len(t, nyUTC, 1)
	require.True(t, nyUTC[0].BucketTime.Equal(dayStartUTC.Add(16*time.Hour)), "UTC 请求下 NY 会话不得移动 cube 桶界（%v）", nyUTC[0].BucketTime)
	require.Equal(t, int64(4), nyUTC[0].RequestCount)
}

// TestPGRawConcurrentDistinctZones 并发不同请求时区互不污染（无进程级可变态
// 的结构性证明）：同一 StatRepo 上 Shanghai（cube）/Kolkata（raw）/UTC 三路
// 并发读同一窗口——各自桶界与计数独立正确（channel 屏障齐发，无 sleep）。
func TestPGRawConcurrentDistinctZones(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	base := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC) // IST 14:30 / CST 17:00
	seedAggWindow(t, repos, base, base.Add(4*time.Hour))
	seedRawRows(t, repos, []*domain.UsageLog{
		usageLogRow("cc-1", base, domain.ErrNone, 1, 5, 42, 9, "m", 1, 1, 0, 0, 1, 1, 0, nil),
		usageLogRow("cc-2", base.Add(30*time.Minute), domain.ErrNone, 1, 5, 42, 9, "m", 1, 1, 0, 0, 1, 1, 0, nil),
	}, nil)
	// 构 cube（UTC-carry 写路径）：cube 路径的 zone 才有数据可读。
	cube, entity, _, err := repos.Stats.LoadAggRange(ctx, base.Add(-time.Hour), base.Add(2*time.Hour))
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, base.Add(-time.Hour), base.Add(2*time.Hour), base.Add(2*time.Hour), cube, entity))
	var wg sync.WaitGroup
	start := make(chan struct{})
	type res struct {
		buckets []*domain.StatBucket
	}
	got := make([]res, 3)
	zones := []*time.Location{time.UTC, ist, cst}
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bs, err := repos.Stats.StatsTrend(ctx, base.Add(-time.Hour), base.Add(2*time.Hour), "hour", 0, "", zones[i])
			require.NoError(t, err)
			got[i].buckets = bs
		}(i)
	}
	close(start)
	wg.Wait()
	// cube 路径（UTC/Shanghai 恒整点）：两行同桶 09:00Z；raw 路径（Kolkata :30
	// 界）：09:00Z 行 → 08:30 桶、09:30Z 行 → 09:30 桶，各自独立正确。
	require.Equal(t, map[string]int64{"09:00": 2}, hourKey(got[0].buckets))
	require.Equal(t, map[string]int64{"08:30": 1, "09:30": 1}, hourKey(got[1].buckets))
	require.Equal(t, map[string]int64{"09:00": 2}, hourKey(got[2].buckets))
}

func hourKey(bs []*domain.StatBucket) map[string]int64 {
	m := map[string]int64{}
	for _, b := range bs {
		m[b.BucketTime.UTC().Format("15:04")] += b.RequestCount
	}
	return m
}

// TestPGRawTTFTHistAndCubeRangeEquivalence overview 幸存面语义：raw 日桶携带
// 直方图（TTFTPercentileMS 可插值）；SummarizeStats 绝对区间数值与时区无关
// ——同窗 UTC-cube 路径与整点界（CST 日界）路径等值（cube 行不被劈开时）。
func TestPGRawTTFTHistAndCubeRangeEquivalence(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	base := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, base, base.Add(4*time.Hour))
	ttft := int64(120)
	seedRawRows(t, repos, []*domain.UsageLog{
		usageLogRow("he-1", base, domain.ErrNone, 1, 5, 42, 9, "m", 1, 1, 0, 0, 1, 1, 0, &ttft),
		usageLogRow("he-2", base.Add(90*time.Minute), domain.ErrNone, 1, 5, 42, 9, "m", 1, 1, 0, 0, 1, 1, 0, &ttft),
	}, nil)
	// raw 日桶（IST → raw 路径）：hist 逐桶带回可插值。
	days, err := repos.Stats.ScanStatsDays(ctx, base, base.Add(4*time.Hour), 0, ist)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Len(t, days[0].TTFTHist, 10)
	require.Greater(t, repository.TTFTPercentileMS(days[0].TTFTHist, days[0].TTFTCount, 0.5), int64(0), "raw hist 可插值（与 overview 口径一致）")

	// cube 构建后：同绝对区间 [base, base+4h) 的 SummarizeStats 数值与时区无关
	// ——CST（整点界 → cube 路径）与 IST（:30 界 → raw 路径）扫的是同源两行，
	// 区间求和恒 2（绝对区间谓词一致，仅分组面不同）。
	cube, entity, _, err := repos.Stats.LoadAggRange(ctx, base, base.Add(4*time.Hour))
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, base, base.Add(4*time.Hour), base.Add(4*time.Hour), cube, entity))
	sSpan, err := repos.Stats.SummarizeStats(ctx, base, base.Add(4*time.Hour), 0, cst)
	require.NoError(t, err)
	sSpanRaw, err := repos.Stats.SummarizeStats(ctx, base, base.Add(4*time.Hour), 0, ist)
	require.NoError(t, err)
	require.Equal(t, int64(2), sSpan.Requests, "cube 区间 sum")
	require.Equal(t, sSpan.Requests, sSpanRaw.Requests, "同绝对区间数值不因请求时区改变（cst=cube / ist=raw）")
	require.Equal(t, sSpan.Cost, sSpanRaw.Cost)
	require.Equal(t, sSpan.TTFTTotalMS, sSpanRaw.TTFTTotalMS)
}

// TestPGRawEmptyWindowAndUnalignedBoundary raw 路径正确性双回归（窗口取
// 2025-04 孤岛日，与其他用例的 2026/now 数据零交叠）：
//   - 空窗口 SummarizeStats（无 GROUP BY 单行聚合）→ 全零结构而非 pgx NULL
//     扫描错误——rawSummary 每个入 int64 的 SUM 恒 COALESCE（空集 sum = NULL）；
//   - 窗口界劈开卷积小时行（[12:30Z,13:30Z)）时 zone=UTC 也必须 raw 路由：
//     12:45Z 行入窗并归 12:00 本地桶——cube 谓词 bucket_time >= 12:30 会把
//     12:00 整行排除（半行丢失形态，对齐前提存在的理由）。
func TestPGRawEmptyWindowAndUnalignedBoundary(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	base := time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, base, base.Add(24*time.Hour))

	s, err := repos.Stats.SummarizeStats(ctx, base, base.Add(24*time.Hour), 0, ist) // :30 偏移 → raw，空窗
	require.NoError(t, err, "rawSummary 空窗不得 NULL 扫描报错")
	require.Equal(t, int64(0), s.Requests)
	require.Equal(t, int64(0), s.Cost)
	require.Equal(t, int64(0), s.RawCost)
	require.Equal(t, int64(0), s.TotalTokens)
	require.Zero(t, s.TTFTAvgMS())

	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		usageLogRow("ua-1", base.Add(12*time.Hour+45*time.Minute), domain.ErrNone, 1, 5, 42, 9, "m", 1, 1, 0, 0, 3, 4, 1, nil),
	}))
	// 同窗 cube 落盘（12:00Z 整行）——若误走 cube 路径，[12:30,13:30) 谓词
	// 排除该行、结果为零桶；raw 逐行则计入。
	cube, entity, _, err := repos.Stats.LoadAggRange(ctx, base, base.Add(24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, base, base.Add(24*time.Hour), base.Add(24*time.Hour), cube, entity))

	bs, err := repos.Stats.StatsTrend(ctx, base.Add(12*time.Hour+30*time.Minute), base.Add(13*time.Hour+30*time.Minute), "hour", 0, "", time.UTC)
	require.NoError(t, err)
	require.Len(t, bs, 1, "界不齐 → UTC 也 raw 路由：12:45 行入窗归 12:00 本地桶（cube 谓词会整行丢失）")
	require.True(t, bs[0].BucketTime.Equal(base.Add(12*time.Hour)), "桶起点 = 12:00Z（%v）", bs[0].BucketTime)
	require.Equal(t, int64(1), bs[0].RequestCount)
}
