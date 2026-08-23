// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
)

// /api/admin/overview + /api/admin/users-top 真实 PG 测试（spec 2026-08-14）：
// summary/trend 走 usage_stats 真实分区表 SQL 侧聚合（COPY 两阶段种子）；
// resources/email 走真实表；accounts/err_top 走 stub 调度器快照（与
// ListAccountViews 同源接口，确定性断言）；alerts/在途走 OpsOptions 注入面。
// 基座同 handler_pricing_test：独立 schema + NewWithPG（pool 注入——聚合
// 查询与 Upsert 均需池）+ 分区 bootstrap 后预建种子日期区间分区。

// handlerOverviewPGTestSchema 本文件 PG 测试专用 schema。
const handlerOverviewPGTestSchema = "handler_overview_test"

// overviewPGTestDB 打开真实 PG（独立 schema）+ 分区基座（三表 bootstrap +
// 种子区间日分区预建），返回仓库。
func overviewPGTestDB(t *testing.T, seedFrom, seedUntil time.Time) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + handlerOverviewPGTestSchema
	} else {
		dsn += "?search_path=" + handlerOverviewPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+handlerOverviewPGTestSchema+` CASCADE; CREATE SCHEMA `+handlerOverviewPGTestSchema+`;`)
	require.NoError(t, err)
	repos, err := repository.NewWithPG(t.Context(), entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageStatsPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureUsageStatsPartitions(ctx, seedFrom, seedUntil))
	// v2.2：AggregateRange 双表事务连带写 usage_entity_stats——bootstrap 必须同步
	// 建表+分区，否则聚合落盘撞 42P01。
	require.NoError(t, repos.EnsureUsageEntityStatsPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureUsageEntityStatsPartitions(ctx, seedFrom, seedUntil))
	return repos
}

// overviewBucket 构造小时桶（UTC 对齐；cost 毫分；v2 瘦身后仅 group/model 维度）。
func overviewBucket(bt time.Time, groupID int64, req, errs, in, out, total, cache, cost int64) *domain.StatBucket {
	return &domain.StatBucket{
		BucketTime: bt, GroupID: groupID,
		Model: "gpt-4o",
		RequestCount: req, ErrorCount: errs, InputTokens: in, OutputTokens: out,
		TotalTokens: total, CacheReadTokens: cache, CacheCreationTokens: 0,
		Cost: cost,
	}
}

// overviewSeedBuckets 逐桶 AggregateRange 种子（spec 2026-08-14 覆盖语义单写
// 者：usage_stats 只由聚合写入面落盘，旧 Upsert 已删）：DELETE 范围 = 单桶小时
// 区间，watermark 推进 bt+30m——等价旧一次性种子语义。
func overviewSeedBuckets(t *testing.T, repos *repository.Repository, buckets ...*domain.StatBucket) {
	t.Helper()
	for _, b := range buckets {
		require.NoError(t, repos.Stats.AggregateRange(context.Background(),
			b.BucketTime, b.BucketTime.Add(time.Hour), b.BucketTime.Add(30*time.Minute),
			[]*domain.StatBucket{b}, nil))
	}
}

// stubSched 可配置运行时快照（overview accounts/err_top 确定性断言）。
type stubSched struct{ rt []scheduler.AccountRuntime }

func (s stubSched) Runtime(id int64) (scheduler.RuntimeInfo, bool) {
	return scheduler.RuntimeInfo{}, false
}
func (s stubSched) Runtimes() []scheduler.AccountRuntime { return s.rt }

// countingStore 统计聚合入口（SQL 聚合/资源计数/email 查询）——缓存命中断言
// 用：TTL 内二次调用应零聚合（statAggs/emailCalls 不变）。
type countingStore struct {
	service.Store
	statAggs   atomic.Int64 // SummarizeStats + ScanStatsDays + CountOverviewResources
	emailCalls atomic.Int64 // ListUserEmails
}

func (c *countingStore) SummarizeStats(ctx context.Context, from, to time.Time, groupID int64) (*repository.StatSummary, error) {
	c.statAggs.Add(1)
	return c.Store.SummarizeStats(ctx, from, to, groupID)
}
func (c *countingStore) ScanStatsDays(ctx context.Context, from, to time.Time, groupID int64) ([]*repository.StatDayAgg, error) {
	c.statAggs.Add(1)
	return c.Store.ScanStatsDays(ctx, from, to, groupID)
}
func (c *countingStore) CountOverviewResources(ctx context.Context) (*repository.OverviewResourceCounts, error) {
	c.statAggs.Add(1)
	return c.Store.CountOverviewResources(ctx)
}
func (c *countingStore) ListUserEmails(ctx context.Context, ids []int64) (map[int64]string, error) {
	c.emailCalls.Add(1)
	return c.Store.ListUserEmails(ctx, ids)
}

// overviewPGRouter 真实 PG + 契约路由（admin token 中间件；count 可为 nil）。
func overviewPGRouter(t *testing.T, st service.Store, sched service.RuntimeProvider, opts OpsOptions) (*AdminAPI, func(method, path string) *httptest.ResponseRecorder) {
	t.Helper()
	svc := service.New(st, sched, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc, opts)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())
	return h, func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer admin-tok")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
}

// seedUsersPG 建 N 个用户（email u{i}@x.test）。
func seedUsersPG(t *testing.T, repos *repository.Repository, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		_, err := repos.Users.CreateUser(context.Background(), &domain.User{
			Email: fmt.Sprintf("u%d@x.test", i), PasswordHash: "h", Role: domain.RoleUser,
			Status: domain.UserStatusActive,
		})
		require.NoError(t, err)
	}
}

func TestPGOverviewSummaryAndTrend(t *testing.T) {
	now := time.Now().UTC()
	day0 := now.Truncate(24 * time.Hour)
	repos := overviewPGTestDB(t, day0.Add(-3*24*time.Hour), day0.Add(24*time.Hour))
	ctx := context.Background()
	// 今日 2 桶（03:00 / 05:00 UTC）+ 前两日各 1 桶（已知值断言聚合）。TTFT/
	// call_count：今日两桶各带 5 次调用 TTFT——合并直方图 {3,5,2,0,…} N=10：
	// avg=(250+450)/10=70、max=200、p50: rank5 → 桶1 内 50+2/5×50=70、
	// p90: rank9 → 桶2 内 100+1/2×100=150、p95/p99: rank10 → 200
	b1 := overviewBucket(day0.Add(3*time.Hour), 7, 10, 2, 100, 50, 150, 20, 100_000) // $1.00
	b1.RawCost = 250_000                                                                 // $2.50（raw 口径独立于 cost）
	b1.CallCount = 5
	b1.TTFTTotalMS = 250
	b1.TTFTCount = 5
	b1.TTFTMaxMS = 120
	b1.TTFTHist = []int64{3, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	b2 := overviewBucket(day0.Add(5*time.Hour), 7, 5, 1, 50, 25, 75, 10, 50_000) // $0.50
	b2.RawCost = 150_000                                                            // $1.50
	b2.CallCount = 5
	b2.TTFTTotalMS = 450
	b2.TTFTCount = 5
	b2.TTFTMaxMS = 200
	b2.TTFTHist = []int64{0, 3, 2, 0, 0, 0, 0, 0, 0, 0}
	b3 := overviewBucket(day0.Add(-21*time.Hour), 7, 20, 3, 200, 100, 300, 0, 200_000) // $2.00
	b3.RawCost = 400_000                                                                  // $4.00
	b4 := overviewBucket(day0.Add(-45*time.Hour), 7, 30, 0, 300, 150, 450, 0, 300_000)   // $3.00
	b4.RawCost = 600_000                                                                  // $6.00
	overviewSeedBuckets(t, repos, b1, b2, b3, b4)
	// 资源计数种子：1 模板 + 1 组 + 2 用户（软删模板不计）
	_, err := repos.Client.Template.Create().SetName("t1").SetBaseURL("https://upstream.example").
		SetSupportedFormats([]string{"openai-chat"}).SetModels([]string{"gpt-4o"}).
		SetFormatModels(map[string][]string{}).SetModelMapping(map[string]string{}).Save(ctx)
	require.NoError(t, err)
	_, err = repos.Client.Group.Create().SetName("g1").Save(ctx)
	require.NoError(t, err)
	seedUsersPG(t, repos, 2)

	sched := stubSched{rt: []scheduler.AccountRuntime{
		{AccountID: 1, Name: "account-01", Status: domain.StatusActive, MaxConcurrency: 20, Concurrency: 3, ErrRate: 0.05, ErrCount: 12},
		{AccountID: 2, Name: "account-02", Status: domain.StatusUnhealthy, MaxConcurrency: 10, Concurrency: 1, ErrRate: 0.02, ErrCount: 5},
		{AccountID: 3, Name: "account-03", Status: domain.Status429, MaxConcurrency: 5, Concurrency: 0, ErrRate: 0, ErrCount: 0},
		{AccountID: 4, Name: "account-04", Status: domain.StatusDisabled, MaxConcurrency: 8, Concurrency: 0, ErrRate: 0.09, ErrCount: 3},
	}}
	_, router := overviewPGRouter(t, repos, sched, OpsOptions{
		BillingAlerts: func() BillingAlerts { return BillingAlerts{Pending: 123, PendingWaterline: 50000, Warned: false} },
	})

	w := router("GET", "/api/admin/overview")
	require.Equal(t, http.StatusOK, w.Code)
	var resp OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// summary：今日两桶聚合（毫分 → USD /1e5；err_rate = errors/requests）
	require.Equal(t, int64(15), resp.Summary.Requests)
	require.Equal(t, int64(3), resp.Summary.Errors)
	require.InDelta(t, 0.2, resp.Summary.ErrRate, 1e-9)
	require.InDelta(t, 1.5, resp.Summary.CostUsd, 1e-9) // (100000+50000)/1e5
	require.InDelta(t, 4.0, resp.Summary.RawCostUsd, 1e-9, "(250000+150000)/1e5——raw 独立口径")
	require.Equal(t, int64(150), resp.Summary.InputTokens)
	require.Equal(t, int64(75), resp.Summary.OutputTokens)
	require.Equal(t, int64(225), resp.Summary.TotalTokens)
	require.Equal(t, int64(30), resp.Summary.CacheReadTokens)
	// TTFT/call_count（spec 2026-08-14 §5）：均值查询侧 Go 除；分位 = 直方图
	// 桶内线性插值（nearest-rank）
	require.Equal(t, int64(10), resp.Summary.CallCount, "call_count = 按次调用合计")
	require.InDelta(t, 70.0, resp.Summary.TtftAvgMs, 1e-9, "(250+450)/10")
	require.Equal(t, int64(200), resp.Summary.TtftMaxMs)
	require.Equal(t, int64(70), resp.Summary.TtftP50Ms)
	require.Equal(t, int64(150), resp.Summary.TtftP90Ms)
	require.Equal(t, int64(200), resp.Summary.TtftP95Ms)
	require.Equal(t, int64(200), resp.Summary.TtftP99Ms)

	// trend：3 个日桶（含今日），升序，值 = 各日聚合（Date 类型序列化 YYYY-MM-DD）
	require.Len(t, resp.Trend, 3)
	require.Equal(t, day0.Add(-2*24*time.Hour).Format("2006-01-02"), resp.Trend[0].Date.Format("2006-01-02"))
	require.Equal(t, int64(30), resp.Trend[0].Requests)
	require.Equal(t, int64(0), resp.Trend[0].Errors)
	require.InDelta(t, 3.0, resp.Trend[0].CostUsd, 1e-9)
	require.InDelta(t, 6.0, resp.Trend[0].RawCostUsd, 1e-9, "日桶 raw 聚合（600000/1e5）")
	require.Equal(t, int64(450), resp.Trend[0].Tokens)
	require.Zero(t, resp.Trend[0].CallCount, "无 TTFT 数据日 call_count 0")
	require.Zero(t, resp.Trend[0].TtftAvgMs)
	require.Equal(t, day0.Add(-1*24*time.Hour).Format("2006-01-02"), resp.Trend[1].Date.Format("2006-01-02"))
	require.Equal(t, int64(20), resp.Trend[1].Requests)
	require.Equal(t, int64(3), resp.Trend[1].Errors)
	require.InDelta(t, 2.0, resp.Trend[1].CostUsd, 1e-9)
	require.InDelta(t, 4.0, resp.Trend[1].RawCostUsd, 1e-9, "日桶 raw 聚合（400000/1e5）")
	require.Equal(t, day0.Format("2006-01-02"), resp.Trend[2].Date.Format("2006-01-02"))
	require.Equal(t, int64(15), resp.Trend[2].Requests)
	require.InDelta(t, 1.5, resp.Trend[2].CostUsd, 1e-9)
	require.InDelta(t, 4.0, resp.Trend[2].RawCostUsd, 1e-9, "今日两桶 raw 合并（250000+150000）/1e5")
	require.Equal(t, int64(225), resp.Trend[2].Tokens)
	require.Equal(t, int64(10), resp.Trend[2].CallCount, "今日趋势 call_count")
	require.InDelta(t, 70.0, resp.Trend[2].TtftAvgMs, 1e-9)

	// accounts：调度器快照分布 + 水位（concurrency / max_concurrency 合计）
	require.Equal(t, 1, resp.Accounts.Active)
	require.Equal(t, 1, resp.Accounts.Unhealthy)
	require.Equal(t, 1, resp.Accounts.N429)
	require.Equal(t, 1, resp.Accounts.Disabled)
	require.Equal(t, int64(4), resp.Accounts.Concurrency)
	require.Equal(t, 43, resp.Accounts.MaxConcurrency)

	// err_top：账号维度 err_rate 降序 Top5（0 率账号排除；name = 账号名）
	require.Len(t, resp.ErrTop, 3)
	require.Equal(t, "account-04", resp.ErrTop[0].Name)
	require.InDelta(t, 0.09, resp.ErrTop[0].ErrRate, 1e-9)
	require.Equal(t, 3, resp.ErrTop[0].ErrCount)
	require.Equal(t, "account-01", resp.ErrTop[1].Name)
	require.Equal(t, "account-02", resp.ErrTop[2].Name)

	// resources：模板/组/用户计数（软删模板不计——本测试无软删）
	require.Equal(t, 1, resp.Resources.Templates)
	require.Equal(t, 1, resp.Resources.Groups)
	require.Equal(t, 2, resp.Resources.Users)

	// alerts：billing 注入面直出
	require.Equal(t, int64(123), resp.Alerts.BillingPending)
	require.Equal(t, int64(50000), resp.Alerts.BillingPendingWaterline)
	require.False(t, resp.Alerts.BillingWarned)
}

func TestPGOverviewTrendDaysParamAndClamp(t *testing.T) {
	now := time.Now().UTC()
	day0 := now.Truncate(24 * time.Hour)
	repos := overviewPGTestDB(t, day0.Add(-3*24*time.Hour), day0.Add(24*time.Hour))
	overviewSeedBuckets(t, repos,
		overviewBucket(day0.Add(3*time.Hour), 7, 10, 2, 100, 50, 150, 20, 100_000),
		overviewBucket(day0.Add(-21*time.Hour), 7, 20, 3, 200, 100, 300, 0, 200_000),
		overviewBucket(day0.Add(-45*time.Hour), 7, 30, 0, 300, 150, 450, 0, 300_000),
	)
	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{})

	// 缺省 days=7 → 3 桶全含
	w := router("GET", "/api/admin/overview")
	var resp OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Trend, 3)

	// days=2 → 仅近两日（今日 + 昨日）
	w = router("GET", "/api/admin/overview?days=2")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Trend, 2)
	require.Equal(t, day0.Add(-24*time.Hour).Format("2006-01-02"), resp.Trend[0].Date.Format("2006-01-02"))

	// days=999 → 钳制 30（数据仅 3 日，不报错）
	w = router("GET", "/api/admin/overview?days=999")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Trend, 3)

	// days=0（非法值）→ 回落缺省 7（同 GetStats 非法 granularity 回落 day）
	w = router("GET", "/api/admin/overview?days=0")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Trend, 3)
}

func TestPGOverviewGroupFilter(t *testing.T) {
	now := time.Now().UTC()
	day0 := now.Truncate(24 * time.Hour)
	repos := overviewPGTestDB(t, day0.Add(-24*time.Hour), day0.Add(24*time.Hour))
	overviewSeedBuckets(t, repos,
		overviewBucket(day0.Add(3*time.Hour), 7, 10, 1, 100, 50, 150, 0, 100_000),
		overviewBucket(day0.Add(4*time.Hour), 8, 99, 9, 990, 990, 1980, 0, 900_000),
		overviewBucket(day0.Add(-21*time.Hour), 8, 5, 0, 50, 50, 100, 0, 50_000),
	)
	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{})

	// 全局：今日 = 10+99
	w := router("GET", "/api/admin/overview")
	var resp OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, int64(109), resp.Summary.Requests)
	require.Len(t, resp.Trend, 2)

	// group_id=7：仅组 7（今日 10；趋势仅今日桶）
	w = router("GET", "/api/admin/overview?group_id=7")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, int64(10), resp.Summary.Requests)
	require.Len(t, resp.Trend, 1)

	// group_id=8：仅组 8（今日 99；趋势 2 桶——组 8 有昨日+今日）
	w = router("GET", "/api/admin/overview?group_id=8")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, int64(99), resp.Summary.Requests)
	require.Len(t, resp.Trend, 2)

	// group_id=404：无数据 → 全零（字段恒存在）
	w = router("GET", "/api/admin/overview?group_id=404")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, int64(0), resp.Summary.Requests)
	require.InDelta(t, 0.0, resp.Summary.CostUsd, 1e-9)
	require.InDelta(t, 0.0, resp.Summary.RawCostUsd, 1e-9, "空区间 raw_cost_usd 恒 0（COALESCE 路径）")
	require.Empty(t, resp.Trend)
}

func TestPGOverviewCacheHit(t *testing.T) {
	now := time.Now().UTC()
	day0 := now.Truncate(24 * time.Hour)
	repos := overviewPGTestDB(t, day0.Add(-24*time.Hour), day0.Add(24*time.Hour))
	overviewSeedBuckets(t, repos,
		overviewBucket(day0.Add(3*time.Hour), 7, 10, 1, 100, 50, 150, 0, 100_000),
	)
	cnt := &countingStore{Store: repos}
	_, router := overviewPGRouter(t, cnt, stubSched{}, OpsOptions{})

	w := router("GET", "/api/admin/overview")
	require.Equal(t, http.StatusOK, w.Code)
	var first OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	aggs := cnt.statAggs.Load()
	require.Equal(t, int64(3), aggs, "首次调用 = summary+trend+resources 三次聚合")

	// TTL 内二次调用：缓存命中 → 零聚合（statAggs 不变），响应一致
	w = router("GET", "/api/admin/overview")
	var second OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &second))
	require.Equal(t, aggs, cnt.statAggs.Load(), "TTL 内二次调用零聚合")
	require.Equal(t, first.Summary.Requests, second.Summary.Requests)

	// 不同参数 → 缓存键分离 → 重新聚合
	w = router("GET", "/api/admin/overview?days=2")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &second))
	require.Equal(t, aggs+3, cnt.statAggs.Load(), "days 参数入缓存键——重新聚合")
}

func TestPGOverviewCacheDayRollover(t *testing.T) {
	now := time.Now().UTC()
	day0 := now.Truncate(24 * time.Hour)
	repos := overviewPGTestDB(t, day0.Add(-24*time.Hour), day0.Add(48*time.Hour))
	overviewSeedBuckets(t, repos,
		overviewBucket(day0.Add(3*time.Hour), 7, 10, 1, 100, 50, 150, 0, 100_000),
	)
	cnt := &countingStore{Store: repos}
	h, router := overviewPGRouter(t, cnt, stubSched{}, OpsOptions{})

	// 时钟固定今日（23:59:59——午夜前最后时刻）：summary 含今日桶
	fakeNow := day0.Add(24*time.Hour - time.Second)
	h.now = func() time.Time { return fakeNow }
	w := router("GET", "/api/admin/overview")
	require.Equal(t, http.StatusOK, w.Code)
	var resp OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, int64(10), resp.Summary.Requests)
	aggs := cnt.statAggs.Load()

	// 跨 UTC 午夜滚转（时钟 +1 天 00:00:01）：缓存键含日界 → 未命中 → 重新
	// 聚合；summary"今日" = 新日界（无种子桶 → 0）
	fakeNow = day0.Add(24*time.Hour + time.Second)
	w = router("GET", "/api/admin/overview")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Greater(t, cnt.statAggs.Load(), aggs, "跨午夜键分离——重新聚合")
	require.Equal(t, int64(0), resp.Summary.Requests, "新日界今日无数据")
}

func TestPGUsersTopTopNOtherAndEmail(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC().Truncate(24*time.Hour), time.Now().UTC().Truncate(24*time.Hour).Add(24*time.Hour))
	seedUsersPG(t, repos, 4)                                    // u1..u4@x.test
	inflight := map[int64]int64{1: 12, 2: 9, 3: 5, 4: 2, 99: 1} // 99 = 不存在用户 → email 空串
	_, router := overviewPGRouter(t, repos, stubSched{}, OpsOptions{InFlightUsers: func() map[int64]int64 { return inflight }})

	// 缺省 top=20 → 全部在途（过滤 0），降序；other = 0
	w := router("GET", "/api/admin/users-top")
	require.Equal(t, http.StatusOK, w.Code)
	var resp UsersTopResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Users, 5)
	require.Equal(t, int64(12), resp.Users[0].Concurrency)
	require.Equal(t, "u1@x.test", resp.Users[0].Email)
	require.Equal(t, int64(9), resp.Users[1].Concurrency)
	require.Equal(t, "u2@x.test", resp.Users[1].Email)
	require.Equal(t, int64(1), resp.Users[4].Concurrency)
	require.Equal(t, int64(99), resp.Users[4].UserId)
	require.Equal(t, "", resp.Users[4].Email, "缺失用户 email 空串兜底")
	require.Equal(t, int64(0), resp.OtherConcurrency)

	// top=2 → Top2 + other = 其余在途合计（5+2+1）
	w = router("GET", "/api/admin/users-top?top=2")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Users, 2)
	require.Equal(t, int64(12), resp.Users[0].Concurrency)
	require.Equal(t, int64(9), resp.Users[1].Concurrency)
	require.Equal(t, int64(8), resp.OtherConcurrency)

	// top=500 → 钳制 100（数据 5 条全出）；top=0 → 回落缺省 20
	w = router("GET", "/api/admin/users-top?top=500")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Users, 5)
	w = router("GET", "/api/admin/users-top?top=0")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Users, 5)
}

func TestPGUsersTopCacheHitAndEmpty(t *testing.T) {
	repos := overviewPGTestDB(t, time.Now().UTC().Truncate(24*time.Hour), time.Now().UTC().Truncate(24*time.Hour).Add(24*time.Hour))
	seedUsersPG(t, repos, 3)
	inflight := map[int64]int64{1: 12, 2: 9, 3: 5}
	cnt := &countingStore{Store: repos}
	_, router := overviewPGRouter(t, cnt, stubSched{}, OpsOptions{InFlightUsers: func() map[int64]int64 { return inflight }})

	w := router("GET", "/api/admin/users-top")
	require.Equal(t, http.StatusOK, w.Code)
	var resp UsersTopResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Users, 3)
	require.Equal(t, int64(1), cnt.emailCalls.Load(), "首次调用一次 IN 查询")

	// TTL 内二次调用 → 缓存命中 → 零 email 查询（快照遍历 + IN 均摊薄）
	w = router("GET", "/api/admin/users-top")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, int64(1), cnt.emailCalls.Load(), "TTL 内二次调用零查询")
	require.Len(t, resp.Users, 3)

	// 无在途（全 0）→ 空列表 + other 0（0 过滤；未装配注入面 = 空）
	_, router2 := overviewPGRouter(t, cnt, stubSched{}, OpsOptions{})
	w = router2("GET", "/api/admin/users-top")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Empty(t, resp.Users)
	require.Equal(t, int64(0), resp.OtherConcurrency)
}

// TestPGOverviewTrendUTCDayBoundary 非 UTC 会话日界回归（评审 P2-1）：trend
// 日桶必须按 UTC 日界分组（与 summary 的 Go 侧 UTC 区间一致）。bug 形态：
// date_trunc('day', timestamptz) 按会话 TimeZone 截断——America/New_York
// （UTC-4/5）会话下 UTC 00:30 的桶会落入前一日桶（date 标签偏移一天）。
// 用独立 NY 会话池构造第二仓库跑真实趋势 SQL；先断言会话 TZ 生效（防
// options 参数静默失效 → 测试真空退化）。
func TestPGOverviewTrendUTCDayBoundary(t *testing.T) {
	now := time.Now().UTC()
	day0 := now.Truncate(24 * time.Hour)
	repos := overviewPGTestDB(t, day0.Add(-24*time.Hour), day0.Add(24*time.Hour))
	ctx := context.Background()
	overviewSeedBuckets(t, repos,
		overviewBucket(day0.Add(-30*time.Minute), 7, 3, 0, 30, 15, 45, 0, 30_000), // UTC 前一日 23:30
		overviewBucket(day0.Add(30*time.Minute), 7, 5, 1, 50, 25, 75, 0, 50_000),  // UTC 今日 00:30
	)

	dsn := os.Getenv("TEST_DATABASE_URL")
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + handlerOverviewPGTestSchema
	} else {
		dsn += "?search_path=" + handlerOverviewPGTestSchema
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
	nyRepos, err := repository.NewWithPG(t.Context(), entsql.OpenDB(dialect.Postgres, nyDB), false, nyPool)
	require.NoError(t, err)

	tr, err := nyRepos.Stats.ScanStatsDays(ctx, day0.Add(-24*time.Hour), day0.Add(24*time.Hour), 0)
	require.NoError(t, err)
	// 修复前：NY 会话下 UTC 00:30 桶落前一日 → 1 桶且日期错位；修复后 2 桶按 UTC 日界
	require.Len(t, tr, 2, "NY 会话下仍按 UTC 日界分桶")
	require.Equal(t, day0.Add(-24*time.Hour).Format("2006-01-02"), tr[0].Date.Format("2006-01-02"))
	require.Equal(t, int64(3), tr[0].Requests)
	require.Equal(t, day0.Format("2006-01-02"), tr[1].Date.Format("2006-01-02"))
	require.Equal(t, int64(5), tr[1].Requests)
}

// pgUserStatus 真实 PG 用户快照 provider（RequireJWT 快照校验；fail-closed）。
type pgUserStatus struct{ repos *repository.Repository }

func (p pgUserStatus) UserSnapshot(userID int64) (domain.UserSnapshot, bool) {
	u, err := p.repos.Users.GetUser(context.Background(), userID)
	if err != nil {
		return domain.UserSnapshot{}, false
	}
	return domain.UserSnapshot{Status: u.Status, Role: u.Role}, true
}
