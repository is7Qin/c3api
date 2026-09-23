// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// A8：quality_class 乘数精确。2 class × 1 候选 → rollup 恰 2 行；编译当前窗
// 仍按 (route, fingerprint) 聚为 1 行（quality_class_id 是 frontier 展示与
// ttft-hist join 维度，不是编译器当前窗的分组维度）。
func TestRoutingQualityClassMultiplierPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	rcA, _, qc1, qc2, fps := windowVals(t)
	fp := fps["cur"]

	for i, qc := range []domain.QualityClassIDVal{qc1, qc2} {
		require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, repository.RoutingQualityRow{
			IdentityVersion: 1, RouteClassID: rcA, QualityClassID: qc, CandidateFingerprint: fp,
			InstanceSrc: "src-A8", BucketMinute: m, AbsoluteSequence: int64(i + 1),
			Attempts: 10, Successes: 7, TTFTN: 10,
		}))
	}
	require.NoError(t, repos.Partitions.RollupQuality(ctx, m, 1))

	var rollupRows int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM routing_quality_rollup WHERE route_class_id=$1 AND bucket_minute=$2`,
		rcA[:], m).Scan(&rollupRows))
	require.Equal(t, int64(2), rollupRows, "two quality classes × one candidate must be exactly two rollup rows")

	cur, err := repos.Partitions.QueryCurrentWindowStats(ctx, 1, m.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, cur, 1, "the compiler current window aggregates by (route, fingerprint) only")
	require.Equal(t, int64(20), cur[0].Attempts)
	require.Equal(t, int64(14), cur[0].Successes)
}
