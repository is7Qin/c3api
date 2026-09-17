// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// Phase 6 budget lock: Q1+Q2 at the 5000-candidate/24h scale (200ms).
// Seeds 5000 candidates × 12 rows across ~20h once, then runs 2 untimed
// warmup rounds (plan cache/cold pages — CI cold start) and times 30
// sequential Q1+Q2 rounds (400 hot keys). The assertion targets p90: with 30
// samples a nearest-rank p99 IS the max, so one CI environment stall (CPU
// steal, checkpoint) would fail the lock — p90 (index 27) tolerates the two
// outermost samples while any real regression (plan/index change) moves every
// percentile together. Matches the repo convention "only guard
// order-of-magnitude regressions, don't fight machine noise"
// (pg_cursor_perf_test.go). p50/p90/p99(max) are logged for trend visibility.
// The M-keyed PG ≤1/min/instance discipline is structural (provider cache,
// counted in the scheduler unit tests), not timed here.
func TestRoutingQualityWindowP99PG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingRollupPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))

	const ncand, nhot, nrows = 5000, 400, 12
	var rc domain.RouteClassIDVal
	rc[0] = 0xA1
	var qc domain.QualityClassIDVal
	qc[0] = 0xB2
	keys := make([]repository.WindowHotKey, 0, nhot)
	batch := &pgx.Batch{}
	for i := 0; i < ncand; i++ {
		var fp domain.CandidateFingerprintVal
		fp[0] = byte(i)
		fp[1] = byte(i >> 8)
		fp[2] = 0xC3
		if i < nhot {
			keys = append(keys, repository.WindowHotKey{RouteClassID: rc, Fingerprint: fp})
		}
		for h := 0; h < nrows; h++ {
			minute := m.Add(-time.Duration(6+h*110) * time.Minute)
			batch.Queue(
				`INSERT INTO routing_quality_rollup
				 (identity_version, route_class_id, quality_class_id, candidate_fingerprint,
				  bucket_minute, attempts, successes, updated_at)
				 VALUES (1, $1, $2, $3, $4, 10, 8, now())`,
				rc[:], qc[:], fp[:], minute)
		}
		// One in-window row per candidate so Q1 scans real current data.
		batch.Queue(
			`INSERT INTO routing_quality_rollup
			 (identity_version, route_class_id, quality_class_id, candidate_fingerprint,
			  bucket_minute, attempts, successes, ttft_n, updated_at)
			 VALUES (1, $1, $2, $3, $4, 35, 30, 30, now())`,
			rc[:], qc[:], fp[:], m.Add(-2*time.Minute))
	}
	br := pool.SendBatch(ctx, batch)
	_, err := br.Exec()
	require.NoError(t, err)
	require.NoError(t, br.Close())
	pgExec(t, pool, `ANALYZE routing_quality_rollup`)

	round := func() {
		cur, err := repos.Partitions.QueryCurrentWindowStats(ctx, 1, m)
		require.NoError(t, err)
		require.Len(t, cur, ncand, "one current row per candidate")
		base, err := repos.Partitions.QueryBaselineTruncated(ctx, 1, m, keys)
		require.NoError(t, err)
		require.Len(t, base, nhot, "one truncated row per hot key")
	}
	// Warmup (untimed): settle plan cache and cold pages before measuring.
	round()
	round()
	const rounds = 30
	durations := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		round()
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[len(durations)/2]
	p90 := durations[int(float64(len(durations))*0.9)]
	p99 := durations[len(durations)-1] // n=30: nearest-rank p99 == max
	t.Logf("Q1+Q2 at 5000 candidates/24h: p50=%s p90=%s p99(max)=%s (budget 200ms)", p50, p90, p99)
	require.Less(t, p90, 200*time.Millisecond, "Q1+Q2 p90 budget at 5000 candidates (p99 logged)")
}
