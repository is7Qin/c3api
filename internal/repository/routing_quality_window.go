// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"
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
// at $5 attempts: the running SUM includes the current row, so
// running-attempts is the total strictly newer — rows keep going while it is
// < $5, i.e. the row reaching ≥ $5 is included and older rows excluded,
// exactly matching scheduler.AccumulateBaseline add-then-break. The
// quality_class_id tiebreaker keeps same-minute multi-class rows
// deterministic. hotKeys arrive as parallel hex text arrays zipped by
// multi-argument unnest; decode() is immutable so the LATERAL inner equality
// on (route_class_id, candidate_fingerprint) + bucket_minute range probes the
// identity index once per hot pair instead of seq-scanning the window.
//
// **[$3,$4) 是调用方选定的「前缀」，不必然是整段回看。** 截断谓词在「按分钟
// 从新到老」的序上是单调停止条件（attempts ≥ 0），故它定义的是整段回看的一个
// 前缀；拿整段 24h 去算它 = 把前缀长度当常量，读 H·1435·N 行只用 H·k·N 行
// （k = 累计到 $5 attempts 所需分钟数）。实测（H=5000、N=3）：整段回看
// 19,459 ms vs 最近 30m 442 ms（44×），且结果**逐字段相等**。
//
// window_attempts 回传该前缀内的**未截断**总 attempts，供调用方判断截断是否
// 已在前缀内完成（完成 ⇔ 与整段回看等价，见 QueryBaselineTruncated）。
//
// The inner aggregation is load-bearing, not decoration: the fact table is
// sharded by instance_src, so the raw scan has an extra row dimension. The
// truncation predicate accumulates newest→oldest until the cumulative of
// strictly-newer rows reaches $5, and the boundary lands on the OLDEST
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
	COALESCE(SUM(sub.attempts)  FILTER (WHERE sub.running - sub.attempts < $5::bigint), 0)::bigint,
	COALESCE(SUM(sub.successes) FILTER (WHERE sub.running - sub.attempts < $5::bigint), 0)::bigint,
	COALESCE(SUM(sub.attempts), 0)::bigint AS window_attempts
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

// QueryBaselineTruncated aggregates routing_quality_fact over the lookback
// ending at M-CurrentWindowLen for hotKeys only, truncated newest→oldest at
// domain.BaselineTruncateAttempts. Empty hotKeys short-circuit without querying.
//
// 分两轮，语义与「整段回看一次算完」逐字段相等：
//
//  1. 只探最近 domain.BaselineProbeLen。前缀内累计已达阈值 ⇔ 截断边界落在该前缀
//     内 ⇔ 与整段回看结果相同（更老的分钟两边都被排除），故这些候选直接采用。
//  2. 其余候选（前缀内不足阈值——历史稀疏、或近期才起量；含在前缀内根本没有行的
//     候选）才回落整段回看。这一轮罕见，且这些候选历史本就稀疏，多出的扫描很小。
//
// 轮 1 的成本是 O(H·BaselineProbeLen·N)，与 24h 回看无关——这正是 A10 预算的解法。
func (r *PartitionRepo) QueryBaselineTruncated(ctx context.Context, evaluatedMinute time.Time, hotKeys []WindowHotKey) ([]WindowBaselineStat, error) {
	if len(hotKeys) == 0 {
		return []WindowBaselineStat{}, nil
	}
	m := evaluatedMinute.UTC().Truncate(time.Minute)
	to := m.Add(-domain.CurrentWindowLen)

	// 去重：旧实现靠 SQL GROUP BY 收敛重复键，行为必须保持。
	uniq := make([]WindowHotKey, 0, len(hotKeys))
	seen := make(map[WindowHotKey]struct{}, len(hotKeys))
	for _, k := range hotKeys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
	}

	out := make([]WindowBaselineStat, 0, len(uniq))
	first, err := r.baselineProbe(ctx, uniq, to.Add(-domain.BaselineProbeLen), to)
	if err != nil {
		return nil, err
	}
	need := make([]WindowHotKey, 0)
	for _, k := range uniq {
		p, ok := first[k]
		if ok && p.windowAttempts >= domain.BaselineTruncateAttempts {
			out = append(out, WindowBaselineStat{
				RouteClassID: k.RouteClassID,
				Fingerprint:  k.Fingerprint,
				Attempts:     p.attempts,
				Successes:    p.successes,
			})
			continue
		}
		need = append(need, k)
	}
	if len(need) > 0 {
		rest, err := r.baselineProbe(ctx, need, m.Add(-domain.BaselineLookback), to)
		if err != nil {
			return nil, err
		}
		for _, k := range need {
			p, ok := rest[k]
			if !ok {
				continue // 整段回看内无行 ⇒ 无基线（与旧行为一致）
			}
			out = append(out, WindowBaselineStat{
				RouteClassID: k.RouteClassID,
				Fingerprint:  k.Fingerprint,
				Attempts:     p.attempts,
				Successes:    p.successes,
			})
		}
	}
	// 旧实现由 SQL ORDER BY 1,2 定序；两轮合并后需自行恢复该确定性。
	sort.Slice(out, func(i, j int) bool {
		if c := bytes.Compare(out[i].RouteClassID[:], out[j].RouteClassID[:]); c != 0 {
			return c < 0
		}
		return bytes.Compare(out[i].Fingerprint[:], out[j].Fingerprint[:]) < 0
	})
	return out, nil
}

// baselineProbeRow 是基线探测的一行：截断后的 attempts/successes，外加该前缀内
// **未截断**的 attempts 总数——后者是「截断是否已在前缀内完成」的判据。
type baselineProbeRow struct {
	attempts       int64
	successes      int64
	windowAttempts int64
}

// baselineProbe 在窗口 [from,to) 内跑一次截断探测，按候选键返回。
// 该窗口内无行的键**不会**出现在结果里，调用方必须按「缺席」处理。
func (r *PartitionRepo) baselineProbe(ctx context.Context, keys []WindowHotKey, from, to time.Time) (map[WindowHotKey]baselineProbeRow, error) {
	rcHex := make([]string, 0, len(keys))
	fpHex := make([]string, 0, len(keys))
	for _, k := range keys {
		rcHex = append(rcHex, hex.EncodeToString(k.RouteClassID[:]))
		fpHex = append(fpHex, hex.EncodeToString(k.Fingerprint[:]))
	}
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, routingQualityWindowBaselineSQL,
		[]any{rcHex, fpHex, from, to, int64(domain.BaselineTruncateAttempts)}, rows); err != nil {
		return nil, fmt.Errorf("routing baseline window query: %w", err)
	}
	defer rows.Close()
	out := make(map[WindowHotKey]baselineProbeRow, len(keys))
	for rows.Next() {
		var p baselineProbeRow
		var rt, fp []byte
		if err := rows.Scan(&rt, &fp, &p.attempts, &p.successes, &p.windowAttempts); err != nil {
			return nil, fmt.Errorf("routing baseline window query: scan: %w", err)
		}
		var k WindowHotKey
		copy(k.RouteClassID[:], rt)
		copy(k.Fingerprint[:], fp)
		out[k] = p
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
