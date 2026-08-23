// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 统计读取面真实 PG 测试套件（spec 2026-08-23 §7.4-7.6）：下推查询族
// （StatsTrend/StatsTop/StatsEntityTrend）vs Go 侧日志直聚金标准对照。
// TTFT 双分支见 pg_stat_ttft_test.go；基座同 pg_stat_test.go。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// aggregateSeed 种子 → 双表落盘（LoadAggRange + AggregateRange 一站式）。
func aggregateSeed(t *testing.T, repos *repository.Repository, from, to time.Time, logs, errLogs []*domain.UsageLog) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, errLogs))
	cube, entity, _, err := repos.Stats.LoadAggRange(ctx, from, to)
	require.NoError(t, err)
	require.NoError(t, repos.Stats.AggregateRange(ctx, from, to, to.Add(-time.Minute), cube, entity))
}

// foldExpTrend 参照桶按 period 折叠（unit=day 折到 UTC 日界；过滤参数与
// StatsTrend 同语义：groupID>0 / model 非空生效）。
func foldExpTrend(want map[expCubeKey]*expCube, unit string, groupID int64, model string) map[time.Time]*expCube {
	out := map[time.Time]*expCube{}
	for k, v := range want {
		if groupID > 0 && k.groupID != groupID {
			continue
		}
		if model != "" && k.model != model {
			continue
		}
		pt := k.hour
		if unit == "day" {
			pt = k.hour.Truncate(24 * time.Hour)
		}
		c := out[pt]
		if c == nil {
			c = newExpCube()
			out[pt] = c
		}
		c.req += v.req
		c.errN += v.errN
		c.in += v.in
		c.out += v.out
		c.tot += v.tot
		c.cr += v.cr
		c.cc += v.cc
		c.cost += v.cost
		c.raw += v.raw
		c.calls += v.calls
		c.ttftS += v.ttftS
		c.ttftC += v.ttftC
		if v.ttftM > c.ttftM {
			c.ttftM = v.ttftM
		}
		for i := range v.hist {
			c.hist[i] += v.hist[i]
		}
	}
	return out
}

// TestPGStatsTrendPushdown trend 下推金标准对照（spec §7.4）：date_trunc
// (hour/day) × 过滤组合（无过滤/group/model/叠加）的 SQL 结果 vs 对日志直接
// 暴力聚合后折叠的金标准，逐字段相等。
func TestPGStatsTrendPushdown(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(2*time.Hour))

	ttft := func(v int64) *int64 { return &v }
	logs := []*domain.UsageLog{
		usageLogRow("tr1", h.Add(10*time.Minute), domain.ErrNone, 1, 0, 0, 0, "gpt-4o", 10, 20, 1, 2, 100, 200, 1, ttft(40)),
		usageLogRow("tr2", h.Add(20*time.Minute), domain.ErrAbort, 1, 0, 0, 0, "claude-x", 5, 5, 0, 0, 50, 80, 0, nil),
		usageLogRow("tr3", h.Add(30*time.Minute), domain.ErrNone, 2, 0, 0, 0, "gpt-4o", 7, 8, 0, 0, 70, 90, 2, ttft(120)),
		usageLogRow("tr4", h.Add(70*time.Minute), domain.ErrNone, 1, 0, 0, 0, "gpt-4o", 1, 2, 0, 0, 30, 60, 0, ttft(60)),
		usageLogRow("tr5", h.Add(80*time.Minute), domain.ErrAbort, 2, 0, 0, 0, "claude-x", 2, 3, 0, 0, 20, 40, 1, ttft(13000)),
	}
	errLogs := []*domain.UsageLog{
		errLogRow("tr6", h.Add(40*time.Minute), domain.Err5xx, 1, 0, 0, 0, "gpt-4o"),
		errLogRow("tr7", h.Add(100*time.Minute), domain.ErrNetwork, 2, 0, 0, 0, "gpt-4o"),
	}
	aggregateSeed(t, repos, h, h.Add(2*time.Hour), logs, errLogs)

	want := expCubeOf(logs, errLogs)
	for _, tc := range []struct {
		name    string
		unit    string
		groupID int64
		model   string
	}{{"hour-all", "hour", 0, ""}, {"day-all", "day", 0, ""},
		{"hour-group1", "hour", 1, ""}, {"day-model-gpt4o", "day", 0, "gpt-4o"},
		{"hour-group1-claude", "hour", 1, "claude-x"}, {"day-group2", "day", 2, ""}} {
		got, err := repos.Stats.StatsTrend(ctx, h, h.Add(2*time.Hour), tc.unit, tc.groupID, tc.model)
		require.NoError(t, err, "%s", tc.name)
		exp := foldExpTrend(want, tc.unit, tc.groupID, tc.model)
		require.Len(t, got, len(exp), "%s: 桶数一致", tc.name)
		for _, b := range got {
			w := exp[b.BucketTime.UTC()]
			require.NotNil(t, w, "%s: 缺失参照桶 %s", tc.name, b.BucketTime)
			assertCubeMeasures(t, b, w)
		}
	}

	// 非法 unit 显式报错（白名单——禁字符串直插的守门断言）
	_, err := repos.Stats.StatsTrend(ctx, h, h.Add(time.Hour), "week", 0, "")
	require.Error(t, err)
}

// TestPGStatsTopPushdown top 下推（spec §7.5）：by 三种排序各自独立序 + limit
// 截断正确；非法 by/entityType 显式报错。
func TestPGStatsTopPushdown(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	bucket := time.Now().UTC().Truncate(time.Hour)

	// 五账号三口径互不一致的画像（排序序彼此可区分）：
	//   cost:     a3(300) > a5(250) > a1(100) > a4(50) > a2(10)
	//   requests: a1(50)  > a2(40)  > a3(30)  > a4(20) > a5(10)
	//   tokens:   a5(900) > a4(800) > a3(700) > a2(600) > a1(500)
	type profile struct {
		id                int64
		req, cost, tokens int64
	}
	profiles := []profile{
		{1, 50, 100, 500}, {2, 40, 10, 600}, {3, 30, 300, 700}, {4, 20, 50, 800}, {5, 10, 250, 900},
	}
	entity := make([]*domain.EntityStatBucket, 0, len(profiles))
	for _, p := range profiles {
		entity = append(entity, &domain.EntityStatBucket{
			BucketTime: bucket, EntityType: "account", EntityID: p.id, Model: "m",
			RequestCount: p.req, Cost: p.cost, TotalTokens: p.tokens,
		})
	}
	require.NoError(t, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute), nil, entity))

	from, to := bucket, bucket.Add(time.Hour)
	idsOf := func(bs []*domain.EntityStatBucket) []int64 {
		out := make([]int64, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.EntityID)
		}
		return out
	}

	// by=cost 截前 3：a3, a5, a1
	got, err := repos.Stats.StatsTop(ctx, from, to, "account", "cost", 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, []int64{3, 5, 1}, idsOf(got), "cost 序截断")
	require.Equal(t, int64(300), got[0].Cost)
	require.Equal(t, int64(30), got[0].RequestCount, "非排序字段随行正确")

	// by=requests 截前 2：a1, a2（与 cost 序不同——排序键真生效）
	got, err = repos.Stats.StatsTop(ctx, from, to, "account", "requests", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, []int64{1, 2}, idsOf(got))

	// by=tokens 全量：a5..a1 完整逆序
	got, err = repos.Stats.StatsTop(ctx, from, to, "account", "tokens", 10)
	require.NoError(t, err)
	require.Len(t, got, 5)
	require.Equal(t, []int64{5, 4, 3, 2, 1}, idsOf(got))

	// 其他实体类型隔离：user 表无数据 → 空
	got, err = repos.Stats.StatsTop(ctx, from, to, "user", "cost", 10)
	require.NoError(t, err)
	require.Empty(t, got)

	// 白名单守门：未知 by / entityType 显式报错（不落 SQL）
	_, err = repos.Stats.StatsTop(ctx, from, to, "account", "raw_cost; DROP TABLE x", 3)
	require.Error(t, err)
	_, err = repos.Stats.StatsTop(ctx, from, to, "template", "cost", 3)
	require.Error(t, err)
}

// TestPGStatsEntityTrendAndModelSplit entity-trend 下推 + model 拆分合并
// （spec §7.6）：不带 model = 各模型桶之和；带 model = 单模型拆分；
// EntityType/EntityID 回填自入参；hour/day 两粒度。
func TestPGStatsEntityTrendAndModelSplit(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	day := h.Truncate(24 * time.Hour)
	require.NoError(t, repos.EnsureUsageEntityStatsPartitions(ctx, day, day))

	// account 7：两小时 × 两模型（m1/m2 各自可区分的测量画像）
	entity := []*domain.EntityStatBucket{
		{BucketTime: h, EntityType: "account", EntityID: 7, Model: "m1",
			RequestCount: 3, ErrorCount: 1, CallCount: 1, InputTokens: 30, TotalTokens: 60, Cost: 300},
		{BucketTime: h, EntityType: "account", EntityID: 7, Model: "m2",
			RequestCount: 5, ErrorCount: 0, CallCount: 2, InputTokens: 50, TotalTokens: 100, Cost: 500},
		{BucketTime: h.Add(time.Hour), EntityType: "account", EntityID: 7, Model: "m1",
			RequestCount: 4, ErrorCount: 2, CallCount: 0, InputTokens: 40, TotalTokens: 80, Cost: 400},
		{BucketTime: h.Add(time.Hour), EntityType: "account", EntityID: 7, Model: "m2",
			RequestCount: 6, ErrorCount: 0, CallCount: 3, InputTokens: 60, TotalTokens: 120, Cost: 600},
		// 干扰项：他实体不入选
		{BucketTime: h, EntityType: "user", EntityID: 7, Model: "m1", RequestCount: 99},
	}
	require.NoError(t, repos.Stats.AggregateRange(ctx, h, h.Add(2*time.Hour), h.Add(90*time.Minute), nil, entity))

	from, to := h, h.Add(2*time.Hour)

	// hour 无 model：两桶 = 各小时 m1+m2 之和
	got, err := repos.Stats.StatsEntityTrend(ctx, from, to, "hour", "account", 7, "")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, h, got[0].BucketTime.UTC())
	require.Equal(t, int64(8), got[0].RequestCount, "hour0 m1+m2 合并（3+5）")
	require.Equal(t, int64(800), got[0].Cost)
	require.Equal(t, int64(10), got[1].RequestCount, "hour1 m1+m2 合并（4+6）")
	require.Equal(t, "account", got[0].EntityType, "实体维度回填自入参")
	require.Equal(t, int64(7), got[0].EntityID)

	// hour 带 model=m1：只回 m1 拆分桶
	got, err = repos.Stats.StatsEntityTrend(ctx, from, to, "hour", "account", 7, "m1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(3), got[0].RequestCount, "hour0 仅 m1")
	require.Equal(t, int64(4), got[1].RequestCount, "hour1 仅 m1")

	// day 无 model：单日桶 = 全部之和（拆分合并守恒）
	got, err = repos.Stats.StatsEntityTrend(ctx, day, day.Add(24*time.Hour), "day", "account", 7, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, day, got[0].BucketTime.UTC())
	require.Equal(t, int64(18), got[0].RequestCount, "day 桶全模型合计（8+10）")
	require.Equal(t, int64(1800), got[0].Cost)

	// 白名单守门：非法 unit / entityType 显式报错
	_, err = repos.Stats.StatsEntityTrend(ctx, from, to, "month", "account", 7, "")
	require.Error(t, err)
	_, err = repos.Stats.StatsEntityTrend(ctx, from, to, "hour", "template", 7, "")
	require.Error(t, err)
}
