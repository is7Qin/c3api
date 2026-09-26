// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 相对窗口 `?window=<dur>` 的 handler 级判据（spec §8 A11 / A12）：
//
//   - A11：同一注入时钟下，`window=168h` 与等价整点 from/to 的回显头相同、
//     桶集合相同（零平移）。写回显头的端点是 /stats/trend。
//   - A12：from+to+window / 只给一端 / 两态都不给 → 400 reason=window_ambiguous，
//     body 不带 storage / limit_seconds / effective_* / retention_days；
//     仅 window=168h → 200 且回显头双界整点；window=7d → 400 reason=window_invalid，
//     同样不带那些字段。
//
// 解码是 6 个端点共享的 httpface.ResolveStatsWindow，故矩阵只在写回显头的
// 趋势端点上跑全；accounts/usage 与用户面各跑一条，证明不是各写一份。
// 排除端点（top / logs / err_logs / routing / overview）的 from/to 仍必填、
// 参数结构没有 Window 字段。
//
// 纪律：require only、无 t.Parallel()、时钟一律 time.Date(2026, …)。

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// relativeWindowNow 固定注入时钟（非整点，ceilHour 必须进位到 16:00Z）。
func relativeWindowNow() time.Time {
	return time.Date(2026, 9, 25, 15, 38, 12, 0, time.UTC)
}

func newRelativeWindowHandler(t *testing.T) (*AdminAPI, *fakeStore) {
	t.Helper()
	h, fake := newStatsHandler(t, domain.Retention{})
	h.now = func() time.Time { return relativeWindowNow() }
	from, to := domain.WindowFromDuration(168*time.Hour, relativeWindowNow())
	fake.stats = []*domain.StatBucket{
		{BucketTime: from.Add(-time.Hour), Model: "m", RequestCount: 1},
		{BucketTime: from, Model: "m", RequestCount: 11},
		{BucketTime: from.Add(24 * time.Hour), Model: "m", RequestCount: 22},
		{BucketTime: to.Add(-time.Hour), Model: "m", RequestCount: 33},
		{BucketTime: to, Model: "m", RequestCount: 44},
	}
	return h, fake
}

func bucketCounts(t *testing.T, body []byte) []int64 {
	t.Helper()
	var pts []StatTrendPoint
	require.NoError(t, json.Unmarshal(body, &pts))
	slices.SortFunc(pts, func(a, b StatTrendPoint) int {
		return a.BucketTime.Compare(*b.BucketTime)
	})
	out := make([]int64, 0, len(pts))
	for _, p := range pts {
		require.NotNil(t, p.RequestCount)
		out = append(out, *p.RequestCount)
	}
	return out
}

func forbidWindowFields(t *testing.T, body map[string]any) {
	t.Helper()
	for _, k := range []string{"storage", "limit_seconds", "effective_from", "effective_to", "retention_days"} {
		require.NotContains(t, body, k)
	}
}

// TestStatsTrendRelativeWindow A11 + A12 在写回显头的趋势端点上。
func TestStatsTrendRelativeWindow(t *testing.T) {
	h, _ := newRelativeWindowHandler(t)
	from, to := domain.WindowFromDuration(168*time.Hour, relativeWindowNow())
	wantFrom, wantTo := from.Format(time.RFC3339), to.Format(time.RFC3339)

	viaWindow := getStats(h, "stats/trend?window=168h&granularity=hour")
	require.Equal(t, http.StatusOK, viaWindow.Code, "body: %s", viaWindow.Body.String())
	require.Equal(t, wantFrom, viaWindow.Header().Get("X-Stats-Effective-From"))
	require.Equal(t, wantTo, viaWindow.Header().Get("X-Stats-Effective-To"))
	require.Equal(t, "cube", viaWindow.Header().Get("X-Stats-Storage"), "双界整点走精确分支")
	require.Zero(t, from.Minute())
	require.Zero(t, from.Second())
	require.Zero(t, to.Minute())

	viaBounds := getStats(h, "stats/trend?from="+wantFrom+"&to="+wantTo+"&granularity=hour")
	require.Equal(t, http.StatusOK, viaBounds.Code, "body: %s", viaBounds.Body.String())
	require.Equal(t, viaWindow.Header().Get("X-Stats-Effective-From"), viaBounds.Header().Get("X-Stats-Effective-From"))
	require.Equal(t, viaWindow.Header().Get("X-Stats-Effective-To"), viaBounds.Header().Get("X-Stats-Effective-To"))
	require.Equal(t, bucketCounts(t, viaBounds.Body.Bytes()), bucketCounts(t, viaWindow.Body.Bytes()),
		"window=168h 与等价整点 from/to 逐桶相同")
	require.Equal(t, []int64{11, 22, 33}, bucketCounts(t, viaWindow.Body.Bytes()))

	cases := []struct {
		name   string
		query  string
		reason string
	}{
		{"两态都不给", "stats/trend?granularity=hour", "window_ambiguous"},
		{"只给 from", "stats/trend?from=" + wantFrom + "&granularity=hour", "window_ambiguous"},
		{"只给 to", "stats/trend?to=" + wantTo + "&granularity=hour", "window_ambiguous"},
		{"from+to+window", "stats/trend?window=168h&from=" + wantFrom + "&to=" + wantTo, "window_ambiguous"},
		{"日历天 7d", "stats/trend?window=7d&granularity=hour", "window_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getStats(h, tc.query)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, tc.reason, body["reason"])
			forbidWindowFields(t, body)
			require.Empty(t, rec.Header().Get("X-Stats-Effective-From"), "拒绝路径不写回显头")
		})
	}
}

// TestAccountsUsageRelativeWindowReason accounts/usage 走同一份解码：冲突与
// 非法时长带 reason 且不带伪造窗口字段；仅 window=24h 的收参双界整点。
func TestAccountsUsageRelativeWindowReason(t *testing.T) {
	h, store := newUsageTestHandler(t, relativeWindowNow(), &hUsageSnap{})
	from, to := domain.WindowFromDuration(24*time.Hour, relativeWindowNow())

	cases := []struct {
		name   string
		query  string
		reason string
	}{
		{"两态都不给", "account_ids=1", "window_ambiguous"},
		{"只给 from", "account_ids=1&from=2026-09-25T16:00:00Z", "window_ambiguous"},
		{"只给 to", "account_ids=1&to=2026-09-25T16:00:00Z", "window_ambiguous"},
		{"window + from/to", "account_ids=1&window=24h&from=2026-09-24T16:00:00Z&to=2026-09-25T16:00:00Z", "window_ambiguous"},
		{"日历天 7d", "account_ids=1&window=7d", "window_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getUsage(h, tc.query)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, tc.reason, body["reason"])
			forbidWindowFields(t, body)
		})
	}

	rec := getUsage(h, "account_ids=1&window=24h")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.True(t, store.aggFrom.Equal(from), "from = ceilHour(now)−24h（%v）", store.aggFrom)
	require.True(t, store.aggTo.Equal(to), "to = ceilHour(now)（%v）", store.aggTo)
	require.Zero(t, store.aggFrom.Minute())
	require.Zero(t, store.aggTo.Minute())
}

// TestExcludedStatsEndpointsKeepAbsoluteWindow 排除端点不接受 window：
// /stats/top 缺 from 仍 400，且参数结构没有 Window 字段；logs / err_logs /
// routing / overview 同样没有 Window，from/to（或 days）保持原形态。
func TestExcludedStatsEndpointsKeepAbsoluteWindow(t *testing.T) {
	// top 的 from/to 仍是必填值类型（不是 *time.Time）。
	require.IsType(t, time.Time{}, GetStatsTopParams{}.From)
	require.IsType(t, time.Time{}, GetStatsTopParams{}.To)

	h, _ := newRelativeWindowHandler(t)
	rec := getStats(h, "stats/top?entity=user&by=cost")
	require.Equal(t, http.StatusBadRequest, rec.Code, "top 缺 from/to 仍 400: %s", rec.Body.String())

	// 多给的 window 查询参数不改变必填：缺 from 照旧 400（生成绑定不认识 window）。
	rec = getStats(h, "stats/top?entity=user&by=cost&window=24h")
	require.Equal(t, http.StatusBadRequest, rec.Code, "top 不因 window 放行: %s", rec.Body.String())

	rec = getStats(h, "usage_logs?window=24h")
	require.Equal(t, http.StatusBadRequest, rec.Code, "usage_logs 缺 from/to 仍 400: %s", rec.Body.String())
	rec = getStats(h, "err_logs?window=24h")
	require.Equal(t, http.StatusBadRequest, rec.Code, "err_logs 缺 from/to 仍 400: %s", rec.Body.String())

	// 合法 from/to 仍 200，证明没有被误收紧成「必须带 window」。
	rec = getStats(h, "stats/top?from=2026-09-24T16:00:00Z&to=2026-09-25T16:00:00Z&entity=user&by=cost")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// 编译期：排除端点的参数结构没有 Window 字段（有则下面的赋值无法通过——
// 这里用一个只接受「没有 Window 方法」的约束做不到字段级，故用反射在运行时钉死）。
func TestExcludedParamsHaveNoWindowField(t *testing.T) {
	require.False(t, hasWindowField[GetStatsTopParams]())
	require.False(t, hasWindowField[GetUsageLogsParams]())
	require.False(t, hasWindowField[GetErrLogsParams]())
	require.False(t, hasWindowField[GetRoutingFlowParams]())
	require.False(t, hasWindowField[GetRoutingFrontierParams]())
	require.False(t, hasWindowField[GetAdminOverviewParams]())
	// 纳入端点有，防止断言写反。
	require.True(t, hasWindowField[GetStatsTrendParams]())
	require.True(t, hasWindowField[GetStatsEntityTrendParams]())
	require.True(t, hasWindowField[GetStatsTTFTParams]())
	require.True(t, hasWindowField[GetAccountsUsageParams]())
}

func hasWindowField[T any]() bool {
	var zero T
	_, ok := reflect.TypeOf(zero).FieldByName("Window")
	return ok
}
