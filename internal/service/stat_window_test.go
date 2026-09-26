// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 相对窗口与等价整点绝对窗口逐桶相同（spec §8 A11）。解码本身在
// httpface.ResolveStatsWindow；本层证明两件事：
//
//  1. 同一注入时钟下，WindowFromDuration(168h) 算出的整点对喂给 QueryStatsTrend
//     后，桶集合与直接给出这对整点时刻的请求逐桶相同，且判定是 Exact（零平移）。
//  2. 客户端用墙钟减法自算的 [now−168h, now) 在时长是整小时倍数时，归一化后
//     的生效窗口与上面那对整点相同，但 Reason 是 WindowShifted——首个不满小时
//     的桶被丢掉。预设改走 window= 就是为了不走进这条平移。
//
// 时间一律固定 time.Date(2026, …)，禁墙钟、禁 t.Parallel。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestQueryStatsTrendRelativeWindowMatchesHourBounds(t *testing.T) {
	now := time.Date(2026, 9, 25, 15, 38, 12, 0, time.UTC)
	from, to := domain.WindowFromDuration(168*time.Hour, now)
	require.True(t, from.Equal(time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)))
	require.True(t, to.Equal(time.Date(2026, 9, 25, 16, 0, 0, 0, time.UTC)))

	seed := []*domain.StatBucket{
		// 墙钟减法会想计入的首小时（15:00），整点窗口与相对窗口都必须丢掉。
		{BucketTime: from.Add(-time.Hour), Model: "m", RequestCount: 1},
		{BucketTime: from, Model: "m", RequestCount: 11},
		{BucketTime: from.Add(24 * time.Hour), Model: "m", RequestCount: 22},
		{BucketTime: to.Add(-time.Hour), Model: "m", RequestCount: 33},
		// 半开上界。
		{BucketTime: to, Model: "m", RequestCount: 44},
	}

	query := func(from, to time.Time) StatsRows[*domain.StatBucket] {
		fs := newFakeStore()
		fs.stats = append([]*domain.StatBucket(nil), seed...)
		svc := statsTestSvc(fs)
		svc.statsNow = func() time.Time { return now }
		res, err := svc.QueryStatsTrend(context.Background(), TrendQuery{
			From: from, To: to, Granularity: "hour", Zone: time.UTC,
		})
		require.NoError(t, err)
		return res
	}

	counts := func(res StatsRows[*domain.StatBucket]) []int64 {
		out := make([]int64, 0, len(res.Buckets))
		for _, b := range res.Buckets {
			out = append(out, b.RequestCount)
		}
		return out
	}

	aligned := query(from, to)
	require.Equal(t, domain.StatsStorageCube, aligned.Exec.Storage)
	require.Equal(t, domain.StatsPlanExact, aligned.Exec.Reason, "双界整点命中精确分支，零平移")
	require.True(t, aligned.Exec.From.Equal(from))
	require.True(t, aligned.Exec.To.Equal(to))
	require.Equal(t, []int64{11, 22, 33}, counts(aligned))

	// 客户端自算 [now−168h, now)：归一化后生效窗口与整点对相同（桶集合相同），
	// 但原因是平移——被丢掉的正是请求下界所在的那个不满小时桶。
	shifted := query(now.Add(-168*time.Hour), now)
	require.Equal(t, counts(aligned), counts(shifted), "归一化后桶集合与整点窗口相同")
	require.Equal(t, domain.StatsPlanWindowShifted, shifted.Exec.Reason)
	require.True(t, shifted.Exec.From.Equal(from))
	require.True(t, shifted.Exec.To.Equal(to))
	require.True(t, shifted.Exec.From.After(now.Add(-168*time.Hour)), "生效下界晚于客户端自算的下界")
}
