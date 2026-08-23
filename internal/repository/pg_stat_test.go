// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 离线聚合 v2 写面真实 PG 测试套件（spec 2026-08-23 §7）：
//   - AggregateRange 双表单事务覆盖落盘（bigint[] 直方图 + watermark 推进）
//   - TestPGStatsAggV2CubeEquivalence：cube 两源聚合 vs Go 侧暴力逐行对照
//     （随机种子；含 abort 进 ec、usage_logs 非 none/abort 行排除、hist 逐档）
//   - TestPGStatsAggV2EntityEquivalence：entity 六查询等价 + IS NOT NULL 丢弃
//     语义 + model 拆分合并（三实体类型）
//   - abort 防双计（err_logs abort 行被 WHERE 排除）
//   - TestPGStatsAggIdempotentDoubleRun：同范围双跑行集与 watermark 恒等
//   - 两范围断言（worker 级端到端：部分小时桶跨周期不截断 + 重放幂等）
//   - TestPGStatsAggDualTableTxRollback：事务中途注入失败 → 双表均回滚 +
//     watermark 不动（重算恢复不双计）
//   - watermark 初始化 ON CONFLICT DO NOTHING / advisory lock 会话级互斥
//   - SummarizeStats/ScanStatsDays TTFT 直方图 array_agg 合并 + 插值（overview
//     幸存面——ScanStatsDays 继续消费 cube hist，v2.2 裁决）
//
// 读取面下推金标准对照在 pg_stat_query_test.go。基座约定同 pg_partition_test.go：
// newPGRepos 每测试重建 schema + 分区 bootstrap（含 usage_entity_stats 与
// stats_agg_watermark）。本包 PG 测试串行（无 t.Parallel——每测试 DROP SCHEMA）。

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/usage"
)

// —— 暴力聚合参照（Go 侧逐行复刻 SQL 口径；等价性测试金标准） ——

// expCubeKey cube 参照键（= aggRowKey = 唯一索引列序）。
type expCubeKey struct {
	hour    time.Time
	groupID int64
	model   string
}

// expCube cube 参照行（hist 10 档）。
type expCube struct {
	req, errN, in, out, tot, cr, cc, cost, raw, calls, ttftS, ttftC, ttftM int64
	hist                                                                   []int64
}

func newExpCube() *expCube { return &expCube{hist: make([]int64, 10)} }

// expCubeOf 暴力复刻 cube 两查询（aggUsageSQL + aggErrLogSQL 口径）：
//   - usage_logs 只取 error_type ∈ {none, abort}（WHERE IN 过滤——其余类型
//     如 5xx 不入 cube）；rc=count(*)、ec=FILTER(<>'none')、测量列全量累加；
//   - err_logs 排除 abort 行（防双计）；rc=count(*) 含豁免 none 行、ec=
//     FILTER(<>'none')、测量列恒 0。
func expCubeOf(logs, errLogs []*domain.UsageLog) map[expCubeKey]*expCube {
	m := map[expCubeKey]*expCube{}
	upsert := func(k expCubeKey) *expCube {
		c := m[k]
		if c == nil {
			c = newExpCube()
			m[k] = c
		}
		return c
	}
	for _, l := range logs {
		if l.ErrorType != domain.ErrNone && l.ErrorType != domain.ErrAbort {
			continue
		}
		k := expCubeKey{l.CreatedAt.UTC().Truncate(time.Hour), l.GroupID, l.Model}
		c := upsert(k)
		c.req++
		if l.ErrorType != domain.ErrNone {
			c.errN++
		}
		c.in += l.InputTokens
		c.out += l.OutputTokens
		c.tot += l.TotalTokens
		c.cr += l.CacheReadTokens
		c.cc += l.CacheCreationTokens
		c.cost += l.Cost
		c.raw += l.RawCost
		c.calls += l.CallCount
		if l.TTFTMS != nil {
			c.ttftS += *l.TTFTMS
			c.ttftC++
			if *l.TTFTMS > c.ttftM {
				c.ttftM = *l.TTFTMS
			}
			c.hist[ttftBucketOf(*l.TTFTMS)]++
		}
	}
	for _, l := range errLogs {
		if l.ErrorType == domain.ErrAbort {
			continue
		}
		k := expCubeKey{l.CreatedAt.UTC().Truncate(time.Hour), l.GroupID, l.Model}
		c := upsert(k)
		c.req++
		if l.ErrorType != domain.ErrNone {
			c.errN++
		}
	}
	return m
}

// ttftBucketOf 直方图档位下标（边界与 stat_repo.go ttftHistBounds 钉死同步；
// 测试侧独立实现——不 import 生产私有变量，锚定口径漂移）。
var testTTFTBounds = []int64{50, 100, 200, 400, 800, 1600, 3200, 6400, 12800}

func ttftBucketOf(v int64) int {
	for i, b := range testTTFTBounds {
		if v < b {
			return i
		}
	}
	return len(testTTFTBounds)
}

// assertCubeEqual cube 等价断言（逐字段含 hist 逐档；键字段由调用方查表对齐）。
func assertCubeEqual(t *testing.T, b *domain.StatBucket, w *expCube) {
	t.Helper()
	assertCubeMeasures(t, b, w)
	require.Equal(t, w.hist, b.TTFTHist, "hist 逐档相等")
}

// assertCubeMeasures cube 测量列断言（无 hist——trend 投影不含数组列，直方图
// 草图走 StatsTTFTSketch，见 stat_query_repo.go StatsTrend 注释）。
func assertCubeMeasures(t *testing.T, b *domain.StatBucket, w *expCube) {
	t.Helper()
	require.Equal(t, w.req, b.RequestCount, "request_count")
	require.Equal(t, w.errN, b.ErrorCount, "error_count")
	require.Equal(t, w.in, b.InputTokens)
	require.Equal(t, w.out, b.OutputTokens)
	require.Equal(t, w.tot, b.TotalTokens)
	require.Equal(t, w.cr, b.CacheReadTokens)
	require.Equal(t, w.cc, b.CacheCreationTokens)
	require.Equal(t, w.cost, b.Cost)
	require.Equal(t, w.raw, b.RawCost)
	require.Equal(t, w.calls, b.CallCount)
	require.Equal(t, w.ttftS, b.TTFTTotalMS)
	require.Equal(t, w.ttftC, b.TTFTCount)
	require.Equal(t, w.ttftM, b.TTFTMaxMS)
}

// expEntityKey entity 参照键（= 唯一索引列序）。
type expEntityKey struct {
	hour  time.Time
	etype string
	eid   int64
	model string
}

// expEntity entity 参照行（无 hist）。
type expEntity struct {
	req, errN, calls, in, out, tot, cr, cc, cost, raw, ttftS, ttftC, ttftM int64
}

// entityIDOf 实体归属值（InsertBatch 契约：id<=0 不写列 → DB NULL → 被
// WHERE IS NOT NULL 丢弃——本函数返回 (id, false) 表达同一丢弃语义）。
func entityIDOf(l *domain.UsageLog, etype string) (int64, bool) {
	switch etype {
	case "account":
		return l.AccountID, l.AccountID > 0
	case "user":
		return l.UserID, l.UserID > 0
	case "key":
		return l.KeyID, l.KeyID > 0
	default:
		return 0, false
	}
}

// expEntityOf 暴力复刻实体六查询（单实体类型）：usage 源全字段 + errlog 源两
// 计数列；IS NOT NULL 丢弃语义经 entityIDOf 第二返回值表达。
func expEntityOf(etype string, logs, errLogs []*domain.UsageLog) map[expEntityKey]*expEntity {
	m := map[expEntityKey]*expEntity{}
	upsert := func(k expEntityKey) *expEntity {
		c := m[k]
		if c == nil {
			c = &expEntity{}
			m[k] = c
		}
		return c
	}
	for _, l := range logs {
		if l.ErrorType != domain.ErrNone && l.ErrorType != domain.ErrAbort {
			continue
		}
		id, ok := entityIDOf(l, etype)
		if !ok {
			continue // WHERE <entity_col> IS NOT NULL
		}
		c := upsert(expEntityKey{l.CreatedAt.UTC().Truncate(time.Hour), etype, id, l.Model})
		c.req++
		if l.ErrorType != domain.ErrNone {
			c.errN++
		}
		c.calls += l.CallCount
		c.in += l.InputTokens
		c.out += l.OutputTokens
		c.tot += l.TotalTokens
		c.cr += l.CacheReadTokens
		c.cc += l.CacheCreationTokens
		c.cost += l.Cost
		c.raw += l.RawCost
		if l.TTFTMS != nil {
			c.ttftS += *l.TTFTMS
			c.ttftC++
			if *l.TTFTMS > c.ttftM {
				c.ttftM = *l.TTFTMS
			}
		}
	}
	for _, l := range errLogs {
		if l.ErrorType == domain.ErrAbort {
			continue
		}
		id, ok := entityIDOf(l, etype)
		if !ok {
			continue
		}
		c := upsert(expEntityKey{l.CreatedAt.UTC().Truncate(time.Hour), etype, id, l.Model})
		c.req++
		if l.ErrorType != domain.ErrNone {
			c.errN++
		}
	}
	return m
}

// assertEntityEqual entity 等价断言（逐字段）。
func assertEntityEqual(t *testing.T, b *domain.EntityStatBucket, w *expEntity) {
	t.Helper()
	require.Equal(t, w.req, b.RequestCount, "request_count")
	require.Equal(t, w.errN, b.ErrorCount, "error_count")
	require.Equal(t, w.calls, b.CallCount)
	require.Equal(t, w.in, b.InputTokens)
	require.Equal(t, w.out, b.OutputTokens)
	require.Equal(t, w.tot, b.TotalTokens)
	require.Equal(t, w.cr, b.CacheReadTokens)
	require.Equal(t, w.cc, b.CacheCreationTokens)
	require.Equal(t, w.cost, b.Cost)
	require.Equal(t, w.raw, b.RawCost)
	require.Equal(t, w.ttftS, b.TTFTTotalMS)
	require.Equal(t, w.ttftC, b.TTFTCount)
	require.Equal(t, w.ttftM, b.TTFTMaxMS)
}

// —— fixture 构造 ——

// usageLogRow 构造放行路径明细（usage_logs 成员；accountID/userID/keyID ≤ 0 =
// InsertBatch 不写列 → DB NULL——IS NOT NULL 丢弃语义的造数入口）。
func usageLogRow(rid string, at time.Time, et domain.ErrorType, groupID, accountID, userID, keyID int64, model string,
	in, out, cacheRead, cacheCreate, cost, rawCost, callCount int64, ttft *int64) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: rid, GroupID: groupID, AccountID: accountID, UserID: userID, KeyID: keyID,
		Model: model, Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: et,
		InputTokens: in, OutputTokens: out, TotalTokens: in + out,
		CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreate,
		CallCount: callCount, Cost: cost, RawCost: rawCost, TTFTMS: ttft, CreatedAt: at,
	}
}

// errLogRow 构造纯错误明细（err_logs 成员；非 abort——abort 行见双计探针）。
func errLogRow(rid string, at time.Time, et domain.ErrorType, groupID, accountID, userID, keyID int64, model string) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: rid, GroupID: groupID, AccountID: accountID, UserID: userID, KeyID: keyID,
		Model: model, Format: domain.FormatOpenAIChat, StatusCode: 500, ErrorType: et, CreatedAt: at,
	}
}

// seedAggWindow 固定历史日分区预建（bootstrap 只建当日+明日——固定日期造数
// 必须显式预建四张分区表分区，先例 pg_errlog_partition_test.go）。
func seedAggWindow(t *testing.T, repos *repository.Repository, from, to time.Time) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, from, to))
	require.NoError(t, repos.EnsureErrLogPartitions(ctx, from, to))
	require.NoError(t, repos.EnsureUsageStatsPartitions(ctx, from, to))
	require.NoError(t, repos.EnsureUsageEntityStatsPartitions(ctx, from, to))
}

// —— 用例 ——

// TestPGStatsAggregateRangeInsert AggregateRange 双表覆盖落盘：参数化 INSERT
// （cube 含 bigint[] 直方图——pgx 原生编码；entity 全测量列）+ watermark 单事务
// 推进；pgx 直查回读全字段（ent 数组列 carve-out）。
func TestPGStatsAggregateRangeInsert(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	now := time.Now().UTC()
	bucket := now.Truncate(time.Hour)

	hist := []int64{3, 2, 1, 0, 0, 0, 0, 0, 0, 0} // 6 样本：<50 ×3、[50,100) ×2、[100,200) ×1
	cube := []*domain.StatBucket{{
		BucketTime: bucket, GroupID: 7, Model: "gpt-4o",
		RequestCount: 6, ErrorCount: 0, InputTokens: 10, OutputTokens: 20,
		TotalTokens: 30, Cost: 123, RawCost: 456, CallCount: 4,
		TTFTTotalMS: 240, TTFTCount: 6, TTFTMaxMS: 150, TTFTHist: hist,
	}}
	entity := []*domain.EntityStatBucket{{
		BucketTime: bucket, EntityType: "account", EntityID: 55, Model: "gpt-4o",
		RequestCount: 6, ErrorCount: 1, CallCount: 4, InputTokens: 10, OutputTokens: 20,
		TotalTokens: 30, Cost: 123, RawCost: 456,
		TTFTTotalMS: 240, TTFTCount: 6, TTFTMaxMS: 150,
	}}
	wmTo := bucket.Add(20 * time.Minute)
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), wmTo, cube, entity))

	// watermark 与落库同事务推进
	gotWm, err := repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, wmTo.Equal(gotWm), "watermark 推进 = wmTo（读窗口 T）")

	// cube 回读（pgx 直查——ent 无法扫描 bigint[]）
	var (
		req, ec, cost, raw, call, ttftS, ttftC, ttftM int64
		gotHist                                       []int64
	)
	err = pool.QueryRow(ctx, `SELECT request_count, error_count, cost, raw_cost, call_count,
		ttft_total_ms, ttft_count, ttft_max_ms, ttft_hist
		FROM usage_stats WHERE bucket_time = $1 AND group_id = 7`, bucket).
		Scan(&req, &ec, &cost, &raw, &call, &ttftS, &ttftC, &ttftM, &gotHist)
	require.NoError(t, err)
	require.Equal(t, int64(6), req)
	require.Equal(t, int64(123), cost)
	require.Equal(t, int64(456), raw, "raw_cost 直查回读")
	require.Equal(t, int64(4), call, "call_count 直查回读")
	require.Equal(t, int64(240), ttftS)
	require.Equal(t, int64(6), ttftC)
	require.Equal(t, int64(150), ttftM)
	require.Equal(t, hist, gotHist, "bigint[] 直方图 round-trip（ent 数组列 carve-out）")

	// entity 回读
	var eReq, eCall int64
	err = pool.QueryRow(ctx, `SELECT request_count, call_count FROM usage_entity_stats
		WHERE bucket_time = $1 AND entity_type = 'account' AND entity_id = 55`, bucket).Scan(&eReq, &eCall)
	require.NoError(t, err)
	require.Equal(t, int64(6), eReq)
	require.Equal(t, int64(4), eCall)
}

// TestPGStatsRawCostRoundtrip raw_cost 全链路 roundtrip（spec 2026-08-19 口径
// 在 v2 维度下的回归）：免费组行 cost=0 raw>0 不丢；v2 同维度合并后单桶累计；
// SummarizeStats/ScanStatsDays（overview 幸存面）raw 正确；重算幂等。
func TestPGStatsRawCostRoundtrip(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(time.Hour))

	// 免费组行：cost=0 raw>0（"实际消耗"口径不丢）；付费行 + abort 行
	logs := []*domain.UsageLog{
		usageLogRow("rc1", h.Add(time.Minute), domain.ErrNone, 1, 0, 42, 0, "gpt-4o", 10, 20, 0, 0, 0, 500, 1, nil),   // 免费组
		usageLogRow("rc2", h.Add(2*time.Minute), domain.ErrNone, 1, 0, 42, 0, "gpt-4o", 5, 5, 0, 0, 100, 250, 0, nil), // 付费行
		usageLogRow("rc3", h.Add(3*time.Minute), domain.ErrAbort, 1, 0, 42, 0, "gpt-4o", 1, 1, 0, 0, 40, 90, 1, nil),
	}
	errLogs := []*domain.UsageLog{
		errLogRow("rc4", h.Add(4*time.Minute), domain.Err5xx, 1, 0, 42, 0, "gpt-4o"),        // 与 abort 同维度合并
		errLogRow("rc5", h.Add(5*time.Minute), domain.ErrNetwork, 2, 0, 9, 0, "claude-3-5"), // 纯 err 桶
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, errLogs))

	rows, _, detail, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(5), detail)

	// v2 同维度合并：group1/gpt-4o 的 none(rc1,rc2)+abort(rc3)+errlog(rc4) 单桶；
	// group2/claude-3-5 纯 errlog 单桶
	require.Len(t, rows, 2)
	byGroup := map[int64]*domain.StatBucket{}
	for _, b := range rows {
		byGroup[b.GroupID] = b
	}
	b1 := byGroup[1]
	require.NotNil(t, b1)
	require.Equal(t, int64(4), b1.RequestCount, "none×2 + abort×1 + errlog×1 合并")
	require.Equal(t, int64(2), b1.ErrorCount, "abort + 5xx 进 ec（none 行不计）")
	require.Equal(t, int64(140), b1.Cost, "cost 合并（0+100+40+0）")
	require.Equal(t, int64(840), b1.RawCost, "raw 合并（500+250+90+0）——免费组不丢")
	b2 := byGroup[2]
	require.NotNil(t, b2)
	require.Equal(t, int64(1), b2.RequestCount)
	require.Equal(t, int64(0), b2.RawCost, "纯 err 桶 raw 恒 0")

	// 落盘 → overview 幸存读取面
	require.NoError(t, repos.Stats.AggregateRange(ctx, h, h.Add(time.Hour), h.Add(30*time.Minute), rows, nil))
	sum, err := repos.Stats.SummarizeStats(ctx, h, h.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Equal(t, int64(840), sum.RawCost, "summary SUM(raw_cost)（840+0）")
	days, err := repos.Stats.ScanStatsDays(ctx, h, h.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, int64(840), days[0].RawCost, "日桶 SUM(raw_cost)")

	// 重算幂等：二次 AggregateRange（同 rows）→ 桶值不变（DELETE+INSERT 覆盖）
	require.NoError(t, repos.Stats.AggregateRange(ctx, h, h.Add(time.Hour), h.Add(30*time.Minute), rows, nil))
	rows2, _, detail2, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, detail, detail2)
	require.Len(t, rows2, 2)
	for _, b := range rows2 {
		w := byGroup[b.GroupID]
		require.Equal(t, w.RawCost, b.RawCost, "重算幂等（raw_cost 不变）")
		require.Equal(t, w.Cost, b.Cost, "重算幂等（cost 不变）")
	}
}

// TestPGStatsAggV2CubeEquivalence cube 两源等价暴力对照（spec §7.1）：随机种子
// 灌 usage_logs+err_logs（含 usage_logs 侧 5xx 行验证 WHERE IN 排除、NULL 归属
// 列、多模型多组、跨三小时、TTFT 全档位含顶桶），LoadAggRange 结果 vs Go 侧
// 暴力逐行聚合，逐字段相等（含 abort 进 ec、hist 逐档）。
func TestPGStatsAggV2CubeEquivalence(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(3*time.Hour))

	rng := rand.New(rand.NewPCG(20260823, 42))
	ttftPool := []*int64{nil, ip(7), ip(49), ip(50), ip(99), ip(100), ip(399), ip(800),
		ip(1599), ip(1600), ip(6399), ip(12799), ip(12800), ip(30000)}
	models := []string{"gpt-4o", "claude-x", ""}
	groups := []int64{0, 1, 2}
	accounts := []int64{0, 11, 12}
	users := []int64{0, 21, 22}
	keys := []int64{0, 31, 32}
	etypes := []domain.ErrorType{domain.ErrNone, domain.ErrNone, domain.ErrNone, domain.ErrAbort, domain.Err5xx}

	logs := make([]*domain.UsageLog, 0, 120)
	for i := 0; i < 120; i++ {
		at := h.Add(time.Duration(rng.IntN(180)) * time.Minute)
		logs = append(logs, usageLogRow(
			"eq-u-"+itoa(i), at, etypes[rng.IntN(len(etypes))],
			groups[rng.IntN(len(groups))], accounts[rng.IntN(len(accounts))],
			users[rng.IntN(len(users))], keys[rng.IntN(len(keys))],
			models[rng.IntN(len(models))],
			rng.Int64N(1000), rng.Int64N(2000), rng.Int64N(50), rng.Int64N(50),
			rng.Int64N(9999), rng.Int64N(9999), rng.Int64N(3),
			ttftPool[rng.IntN(len(ttftPool))]))
	}
	errLogs := make([]*domain.UsageLog, 0, 40)
	errEtypes := []domain.ErrorType{domain.ErrNone, domain.Err4xx, domain.Err5xx, domain.ErrNetwork}
	for i := 0; i < 40; i++ {
		at := h.Add(time.Duration(rng.IntN(180)) * time.Minute)
		errLogs = append(errLogs, errLogRow(
			"eq-e-"+itoa(i), at, errEtypes[rng.IntN(len(errEtypes))],
			groups[rng.IntN(len(groups))], accounts[rng.IntN(len(accounts))],
			users[rng.IntN(len(users))], keys[rng.IntN(len(keys))],
			models[rng.IntN(len(models))]))
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, errLogs))

	cube, entity, detail, err := repos.Stats.LoadAggRange(ctx, h, h.Add(3*time.Hour))
	require.NoError(t, err)

	want := expCubeOf(logs, errLogs)
	require.Len(t, cube, len(want), "桶数与暴力参照一致")
	for _, b := range cube {
		k := expCubeKey{b.BucketTime.UTC(), b.GroupID, b.Model}
		w := want[k]
		require.NotNil(t, w, "离线 SQL 缺失参照桶 %+v", k)
		assertCubeEqual(t, b, w)
	}

	// 明细行数口径：usage(none|abort) 行数 + errlog(非 abort) 行数
	var wantDetail int64
	for _, l := range logs {
		if l.ErrorType == domain.ErrNone || l.ErrorType == domain.ErrAbort {
			wantDetail++
		}
	}
	for _, l := range errLogs {
		if l.ErrorType != domain.ErrAbort {
			wantDetail++
		}
	}
	require.Equal(t, wantDetail, detail, "消费明细行数 = cube 两查询 count(*) 合计")

	// entity 半区同批数据的等价断言归 TestPGStatsAggV2EntityEquivalence（此处
	// 仅防御性检查返回非 nil）。
	require.NotNil(t, entity)
}

// TestPGStatsAggV2EntityEquivalence entity 六查询等价（spec §7.2）：三实体类型
// × IS NOT NULL 丢弃语义 × model 拆分合并——随机种子同族数据，LoadAggRange 的
// entity 结果 vs Go 侧暴力逐行聚合逐字段相等；归属列为 NULL 的行不入卷积表
// （entity_id=0 行不存在）；同实体多模型拆分为多桶、跨模型合计与 cube 同源
// 口径一致。
func TestPGStatsAggV2EntityEquivalence(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(3*time.Hour))

	rng := rand.New(rand.NewPCG(20260823, 77))
	ttftPool := []*int64{nil, ip(30), ip(60), ip(120), ip(900), ip(20000)}
	models := []string{"gpt-4o", "claude-x", ""}
	ids := []int64{0, 101, 102, 103} // 0 = NULL（丢弃探针）

	logs := make([]*domain.UsageLog, 0, 100)
	for i := 0; i < 100; i++ {
		at := h.Add(time.Duration(rng.IntN(180)) * time.Minute)
		logs = append(logs, usageLogRow(
			"en-u-"+itoa(i), at,
			[]domain.ErrorType{domain.ErrNone, domain.ErrAbort}[rng.IntN(2)],
			int64(rng.IntN(3)), ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))],
			ids[rng.IntN(len(ids))], models[rng.IntN(len(models))],
			rng.Int64N(500), rng.Int64N(500), rng.Int64N(20), rng.Int64N(20),
			rng.Int64N(5000), rng.Int64N(5000), rng.Int64N(2),
			ttftPool[rng.IntN(len(ttftPool))]))
	}
	errLogs := make([]*domain.UsageLog, 0, 30)
	for i := 0; i < 30; i++ {
		at := h.Add(time.Duration(rng.IntN(180)) * time.Minute)
		errLogs = append(errLogs, errLogRow(
			"en-e-"+itoa(i), at,
			[]domain.ErrorType{domain.ErrNone, domain.Err4xx, domain.ErrNetwork}[rng.IntN(3)],
			int64(rng.IntN(3)), ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))],
			ids[rng.IntN(len(ids))], models[rng.IntN(len(models))]))
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, errLogs))

	_, entities, _, err := repos.Stats.LoadAggRange(ctx, h, h.Add(3*time.Hour))
	require.NoError(t, err)

	for _, etype := range []string{"account", "user", "key"} {
		want := expEntityOf(etype, logs, errLogs)
		got := map[expEntityKey]*domain.EntityStatBucket{}
		for _, b := range entities {
			if b.EntityType == etype {
				got[expEntityKey{b.BucketTime.UTC(), b.EntityType, b.EntityID, b.Model}] = b
				require.NotEqual(t, int64(0), b.EntityID, "IS NOT NULL 丢弃语义：entity_id=0 行不得存在（%s）", etype)
			}
		}
		require.Len(t, got, len(want), "%s 桶数与暴力参照一致", etype)
		for k, w := range want {
			b := got[k]
			require.NotNil(t, b, "%s 缺失参照桶 %+v", etype, k)
			assertEntityEqual(t, b, w)
		}
	}

	// model 拆分合并：account 视角同 (hour, entity) 跨模型桶的 request_count
	// 合计 = 该实体全模型合计（拆分不丢量）。
	perEntity := map[int64]int64{}
	for _, b := range entities {
		if b.EntityType == "account" {
			perEntity[b.EntityID] += b.RequestCount
		}
	}
	for id, total := range perEntity {
		var wantTotal int64
		for k, w := range expEntityOf("account", logs, errLogs) {
			if k.eid == id {
				wantTotal += w.req
			}
		}
		require.Equal(t, wantTotal, total, "account %d 跨模型拆分合计守恒", id)
	}
}

// TestPGStatsAbortSplitNoDoubleCount abort 防双计（P1-B 回归）：abort 行只经
// usage_logs 全字段计一次；err_logs 的 abort 行（豁免通道实际不写，防御性）
// 被 WHERE error_type <> 'abort' 排除；v2 同维度 none+abort+errlog 合并单桶时
// ec 只计错误行。
func TestPGStatsAbortSplitNoDoubleCount(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 14, 11, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(time.Hour))

	ttft := func(v int64) *int64 { return &v }
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		usageLogRow("a1", h.Add(time.Minute), domain.ErrAbort, 3, 0, 42, 0, "gpt-4o", 7, 8, 1, 1, 60, 120, 5, ttft(200)),
		usageLogRow("a2", h.Add(2*time.Minute), domain.ErrNone, 3, 0, 42, 0, "gpt-4o", 1, 1, 0, 0, 10, 20, 0, nil),
	}))
	// err_logs 插入一条 abort 行（防御性双计探针——实际豁免通道不写，但 SQL
	// 语义必须排除）
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{
		{RequestID: "x-abort", GroupID: 3, UserID: 42, Model: "gpt-4o",
			Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrAbort, CreatedAt: h.Add(3 * time.Minute)},
	}))

	rows, _, detail, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(2), detail, "err_logs abort 行被排除（usage none+abort 两行）")
	require.Len(t, rows, 1, "同维度 v2 合并单桶")
	b := rows[0]
	require.Equal(t, int64(2), b.RequestCount, "abort(1) + none(1)，errlog abort 不计")
	require.Equal(t, int64(1), b.ErrorCount, "仅 abort 行进 ec")
	require.Equal(t, int64(5), b.CallCount, "abort 行全字段（call_count）")
	require.Equal(t, int64(17), b.TotalTokens, "abort+none tokens 合并（15+2）")
	require.Equal(t, int64(70), b.Cost, "abort+none cost 合并（60+10）")
	require.Equal(t, int64(140), b.RawCost, "abort+none raw 合并（120+20）")
	require.Equal(t, int64(200), b.TTFTMaxMS, "abort 行含 TTFT 也计入其桶")
	require.Equal(t, int64(1), b.TTFTCount)
	require.Equal(t, []int64{0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, b.TTFTHist, "200 在 [200,400) 档（第 4 档）")
}

// TestPGStatsAsyncAggregation 异步语义断言 + 两范围属性端到端（spec 测试节）：
// 真实时钟短周期 worker 驱动——插入明细后 usage_stats/usage_entity_stats 立即
// 无变化（异步）→ 周期后落库；同小时追加行 → 下周期整小时桶重建（部分小时桶
// 跨周期不截断，P1-A）；再周期重放 → 桶值不变（幂等）。
func TestPGStatsAsyncAggregation(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Now().UTC().Truncate(time.Hour)

	// cycle 1 窗口行（h+1m..h+8m；created_at 固定过去时刻，worker 滞后即可消费）
	logs1 := make([]*domain.UsageLog, 0, 8)
	for i := 0; i < 8; i++ {
		logs1 = append(logs1, usageLogRow("c1-"+itoa(i), h.Add(time.Duration(i+1)*time.Minute),
			domain.ErrNone, 1, 0, 42, 0, "gpt-4o", 1, 1, 0, 0, 0, 0, 0, nil))
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs1))

	// 异步语义：worker 周期前两表无变化
	sum, err := repos.Stats.SummarizeStats(ctx, h, h.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Zero(t, sum.Requests, "Record 后 usage_stats 无变化（请求路径零统计投递）")
	require.Zero(t, pgCount(t, pgTestPool(t), `SELECT COUNT(*) FROM usage_entity_stats`),
		"Record 后 usage_entity_stats 无变化")

	w := usage.NewStatsAgg(usage.StatsAggConfig{Interval: 150 * time.Millisecond, Lag: 50 * time.Millisecond}, repos.Stats, nil)
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() { require.NoError(t, w.Close(context.Background())) })

	// worker 周期后落库（轮询收敛——无固定 sleep）
	waitForSummaryReq(t, repos, h, 8, "cycle 1 窗口行已入桶")

	// 同小时追加行（h+25m..h+27m）→ 下周期整小时桶重建（部分小时桶不截断）
	logs2 := make([]*domain.UsageLog, 0, 3)
	for i := 0; i < 3; i++ {
		logs2 = append(logs2, usageLogRow("c2-"+itoa(i), h.Add(time.Duration(25+i)*time.Minute),
			domain.ErrNone, 1, 0, 42, 0, "gpt-4o", 1, 1, 0, 0, 0, 0, 0, nil))
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs2))
	waitForSummaryReq(t, repos, h, 11, "部分小时桶跨周期累积不截断（8+3 全量重建）")

	// 幂等重放：无新行再周期 → 桶值不变（不双计）
	waitForSummaryReq(t, repos, h, 11, "重放同范围结果一致")
	sum, err = repos.Stats.SummarizeStats(ctx, h, h.Add(time.Hour), 0)
	require.NoError(t, err)
	require.Equal(t, int64(22), sum.TotalTokens, "桶由全量行重建（首批 token 不丢）")
	// entity 半端同步落库（同批源行的 user=42 卷积——hour×model 一桶）
	require.Equal(t, int64(1), pgCount(t, pgTestPool(t),
		`SELECT COUNT(*) FROM usage_entity_stats WHERE entity_type = 'user' AND entity_id = 42 AND request_count = 11`),
		"user=42 实体桶同步重建")
}

// waitForSummaryReq 轮询 SummarizeStats 至区间 requests 达 want（真实时钟异步
// 收敛；5s 超时——channel 屏障不适配外部 worker 时钟，轮询窗口有界）。
func waitForSummaryReq(t *testing.T, repos *repository.Repository, h time.Time, want int64, msg string) {
	t.Helper()
	// 每次尝试独立超时：deadline 判定必须在非阻塞位置——若 DB 调用本身卡死
	// （瞬态拨号停滞等），无界调用永远走不到下面的 FailNow（真实事故：
	// 2026-08-23 本用例整测挂死 7 分钟）。
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		sum, err := repos.Stats.SummarizeStats(ctx, h, h.Add(time.Hour), 0)
		cancel()
		if err == nil && sum.Requests == want {
			return
		}
		if time.Now().After(deadline) {
			require.FailNowf(t, msg, "summary requests=%d err=%v want=%d", sum.Requests, err, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPGStatsAggDualTableTxRollback 事务中途注入失败（spec §7.3）→ 双表均回滚
// + watermark 不动（幂等铁律回归）：坏行 bucket_time 落在无分区区间（2099——
// bootstrap 只预建当日/明日分区；INSERT 撞 "no partition of relation found"）
// → 整事务回滚。两个方向都验：坏 cube 行拖垮好 entity 行、坏 entity 行拖垮
// 好 cube 行（双表原子性）；修复后重跑成功。
func TestPGStatsAggDualTableTxRollback(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	bucket := time.Now().UTC().Truncate(time.Hour)
	wmTo := bucket.Add(15 * time.Minute)
	far := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

	goodCube := &domain.StatBucket{BucketTime: bucket, GroupID: 1, Model: "m",
		RequestCount: 1, TTFTHist: make([]int64, 10)}
	badCube := &domain.StatBucket{BucketTime: far, GroupID: 2, Model: "m",
		RequestCount: 1, TTFTHist: make([]int64, 10)}
	goodEntity := &domain.EntityStatBucket{BucketTime: bucket, EntityType: "user",
		EntityID: 42, Model: "m", RequestCount: 1}
	badEntity := &domain.EntityStatBucket{BucketTime: far, EntityType: "user",
		EntityID: 43, Model: "m", RequestCount: 1}

	// 方向 A：坏 cube 行 → entity 也必须回滚
	err := repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), wmTo,
		[]*domain.StatBucket{goodCube, badCube}, []*domain.EntityStatBucket{goodEntity})
	require.Error(t, err, "无分区 bucket_time 使 cube INSERT 失败（整事务回滚）")
	require.Zero(t, pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats`), "cube 无部分行")
	require.Zero(t, pgCount(t, pool, `SELECT COUNT(*) FROM usage_entity_stats`), "entity 随同回滚")
	gotWm, err := repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, gotWm.IsZero(), "事务回滚 → 游标不动")

	// 方向 B：坏 entity 行 → cube 也必须回滚
	err = repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), wmTo,
		[]*domain.StatBucket{goodCube}, []*domain.EntityStatBucket{goodEntity, badEntity})
	require.Error(t, err, "无分区 bucket_time 使 entity INSERT 失败（整事务回滚）")
	require.Zero(t, pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats`), "cube 随同回滚")
	require.Zero(t, pgCount(t, pool, `SELECT COUNT(*) FROM usage_entity_stats`), "entity 无部分行")
	gotWm, err = repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, gotWm.IsZero(), "事务回滚 → 游标不动")

	// 修复后重跑成功（重算恢复不双计）
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), wmTo,
		[]*domain.StatBucket{goodCube}, []*domain.EntityStatBucket{goodEntity}))
	gotWm, err = repos.Stats.LoadStatsAggWatermark(ctx)
	require.NoError(t, err)
	require.True(t, gotWm.Equal(wmTo), "修复后重跑成功")
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats`))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_entity_stats`))
}

// TestPGStatsAggIdempotentDoubleRun 幂等双跑（spec §7.11）：同范围 AggregateRange
// 连跑两遍，双表行集与 watermark 恒等（覆盖语义——DELETE+INSERT 重放一致）。
func TestPGStatsAggIdempotentDoubleRun(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	h := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(time.Hour))

	logs := []*domain.UsageLog{
		usageLogRow("id1", h.Add(time.Minute), domain.ErrNone, 1, 11, 21, 31, "gpt-4o", 1, 2, 0, 0, 30, 60, 1, nil),
		usageLogRow("id2", h.Add(2*time.Minute), domain.ErrAbort, 1, 11, 21, 31, "gpt-4o", 3, 4, 0, 0, 40, 80, 1, nil),
		usageLogRow("id3", h.Add(3*time.Minute), domain.ErrNone, 2, 12, 22, 32, "claude-x", 5, 6, 0, 0, 50, 100, 0, nil),
	}
	errLogs := []*domain.UsageLog{
		errLogRow("id4", h.Add(4*time.Minute), domain.Err5xx, 1, 11, 21, 31, "gpt-4o"),
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, errLogs))

	snapshot := func() (cubeRows, cubeReq, entityRows, entityReq int64, wm time.Time) {
		cubeRows = pgCount(t, pool, `SELECT COUNT(*) FROM usage_stats`)
		require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(sum(request_count), 0) FROM usage_stats`).Scan(&cubeReq))
		entityRows = pgCount(t, pool, `SELECT COUNT(*) FROM usage_entity_stats`)
		require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(sum(request_count), 0) FROM usage_entity_stats`).Scan(&entityReq))
		wm, err := repos.Stats.LoadStatsAggWatermark(ctx)
		require.NoError(t, err)
		return cubeRows, cubeReq, entityRows, entityReq, wm
	}

	run := func() {
		cube, entity, _, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
		require.NoError(t, err)
		require.NoError(t, repos.Stats.AggregateRange(ctx, h, h.Add(time.Hour), h.Add(30*time.Minute), cube, entity))
	}

	run()
	a1, a2, a3, a4, aWm := snapshot()
	run()
	b1, b2, b3, b4, bWm := snapshot()
	require.Equal(t, a1, b1, "双跑 cube 行集恒等")
	require.Equal(t, a2, b2, "双跑 cube request_count 恒等")
	require.Equal(t, a3, b3, "双跑 entity 行集恒等")
	require.Equal(t, a4, b4, "双跑 entity request_count 恒等")
	require.True(t, aWm.Equal(bWm), "双跑 watermark 恒等")
	// 双源合并后的期望形态：cube 2 桶（group1 合并 abort+errlog；group2 纯成功）、
	// entity 6 桶（3 类型 × 各自去重实体）
	require.Equal(t, int64(2), a1)
	require.Equal(t, int64(6), a3)
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

// TestPGStatsSummarizeTTFT array_agg 直方图合并 + Go 侧插值（overview 幸存面
// 回归）：多行同区间直方图 → SummarizeStats 合并后 p50/p90/p95/p99/avg/max
// 数值断言 + call_count/ttft 字段。
func TestPGStatsSummarizeTTFT(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	bucket := time.Now().UTC().Truncate(time.Hour)

	// 已知分布：每行 6 样本 <50 + 4 样本 [50,100)（两行合并后 = [12,8,0...0]）
	// **合并先于插值**（array_agg 逐元素合并后 Go 侧插值）：N=10，merged hist
	// [12,8]：
	//   p50 rank=5 → 桶0 → 0 + 5/12×50 ≈ 20；p90 rank=9 → 0 + 9/12×50 ≈ 37；
	//   p95/p99 rank=10 → 0 + 10/12×50 ≈ 41。
	hist := []int64{6, 4, 0, 0, 0, 0, 0, 0, 0, 0}
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute),
		[]*domain.StatBucket{
			{BucketTime: bucket, GroupID: 1, Model: "m", RequestCount: 5, CallCount: 3,
				Cost: 100, RawCost: 300, TTFTTotalMS: 300, TTFTCount: 5, TTFTMaxMS: 90, TTFTHist: hist},
			{BucketTime: bucket, GroupID: 2, Model: "m", RequestCount: 5, CallCount: 2,
				Cost: 50, RawCost: 150, TTFTTotalMS: 180, TTFTCount: 5, TTFTMaxMS: 60, TTFTHist: hist},
		}, nil))

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

// TestPGStatsSummarizeNoData 空区间：summary 全零 + 空直方图（无 42703/扫描
// 错误——COALESCE 空数组回落）。
func TestPGStatsSummarizeNoData(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	bucket := time.Now().UTC().Truncate(time.Hour)
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

// —— 小工具 ——

// ip int64 指针字面量（fixture 用）。
func ip(v int64) *int64 { return &v }

// itoa 微型整数转字符串（fixture request_id 后缀；避免引 fmt 拉全格式化栈）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
