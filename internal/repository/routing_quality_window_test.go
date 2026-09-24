// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// RED: windowed quality reads over routing_quality_fact (single-shard fact).
// Q1 = current window [M-5m, M) grouped by (route, fingerprint), summed
// across quality classes and instance shards. Q2 = per-(route, fp) baseline
// [M-24h, M-5m) truncated newest→oldest at attempts ≥ 30 in SQL, restricted
// to hotKeys (pre-aggregated back to (minute, quality_class) granularity so the
// truncation boundary matches the old merged rollup).

var windowTestMinute = time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)

func seedQualityFactRow(t *testing.T, pool *pgxpool.Pool, rc domain.RouteClassIDVal, qc domain.QualityClassIDVal, fp domain.CandidateFingerprintVal, minute time.Time, attempts, successes int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO routing_quality_fact
		 (identity_version, route_class_id, quality_class_id, candidate_fingerprint,
		  instance_src, bucket_minute, absolute_sequence, attempts, successes, ttft_n,
		  ttft_sum_log_q32, ttft_sumsq_log_q32,
		  input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, updated_at)
		 VALUES (1, $1, $2, $3, 'src-seed', $4, 1, $5, $6, $5, 0, 0, $5, $6, 0, 0, now())`,
		rc[:], qc[:], fp[:], minute.UTC(), attempts, successes)
	require.NoError(t, err)
}

func windowVals(t *testing.T) (rcA, rcB domain.RouteClassIDVal, qc1, qc2 domain.QualityClassIDVal, fps map[string]domain.CandidateFingerprintVal) {
	t.Helper()
	var err error
	rcA, err = domain.RouteClassID(7, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	require.NoError(t, err)
	rcB, err = domain.RouteClassID(9, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	require.NoError(t, err)
	qc1, err = domain.QualityClassID(domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	require.NoError(t, err)
	qc2, err = domain.QualityClassID(domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o-mini", domain.OpChatCompletions)
	require.NoError(t, err)
	names := []string{"n29", "n30", "n31", "split", "cut", "old", "cold", "order", "tie", "cur", "other"}
	fps = make(map[string]domain.CandidateFingerprintVal, len(names))
	for i, n := range names {
		var fp domain.CandidateFingerprintVal
		for j := range fp {
			fp[j] = byte(i*31 + j)
		}
		fp[0] = byte(i + 1)
		fps[n] = fp
	}
	return rcA, rcB, qc1, qc2, fps
}

func TestRoutingQualityWindowCurrentPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := windowTestMinute
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))

	rcA, rcB, qc1, qc2, fps := windowVals(t)
	fpk := func(n string) string { v := fps[n]; return string(v[:]) }

	// Multi-quality-class summation inside the window.
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], m.Add(-4*time.Minute), 10, 7)
	seedQualityFactRow(t, pool, rcA, qc2, fps["cur"], m.Add(-2*time.Minute), 20, 11)
	// Half-open bounds: [M-5m, M) — lower edge included, M excluded, older excluded.
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], m.Add(-5*time.Minute), 5, 5)
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], m, 100, 100)
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], m.Add(-6*time.Minute), 100, 100)
	// Other route isolated.
	seedQualityFactRow(t, pool, rcB, qc1, fps["cur"], m.Add(-1*time.Minute), 3, 3)

	got, err := repos.Partitions.QueryCurrentWindowStats(ctx, 1, m)
	require.NoError(t, err)
	byKey := map[string]repository.WindowCurrentStat{}
	for _, s := range got {
		byKey[string(s.RouteClassID[:])+string(s.Fingerprint[:])] = s
	}
	require.Len(t, got, 2)
	cur := byKey[string(rcA[:])+fpk("cur")]
	require.Equal(t, int64(35), cur.Attempts)
	require.Equal(t, int64(23), cur.Successes)
	require.Equal(t, int64(35), cur.TTFTN)
	require.Equal(t, int64(35), cur.InputTokens)
	require.Equal(t, int64(23), cur.OutputTokens)
	other := byKey[string(rcB[:])+fpk("cur")]
	require.Equal(t, int64(3), other.Attempts)

	// Identity-version mismatch matches nothing (never an error).
	empty, err := repos.Partitions.QueryCurrentWindowStats(ctx, 2, m)
	require.NoError(t, err)
	require.Empty(t, empty)

	// Repository facade delegates.
	gotFacade, err := repos.QueryCurrentWindowStats(ctx, 1, m)
	require.NoError(t, err)
	require.Len(t, gotFacade, 2)
}

func TestRoutingQualityWindowBaselinePG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := windowTestMinute
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))

	rcA, _, qc1, qc2, fps := windowVals(t)
	fpk := func(n string) string { v := fps[n]; return string(v[:]) }
	hot := func(names ...string) []repository.WindowHotKey {
		out := make([]repository.WindowHotKey, 0, len(names))
		for _, n := range names {
			out = append(out, repository.WindowHotKey{RouteClassID: rcA, Fingerprint: fps[n]})
		}
		return out
	}

	// Truncation goldens 29/30/31 (single newest row).
	seedQualityFactRow(t, pool, rcA, qc1, fps["n29"], m.Add(-6*time.Minute), 29, 20)
	seedQualityFactRow(t, pool, rcA, qc1, fps["n30"], m.Add(-6*time.Minute), 30, 21)
	seedQualityFactRow(t, pool, rcA, qc1, fps["n31"], m.Add(-6*time.Minute), 31, 22)
	// Split across minutes: 20 newest + 20 older → both kept (40).
	seedQualityFactRow(t, pool, rcA, qc1, fps["split"], m.Add(-6*time.Minute), 20, 14)
	seedQualityFactRow(t, pool, rcA, qc1, fps["split"], m.Add(-60*time.Minute), 20, 13)
	// Cut: 30 newest + 50 older → older excluded (running-attempts 30, not < 30).
	seedQualityFactRow(t, pool, rcA, qc1, fps["cut"], m.Add(-6*time.Minute), 30, 19)
	seedQualityFactRow(t, pool, rcA, qc1, fps["cut"], m.Add(-60*time.Minute), 50, 40)
	// Outside the baseline window entirely.
	seedQualityFactRow(t, pool, rcA, qc1, fps["old"], m.Add(-25*time.Hour), 100, 90)
	seedQualityFactRow(t, pool, rcA, qc1, fps["old"], m.Add(-4*time.Minute), 100, 90)
	// Cold candidate: hot-key restriction excludes it despite attempts.
	seedQualityFactRow(t, pool, rcA, qc1, fps["cold"], m.Add(-6*time.Minute), 100, 90)
	// Newest-first: inserted oldest-first, truncation still follows minute order
	// (25 newest + 25 + 25 oldest → 50, oldest excluded).
	seedQualityFactRow(t, pool, rcA, qc1, fps["order"], m.Add(-3*time.Hour), 25, 20)
	seedQualityFactRow(t, pool, rcA, qc1, fps["order"], m.Add(-2*time.Hour), 25, 20)
	seedQualityFactRow(t, pool, rcA, qc1, fps["order"], m.Add(-6*time.Minute), 25, 20)
	// Same-minute multi-quality-class tie: 20 + 20 → 40 deterministically.
	seedQualityFactRow(t, pool, rcA, qc1, fps["tie"], m.Add(-10*time.Minute), 20, 15)
	seedQualityFactRow(t, pool, rcA, qc2, fps["tie"], m.Add(-10*time.Minute), 20, 15)

	keys := hot("n29", "n30", "n31", "split", "cut", "old", "order", "tie")
	got, err := repos.Partitions.QueryBaselineTruncated(ctx, 1, m, keys)
	require.NoError(t, err)
	byFP := map[string]repository.WindowBaselineStat{}
	for _, s := range got {
		require.Equal(t, rcA, s.RouteClassID)
		byFP[string(s.Fingerprint[:])] = s
	}
	require.Len(t, got, 7, "old is window-excluded, cold is hot-key-excluded")
	require.Equal(t, int64(29), byFP[fpk("n29")].Attempts)
	require.Equal(t, int64(20), byFP[fpk("n29")].Successes)
	require.Equal(t, int64(30), byFP[fpk("n30")].Attempts)
	require.Equal(t, int64(31), byFP[fpk("n31")].Attempts)
	require.Equal(t, int64(40), byFP[fpk("split")].Attempts)
	require.Equal(t, int64(27), byFP[fpk("split")].Successes)
	require.Equal(t, int64(30), byFP[fpk("cut")].Attempts)
	require.Equal(t, int64(19), byFP[fpk("cut")].Successes)
	require.Equal(t, int64(50), byFP[fpk("order")].Attempts)
	require.Equal(t, int64(40), byFP[fpk("tie")].Attempts)
	require.Equal(t, int64(30), byFP[fpk("tie")].Successes)

	// Empty hotKeys short-circuits without querying.
	empty, err := repos.Partitions.QueryBaselineTruncated(ctx, 1, m, nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	// Identity-version mismatch matches nothing.
	empty, err = repos.Partitions.QueryBaselineTruncated(ctx, 2, m, keys)
	require.NoError(t, err)
	require.Empty(t, empty)

	// Repository facade delegates.
	gotFacade, err := repos.QueryBaselineTruncated(ctx, 1, m, keys)
	require.NoError(t, err)
	require.Len(t, gotFacade, 7)
}

func TestRoutingQualityWindowProbeIndexExistsPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	var n int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'routing_quality_fact' AND indexname = 'routing_quality_fact_uniq'`).Scan(&n))
	require.Equal(t, int64(1), n, "identity index must ship in fact index DDLs; it serves the baseline LATERAL probe")
}
