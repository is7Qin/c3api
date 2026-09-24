// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
)

// Routing fact read face (repository lane): aggregate reads over
// routing_quality_fact / routing_flow_fact ONLY — never instance tables,
// never raw usage/err logs. Half-open window [from, to) on the bucket column,
// direct route_class_id filter, SUM aggregation grouped by
// the complete edge/candidate identity, deterministic ORDER BY, non-nil empty
// results.

// RoutingQualityStat is one candidate identity aggregated over the window
// (route_class_id, quality_class_id, candidate_fingerprint).
type RoutingQualityStat struct {
	RouteClassID         domain.RouteClassIDVal
	QualityClassID       domain.QualityClassIDVal
	CandidateFingerprint domain.CandidateFingerprintVal
	Attempts             int64
	Successes            int64
	Count429             int64
	CountOrdinary4xx     int64
	Count5xx             int64
	CountNetwork         int64
	TTFTN                int64
	TTFTSumLogQ32        int64
	TTFTSumSqLogQ32      int64
	TTFTHist             []int64
	InputTokens          int64
	OutputTokens         int64
	CacheReadTokens      int64
	CacheCreateTokens    int64
	Calls                int64
	Images               int64
}

// RoutingFlowStat is one merged edge identity aggregated over the window
// (identity = every non-aggregated merged-layer dimension — instance_src and
// min_generation excluded; chain_count is the cross-shard SUM,
// min_generation the cross-shard MIN).
type RoutingFlowStat struct {
	RouteClassID      domain.RouteClassIDVal
	Ordinal           int16
	Lane              string
	AccountID         int64
	PreviousAccountID *int64
	PreviousOutcome   string
	TransitionReason  string
	Outcome           string
	IsTerminal        bool
	MinGeneration     int64
	ChainCount        int64
}

// qualityFactStatsSQL sums every measure per candidate identity; the bigint[]
// histogram is element-wise summed via generate_subscripts and reassembled in
// index order (ARRAY_AGG ... ORDER BY idx) so the merge is order-deterministic.
// bytea ORDER BY is binary comparison — deterministic across collations.
// 底表为单一分片事实表 routing_quality_fact（度量全可加，跨 instance_src 直接
// 求和即等价于旧合并层）。
const qualityFactStatsSQL = `
WITH facts AS (
	SELECT route_class_id, quality_class_id, candidate_fingerprint,
		attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network,
		ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist,
		input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images
	FROM routing_quality_fact
	WHERE route_class_id = $1 AND bucket_minute >= $2 AND bucket_minute < $3
), hist_elem AS (
	SELECT route_class_id, quality_class_id, candidate_fingerprint, idx, SUM(ttft_hist[idx])::bigint AS v
	FROM facts, generate_subscripts(facts.ttft_hist, 1) AS idx
	GROUP BY 1, 2, 3, 4
), hist AS (
	SELECT route_class_id, quality_class_id, candidate_fingerprint, ARRAY_AGG(v ORDER BY idx)::text AS hist_text
	FROM hist_elem
	GROUP BY 1, 2, 3
)
SELECT f.route_class_id, f.quality_class_id, f.candidate_fingerprint,
	SUM(f.attempts)::bigint, SUM(f.successes)::bigint, SUM(f.count_429)::bigint,
	SUM(f.count_ordinary_4xx)::bigint, SUM(f.count_5xx)::bigint, SUM(f.count_network)::bigint,
	SUM(f.ttft_n)::bigint, SUM(f.ttft_sum_log_q32)::bigint, SUM(f.ttft_sumsq_log_q32)::bigint,
	SUM(f.input_tokens)::bigint, SUM(f.output_tokens)::bigint, SUM(f.cache_read_tokens)::bigint,
	SUM(f.cache_create_tokens)::bigint, SUM(f.calls)::bigint, SUM(f.images)::bigint,
	COALESCE(MAX(h.hist_text), '')
FROM facts f
LEFT JOIN hist h ON h.route_class_id = f.route_class_id AND h.quality_class_id = f.quality_class_id AND h.candidate_fingerprint = f.candidate_fingerprint
GROUP BY f.route_class_id, f.quality_class_id, f.candidate_fingerprint
ORDER BY f.quality_class_id, f.candidate_fingerprint`

// flowFactStatsSQL groups by the merged edge identity (every dimension of
// routing_flow_fact_uniq except terminal_minute/instance_src)
// and aggregates cross-shard: SUM(chain_count), MIN(min_generation).
// NULL previous_account_id pinned first for a total order. The access path is
// (route_class_id, terminal_minute range), served by the
// routing_flow_fact_read index (A14 asserts via EXPLAIN).
const flowFactStatsSQL = `
SELECT route_class_id, ordinal, lane, account_id, previous_account_id, previous_outcome,
	transition_reason, outcome, is_terminal,
	MIN(min_generation)::bigint, SUM(chain_count)::bigint
FROM routing_flow_fact
WHERE route_class_id = $1 AND terminal_minute >= $2 AND terminal_minute < $3
GROUP BY route_class_id, ordinal, lane, account_id, previous_account_id, previous_outcome,
	transition_reason, outcome, is_terminal
ORDER BY ordinal, lane, account_id, previous_account_id NULLS FIRST, previous_outcome,
	transition_reason, outcome, is_terminal DESC`

// QueryQualityFactStats aggregates routing_quality_fact over the half-open
// minute window [from, to) for one route class. The read is not
// version-scoped: the DB has no identity_version dimension.
func (r *PartitionRepo) QueryQualityFactStats(ctx context.Context, routeClass domain.RouteClassIDVal, from, to time.Time) ([]RoutingQualityStat, error) {
	from, to = from.UTC().Truncate(time.Minute), to.UTC().Truncate(time.Minute)
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, qualityFactStatsSQL, []any{routeClass[:], from, to}, rows); err != nil {
		return nil, fmt.Errorf("routing quality fact query: %w", err)
	}
	defer rows.Close()
	out := []RoutingQualityStat{}
	for rows.Next() {
		var s RoutingQualityStat
		var rt, qc, fp []byte
		var histText string
		if err := rows.Scan(&rt, &qc, &fp,
			&s.Attempts, &s.Successes, &s.Count429, &s.CountOrdinary4xx, &s.Count5xx, &s.CountNetwork,
			&s.TTFTN, &s.TTFTSumLogQ32, &s.TTFTSumSqLogQ32,
			&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheCreateTokens, &s.Calls, &s.Images,
			&histText); err != nil {
			return nil, fmt.Errorf("routing quality fact query: scan: %w", err)
		}
		copy(s.RouteClassID[:], rt)
		copy(s.QualityClassID[:], qc)
		copy(s.CandidateFingerprint[:], fp)
		s.TTFTHist = parseRoutingHist(histText)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("routing quality fact query: %w", err)
	}
	return out, nil
}

// QueryFlowFactStats aggregates routing_flow_fact over the half-open minute
// window [from, to) for one route class, one row per
// complete edge identity. The read is not version-scoped.
func (r *PartitionRepo) QueryFlowFactStats(ctx context.Context, routeClass domain.RouteClassIDVal, from, to time.Time) ([]RoutingFlowStat, error) {
	from, to = from.UTC().Truncate(time.Minute), to.UTC().Truncate(time.Minute)
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, flowFactStatsSQL, []any{routeClass[:], from, to}, rows); err != nil {
		return nil, fmt.Errorf("routing flow fact query: %w", err)
	}
	defer rows.Close()
	out := []RoutingFlowStat{}
	for rows.Next() {
		var s RoutingFlowStat
		var rt []byte
		var prevAcc sql.NullInt64
		if err := rows.Scan(&rt, &s.Ordinal, &s.Lane, &s.AccountID, &prevAcc, &s.PreviousOutcome,
			&s.TransitionReason, &s.Outcome, &s.IsTerminal, &s.MinGeneration, &s.ChainCount); err != nil {
			return nil, fmt.Errorf("routing flow fact query: scan: %w", err)
		}
		copy(s.RouteClassID[:], rt)
		if prevAcc.Valid {
			v := prevAcc.Int64
			s.PreviousAccountID = &v
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("routing flow fact query: %w", err)
	}
	return out, nil
}

// QueryQualityFactStats 组合面委托（service.Store 能力探测经此达 Partitions）。
func (r *Repository) QueryQualityFactStats(ctx context.Context, routeClass domain.RouteClassIDVal, from, to time.Time) ([]RoutingQualityStat, error) {
	return r.Partitions.QueryQualityFactStats(ctx, routeClass, from, to)
}

// QueryFlowFactStats 组合面委托（同上）。
func (r *Repository) QueryFlowFactStats(ctx context.Context, routeClass domain.RouteClassIDVal, from, to time.Time) ([]RoutingFlowStat, error) {
	return r.Partitions.QueryFlowFactStats(ctx, routeClass, from, to)
}

// parseRoutingHist parses a Postgres bigint[] literal ("{1,2,3}") into a slice;
// empty/garbage-free input only ("" or "{}" → nil).
func parseRoutingHist(text string) []int64 {
	if len(text) < 2 || text[0] != '{' || text[len(text)-1] != '}' {
		return nil
	}
	trim := text[1 : len(text)-1]
	if trim == "" {
		return nil
	}
	parts := strings.Split(trim, ",")
	out := make([]int64, len(parts))
	for i, p := range parts {
		var v int64
		fmt.Sscanf(strings.TrimSpace(p), "%d", &v)
		out[i] = v
	}
	return out
}
