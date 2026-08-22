// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 统计读取面下推基准（spec 2026-08-23 §7）：真实 PG 上 trend/top 两查询的
// 单次往返成本（30 天 × 多维度桶量级）。跑法：
//
//	TEST_DATABASE_URL=... go test -bench BenchmarkStats -run '^$' ./internal/repository/
//
// 基座同 pg_stat_test.go（newPGRepos 每 benchmark 重建 schema；串行）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// BenchmarkStatsTrendPushdown cube trend 下推：8640 桶（30 天 × 24 小时 ×
// 4 组 × 3 模型）上 date_trunc('day') 全窗聚合单次往返。
func BenchmarkStatsTrendPushdown(b *testing.B) {
	repos := newPGRepos(b)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(24 * time.Hour)
	require.NoError(b, repos.EnsureUsageStatsPartitions(ctx, base.Add(-29*24*time.Hour), base))

	// 30 天 × 24 小时 × 4 组 × 3 模型 = 8640 桶（唯一键不撞）
	cube := make([]*domain.StatBucket, 0, 8640)
	for d := 0; d < 30; d++ {
		day := base.AddDate(0, 0, -d)
		for h := 0; h < 24; h++ {
			bt := day.Add(time.Duration(h) * time.Hour)
			for g := int64(1); g <= 4; g++ {
				for m, model := range []string{"gpt-4o", "claude-x", "gemini-y"} {
					cube = append(cube, &domain.StatBucket{
						BucketTime: bt, GroupID: g, Model: model,
						RequestCount: int64(10 + m), TotalTokens: 100, Cost: 5,
						TTFTHist: make([]int64, 10),
					})
				}
			}
		}
	}
	require.NoError(b, repos.Stats.AggregateRange(ctx, base.Add(-29*24*time.Hour), base.Add(24*time.Hour), base.Add(time.Hour), cube, nil))

	from := base.Add(-29 * 24 * time.Hour)
	to := base.Add(24 * time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := repos.Stats.StatsTrend(ctx, from, to, "day", 0, "")
		require.NoError(b, err)
		require.Len(b, got, 30, "日桶数 = 窗口天数")
	}
}

// BenchmarkStatsTop entity top 下推：2000 账号 × 1 桶上 by=cost LIMIT 20 排序
// 截断单次往返。
func BenchmarkStatsTop(b *testing.B) {
	repos := newPGRepos(b)
	ctx := context.Background()
	bucket := time.Now().UTC().Truncate(time.Hour)

	entity := make([]*domain.EntityStatBucket, 0, 2000)
	for id := int64(1); id <= 2000; id++ {
		entity = append(entity, &domain.EntityStatBucket{
			BucketTime: bucket, EntityType: "account", EntityID: id, Model: "m",
			RequestCount: id % 50, Cost: id * 7 % 100000, TotalTokens: id * 13 % 999999,
		})
	}
	require.NoError(b, repos.Stats.AggregateRange(ctx, bucket, bucket.Add(time.Hour), bucket.Add(30*time.Minute), nil, entity))

	from, to := bucket, bucket.Add(time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := repos.Stats.StatsTop(ctx, from, to, "account", "cost", 20)
		require.NoError(b, err)
		require.Len(b, got, 20, "limit 截断")
	}
}
