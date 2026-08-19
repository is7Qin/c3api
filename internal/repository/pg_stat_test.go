// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 离线聚合（spec 2026-08-14 使用量统计离线聚合化）真实 PG 测试套件：
//   - AggregateRange 覆盖落盘（参数化 INSERT 含 bigint[] 直方图 + watermark 推进）
//   - LoadAggRange 三查询 vs 旧聚合逻辑等价（旧 aggregateLocked 留存于本文件）
//   - abort 拆分断言（isErr 桶全字段 + err_logs 无双计）
//   - 两范围断言（部分小时桶跨周期不截断 + 重放幂等，worker 级时钟注入）
//   - 事务中途失败 → 游标未推进（重算不双计）
//   - watermark 初始化 ON CONFLICT DO NOTHING（多实例并发容忍）
//   - advisory lock 会话级互斥
//   - SummarizeStats/ScanStatsDays TTFT 直方图 array_agg 合并 + 插值
//   - ent carve-out（schema 无 ttft_hist 字段）+ ScanStats pgx 直查
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试重建 schema + 分区
// bootstrap（含 stats_agg_watermark 单行表）。本包 PG 测试串行（无 t.Parallel）。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/usage"
)

// —— 旧聚合逻辑留存（spec 测试节：等价性测试基座；aggregateLocked 已从生产删除） ——

// legacyAggKey 旧统计桶键（原 statBucketKey 留存于测试）。
type legacyAggKey struct {
	hourUnix   int64
	groupID    int64
	accountID  int64
	templateID int64
	userID     int64
	model      string
	isErr      bool
}

// legacyAggregate 旧 aggregateLocked 逐字段复刻（除 TotalLatencyMS——列已删除；
// TTFT/call_count 旧逻辑无累加，新列单独断言）：按小时/维度/isErr 分桶，
// 计数 + token/cost 求和。
func legacyAggregate(logs []*domain.UsageLog) map[legacyAggKey]*domain.StatBucket {
	m := make(map[legacyAggKey]*domain.StatBucket)
	for _, l := range logs {
		hour := l.CreatedAt.UTC().Truncate(time.Hour)
		isErr := l.ErrorType != domain.ErrNone
		k := legacyAggKey{hourUnix: hour.Unix(), groupID: l.GroupID, accountID: l.AccountID,
			templateID: l.TemplateID, userID: l.UserID, model: l.Model, isErr: isErr}
		c, ok := m[k]
		if !ok {
			c = &domain.StatBucket{BucketTime: hour, GroupID: l.GroupID, AccountID: l.AccountID,
				TemplateID: l.TemplateID, UserID: l.UserID, Model: l.Model, IsError: isErr}
			m[k] = c
		}
		c.RequestCount++
		if isErr {
			c.ErrorCount++
		}
		c.InputTokens += l.InputTokens
		c.OutputTokens += l.OutputTokens
		c.TotalTokens += l.TotalTokens
		c.CacheReadTokens += l.CacheReadTokens
		c.CacheCreationTokens += l.CacheCreationTokens
		c.Cost += l.Cost
	}
	return m
}

// usageLogRow 构造放行路径明细（usage_logs 成员：none/abort；全字段含 raw_cost）。
func usageLogRow(rid string, at time.Time, et domain.ErrorType, groupID, userID int64, model string,
	in, out, cacheRead, cacheCreate, cost, rawCost, callCount int64, ttft *int64) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: rid, GroupID: groupID, UserID: userID, Model: model,
		Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: et,
		InputTokens: in, OutputTokens: out, TotalTokens: in + out,
		CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreate,
		CallCount: callCount, Cost: cost, RawCost: rawCost, TTFTMS: ttft, CreatedAt: at,
	}
}

// errLogRow 构造纯错误明细（err_logs 成员：4xx/5xx/network——非 abort）。
func errLogRow(rid string, at time.Time, et domain.ErrorType, groupID, userID int64, model string) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: rid, GroupID: groupID, UserID: userID, Model: model,
		Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: et, CreatedAt: at,
	}
}

// assertBucketEqual 等价断言（旧逻辑字段逐位一致；TTFT/call_count 单独断言）。
func assertBucketEqual(t *testing.T, got, want *domain.StatBucket) {
	t.Helper()
	require.Equal(t, want.BucketTime.UTC(), got.BucketTime.UTC(), "bucket_time")
	require.Equal(t, want.GroupID, got.GroupID)
	require.Equal(t, want.AccountID, got.AccountID)
	require.Equal(t, want.TemplateID, got.TemplateID)
	require.Equal(t, want.UserID, got.UserID)
	require.Equal(t, want.Model, got.Model)
	require.Equal(t, want.IsError, got.IsError)
	require.Equal(t, want.RequestCount, got.RequestCount, "request_count")
	require.Equal(t, want.ErrorCount, got.ErrorCount, "error_count")
	require.Equal(t, want.InputTokens, got.InputTokens)
	require.Equal(t, want.OutputTokens, got.OutputTokens)
	require.Equal(t, want.TotalTokens, got.TotalTokens)
	require.Equal(t, want.CacheReadTokens, got.CacheReadTokens)
	require.Equal(t, want.CacheCreationTokens, got.CacheCreationTokens)
	require.Equal(t, want.Cost, got.Cost)
}

// TestPGStatsAggregateRangeInsert AggregateRange 覆盖落盘：参数化 INSERT（含
// bigint[] 直方图——pgx 原生编码）+ watermark 单事务推进；ScanStats pgx 直查
// 回读全字段。
func TestPGStatsAggregateRangeInsert(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)

	hist := []int64{3, 2, 1, 0, 0, 0, 0, 0, 0, 0} // 6 样本：<50 ×3、[50,100) ×2、[100,200) ×1
	rows := []*domain.StatBucket{{
		BucketTime: bucket, GroupID: 7, UserID: 42, Model: "gpt-4o",
		RequestCount: 6, ErrorCount: 0, InputTokens: 10, OutputTokens: 20,
		TotalTokens: 30, Cost: 123, RawCost: 456, CallCount: 4,
		TTFTTotalMS: 240, TTFTCount: 6, TTFTMaxMS: 150, TTFTHist: hist,
	}}
	wmTo := bucket.Add(20 * time.Minute)
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), wmTo, rows))

	// watermark 与落库同事务推进
	gotWm, err := repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, wmTo.Equal(gotWm), "watermark 推进 = wmTo（读窗口 T）")

	// ScanStats pgx 直查回读（ent carve-out：ttft_hist 经 pgx 扫描）
	got, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: bucket, To: bucket.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 1)
	b := got[0]
	require.Equal(t, int64(6), b.RequestCount)
	require.Equal(t, int64(123), b.Cost)
	require.Equal(t, int64(456), b.RawCost, "raw_cost 直查回读")
	require.Equal(t, int64(4), b.CallCount, "call_count 直查回读")
	require.Equal(t, int64(240), b.TTFTTotalMS)
	require.Equal(t, int64(6), b.TTFTCount)
	require.Equal(t, int64(150), b.TTFTMaxMS)
	require.Equal(t, hist, b.TTFTHist, "bigint[] 直方图 round-trip（ent 数组列 carve-out）")
}

// TestPGStatsRawCostRoundtrip raw_cost 全链路 roundtrip（spec 2026-08-19 测试
// 节）：usage_logs 造数（含免费组行 cost=0 raw>0）→ LoadAggRange → AggregateRange
// 落盘 → ScanStats/SummarizeStats/ScanStatsDays 读取面 raw 正确（免费组不丢）；
// 重算幂等（二次 AggregateRange 同值——DELETE+INSERT 覆盖语义）。
func TestPGStatsRawCostRoundtrip(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, h, h))
	require.NoError(t, repos.EnsureErrLogPartitions(ctx, h, h))
	require.NoError(t, repos.EnsureUsageStatsPartitions(ctx, h, h))

	// 免费组行：cost=0 raw>0（"实际消耗"口径不丢）；付费行 + abort 行
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		usageLogRow("rc1", h.Add(time.Minute), domain.ErrNone, 1, 42, "gpt-4o", 10, 20, 0, 0, 0, 500, 1, nil),   // 免费组
		usageLogRow("rc2", h.Add(2*time.Minute), domain.ErrNone, 1, 42, "gpt-4o", 5, 5, 0, 0, 100, 250, 0, nil), // 付费行
		usageLogRow("rc3", h.Add(3*time.Minute), domain.ErrAbort, 1, 42, "gpt-4o", 1, 1, 0, 0, 40, 90, 1, nil),
	}))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{
		errLogRow("rc4", h.Add(4*time.Minute), domain.Err5xx, 1, 42, "gpt-4o"), // 与 abort 同维度合并
		errLogRow("rc5", h.Add(5*time.Minute), domain.ErrNetwork, 2, 9, "claude-3-5"), // 纯 err 桶
	}))

	rows, detail, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(5), detail)

	// AggregateRange 落盘 → ScanStats 读取面
	require.NoError(t, repos.Stats.AggregateRange(ctx, h, h.Add(time.Hour), h.Add(30*time.Minute), rows))
	got, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: h, To: h.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 3)
	rawOf := map[string]int64{}
	costOf := map[string]int64{}
	for _, b := range got {
		key := fmt.Sprintf("%d|%d|%v", b.GroupID, b.UserID, b.IsError)
		rawOf[key] = b.RawCost
		costOf[key] = b.Cost
	}
	require.Equal(t, int64(750), rawOf["1|42|false"], "成功桶 SUM(raw_cost)（免费组 500 + 付费 250）")
	require.Equal(t, int64(100), costOf["1|42|false"], "免费组 cost=0 不污染付费口径")
	require.Equal(t, int64(90), rawOf["1|42|true"], "abort 桶 SUM(raw_cost)（err_logs 恒 0）")
	require.Equal(t, int64(40), costOf["1|42|true"])
	require.Equal(t, int64(0), rawOf["2|9|true"], "纯 err 桶 raw 恒 0")
	require.Equal(t, int64(0), costOf["2|9|true"])

	// 读取面其余两处：SummarizeStats 总览 + ScanStatsDays 日桶
	sum, err := repos.Stats.SummarizeStats(ctx, h, h.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Equal(t, int64(840), sum.RawCost, "summary SUM(raw_cost)（750+90+0）")
	days, err := repos.Stats.ScanStatsDays(ctx, h, h.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, int64(840), days[0].RawCost, "日桶 SUM(raw_cost)")

	// 重算幂等：二次 AggregateRange（同 rows）→ 桶值不变（DELETE+INSERT 覆盖）
	require.NoError(t, repos.Stats.AggregateRange(ctx, h, h.Add(time.Hour), h.Add(30*time.Minute), rows))
	got2, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: h, To: h.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, got2, 3)
	for _, b := range got2 {
		key := fmt.Sprintf("%d|%d|%v", b.GroupID, b.UserID, b.IsError)
		require.Equal(t, rawOf[key], b.RawCost, "重算幂等（raw_cost 不变）")
		require.Equal(t, costOf[key], b.Cost, "重算幂等（cost 不变）")
	}
}

// TestPGStatsLoadAggRangeEquivalent 聚合等价断言（spec 测试节）：同一批
// UsageLog/ErrLog 数据，离线聚合 SQL 结果 vs 旧聚合逻辑 → 桶逐字段一致（除
// 延迟列——已删除；TTFT/call_count 旧逻辑无累加，新列单独断言；含 abort 桶
// error_count = request_count）。fixture 全量队列（拒绝行无丢样——等价成立）。
func TestPGStatsLoadAggRangeEquivalent(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	// fixture 固定历史日：bootstrap 只预建执行当日+明日分区（partition.go:388）
	// ——显式预建固定日期分区（与既有先例一致 pg_errlog_partition_test.go:130-132）；
	// usage/err 两张表同时预建（只补 usage 会让同缺口顺延到 err_logs）。
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, h, h))
	require.NoError(t, repos.EnsureErrLogPartitions(ctx, h, h))

	ttft := func(v int64) *int64 { return &v }
	logs := []*domain.UsageLog{
		// usage_logs：none 成功行（全字段）
		usageLogRow("r1", h.Add(1*time.Minute), domain.ErrNone, 1, 42, "gpt-4o", 10, 20, 4, 2, 100, 300, 3, ttft(30)),
		// r2 = 免费组行：cost=0 但 raw>0（"实际消耗"可见）
		usageLogRow("r2", h.Add(2*time.Minute), domain.ErrNone, 1, 42, "gpt-4o", 0, 0, 0, 0, 0, 150, 1, nil),
		usageLogRow("r3", h.Add(3*time.Minute), domain.ErrNone, 1, 7, "gpt-4o", 5, 5, 0, 0, 50, 200, 0, ttft(80)),
		// usage_logs：abort 双轨行（全字段——P1-B）
		usageLogRow("r4", h.Add(4*time.Minute), domain.ErrAbort, 1, 42, "gpt-4o", 3, 6, 0, 0, 40, 90, 2, ttft(120)),
		usageLogRow("r5", h.Add(5*time.Minute), domain.ErrAbort, 1, 7, "gpt-4o", 1, 1, 0, 0, 10, 20, 1, nil),
	}
	// err_logs：纯错误行（4xx/5xx/network——count 语义）
	errLogs := []*domain.UsageLog{
		errLogRow("e1", h.Add(6*time.Minute), domain.Err5xx, 1, 42, "gpt-4o"),
		errLogRow("e2", h.Add(7*time.Minute), domain.Err4xx, 1, 7, "gpt-4o"),
		errLogRow("e3", h.Add(8*time.Minute), domain.ErrNetwork, 2, 9, "claude-3-5"),
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, errLogs))

	rows, detail, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(8), detail, "消费明细行数 = 三查询 count(*) 合计")

	// 旧逻辑全量复刻（放行行 + err_logs 行——fixture 无丢样）
	want := legacyAggregate(append(append([]*domain.UsageLog{}, logs...), errLogs...))
	require.Len(t, rows, len(want), "桶数与旧逻辑一致")
	gotByKey := make(map[legacyAggKey]*domain.StatBucket, len(rows))
	for _, b := range rows {
		gotByKey[legacyAggKey{hourUnix: b.BucketTime.Unix(), groupID: b.GroupID, accountID: b.AccountID,
			templateID: b.TemplateID, userID: b.UserID, model: b.Model, isErr: b.IsError}] = b
	}
	for k, wb := range want {
		gb, ok := gotByKey[k]
		require.True(t, ok, "离线 SQL 缺失旧逻辑桶 %+v", k)
		assertBucketEqual(t, gb, wb)
	}
	// 新列单独断言（旧逻辑无累加）
	okBucket := gotByKey[legacyAggKey{hourUnix: h.Unix(), groupID: 1, accountID: 0, templateID: 0, userID: 42, model: "gpt-4o", isErr: false}]
	require.NotNil(t, okBucket)
	require.Equal(t, int64(100), okBucket.Cost)
	require.Equal(t, int64(450), okBucket.RawCost, "成功桶 SUM(raw_cost)（r1 300 + 免费组 r2 150）")
	require.Equal(t, int64(4), okBucket.CallCount, "none 行 call_count sum（3+1）")
	require.Equal(t, int64(1), okBucket.TTFTCount, "TTFT 样本数 = 非 nil 行数（r2 无首 token）")
	require.Equal(t, int64(30), okBucket.TTFTTotalMS)
	require.Equal(t, int64(30), okBucket.TTFTMaxMS)
	require.Equal(t, []int64{1, 0, 0, 0, 0, 0, 0, 0, 0, 0}, okBucket.TTFTHist, "30 在 [0,50) 档")

	// abort 桶（同维度 err_logs 纯错误行双源合并）：r4（abort）+ e1（5xx，
	// 同 group1/user42/gpt-4o）→ RequestCount=2；abort 行的 error_count =
	// count(*)（P1-B）+ err_logs count 语义合并。
	abortBucket := gotByKey[legacyAggKey{hourUnix: h.Unix(), groupID: 1, userID: 42, model: "gpt-4o", isErr: true}]
	require.NotNil(t, abortBucket)
	require.Equal(t, int64(2), abortBucket.RequestCount, "abort(1) + err_logs 5xx(1) 双源合并")
	require.Equal(t, int64(2), abortBucket.ErrorCount, "abort 行 error_count = count(*) 与 err_logs count 语义合并")
	require.Equal(t, int64(2), abortBucket.CallCount, "abort 行 CallCount 计入（err_logs 恒 0）")
	require.Equal(t, int64(120), abortBucket.TTFTMaxMS, "abort 行含 TTFT 也计入其桶")
	require.Equal(t, int64(3), abortBucket.InputTokens, "abort 行 tokens 计入（err_logs 恒 0）")
	require.Equal(t, int64(6), abortBucket.OutputTokens)
	require.Equal(t, int64(40), abortBucket.Cost)
	require.Equal(t, int64(90), abortBucket.RawCost, "abort 桶 SUM(raw_cost)（err_logs 恒 0）")

	// err_logs 合并桶：abort（usage_logs）+ 纯错误（err_logs）双源合并
	errMergeBucket := gotByKey[legacyAggKey{hourUnix: h.Unix(), groupID: 1, userID: 7, model: "gpt-4o", isErr: true}]
	require.NotNil(t, errMergeBucket)
	require.Equal(t, int64(2), errMergeBucket.RequestCount, "abort(1) + 4xx(1) 合并")
	require.Equal(t, int64(2), errMergeBucket.ErrorCount, "双源错误行 count 语义")
	require.Equal(t, int64(1), errMergeBucket.CallCount, "abort 行 CallCount 计入（err_logs 恒 0）")
	require.Equal(t, int64(1), errMergeBucket.InputTokens, "abort 行 tokens 计入（err_logs 恒 0）")
	require.Equal(t, int64(2), errMergeBucket.TotalTokens, "abort 行 TotalTokens（1+1）")
	require.Equal(t, int64(20), errMergeBucket.RawCost, "abort 行 raw 计入（err_logs 恒 0）")

	// 纯 err_logs 桶（e3——无 usage_logs 同维度行）：raw 恒 0
	errOnlyBucket := gotByKey[legacyAggKey{hourUnix: h.Unix(), groupID: 2, userID: 9, model: "claude-3-5", isErr: true}]
	require.NotNil(t, errOnlyBucket)
	require.Equal(t, int64(0), errOnlyBucket.RawCost, "err 桶（瘦表）raw 恒 0")
}

// TestPGStatsAbortSplitNoDoubleCount abort 拆分断言（P1-B）：abort 行只经
// usage_logs 全字段计一次；err_logs 的 abort 行（若存在——豁免通道实际不写
// abort 到 err_logs，防御性）被 WHERE error_type <> 'abort' 排除，无双计。
func TestPGStatsAbortSplitNoDoubleCount(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 14, 11, 0, 0, 0, time.UTC)
	// 固定历史日分区显式预建（bootstrap 只建当日+明日；同 TestPGStatsLoadAggRangeEquivalent）
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, h, h))
	require.NoError(t, repos.EnsureErrLogPartitions(ctx, h, h))

	ttft := func(v int64) *int64 { return &v }
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		usageLogRow("a1", h.Add(time.Minute), domain.ErrAbort, 3, 42, "gpt-4o", 7, 8, 1, 1, 60, 120, 5, ttft(200)),
	}))
	// err_logs 插入一条 abort 行（防御性双计探针——实际豁免通道不写，但 SQL
	// 语义必须排除）
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{
		{RequestID: "x-abort", GroupID: 3, UserID: 42, Model: "gpt-4o",
			Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrAbort, CreatedAt: h.Add(2 * time.Minute)},
	}))

	rows, detail, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), detail, "err_logs abort 行被排除（无双计）")
	require.Len(t, rows, 1, "仅 usage_logs abort 一行成桶")
	b := rows[0]
	require.True(t, b.IsError)
	require.Equal(t, int64(1), b.RequestCount, "abort 行恰计一次")
	require.Equal(t, int64(1), b.ErrorCount)
	require.Equal(t, int64(5), b.CallCount, "abort 行全字段（call_count）")
	require.Equal(t, int64(15), b.TotalTokens, "abort 行全字段（tokens）")
	require.Equal(t, int64(60), b.Cost, "abort 行全字段（cost）")
	require.Equal(t, int64(120), b.RawCost, "abort 行全字段（raw_cost）")
	require.Equal(t, int64(200), b.TTFTMaxMS, "abort 行全字段（TTFT）")
	require.Equal(t, int64(1), b.TTFTCount)
}

// TestPGStatsAsyncAggregation 异步语义断言 + 两范围属性端到端（spec 测试节：
// "Record 后 usage_stats 无变化；worker 周期后落库"）：真实时钟短周期 worker
// 驱动——插入明细（created_at 固定小时 h）后 usage_stats 立即无变化（异步）→
// 周期后落库；同小时追加行 → 下周期整小时桶重建（部分小时桶跨周期不截断，
// P1-A——若按读窗口 DELETE 则首批行丢失）；再周期重放 → 桶值不变（幂等）。
func TestPGStatsAsyncAggregation(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Now().UTC().Truncate(time.Hour)

	// cycle 1 窗口行（h+1m..h+8m；created_at 固定过去时刻，worker 滞后即可消费）
	logs1 := make([]*domain.UsageLog, 0, 8)
	for i := 0; i < 8; i++ {
		logs1 = append(logs1, usageLogRow("c1-"+string(rune('a'+i)), h.Add(time.Duration(i+1)*time.Minute),
			domain.ErrNone, 1, 42, "gpt-4o", 1, 1, 0, 0, 0, 0, 0, nil))
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs1))

	// 异步语义：worker 周期前 usage_stats 无变化
	got, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: h, To: h.Add(time.Hour)})
	require.NoError(t, err)
	require.Empty(t, got, "Record 后 usage_stats 无变化（请求路径零统计投递）")

	w := usage.NewStatsAgg(usage.StatsAggConfig{Interval: 150 * time.Millisecond, Lag: 50 * time.Millisecond}, repos.Stats, nil)
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() { require.NoError(t, w.Close(context.Background())) })

	// worker 周期后落库（轮询收敛——无固定 sleep）
	waitForRequestCount(t, repos, h, 8, "cycle 1 窗口行已入桶")

	// 同小时追加行（h+25m..h+27m）→ 下周期整小时桶重建（部分小时桶不截断）
	logs2 := make([]*domain.UsageLog, 0, 3)
	for i := 0; i < 3; i++ {
		logs2 = append(logs2, usageLogRow("c2-"+string(rune('a'+i)), h.Add(time.Duration(25+i)*time.Minute),
			domain.ErrNone, 1, 42, "gpt-4o", 1, 1, 0, 0, 0, 0, 0, nil))
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs2))
	waitForRequestCount(t, repos, h, 11, "部分小时桶跨周期累积不截断（8+3 全量重建）")

	// 幂等重放：无新行再周期 → 桶值不变（不双计）
	b := waitForRequestCount(t, repos, h, 11, "重放同范围结果一致")
	require.Equal(t, int64(22), b.TotalTokens, "桶由全量行重建（首批 token 不丢）")
}

// waitForRequestCount 轮询 ScanStats 至桶 request_count 达 want（真实时钟异步
// 收敛；5s 超时）。返回桶（供后续字段断言）。
func waitForRequestCount(t *testing.T, repos *repository.Repository, h time.Time, want int64, msg string) *domain.StatBucket {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := repos.Stats.ScanStats(context.Background(), repository.StatQuery{From: h, To: h.Add(time.Hour)})
		require.NoError(t, err)
		if len(got) == 1 && got[0].RequestCount == want {
			return got[0]
		}
		if time.Now().After(deadline) {
			gotCount := int64(0)
			if len(got) == 1 {
				gotCount = got[0].RequestCount
			}
			require.FailNow(t, "%s: 桶 request_count=%d want=%d", msg, gotCount, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPGStatsAggregateRangeTxAtomic 事务中途失败（注入）→ 游标未推进（重算
// 恢复不双计）：坏行 bucket_time 落在无分区区间（2099——bootstrap 只预建当日/
// 明日分区；INSERT 撞 "no partition of relation found for row"——正是 watermark
// 初始化 now−滞后防的 DELETE 撞已 DROP 分区同族失败面）→ 整事务回滚 → watermark
// 不动、usage_stats 无部分行；修复后重跑成功。
func TestPGStatsAggregateRangeTxAtomic(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	wmTo := bucket.Add(15 * time.Minute)

	good := &domain.StatBucket{BucketTime: bucket, GroupID: 1, UserID: 42, Model: "m",
		RequestCount: 1, TTFTHist: make([]int64, 10)}
	bad := &domain.StatBucket{BucketTime: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		GroupID: 2, UserID: 43, Model: "m", RequestCount: 1, TTFTHist: make([]int64, 10)}
	err := repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), wmTo, []*domain.StatBucket{good, bad})
	require.Error(t, err, "无分区 bucket_time 使 INSERT 失败（整事务回滚）")

	gotWm, err := repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, gotWm.IsZero(), "事务回滚 → 游标不动")
	got, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: bucket, To: bucket.Add(time.Hour)})
	require.NoError(t, err)
	require.Empty(t, got, "事务回滚 → usage_stats 无部分行")

	// 修复后重跑成功（重算恢复不双计）
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), wmTo, []*domain.StatBucket{good}))
	gotWm, err = repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, gotWm.Equal(wmTo), "修复后重跑成功")
}

// TestPGStatsWatermarkInitConcurrent watermark 初始化：全新库（无行）→
// ON CONFLICT DO NOTHING 容忍多实例并发初始化（先到先得，败者不覆盖）；已存在
// 不重置。
func TestPGStatsWatermarkInitConcurrent(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	t1 := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)

	require.NoError(t, repos.Stats.InitStatsAggWatermark(ctx, t1))
	require.NoError(t, repos.Stats.InitStatsAggWatermark(ctx, t2), "并发初始化 ON CONFLICT DO NOTHING 容忍")
	got, err := repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, got.Equal(t1), "先到先得，后到不覆盖（%s）", got)
	require.NoError(t, repos.Stats.InitStatsAggWatermark(ctx, t2))
	got, err = repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, got.Equal(t1), "重复初始化不重置")
}

// TestPGStatsAdvisoryLock 会话级 advisory lock：首个获取者持有期间另一获取
// 失败（ok=false）；释放后可再获取。
func TestPGStatsAdvisoryLock(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	rel1, ok, err := repos.Stats.AcquireStatsAggLock(ctx)
	require.NoError(t, err)
	require.True(t, ok, "首获取者成功")
	rel2, ok, err := repos.Stats.AcquireStatsAggLock(ctx)
	require.NoError(t, err)
	require.False(t, ok, "锁持有期间其他实例抢锁失败（单写者）")
	require.Nil(t, rel2)
	rel1() // 释放
	rel3, ok, err := repos.Stats.AcquireStatsAggLock(ctx)
	require.NoError(t, err)
	require.True(t, ok, "释放后可再获取")
	rel3()
}

// TestPGStatsSummarizeTTFT array_agg 直方图合并 + Go 侧插值（spec 测试节）：
// 多行同区间直方图 → SummarizeStats 合并后 p50/p90/p95/p99/avg/max 数值断言
// + call_count/ttft 字段 + 缓存键不变（无新参数）。
func TestPGStatsSummarizeTTFT(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)

	// 已知分布：每行 6 样本 <50 + 4 样本 [50,100)（两行合并后 = [12,8,0...0]）
	// **合并先于插值**（array_agg 逐元素合并后 Go 侧插值）：N=10，merged hist
	// [12,8]：
	//   p50 rank=5 → 桶0 → 0 + 5/12×50 ≈ 20；p90 rank=9 → 0 + 9/12×50 ≈ 37；
	//   p95/p99 rank=10 → 0 + 10/12×50 ≈ 41。
	hist := []int64{6, 4, 0, 0, 0, 0, 0, 0, 0, 0}
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute),
		[]*domain.StatBucket{
			{BucketTime: bucket, GroupID: 1, UserID: 42, Model: "m", RequestCount: 5, CallCount: 3,
				Cost: 100, RawCost: 300, TTFTTotalMS: 300, TTFTCount: 5, TTFTMaxMS: 90, TTFTHist: hist},
			{BucketTime: bucket, GroupID: 2, UserID: 42, Model: "m", RequestCount: 5, CallCount: 2,
				Cost: 50, RawCost: 150, TTFTTotalMS: 180, TTFTCount: 5, TTFTMaxMS: 60, TTFTHist: hist},
		}))

	sum, err := repos.Stats.SummarizeStats(ctx, bucket, bucket.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Equal(t, int64(10), sum.Requests)
	require.Equal(t, int64(450), sum.RawCost, "raw_cost 跨行合并（300+150）")
	require.Equal(t, int64(150), sum.Cost, "cost 跨行合并（100+50）")
	require.Equal(t, int64(5), sum.CallCount, "call_count 跨行合并")
	require.Equal(t, int64(480), sum.TTFTTotalMS, "ttft_total 跨行合并")
	require.Equal(t, int64(10), sum.TTFTCount)
	require.Equal(t, int64(90), sum.TTFTMaxMS, "max 取大")
	require.Equal(t, int64(48), int64(sum.TTFTAvgMS()), "avg = 查询侧 Go 除（480/10）")
	require.Equal(t, []int64{12, 8, 0, 0, 0, 0, 0, 0, 0, 0}, sum.TTFTHist, "array_agg 直方图逐元素合并")
	require.Equal(t, int64(20), sum.TTFTPercentileMS(0.50), "p50 桶内线性插值（nearest-rank；合并后 hist）")
	require.Equal(t, int64(37), sum.TTFTPercentileMS(0.90), "p90 插值")
	require.Equal(t, int64(41), sum.TTFTPercentileMS(0.95), "p95 插值")
	require.Equal(t, int64(41), sum.TTFTPercentileMS(0.99), "p99 插值")
}

// TestPGStatsScanStatsCarveOut ent carve-out 断言（评审 P1-1）：数组列不进 ent
// schema（UsageStat 实体无 TtftHist 字段——ent 无 PG 数组类型）；ScanStats 改
// pgx 直查仍返回 ttft_hist（TestPGStatsAggregateRangeInsert 已 round-trip）。
func TestPGStatsScanStatsCarveOut(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	bucket := time.Now().UTC().Truncate(time.Hour)
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(10*time.Minute),
		[]*domain.StatBucket{{BucketTime: bucket, GroupID: 1, UserID: 42, Model: "m",
			RequestCount: 1, TTFTHist: make([]int64, 10)}}))

	got, err := repos.Stats.ScanStats(ctx, repository.StatQuery{From: bucket, To: bucket.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].TTFTHist, 10, "pgx 直查扫描 bigint[]（ent 无法扫描）")
	// 过滤面（group_id/template_id/user_id/model）仍生效（评审 Important-1：
	// template_id 过滤依赖契约——rewrite spec /stats 端点接线）
	got, err = repos.Stats.ScanStats(ctx, repository.StatQuery{From: bucket, To: bucket.Add(time.Hour), GroupID: 2})
	require.NoError(t, err)
	require.Empty(t, got, "group 过滤生效")
	got, err = repos.Stats.ScanStats(ctx, repository.StatQuery{From: bucket, To: bucket.Add(time.Hour), TemplateID: 1})
	require.NoError(t, err)
	require.Empty(t, got, "template 过滤生效（行 template_id=0 ≠ 1）")
	// template_id 正向命中：同维度列再种一行 template_id=5 → 过滤只回该行
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(20*time.Minute),
		[]*domain.StatBucket{{BucketTime: bucket, GroupID: 1, TemplateID: 5, UserID: 42, Model: "m",
			RequestCount: 7, TTFTHist: make([]int64, 10)}}))
	got, err = repos.Stats.ScanStats(ctx, repository.StatQuery{From: bucket, To: bucket.Add(time.Hour), TemplateID: 5})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(7), got[0].RequestCount, "template 过滤命中行")
}

// TestPGStatsSummarizeNoData 空区间：summary 全零 + 空直方图（无 42703/扫描
// 错误——COALESCE 空数组回落）。
func TestPGStatsSummarizeNoData(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)
	sum, err := repos.Stats.SummarizeStats(ctx, bucket, bucket.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Zero(t, sum.Requests)
	require.Zero(t, sum.CallCount)
	require.Zero(t, sum.RawCost, "空区间 COALESCE 归零（裸 sum(raw_cost) = NULL 扫描报错）")
	require.Zero(t, sum.TTFTCount)
	require.Zero(t, sum.TTFTPercentileMS(0.95), "无样本 → 0")
	require.Zero(t, sum.TTFTAvgMS())
	require.Equal(t, make([]int64, 10), sum.TTFTHist, "空直方图合并 = 全零 10 档")
	trend, err := repos.Stats.ScanStatsDays(ctx, bucket, bucket.Add(24*time.Hour), 0)
	require.NoError(t, err)
	require.Empty(t, trend, "无日桶")
}
