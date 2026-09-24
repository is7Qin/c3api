// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
)

// Windowed quality reads (incident-wiring): the settled-PG half of
// the compiler's windowed data path. Both queries read routing_quality_fact
// (the single-shard fact table; the legacy instance + rollup pair is no longer
// read) and return one row per (route_class_id, candidate_fingerprint), summed
// across quality classes and instance shards — the compiler keys quality by
// (route, fingerprint) without quality class.
//
// Q1 (current): half-open [M-5m, M), full sufficient stats for lanes
// (classify + cost). Served by the routing_quality_fact_bucket index
// (bucket_minute) + partition pruning.
// Q2 (baseline): half-open [M-24h, M-5m), attempts+successes only
// (incidents use success intervals), truncated newest→oldest at
// attempts ≥ 30 IN SQL via a running SUM window, restricted to hotKeys
// (candidates with current attempts ≥ 30 — the provider computes them from
// the merged current). Requires the routing_quality_fact_uniq index, whose
// leading (route_class_id, candidate_fingerprint, bucket_minute) serves the
// LATERAL probe once per hot pair.

// WindowHotKey is one baseline-eligible candidate: current attempts ≥ 30.
type WindowHotKey struct {
	RouteClassID domain.RouteClassIDVal
	Fingerprint  domain.CandidateFingerprintVal
}

// WindowCurrentStat is one candidate aggregated over [M-5m, M).
type WindowCurrentStat struct {
	RouteClassID      domain.RouteClassIDVal
	Fingerprint       domain.CandidateFingerprintVal
	Attempts          int64
	Successes         int64
	TTFTN             int64
	TTFTSumLogQ32     int64
	TTFTSumSqLogQ32   int64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// WindowBaselineStat is one hot candidate truncated to attempts ≥ 30 newest-first.
type WindowBaselineStat struct {
	RouteClassID domain.RouteClassIDVal
	Fingerprint  domain.CandidateFingerprintVal
	Attempts     int64
	Successes    int64
}

// routingQualityWindowCurrentSQL sums every lane measure per (route, fp).
// bytea ORDER BY is binary comparison — deterministic across collations.
const routingQualityWindowCurrentSQL = `
SELECT route_class_id, candidate_fingerprint,
	SUM(attempts)::bigint, SUM(successes)::bigint, SUM(ttft_n)::bigint,
	SUM(ttft_sum_log_q32)::bigint, SUM(ttft_sumsq_log_q32)::bigint,
	SUM(input_tokens)::bigint, SUM(output_tokens)::bigint,
	SUM(cache_read_tokens)::bigint, SUM(cache_create_tokens)::bigint
FROM routing_quality_fact
WHERE bucket_minute >= $1 AND bucket_minute < $2
GROUP BY 1, 2
ORDER BY 1, 2`

// routingQualityWindowBaselineSQL truncates each hot (route, fp) newest→oldest
// at attempts ≥ 30: the running SUM includes the current row, so
// running-attempts is the total strictly newer — rows keep going while it is
// < 30, i.e. the row reaching ≥ 30 is included and older rows excluded,
// exactly matching scheduler.AccumulateBaseline add-then-break. The
// quality_class_id tiebreaker keeps same-minute multi-class rows
// deterministic. hotKeys arrive as parallel hex text arrays zipped by
// multi-argument unnest; decode() is immutable so the LATERAL inner equality
// on (route_class_id, candidate_fingerprint) + bucket_minute range probes the
// (route_class_id, candidate_fingerprint, bucket_minute) identity index once
// per hot pair instead of seq-scanning the 24h window.
//
// The inner aggregation is load-bearing, not decoration: the fact table is
// sharded by instance_src, so the raw scan has an extra row dimension. The
// truncation predicate accumulates newest→oldest until the cumulative of
// strictly-newer rows reaches 30, and the boundary lands on the OLDEST
// included minute — an extra row per minute moves that boundary and silently
// changes the returned values (proven counterexample: baseline (38,15) vs
// un-aggregated (33,13)). Folding back to one row per (bucket_minute,
// quality_class_id) restores the exact row set, order and values of the old
// merged rollup. Cost: one HashAggregate + one extra Sort per hot pair.
const routingQualityWindowBaselineSQL = `
WITH hot AS (
	SELECT decode(rc, 'hex') AS rc, decode(fp, 'hex') AS fp
	FROM unnest($1::text[], $2::text[]) AS t(rc, fp)
)
SELECT hot.rc AS route_class_id, hot.fp AS candidate_fingerprint,
	SUM(sub.attempts)::bigint, SUM(sub.successes)::bigint
FROM hot,
LATERAL (
	SELECT m.attempts, m.successes,
		SUM(m.attempts) OVER (
			ORDER BY m.bucket_minute DESC, m.quality_class_id
			ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		) AS running
	FROM (
		-- 先把分片折叠回基线 rollup 的行粒度；此后行集、行序、谓词逐行一致。
		SELECT r.bucket_minute, r.quality_class_id,
			SUM(r.attempts)::bigint  AS attempts,
			SUM(r.successes)::bigint AS successes
		FROM routing_quality_fact r
		WHERE r.route_class_id = hot.rc
			AND r.candidate_fingerprint = hot.fp
			AND r.bucket_minute >= $3 AND r.bucket_minute < $4
		GROUP BY r.bucket_minute, r.quality_class_id
	) AS m
) AS sub
WHERE sub.running - sub.attempts < 30
GROUP BY 1, 2
ORDER BY 1, 2`

// QueryCurrentWindowStats aggregates routing_quality_fact over [M-5m, M).
// The read is not version-scoped: the DB has no identity_version dimension —
// versioning lives in the identity hash's first byte, so different versions
// land on different route/fp rows.
func (r *PartitionRepo) QueryCurrentWindowStats(ctx context.Context, evaluatedMinute time.Time) ([]WindowCurrentStat, error) {
	m := evaluatedMinute.UTC().Truncate(time.Minute)
	from, to := m.Add(-domain.CurrentWindowLen), m
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, routingQualityWindowCurrentSQL, []any{from, to}, rows); err != nil {
		return nil, fmt.Errorf("routing current window query: %w", err)
	}
	defer rows.Close()
	out := []WindowCurrentStat{}
	for rows.Next() {
		var s WindowCurrentStat
		var rt, fp []byte
		if err := rows.Scan(&rt, &fp,
			&s.Attempts, &s.Successes, &s.TTFTN, &s.TTFTSumLogQ32, &s.TTFTSumSqLogQ32,
			&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheCreateTokens); err != nil {
			return nil, fmt.Errorf("routing current window query: scan: %w", err)
		}
		copy(s.RouteClassID[:], rt)
		copy(s.Fingerprint[:], fp)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("routing current window query: %w", err)
	}
	return out, nil
}

// QueryBaselineTruncated aggregates routing_quality_fact over [M-24h, M-5m)
// for hotKeys only, truncated newest→oldest at attempts ≥ 30. Empty hotKeys
// short-circuit without querying.
func (r *PartitionRepo) QueryBaselineTruncated(ctx context.Context, evaluatedMinute time.Time, hotKeys []WindowHotKey) ([]WindowBaselineStat, error) {
	if len(hotKeys) == 0 {
		return []WindowBaselineStat{}, nil
	}
	m := evaluatedMinute.UTC().Truncate(time.Minute)
	from, to := m.Add(-domain.BaselineLookback), m.Add(-domain.CurrentWindowLen)
	rcHex := make([]string, 0, len(hotKeys))
	fpHex := make([]string, 0, len(hotKeys))
	for _, k := range hotKeys {
		rcHex = append(rcHex, hex.EncodeToString(k.RouteClassID[:]))
		fpHex = append(fpHex, hex.EncodeToString(k.Fingerprint[:]))
	}
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, routingQualityWindowBaselineSQL, []any{rcHex, fpHex, from, to}, rows); err != nil {
		return nil, fmt.Errorf("routing baseline window query: %w", err)
	}
	defer rows.Close()
	out := []WindowBaselineStat{}
	for rows.Next() {
		var s WindowBaselineStat
		var rt, fp []byte
		if err := rows.Scan(&rt, &fp, &s.Attempts, &s.Successes); err != nil {
			return nil, fmt.Errorf("routing baseline window query: scan: %w", err)
		}
		copy(s.RouteClassID[:], rt)
		copy(s.Fingerprint[:], fp)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("routing baseline window query: %w", err)
	}
	return out, nil
}

// QueryCurrentWindowStats 组合面委托（service.Store 能力探测经此达 Partitions）。
func (r *Repository) QueryCurrentWindowStats(ctx context.Context, evaluatedMinute time.Time) ([]WindowCurrentStat, error) {
	return r.Partitions.QueryCurrentWindowStats(ctx, evaluatedMinute)
}

// QueryBaselineTruncated 组合面委托（同上）。
func (r *Repository) QueryBaselineTruncated(ctx context.Context, evaluatedMinute time.Time, hotKeys []WindowHotKey) ([]WindowBaselineStat, error) {
	return r.Partitions.QueryBaselineTruncated(ctx, evaluatedMinute, hotKeys)
}
