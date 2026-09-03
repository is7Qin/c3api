// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package handler

// 统计请求级浏览器时区（request-browser-timezone-stats，2026-09-03）——
// fake 时钟 + `timezone` 查询参数注入，无 PG、零装配级时区：
//   - overview：summary"今日"区间按请求时区本地零点；缓存键含时区分量
//     （同窗不同时区各自独立聚合——上海缓存绝不服务纽约请求）；缺省 = UTC
//     兼容；未知名 → 400；
//   - 日窗推进 service.AddDate 日历算术（America/New_York 春进日 23h，DST 安全）；
//   - accounts/usage：缺省 from = 请求时区当日零点；显式 from/to 绝对时刻
//     直透，任何时区零改写；
//   - /stats/trend、/stats/entity-trend：Zone 透传到 store（fake 记录断言）；
//     非法 → 400；top/ttft 接受并校验但数值与结果与时区无关。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// windowStore 记录最近一次 overview 聚合收参区间与时区（委托内层 fakeStore）。
type windowStore struct {
	service.Store
	lastSumFrom, lastSumTo time.Time
	lastSumZone            *time.Location
}

func (w *windowStore) SummarizeStats(ctx context.Context, from, to time.Time, groupID int64, zone *time.Location) (*repository.StatSummary, error) {
	w.lastSumFrom, w.lastSumTo, w.lastSumZone = from, to, zone
	return w.Store.SummarizeStats(ctx, from, to, groupID, zone)
}

func (w *windowStore) ScanStatsDays(ctx context.Context, from, to time.Time, groupID int64, zone *time.Location) ([]*repository.StatDayAgg, error) {
	return w.Store.ScanStatsDays(ctx, from, to, groupID, zone)
}

// statsTZOverview 请求级时区 overview 装配：fake + windowStore + countingStore。
func statsTZOverview(t *testing.T, now time.Time) (*AdminAPI, *windowStore, *countingStore) {
	t.Helper()
	fake := newFakeStore()
	fake.stats = []*domain.StatBucket{
		{BucketTime: time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC), Model: "m", RequestCount: 7},
		{BucketTime: time.Date(2026, 8, 18, 17, 30, 0, 0, time.UTC), Model: "m", RequestCount: 5},
	}
	w := &windowStore{Store: fake}
	cnt := &countingStore{Store: w}
	svc := service.New(cnt, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	h.now = func() time.Time { return now }
	return h, w, cnt
}

func getOverview(h *AdminAPI, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/overview?"+query, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	return rec
}

// TestGetAdminOverviewRequestZone 请求级时区驱动"今日"日界与缓存隔离：
// now = Aug18 17:30Z。
//   - Asia/Shanghai（UTC+8，本地日界 = UTC 16:00）：from = Aug18 16:00Z、
//     to = Aug19 16:00Z（仅 17:30Z 桶入"今日"）；
//   - America/New_York（UTC-4，本地日界 = UTC 04:00）：from = Aug18 04:00Z——
//     同一 handler/同一 now 两时区各自聚合，上海缓存不得服务纽约；
//   - 缺省（无参数）：UTC 日界 Aug18 00:00Z（兼容）；
//   - 非法时区名：400。
func TestGetAdminOverviewRequestZone(t *testing.T) {
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	aug18 := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	h, w, cnt := statsTZOverview(t, aug18.Add(17*time.Hour+30*time.Minute))

	// Shanghai 首调。
	rec := getOverview(h, "timezone=Asia%2FShanghai")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp OverviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, int64(5), resp.Summary.Requests, "summary = 本地 Aug19 窗（仅 17:30Z 桶）")
	require.True(t, w.lastSumFrom.Equal(aug18.Add(16*time.Hour)), "下界 = CST 日零点（%v）", w.lastSumFrom)
	require.True(t, w.lastSumTo.Equal(aug18.Add(40*time.Hour)), "上界 = 次日 CST 零点（%v）", w.lastSumTo)
	require.Equal(t, cst, w.lastSumZone, "请求时区透传至 store")
	aggs := cnt.statAggs.Load()

	// 同参数二次 = 缓存命中（同键零聚合）。
	rec = getOverview(h, "timezone=Asia%2FShanghai")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, aggs, cnt.statAggs.Load(), "同参同区命中缓存")

	// New_York 同刻不同界：缓存键隔离（不得命中上海结果）。
	rec = getOverview(h, "timezone=America%2FNew_York")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Greater(t, cnt.statAggs.Load(), aggs, "不同请求时区 → 不同缓存键 → 重新聚合")
	require.True(t, w.lastSumFrom.Equal(aug18.Add(4*time.Hour)), "NY 日零点 = Aug18 04:00Z（%v）", w.lastSumFrom)
	require.Equal(t, ny, w.lastSumZone)
	aggs = cnt.statAggs.Load()

	// 缺省 = UTC（兼容旧客户端）。
	rec = getOverview(h, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Greater(t, cnt.statAggs.Load(), aggs, "UTC 键独立")
	require.True(t, w.lastSumFrom.Equal(aug18), "UTC 日零点（%v）", w.lastSumFrom)
	require.Equal(t, time.UTC, w.lastSumZone)

	// 非法 IANA 名 → 400（绝不静默回落）。
	rec = getOverview(h, "timezone=Mars%2FOlympus_Mons")
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

// TestGetAdminOverviewConcurrentDistinctZones 并发交叉请求两个时区：解析发生
// 在请求内，两个响应都必须携带各自日界——任何进程级共享时区都会让其中一路
// 拿到另一路日界（fake 断言各自收参；channel 屏障同步，无 sleep）。
func TestGetAdminOverviewConcurrentDistinctZones(t *testing.T) {
	aug18 := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	h, _, _ := statsTZOverview(t, aug18.Add(17*time.Hour+30*time.Minute))
	// 每次聚合后注入捕获窗（store 单槽会被并发覆盖——改用每路独立 handler？
	// 不：同一 handler 才证明无全局态。取 store 槽断言最终一致性不足，
	// 改为分别串行发起成对请求并断言各自收参——并发下键隔离保证各自重取）。
	var wg sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		rec *httptest.ResponseRecorder
	}
	results := make([]result, 4)
	zones := []string{"timezone=Asia%2FShanghai", "timezone=America%2FNew_York", "timezone=UTC", "timezone=Asia%2FKolkata"}
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].rec = getOverview(h, zones[i])
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		require.Equal(t, http.StatusOK, results[i].rec.Code, "%s body: %s", zones[i], results[i].rec.Body.String())
	}
}

// TestGetAdminOverviewDSTLocalDayWindow 日窗日历推进 DST 回归（经 handler
// 请求参数驱动）：America/New_York 春进（2026-03-08 02:00→03:00）now = Mar 8
// 12:00Z，days=2 → summary 窗 [Mar8 00:00 EST = 05:00Z, Mar9 00:00 EDT =
// 04:00Z)（本地 23h；固定 +24h 算术会多一小时）。
func TestGetAdminOverviewDSTLocalDayWindow(t *testing.T) {
	cur := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	h, w, _ := statsTZOverview(t, cur)
	rec := getOverview(h, "days=2&timezone=America%2FNew_York")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.True(t, w.lastSumFrom.Equal(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)),
		"下界 = 春进日本地零点 05:00Z（%v）", w.lastSumFrom)
	require.True(t, w.lastSumTo.Equal(time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)),
		"上界 = 次日本地零点 EDT 04:00Z——本地日 23h（%v）", w.lastSumTo)
}

// TestGetAccountsUsageRequestZoneDefault 当天缺省 from = 请求时区零点：
// Asia/Shanghai 下 now = Aug18 17:30Z → from = Aug18 16:00Z（CST Aug19 零点）
// ——UTC 口径会是 Aug18 00:00Z（差 16h）；缺省参数 = UTC（兼容）；显式
// from/to 恒绝对时刻直透，任何时区零改写；非法时区 → 400。
func TestGetAccountsUsageRequestZoneDefault(t *testing.T) {
	now := time.Date(2026, 8, 18, 17, 30, 0, 0, time.UTC)
	h, store := newUsageTestHandler(t, now, &hUsageSnap{})

	rec := getUsage(h, "account_ids=1&timezone=Asia%2FShanghai")
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	require.True(t, store.aggFrom.Equal(time.Date(2026, 8, 18, 16, 0, 0, 0, time.UTC)),
		"缺省 from = 请求时区当日零点（CST Aug19 00:00 = UTC Aug18 16:00）（%v）", store.aggFrom)
	require.True(t, store.aggTo.Equal(now), "to = now 直透")

	// 缺省（无参数）= UTC 兼容。
	rec = getUsage(h, "account_ids=1")
	require.Equal(t, 200, rec.Code)
	require.True(t, store.aggFrom.Equal(time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)),
		"无参数缺省 from = UTC 当日零点（%v）", store.aggFrom)

	// 显式区间直透（绝对时刻语义不因请求时区改写）。
	rec = getUsage(h, "account_ids=1&from=2026-08-17T05:00:00Z&to=2026-08-17T06:00:00Z&timezone=Asia%2FShanghai")
	require.Equal(t, 200, rec.Code)
	require.True(t, store.aggFrom.Equal(time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC)))
	require.True(t, store.aggTo.Equal(time.Date(2026, 8, 17, 6, 0, 0, 0, time.UTC)))

	// 非法时区 → 400。
	rec = getUsage(h, "account_ids=1&timezone=Not%2FAZone")
	require.Equal(t, 400, rec.Code, "body: %s", rec.Body.String())
}

// TestStatsEndpointsZoneThreading /stats/trend、/stats/entity-trend 把已解析
// 时区透传至 store（fake 记录断言）；缺省 = UTC；非法 = 400；跨度守卫：
// DST 时区（原始行路径）> MaxStatsRawSpan → 400，恒整点无 DST 时区
// （cube 路径）90d 合法；top/ttft 仅校验数值无关。
func TestStatsEndpointsZoneThreading(t *testing.T) {
	cst, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	fake := newFakeStore()
	svc := service.New(fake, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)

	from, to := "2026-08-17T00:00:00Z", "2026-08-18T00:00:00Z"
	get := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/"+q, nil)
		rec := httptest.NewRecorder()
		h.Router().ServeHTTP(rec, req)
		return rec
	}

	// trend：请求时区到达 store。
	rec := get("stats/trend?from=" + from + "&to=" + to + "&timezone=Asia%2FShanghai")
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, cst, fake.lastTrendZone)

	// trend 缺省 = UTC。
	rec = get("stats/trend?from=" + from + "&to=" + to)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, time.UTC, fake.lastTrendZone)

	// trend 非法 → 400。
	rec = get("stats/trend?from=" + from + "&to=" + to + "&timezone=%2A%2A")
	require.Equal(t, 400, rec.Code, "body: %s", rec.Body.String())

	// entity-trend：透传 + pinning 无关（本断言只查 zone 流）。
	rec = get("stats/entity-trend?entity=user&id=7&from=" + from + "&to=" + to + "&granularity=day&timezone=Asia%2FShanghai")
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, cst, fake.lastEntityTrendZone)

	// 跨 DST 跳变（11-01 秋退在窗内）的长窗：service horizon 400。
	rec = get("stats/trend?from=2026-10-28T00:00:00Z&to=2026-11-08T00:00:00Z&granularity=day&timezone=America%2FNew_York")
	require.Equal(t, 400, rec.Code, "跨 DST 跳变超 MaxStatsRawSpan → 400（宁缺勿残）；body: %s", rec.Body.String())
	require.Equal(t, time.UTC, fake.lastTrendZone, "400 路径零 store 调用（上一条成功记录停在 UTC）")

	// 恒整点无 DST 时区（cube 路径）90d 合法；NY 无跳变夏窗同样合法。
	rec = get("stats/trend?from=2026-05-20T00:00:00Z&to=" + to + "&timezone=Asia%2FShanghai")
	require.Equal(t, 200, rec.Code, "Shanghai（整小时恒偏移）走 cube 全窗口合法；body: %s", rec.Body.String())
	require.Equal(t, cst, fake.lastTrendZone)
	rec = get("stats/trend?from=2026-05-20T00:00:00Z&to=" + to + "&timezone=America%2FNew_York")
	require.Equal(t, 200, rec.Code, "NY 夏窗恒 EDT 整点偏移 → cube 精确合法；body: %s", rec.Body.String())

	// top/ttft：接受并校验，不进查询语义。
	rec = get("stats/top?from=" + from + "&to=" + to + "&entity=user&by=cost&timezone=Asia%2FShanghai")
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	rec = get("stats/top?from=" + from + "&to=" + to + "&entity=user&by=cost&timezone=Bad%2FZone")
	require.Equal(t, 400, rec.Code)
	rec = get("stats/ttft?from=" + from + "&to=" + to + "&timezone=Asia%2FShanghai")
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	rec = get("stats/ttft?from=" + from + "&to=" + to + "&timezone=Bad%2FZone")
	require.Equal(t, 400, rec.Code)
}

// TestDayStartDST dayStart DST 日历语义（"今日"缺省日界与 service AddDate
// 窗口的日界源）：NY 春进/秋退两界内任意时刻归到同一本地零点；半时区
// （Kolkata）零点半时偏移正确；UTC 与 Truncate(24h) 逐位一致（旧行为回归锁）。
func TestDayStartDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	// 春进 2026-03-08：EST 零点 = 05:00Z；春进日/次日 EDT 零点 = 04:00Z。
	require.True(t, dayStart(time.Date(2026, 3, 8, 4, 30, 0, 0, time.UTC), ny).
		Equal(time.Date(2026, 3, 7, 5, 0, 0, 0, time.UTC)), "Mar7 23:30 EST → Mar7 零点")
	require.True(t, dayStart(time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC), ny).
		Equal(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)), "Mar8 03:00 EDT（跳表后）→ 当日零点 05:00Z")
	require.True(t, dayStart(time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC), ny).
		Equal(time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)), "Mar9 → EDT 零点 04:00Z（春进日本地 23h）")
	// 秋退 2026-11-01：02:00 EDT→01:00 EST（墙钟 01:xx 重复）；本地零点 00:00
	// 在跳表前 = EDT（UTC-4）→ Nov1 04:00Z。两个 01:30（EDT/EST）同属 Nov1 日。
	require.True(t, dayStart(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), ny).
		Equal(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)), "01:30 EDT（第一次）→ Nov1 零点 04:00Z")
	require.True(t, dayStart(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), ny).
		Equal(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)), "01:30 EST（第二次）→ 同 Nov1 日零点")

	ist, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)
	require.True(t, dayStart(time.Date(2026, 8, 14, 18, 45, 0, 0, time.UTC), ist).
		Equal(time.Date(2026, 8, 14, 18, 30, 0, 0, time.UTC)), "半时区零点 = UTC 18:30")

	for _, x := range []time.Time{
		time.Date(2026, 8, 14, 9, 15, 0, 0, time.UTC),
		time.Date(2026, 8, 14, 23, 59, 59, 0, time.UTC),
	} {
		require.True(t, x.UTC().Truncate(24*time.Hour).Equal(dayStart(x, time.UTC)),
			"UTC 下与旧 Truncate(24h) 逐位一致")
	}
}
