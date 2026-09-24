// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"encoding/hex"
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

func seedQualityFactRow(t *testing.T, pool *pgxpool.Pool, rc domain.RouteClassIDVal, qc domain.QualityClassIDVal, fp domain.CandidateFingerprintVal, instanceSrc string, minute time.Time, attempts, successes int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO routing_quality_fact
		 (identity_version, route_class_id, quality_class_id, candidate_fingerprint,
		  instance_src, bucket_minute, absolute_sequence, attempts, successes, ttft_n,
		  ttft_sum_log_q32, ttft_sumsq_log_q32,
		  input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, updated_at)
		 VALUES (1, $1, $2, $3, $4, $5, 1, $6, $7, $6, 0, 0, $6, $7, 0, 0, now())`,
		rc[:], qc[:], fp[:], instanceSrc, minute.UTC(), attempts, successes)
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
	names := []string{"n29", "n30", "n31", "split", "cut", "old", "cold", "order", "tie", "cur", "other", "straddle", "equal", "single"}
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
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], "src-seed", m.Add(-4*time.Minute), 10, 7)
	seedQualityFactRow(t, pool, rcA, qc2, fps["cur"], "src-seed", m.Add(-2*time.Minute), 20, 11)
	// Half-open bounds: [M-5m, M) — lower edge included, M excluded, older excluded.
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], "src-seed", m.Add(-5*time.Minute), 5, 5)
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], "src-seed", m, 100, 100)
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], "src-seed", m.Add(-6*time.Minute), 100, 100)
	// Other route isolated.
	seedQualityFactRow(t, pool, rcB, qc1, fps["cur"], "src-seed", m.Add(-1*time.Minute), 3, 3)

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

// The current-window read must fold instance shards. The same
// (bucket_minute, quality_class_id, candidate_fingerprint) published from two
// distinct instance_src values is one candidate-minute, so the read returns a
// single row per candidate with the metrics summed across both shards. If
// instance_src ever leaked into the SQL's GROUP BY, this would silently return
// two rows per candidate and corrupt the compiler's classify/cost input.
func TestRoutingQualityWindowCurrentFoldsShardsPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := windowTestMinute
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))

	rcA, _, qc1, _, fps := windowVals(t)
	fpk := func(n string) string { v := fps[n]; return string(v[:]) }

	// Same (bucket_minute, quality_class_id, candidate_fingerprint), two shards.
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], "src-a", m.Add(-3*time.Minute), 10, 6)
	seedQualityFactRow(t, pool, rcA, qc1, fps["cur"], "src-b", m.Add(-3*time.Minute), 4, 3)

	got, err := repos.Partitions.QueryCurrentWindowStats(ctx, 1, m)
	require.NoError(t, err)
	require.Len(t, got, 1, "one candidate row per (route, fp), not one per instance shard")
	require.Equal(t, rcA, got[0].RouteClassID)
	require.Equal(t, fpk("cur"), string(got[0].Fingerprint[:]))
	require.Equal(t, int64(14), got[0].Attempts, "attempts summed across both shards")
	require.Equal(t, int64(9), got[0].Successes, "successes summed across both shards")
	require.Equal(t, int64(14), got[0].TTFTN)
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
	seedQualityFactRow(t, pool, rcA, qc1, fps["n29"], "src-seed", m.Add(-6*time.Minute), 29, 20)
	seedQualityFactRow(t, pool, rcA, qc1, fps["n30"], "src-seed", m.Add(-6*time.Minute), 30, 21)
	seedQualityFactRow(t, pool, rcA, qc1, fps["n31"], "src-seed", m.Add(-6*time.Minute), 31, 22)
	// Split across minutes: 20 newest + 20 older → both kept (40).
	seedQualityFactRow(t, pool, rcA, qc1, fps["split"], "src-seed", m.Add(-6*time.Minute), 20, 14)
	seedQualityFactRow(t, pool, rcA, qc1, fps["split"], "src-seed", m.Add(-60*time.Minute), 20, 13)
	// Cut: 30 newest + 50 older → older excluded (running-attempts 30, not < 30).
	seedQualityFactRow(t, pool, rcA, qc1, fps["cut"], "src-seed", m.Add(-6*time.Minute), 30, 19)
	seedQualityFactRow(t, pool, rcA, qc1, fps["cut"], "src-seed", m.Add(-60*time.Minute), 50, 40)
	// Outside the baseline window entirely.
	seedQualityFactRow(t, pool, rcA, qc1, fps["old"], "src-seed", m.Add(-25*time.Hour), 100, 90)
	seedQualityFactRow(t, pool, rcA, qc1, fps["old"], "src-seed", m.Add(-4*time.Minute), 100, 90)
	// Cold candidate: hot-key restriction excludes it despite attempts.
	seedQualityFactRow(t, pool, rcA, qc1, fps["cold"], "src-seed", m.Add(-6*time.Minute), 100, 90)
	// Newest-first: inserted oldest-first, truncation still follows minute order
	// (25 newest + 25 + 25 oldest → 50, oldest excluded).
	seedQualityFactRow(t, pool, rcA, qc1, fps["order"], "src-seed", m.Add(-3*time.Hour), 25, 20)
	seedQualityFactRow(t, pool, rcA, qc1, fps["order"], "src-seed", m.Add(-2*time.Hour), 25, 20)
	seedQualityFactRow(t, pool, rcA, qc1, fps["order"], "src-seed", m.Add(-6*time.Minute), 25, 20)
	// Same-minute multi-quality-class tie: 20 + 20 → 40 deterministically.
	seedQualityFactRow(t, pool, rcA, qc1, fps["tie"], "src-seed", m.Add(-10*time.Minute), 20, 15)
	seedQualityFactRow(t, pool, rcA, qc2, fps["tie"], "src-seed", m.Add(-10*time.Minute), 20, 15)

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

// TestRoutingQualityWindowBaselineShardStraddlePG is the A4 tripwire: it proves
// the inner GROUP BY (bucket_minute, quality_class_id) in
// routingQualityWindowBaselineSQL is load-bearing, not decoration.
//
// Why this is the most important test in the change: the fact table is sharded
// by instance_src, so a raw scan has an extra row dimension the old merged
// rollup did not. The truncation predicate accumulates newest→oldest until the
// cumulative of strictly-newer rows reaches 30, and the boundary lands on the
// OLDEST included minute. An extra row per minute moves that boundary and
// silently changes the returned values. Every other case in
// TestRoutingQualityWindowBaselinePG seeds a single instance ('src-seed'), so
// deleting the inner aggregation would still leave them green. This fixture
// seeds a straddling minute across two instances and would fail.
//
// Offsets are minutes before m; the baseline window is [M-24h, M-5m), so every
// offset is ≥ 6 (m-5 itself is the excluded upper bound).
//
//	straddle: off6=10(A), off7=10(A), off8=8(A), off9=5(A)+5(B), off10=5(A)
//	  strictly-newer sum before off9 is 28; the merged row for off9 is 10, so
//	  running = 38 and 38-10 = 28 < 30 keeps the whole minute → (38,15).
//	  WITHOUT the inner aggregation off9 has two rows (5,5): the second row sees
//	  running 38 with 38-5 = 33 ≥ 30 and is DROPPED → (33,13). Different value ⇒
//	  this candidate is the tripwire.
//	equal: off6=10(A), off7=10(A), off8=7(A)+8(B), off9=5(A)
//	  strictly-newer sum before off8 is 20; the minute totals 15 split 7+8 → both
//	  sub-rows satisfy S + max(t1,t2) = 28 < 30, so both orderings keep both rows
//	  → (35,14) either way. Control proving the tripwire is not "any split differs".
//	single: off6=10(A), off7=10(A), off8=8(A), off9=10(A), off10=5(A)
//	  one instance ⇒ no extra row dimension ⇒ (38,15) either way. Control proving
//	  the tripwire is specifically about sharding.
//
// Successes (merged granularity): straddle = 4+4+3+(2+2) = 15;
// equal = 4+4+(3+3) = 14; single = 4+4+3+4 = 15.
func TestRoutingQualityWindowBaselineShardStraddlePG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := windowTestMinute
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))

	rcA, _, qc1, _, fps := windowVals(t)
	fpk := func(n string) string { v := fps[n]; return string(v[:]) }
	const instA, instB = "inst-A", "inst-B"

	// straddle: off9 splits 5+5 across two instances (merged minute = 10/4).
	seedQualityFactRow(t, pool, rcA, qc1, fps["straddle"], instA, m.Add(-6*time.Minute), 10, 4)
	seedQualityFactRow(t, pool, rcA, qc1, fps["straddle"], instA, m.Add(-7*time.Minute), 10, 4)
	seedQualityFactRow(t, pool, rcA, qc1, fps["straddle"], instA, m.Add(-8*time.Minute), 8, 3)
	seedQualityFactRow(t, pool, rcA, qc1, fps["straddle"], instA, m.Add(-9*time.Minute), 5, 2)
	seedQualityFactRow(t, pool, rcA, qc1, fps["straddle"], instB, m.Add(-9*time.Minute), 5, 2)
	seedQualityFactRow(t, pool, rcA, qc1, fps["straddle"], instA, m.Add(-10*time.Minute), 5, 2)
	// equal: off8 splits 7+8 (merged minute = 15/6); off9 is dropped.
	seedQualityFactRow(t, pool, rcA, qc1, fps["equal"], instA, m.Add(-6*time.Minute), 10, 4)
	seedQualityFactRow(t, pool, rcA, qc1, fps["equal"], instA, m.Add(-7*time.Minute), 10, 4)
	seedQualityFactRow(t, pool, rcA, qc1, fps["equal"], instA, m.Add(-8*time.Minute), 7, 3)
	seedQualityFactRow(t, pool, rcA, qc1, fps["equal"], instB, m.Add(-8*time.Minute), 8, 3)
	seedQualityFactRow(t, pool, rcA, qc1, fps["equal"], instA, m.Add(-9*time.Minute), 5, 3)
	// single: one instance, no extra row dimension.
	seedQualityFactRow(t, pool, rcA, qc1, fps["single"], instA, m.Add(-6*time.Minute), 10, 4)
	seedQualityFactRow(t, pool, rcA, qc1, fps["single"], instA, m.Add(-7*time.Minute), 10, 4)
	seedQualityFactRow(t, pool, rcA, qc1, fps["single"], instA, m.Add(-8*time.Minute), 8, 3)
	seedQualityFactRow(t, pool, rcA, qc1, fps["single"], instA, m.Add(-9*time.Minute), 10, 4)
	seedQualityFactRow(t, pool, rcA, qc1, fps["single"], instA, m.Add(-10*time.Minute), 5, 2)

	keys := []repository.WindowHotKey{
		{RouteClassID: rcA, Fingerprint: fps["straddle"]},
		{RouteClassID: rcA, Fingerprint: fps["equal"]},
		{RouteClassID: rcA, Fingerprint: fps["single"]},
	}

	// 1. Production query (inner aggregation restored) returns the merged-granularity
	// totals. This alone fails if the inner GROUP BY is removed.
	got, err := repos.Partitions.QueryBaselineTruncated(ctx, 1, m, keys)
	require.NoError(t, err)
	byFP := map[string]repository.WindowBaselineStat{}
	for _, s := range got {
		require.Equal(t, rcA, s.RouteClassID)
		byFP[string(s.Fingerprint[:])] = s
	}
	require.Len(t, got, 3)
	require.Equal(t, int64(38), byFP[fpk("straddle")].Attempts, "straddle merged minute keeps the whole boundary minute")
	require.Equal(t, int64(15), byFP[fpk("straddle")].Successes)
	require.Equal(t, int64(35), byFP[fpk("equal")].Attempts, "equal split keeps both sub-rows under either ordering")
	require.Equal(t, int64(14), byFP[fpk("equal")].Successes)
	require.Equal(t, int64(38), byFP[fpk("single")].Attempts, "single instance has no extra row dimension")
	require.Equal(t, int64(15), byFP[fpk("single")].Successes)

	// 2. Explicit negative control: the same LATERAL with the inner GROUP BY
	// removed yields a different straddle value (33,13). This raw SQL exists only
	// to prove the tripwire has teeth — it must never be used in production (its
	// result depends on the shard row layout).
	raw := queryUnaggregatedBaseline(t, pool, m, keys)
	require.Len(t, raw, 3)
	require.Equal(t, int64(33), raw[fpk("straddle")].Attempts, "un-aggregated scan drops the boundary minute's second shard row")
	require.Equal(t, int64(13), raw[fpk("straddle")].Successes)
	require.Equal(t, int64(35), raw[fpk("equal")].Attempts, "equal split is invariant to aggregation")
	require.Equal(t, int64(14), raw[fpk("equal")].Successes)
	require.Equal(t, int64(38), raw[fpk("single")].Attempts, "single instance is invariant to aggregation")
	require.Equal(t, int64(15), raw[fpk("single")].Successes)
}

// queryUnaggregatedBaseline mirrors routingQualityWindowBaselineSQL with the
// inner GROUP BY (bucket_minute, quality_class_id) REMOVED. It exists solely as
// the negative control for TestRoutingQualityWindowBaselineShardStraddlePG and
// must never be used in production: without the pre-aggregation the truncation
// boundary moves and the returned values drift with the shard layout.
func queryUnaggregatedBaseline(t *testing.T, pool *pgxpool.Pool, m time.Time, keys []repository.WindowHotKey) map[string]repository.WindowBaselineStat {
	t.Helper()
	ctx := context.Background()
	const rawSQL = `
WITH hot AS (
	SELECT decode(rc, 'hex') AS rc, decode(fp, 'hex') AS fp
	FROM unnest($2::text[], $3::text[]) AS t(rc, fp)
)
SELECT hot.rc AS route_class_id, hot.fp AS candidate_fingerprint,
	SUM(sub.attempts)::bigint, SUM(sub.successes)::bigint
FROM hot,
LATERAL (
	SELECT r.attempts, r.successes,
		SUM(r.attempts) OVER (
			ORDER BY r.bucket_minute DESC, r.quality_class_id
			ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		) AS running
	FROM routing_quality_fact r
	WHERE r.identity_version = $1
		AND r.route_class_id = hot.rc
		AND r.candidate_fingerprint = hot.fp
		AND r.bucket_minute >= $4 AND r.bucket_minute < $5
) AS sub
WHERE sub.running - sub.attempts < 30
GROUP BY 1, 2
ORDER BY 1, 2`
	rcHex := make([]string, 0, len(keys))
	fpHex := make([]string, 0, len(keys))
	for _, k := range keys {
		rcHex = append(rcHex, hex.EncodeToString(k.RouteClassID[:]))
		fpHex = append(fpHex, hex.EncodeToString(k.Fingerprint[:]))
	}
	from, to := m.Add(-domain.BaselineLookback), m.Add(-domain.CurrentWindowLen)
	rows, err := pool.Query(ctx, rawSQL, 1, rcHex, fpHex, from, to)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]repository.WindowBaselineStat{}
	for rows.Next() {
		var s repository.WindowBaselineStat
		var rt, fp []byte
		require.NoError(t, rows.Scan(&rt, &fp, &s.Attempts, &s.Successes))
		copy(s.RouteClassID[:], rt)
		copy(s.Fingerprint[:], fp)
		out[string(s.Fingerprint[:])] = s
	}
	require.NoError(t, rows.Err())
	return out
}
