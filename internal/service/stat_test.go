// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 统计查询面校验单测（spec-stats-p1-backend §5/§7.10）：全部校验路径 +
// 经 fakestore 的参数透传断言。fake 不复算 SQL 聚合语义（下推正确性由
// repository PG 金标准对照负责）；此处借 fake 的过滤/合并行为反推 service
// 实际透传的参数（粒度归一化、limit 钳制、TTFT 分支选择、self 钉死）。

import (
	"context"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

var statsTestFrom = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func statsTestSvc(fs *fakeStore) *Service {
	return &Service{store: fs, inv: &invRecorder{}}
}

// seedTrend 两小时桶同日同组同模型（day 粒度合并为 1 行、hour 保持 2 行的探针）。
func seedTrend(fs *fakeStore) {
	fs.stats = []*domain.StatBucket{
		{BucketTime: statsTestFrom, GroupID: 1, Model: "m1", RequestCount: 10},
		{BucketTime: statsTestFrom.Add(time.Hour), GroupID: 1, Model: "m1", RequestCount: 5},
		{BucketTime: statsTestFrom, GroupID: 2, Model: "m2", RequestCount: 100},
	}
}

// TestQueryStatsTrend_whenWindowInvalid_thenSentinel 窗口三连：缺参/倒序/超跨度。
func TestQueryStatsTrend_whenWindowInvalid_thenSentinel(t *testing.T) {
	svc := statsTestSvc(newFakeStore())
	cases := []struct {
		name string
		q    TrendQuery
	}{
		{"missing from", TrendQuery{To: statsTestFrom.Add(time.Hour)}},
		{"missing to", TrendQuery{From: statsTestFrom}},
		{"to equals from", TrendQuery{From: statsTestFrom, To: statsTestFrom}},
		{"to before from", TrendQuery{From: statsTestFrom, To: statsTestFrom.Add(-time.Hour)}},
		{"span over 90d", TrendQuery{From: statsTestFrom, To: statsTestFrom.Add(MaxStatsTrendSpan + time.Hour)}},
	}
	for _, tc := range cases {
		_, err := svc.QueryStatsTrend(context.Background(), tc.q)
		require.ErrorIs(t, err, ErrInvalidInput, tc.name)
	}
}

// TestQueryStatsTrend_whenSpanAtLimit_thenOK 跨度恰为 90d 上界合法。
func TestQueryStatsTrend_whenSpanAtLimit_thenOK(t *testing.T) {
	fs := newFakeStore()
	seedTrend(fs)
	svc := statsTestSvc(fs)
	rows, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
		From: statsTestFrom, To: statsTestFrom.Add(MaxStatsTrendSpan), Granularity: "hour",
	})
	require.NoError(t, err)
	require.Len(t, rows, 2, "三个桶落在两个不同小时（同小时异维度按趋势面口径合一）")
}

// TestQueryStatsTrend_whenGranularityVariants_thenNormalizedOrRejected 粒度白名单：
// 空 = day 归一化（两小时桶合一日桶）、hour 原样、非法值拒绝且不触 store。
func TestQueryStatsTrend_whenGranularityVariants_thenNormalizedOrRejected(t *testing.T) {
	fs := newFakeStore()
	seedTrend(fs)
	svc := statsTestSvc(fs)

	rows, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
		From: statsTestFrom, To: statsTestFrom.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, rows, 1, "空粒度归一化 day：窗口内全部小时桶合一日桶")
	require.Equal(t, int64(115), rows[0].RequestCount, "10+5+100 三桶同日合并")

	rows, err = svc.QueryStatsTrend(context.Background(), TrendQuery{
		From: statsTestFrom, To: statsTestFrom.Add(2 * time.Hour), Granularity: "hour",
	})
	require.NoError(t, err)
	require.Len(t, rows, 2, "hour 原样透传")

	_, err = svc.QueryStatsTrend(context.Background(), TrendQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), Granularity: "week",
	})
	require.ErrorIs(t, err, ErrInvalidInput, "非法粒度 → 校验错误")
}

// TestQueryStatsTrend_whenFiltersSet_thenPassedThrough group/model 过滤逐字透传。
func TestQueryStatsTrend_whenFiltersSet_thenPassedThrough(t *testing.T) {
	fs := newFakeStore()
	seedTrend(fs)
	svc := statsTestSvc(fs)
	rows, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
		From: statsTestFrom, To: statsTestFrom.Add(2 * time.Hour), Granularity: "hour",
		GroupID: 1, Model: "m1",
	})
	require.NoError(t, err)
	require.Len(t, rows, 2, "group=1 model=m1 过滤生效")
	require.Equal(t, int64(15), rows[0].RequestCount+rows[1].RequestCount)
}

// TestQueryStatsTop_whenParamsInvalid_thenSentinel 窗口先于白名单；实体类型/
// 排序键白名单拦截。
func TestQueryStatsTop_whenParamsInvalid_thenSentinel(t *testing.T) {
	svc := statsTestSvc(newFakeStore())
	_, err := svc.QueryStatsTop(context.Background(), TopQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "template", By: "cost"})
	require.ErrorIs(t, err, ErrInvalidInput, "entityType 非白名单")
	_, err = svc.QueryStatsTop(context.Background(), TopQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "account", By: "raw_cost"})
	require.ErrorIs(t, err, ErrInvalidInput, "by 非白名单")
	_, err = svc.QueryStatsTop(context.Background(), TopQuery{
		EntityType: "account", By: "cost"})
	require.ErrorIs(t, err, ErrInvalidInput, "窗口缺参先于白名单")
}

// seedEntities n 个独立账号单桶（排行截断探针）。
func seedEntities(fs *fakeStore, n int) {
	fs.entityStats = make([]*domain.EntityStatBucket, 0, n)
	for i := 0; i < n; i++ {
		fs.entityStats = append(fs.entityStats, &domain.EntityStatBucket{
			BucketTime: statsTestFrom, EntityType: "account", EntityID: int64(i + 1), RequestCount: 1,
		})
	}
}

// TestQueryStatsTop_whenLimitEdge_thenDefaultedAndClamped limit 缺省 20、上限钳 200
// （ClampLimit(200) 同语义；210 实体 + limit 250 → 200 行证明钳制而非透传）。
func TestQueryStatsTop_whenLimitEdge_thenDefaultedAndClamped(t *testing.T) {
	win := TopQuery{From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "account", By: "cost"}

	fs := newFakeStore()
	seedEntities(fs, 25)
	svc := statsTestSvc(fs)
	rows, err := svc.QueryStatsTop(context.Background(), win)
	require.NoError(t, err)
	require.Len(t, rows, 20, "limit ≤0 缺省 20")

	fs = newFakeStore()
	seedEntities(fs, 210)
	svc = statsTestSvc(fs)
	rows, err = svc.QueryStatsTop(context.Background(), TopQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "account", By: "cost", Limit: 250})
	require.NoError(t, err)
	require.Len(t, rows, 200, "limit >200 钳到 200")

	rows, err = svc.QueryStatsTop(context.Background(), TopQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "account", By: "cost", Limit: 200})
	require.NoError(t, err)
	require.Len(t, rows, 200, "limit 恰为上界原样透传")
}

// TestQueryEntityTrend_whenParamsInvalid_thenSentinel 实体趋势校验路径。
func TestQueryEntityTrend_whenParamsInvalid_thenSentinel(t *testing.T) {
	svc := statsTestSvc(newFakeStore())
	_, err := svc.QueryEntityTrend(context.Background(), EntityTrendQuery{
		EntityType: "group", EntityID: 7, From: statsTestFrom, To: statsTestFrom.Add(time.Hour)})
	require.ErrorIs(t, err, ErrInvalidInput, "entityType 非白名单")
	_, err = svc.QueryEntityTrend(context.Background(), EntityTrendQuery{
		EntityType: "account", EntityID: 7, From: statsTestFrom, To: statsTestFrom.Add(time.Hour),
		Granularity: "week"})
	require.ErrorIs(t, err, ErrInvalidInput, "非法粒度")
	_, err = svc.QueryEntityTrend(context.Background(), EntityTrendQuery{EntityType: "account", EntityID: 7})
	require.ErrorIs(t, err, ErrInvalidInput, "窗口缺参")
}

// TestQueryEntityTrend_whenValid_thenPassedThrough 实体/模型/粒度透传。
func TestQueryEntityTrend_whenValid_thenPassedThrough(t *testing.T) {
	fs := newFakeStore()
	fs.entityStats = []*domain.EntityStatBucket{
		{BucketTime: statsTestFrom, EntityType: "user", EntityID: 7, Model: "m1", RequestCount: 1},
		{BucketTime: statsTestFrom.Add(time.Hour), EntityType: "user", EntityID: 7, Model: "m2", RequestCount: 2},
		{BucketTime: statsTestFrom, EntityType: "user", EntityID: 8, Model: "m1", RequestCount: 4},
	}
	svc := statsTestSvc(fs)
	rows, err := svc.QueryEntityTrend(context.Background(), EntityTrendQuery{
		EntityType: "user", EntityID: 7, From: statsTestFrom, To: statsTestFrom.Add(2 * time.Hour),
		Model: "m1",
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(1), rows[0].RequestCount, "entity+model 过滤透传")
	require.Equal(t, "user", rows[0].EntityType)
	require.Equal(t, int64(7), rows[0].EntityID)

	rows, err = svc.QueryEntityTrend(context.Background(), EntityTrendQuery{
		EntityType: "user", EntityID: 7, From: statsTestFrom, To: statsTestFrom.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, rows, 1, "空粒度归一化 day：同日两小时桶合一")
	require.Equal(t, int64(3), rows[0].RequestCount)
}

// seedTTFT cube 与实体表各置不同 TTFT 样本数——Count 值即分支选择探针
// （sketch 读 cube、exact 读实体表）。
func seedTTFT(fs *fakeStore) {
	fs.stats = []*domain.StatBucket{{BucketTime: statsTestFrom, TTFTTotalMS: 500, TTFTCount: 5, TTFTMaxMS: 200}}
	fs.entityStats = append(fs.entityStats, &domain.EntityStatBucket{
		BucketTime: statsTestFrom, EntityType: "account", EntityID: 7, TTFTTotalMS: 900, TTFTCount: 9, TTFTMaxMS: 300})
}

// TestQueryStatsTTFT_whenSketchBranch_thenBucketCapApplies sketch 分支：分支选择 +
// 桶数上限（2160h 恰好合法、2161h 拒绝）。
func TestQueryStatsTTFT_whenSketchBranch_thenBucketCapApplies(t *testing.T) {
	fs := newFakeStore()
	seedTTFT(fs)
	svc := statsTestSvc(fs)

	sum, err := svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, int64(5), sum.Count, "EntityType 空 → sketch 分支（读 cube）")
	require.Equal(t, "sketch", sum.Source)

	sum, err = svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(MaxStatsSketchBuckets * time.Hour)})
	require.NoError(t, err, "桶数恰为上限合法")
	require.NotNil(t, sum)

	_, err = svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(MaxStatsSketchBuckets*time.Hour + time.Hour)})
	require.ErrorIs(t, err, ErrInvalidInput, "桶数超上限")
}

// TestQueryStatsTTFT_whenExactBranch_thenWhitelistSpanAndID exact 分支：白名单 +
// EntityID 必配 + 168h 独立上限。
func TestQueryStatsTTFT_whenExactBranch_thenWhitelistSpanAndID(t *testing.T) {
	fs := newFakeStore()
	seedTTFT(fs)
	svc := statsTestSvc(fs)

	sum, err := svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "account", EntityID: 7})
	require.NoError(t, err)
	require.Equal(t, int64(9), sum.Count, "带实体过滤 → exact 分支（读实体表）")
	require.Equal(t, "exact", sum.Source)

	_, err = svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "account", EntityID: 0})
	require.ErrorIs(t, err, ErrInvalidInput, "实体分支必须配 EntityID≠0")
	_, err = svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour), EntityType: "group", EntityID: 7})
	require.ErrorIs(t, err, ErrInvalidInput, "entityType 非白名单")
	_, err = svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(MaxStatsTTFTExactSpan + time.Hour),
		EntityType: "account", EntityID: 7})
	require.ErrorIs(t, err, ErrInvalidInput, "exact 跨度超 168h")
	require.Contains(t, err.Error(), "exact-track limit")

	_, err = svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(MaxStatsTTFTExactSpan + time.Millisecond),
		EntityType: "account", EntityID: 7})
	require.ErrorIs(t, err, ErrInvalidInput, "exact 跨度超 168h 1ms 亦拒绝")
	require.Contains(t, err.Error(), "exact-track limit")

	sum, err = svc.QueryStatsTTFT(context.Background(), TTFTQuery{
		From: statsTestFrom, To: statsTestFrom.Add(MaxStatsTTFTExactSpan),
		EntityType: "account", EntityID: 7})
	require.NoError(t, err, "exact 跨度恰为 168h 合法")
	require.NotNil(t, sum)
}

// TestUserStats_whenCallerForgesEntity_thenPinnedToSelf UserStats/UserStatsTTFT
// 忽略调用方 entity 参数，userID 钉死注入（越权防御回归）。
func TestUserStats_whenCallerForgesEntity_thenPinnedToSelf(t *testing.T) {
	fs := newFakeStore()
	seedTTFT(fs) // cube TTFTCount=5（sketch 探针）+ account/7 TTFTCount=9
	fs.entityStats = append(fs.entityStats,
		// user/7 带 7 样本 TTFT——钉死探针：误入 sketch 应为 5、误用伪造 account/999 应为 0
		&domain.EntityStatBucket{BucketTime: statsTestFrom, EntityType: "user", EntityID: 7,
			RequestCount: 1, TTFTTotalMS: 700, TTFTCount: 7},
		&domain.EntityStatBucket{BucketTime: statsTestFrom, EntityType: "user", EntityID: 8, RequestCount: 2},
		&domain.EntityStatBucket{BucketTime: statsTestFrom, EntityType: "account", EntityID: 999, RequestCount: 4},
	)
	svc := statsTestSvc(fs)

	rows, err := svc.UserStats(context.Background(), 7, EntityTrendQuery{
		EntityType: "account", EntityID: 999,
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, rows, 1, "伪造 entity 参数被覆盖：只见自己（user/7）数据")
	require.Equal(t, "user", rows[0].EntityType)
	require.Equal(t, int64(7), rows[0].EntityID)
	require.Equal(t, int64(1), rows[0].RequestCount)

	sum, err := svc.UserStatsTTFT(context.Background(), 7, TTFTQuery{
		EntityType: "account", EntityID: 999,
		From: statsTestFrom, To: statsTestFrom.Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, int64(7), sum.Count, "self 钉死走 exact 读 user/7（sketch=5 / 伪造实体=0 均排除）")
	require.Equal(t, "exact", sum.Source)
}
