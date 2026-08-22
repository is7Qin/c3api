// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/usagestat"
)

// TestVariantAggregateRangeOverwrite spec 2026-08-14 覆盖语义（issue #8 教训：
// 修正/补账通过重算 bucket 实现，非累加）：同范围两跑结果一致（幂等重放）；
// 重跑改值 → 覆盖为新值（非累加）。单写者由会话级 advisory lock 保证
// （usage/stats_agg.go），本测试验证 AggregateRange 覆盖行为。
func TestVariantAggregateRangeOverwrite(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Hour)
	mk := func(g, req int64) *domain.StatBucket {
		return &domain.StatBucket{
			BucketTime: base, GroupID: g,
			Model:        "gpt-4o",
			RequestCount: req, ErrorCount: 0, InputTokens: 0, OutputTokens: 0,
			TotalTokens: 10 * req, CacheReadTokens: 0, CacheCreationTokens: 0, Cost: 0,
			TTFTHist: make([]int64, 10),
		}
	}
	const n = 500
	buckets := make([]*domain.StatBucket, 0, n)
	for g := int64(1); g <= n; g++ {
		buckets = append(buckets, mk(g, 1))
	}
	// 第一跑：全新范围（DELETE 空）→ 全 INSERT
	require.NoError(t, repos.Stats.AggregateRange(ctx, base, base.Add(time.Hour), base.Add(30*time.Minute), buckets, nil))

	// 第二跑：同范围同值 → DELETE 后全量覆盖，结果一致（幂等重放）
	require.NoError(t, repos.Stats.AggregateRange(ctx, base, base.Add(time.Hour), base.Add(30*time.Minute), buckets, nil))
	total, err := repos.Client.UsageStat.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, n, total, "重放同范围不重复计（覆盖语义）")

	// 第三跑：同范围改值（request_count 3）→ 覆盖为新值（非累加——修正/补账语义）
	changed := make([]*domain.StatBucket, 0, n)
	for g := int64(1); g <= n; g++ {
		changed = append(changed, mk(g, 3))
	}
	require.NoError(t, repos.Stats.AggregateRange(ctx, base, base.Add(time.Hour), base.Add(30*time.Minute), changed, nil))
	g1, err := repos.Client.UsageStat.Query().Where(usagestat.GroupIDEQ(1)).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), g1.RequestCount, "重跑同范围覆盖为新值（非累加）")
	require.Equal(t, int64(30), g1.TotalTokens)
}
