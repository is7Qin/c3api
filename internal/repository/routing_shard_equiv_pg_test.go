// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

// 为何旧路径在本文件内本地重建而不调用生产代码（WHY）：
//
// 本分支已删除旧读路径的全部生产载体：RollupQuality（重算 worker）与
// routing_quality_rollup / routing_flow_rollup 表。旧实现已无任何可调用的函数与
// 可查询的表，故“同一逻辑事件集灌入旧路径与新路径”只能把旧路径的形状在本文件内
// 重建：旧表按基线 commit（29b5b22）的列定义逐字复刻为本文件专用的对照表
// （shard_equiv_quality_rollup / shard_equiv_flow_rollup，均为普通表），
// 旧读 SQL 按同一基线的查询文本逐字复刻（仅改表名与去 identity_version 参数位）。
// 这不是线上表：identity_version 常量列是刻意的历史重建，A9 门禁不适用它
// （同 routing_flow_merged_pg_test.go 中 baseline_flow_* 表的先例）。
// 若未来有人改动新读路径的折叠语义，四处对拍必须失败；
// 若有人删掉新基线窗 SQL 中的内层 GROUP BY，阴性对照必须失败。
// 两个方向都由硬编码金值钉住，而不是互相回显。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// shardEquivMinute 是对拍夹具的评估分钟 M（UTC 整分）。
var shardEquivMinute = time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)

// shardEquivOldQualityRollupDDL 复刻基线 routing_quality_rollup 的列口径
// （含已退役的常量 identity_version 列——刻意的历史重建，不是线上表）。
const shardEquivOldQualityRollupDDL = `CREATE TABLE shard_equiv_quality_rollup (
	identity_version smallint NOT NULL CHECK (identity_version = 1),
	route_class_id bytea NOT NULL CHECK (octet_length(route_class_id) = 32),
	quality_class_id bytea NOT NULL CHECK (octet_length(quality_class_id) = 32),
	candidate_fingerprint bytea NOT NULL CHECK (octet_length(candidate_fingerprint) = 32),
	bucket_minute timestamptz NOT NULL,
	attempts bigint NOT NULL DEFAULT 0,
	successes bigint NOT NULL DEFAULT 0,
	count_429 bigint NOT NULL DEFAULT 0,
	count_ordinary_4xx bigint NOT NULL DEFAULT 0,
	count_5xx bigint NOT NULL DEFAULT 0,
	count_network bigint NOT NULL DEFAULT 0,
	ttft_n bigint NOT NULL DEFAULT 0,
	ttft_sum_log_q32 bigint NOT NULL DEFAULT 0,
	ttft_sumsq_log_q32 bigint NOT NULL DEFAULT 0,
	ttft_hist bigint[] NOT NULL DEFAULT '{0,0,0,0,0,0,0,0,0,0}',
	input_tokens bigint NOT NULL DEFAULT 0,
	output_tokens bigint NOT NULL DEFAULT 0,
	cache_read_tokens bigint NOT NULL DEFAULT 0,
	cache_create_tokens bigint NOT NULL DEFAULT 0,
	calls bigint NOT NULL DEFAULT 0,
	images bigint NOT NULL DEFAULT 0
)`

// shardEquivOldFlowRollupDDL 复刻基线 routing_flow_rollup 的列口径
// （含已退役的常量 identity_version 列——刻意的历史重建，不是线上表）。
const shardEquivOldFlowRollupDDL = `CREATE TABLE shard_equiv_flow_rollup (
	identity_version smallint NOT NULL CHECK (identity_version = 1),
	route_class_id bytea NOT NULL CHECK (octet_length(route_class_id) = 32),
	terminal_minute timestamptz NOT NULL,
	ordinal smallint NOT NULL,
	lane text NOT NULL,
	account_id bigint NOT NULL,
	previous_account_id bigint NULL,
	previous_outcome text NOT NULL DEFAULT '',
	transition_reason text NOT NULL,
	outcome text NOT NULL,
	is_terminal boolean NOT NULL,
	instance_src text NOT NULL,
	min_generation bigint NOT NULL,
	chain_count bigint NOT NULL DEFAULT 0
)`

// 以下旧读 SQL 逐字复刻基线 commit 的查询文本，仅把底表名换成对照表
// （identity_version 参数位按新模型前移/固定为 1）。
// 新读与旧读的唯一语义差是基线窗有无内层 GROUP BY 折叠（其余三处仅换表名）。

// 旧当前窗：与新 routingQualityWindowCurrentSQL 同形，底表为旧 rollup。
const shardEquivOldCurrentSQL = `
SELECT route_class_id, candidate_fingerprint,
	SUM(attempts)::bigint, SUM(successes)::bigint, SUM(ttft_n)::bigint,
	SUM(ttft_sum_log_q32)::bigint, SUM(ttft_sumsq_log_q32)::bigint,
	SUM(input_tokens)::bigint, SUM(output_tokens)::bigint,
	SUM(cache_read_tokens)::bigint, SUM(cache_create_tokens)::bigint
FROM shard_equiv_quality_rollup
WHERE identity_version = 1 AND bucket_minute >= $1 AND bucket_minute < $2
GROUP BY 1, 2
ORDER BY 1, 2`

// 旧基线窗：与基线 routingQualityWindowBaselineSQL 逐字一致（含字面阈值 30），
// 关键在它**没有**内层 GROUP BY (bucket_minute, quality_class_id) 折叠——
// 旧 rollup 行本来就已是该粒度。把该折叠删掉的新查询会退化成此形状，
// 故它是“折叠是否存在”的判定器。
const shardEquivOldBaselineSQL = `
WITH hot AS (
	SELECT decode(rc, 'hex') AS rc, decode(fp, 'hex') AS fp
	FROM unnest($1::text[], $2::text[]) AS t(rc, fp)
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
	FROM shard_equiv_quality_rollup r
	WHERE r.identity_version = 1
		AND r.route_class_id = hot.rc
		AND r.candidate_fingerprint = hot.fp
		AND r.bucket_minute >= $3 AND r.bucket_minute < $4
) AS sub
WHERE sub.running - sub.attempts < 30
GROUP BY 1, 2
ORDER BY 1, 2`

// 未折叠探针：与新基线窗 SQL 同形但**故意删掉内层折叠**，直接扫分片事实表。
// 它必须在绊线候选上给出不同值，否则本 harness 是空转的（见 A4）。
const shardEquivUnaggregatedSQL = `
WITH hot AS (
	SELECT decode(rc, 'hex') AS rc, decode(fp, 'hex') AS fp
	FROM unnest($1::text[], $2::text[]) AS t(rc, fp)
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
	WHERE r.route_class_id = hot.rc
		AND r.candidate_fingerprint = hot.fp
		AND r.bucket_minute >= $3 AND r.bucket_minute < $4
) AS sub
WHERE sub.running - sub.attempts < 30
GROUP BY 1, 2
ORDER BY 1, 2`

// 旧 frontier 窗：与新 qualityFactStatsSQL 同形，底表换旧 rollup 并保留版本谓词。
const shardEquivOldFrontierSQL = `
WITH facts AS (
	SELECT route_class_id, quality_class_id, candidate_fingerprint,
		attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network,
		ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist,
		input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images
	FROM shard_equiv_quality_rollup
	WHERE route_class_id = $1 AND identity_version = 1 AND bucket_minute >= $2 AND bucket_minute < $3
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

// 旧 flow 窗：与新 flowFactStatsSQL 同形，底表换旧 rollup 并保留版本谓词。
const shardEquivOldFlowSQL = `
SELECT route_class_id, ordinal, lane, account_id, previous_account_id, previous_outcome,
	transition_reason, outcome, is_terminal,
	MIN(min_generation)::bigint, SUM(chain_count)::bigint
FROM shard_equiv_flow_rollup
WHERE route_class_id = $1 AND identity_version = 1 AND terminal_minute >= $2 AND terminal_minute < $3
GROUP BY route_class_id, ordinal, lane, account_id, previous_account_id, previous_outcome,
	transition_reason, outcome, is_terminal
ORDER BY ordinal, lane, account_id, previous_account_id NULLS FIRST, previous_outcome,
	transition_reason, outcome, is_terminal DESC`

// shardEquivRollAcc 是同一逻辑事件集在旧粒度的累加器：新路径按实例行写入事实表，
// 旧路径按 (分钟, 质量类) 折叠求和后写入对照 rollup 表（即当年 RollupQuality 的输出语义）。
type shardEquivRollAcc struct {
	row  repository.RoutingQualityRow
	hist []int64
}

// TestRoutingShardEquivPG 是 A3 对拍 harness：同一逻辑事件集灌入旧路径
// （对照 rollup 表 + 旧读 SQL）与新路径（分片事实表 + 生产读），四处读逐字段精确相等。
//
// 夹具 = 2 路由 × 3 候选 × 2 质量类 × 2 实例 × 8 分钟（分钟为 M-1m … M-8m）：
// 当前窗 [M-5m, M) 取走 M-1m…M-5m，基线窗 [M-24h, M-5m) 取走 M-6m…M-8m，
// frontier/flow 窗取 [M-8m, M) 覆盖全部 8 分钟。
// ttft_hist 长度恒为 10 且尾元素非零，逐元素比对能抓住截断与尾部丢数。
//
// 基线窗内埋入 A4 绊线（路由 0 / 候选 0）：严格更新的 M-6m 与 M-7m 合并 attempts
// 为 8+6=14（S=28），跨界分钟 M-8m 在分钟内排序第一的质量类上拆 5+5（合并 10），
// 第二质量类 1+1（合并 2）。合并粒度下 running=38、38-10=28<30 收下整分钟 →
// attempts 38；未折叠扫描下跨界分钟第二片 running=38、38-5=33≥30 被丢弃 → 33。
// 两片 attempts 对称（5+5）故结论与分片物理行序无关。successes 合并 16、未折叠 14。
// 这两组金值被硬编码断言：改任一处都会先算术重证。
func TestRoutingShardEquivPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := shardEquivMinute
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))
	_, err := pool.Exec(ctx, shardEquivOldQualityRollupDDL)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, shardEquivOldFlowRollupDDL)
	require.NoError(t, err)

	// 维度值：复用 routing_pg_test.go 的 helpers（同 repository_test 包）。
	rcs := []domain.RouteClassIDVal{
		mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions),
		mustRouteClassVal(t, 2, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions),
	}
	qcA := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qcB := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o-mini", domain.OpChatCompletions)
	// 基线窗 ORDER BY quality_class_id ASC（bytea 二进制比较）：先算出分钟内排第一的
	// 质量类，绊线拆分必须落在它身上，否则算术证明不成立。
	qcFirst := qcA
	if bytes.Compare(qcB[:], qcA[:]) < 0 {
		qcFirst = qcB
	}
	qcs := []domain.QualityClassIDVal{qcA, qcB}
	fps := []domain.CandidateFingerprintVal{
		mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-equiv-1", "", "", "", false, "inst", "sess", "thr", "win"),
		mustFPVal(t, 2, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-equiv-2", "", "", "", false, "inst", "sess", "thr", "win"),
		mustFPVal(t, 3, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-equiv-3", "", "", "", false, "inst", "sess", "thr", "win"),
	}
	insts := []string{"shard-A", "shard-B"}
	minutes := make([]time.Time, 0, 8)
	for k := 1; k <= 8; k++ {
		minutes = append(minutes, m.Add(-time.Duration(k)*time.Minute))
	}

	// 绊线取值：路由 0 / 候选 0 / 基线分钟（M-6m…M-8m，即 minutes[5..7]）。
	// 拆分对称（5+5、4+4、3+3）使结论与分片物理行序无关。
	shardEquivAttempts := func(ri, ci int, qc domain.QualityClassIDVal, mi int) int64 {
		if ri == 0 && ci == 0 && mi >= 5 {
			if mi <= 6 {
				if qc == qcFirst {
					return 4
				}
				return 3
			}
			if qc == qcFirst {
				return 5
			}
			return 1
		}
		return int64(1 + ((ri*13 + ci*7 + mi) % 5))
	}

	type rollKey struct {
		rc, qc, fp string
		min        time.Time
	}
	roll := map[rollKey]*shardEquivRollAcc{}

	// 同一逻辑事件集双灌：新路径走生产 UpsertQualityRow，旧路径在 Go 侧按
	// (分钟, 质量类) 折叠求和后写入对照表。
	var seq int64 = 1
	for ri, rc := range rcs {
		for ci, fp := range fps {
			for _, qc := range qcs {
				for ii, inst := range insts {
					for mi, minute := range minutes {
						att := shardEquivAttempts(ri, ci, qc, mi)
						base := int64(1 + ((ri*13 + ci*7 + ii*3 + mi) % 5))
						hist := make([]int64, 10)
						for j := range hist {
							hist[j] = base * int64(j+1)
						}
						row := repository.RoutingQualityRow{
							IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc,
							CandidateFingerprint: fp, InstanceSrc: inst, BucketMinute: minute,
							AbsoluteSequence:  seq,
							Attempts:          att,
							Successes:         att / 2,
							Count429:          base % 2,
							CountOrdinary4xx:  base % 3,
							Count5xx:          base % 2,
							CountNetwork:      int64(mi % 2),
							TTFTN:             att,
							TTFTSumLogQ32:     base * 10,
							TTFTSumSqLogQ32:   base * 20,
							TTFTHist:          hist,
							InputTokens:       base * 100,
							OutputTokens:      base * 50,
							CacheReadTokens:   base * 7,
							CacheCreateTokens: base * 3,
							Calls:             base,
							Images:            int64(mi % 3),
						}
						require.NoError(t, repos.Partitions.UpsertQualityRow(ctx, row))
						k := rollKey{string(rc[:]), string(qc[:]), string(fp[:]), minute}
						acc, ok := roll[k]
						if !ok {
							acc = &shardEquivRollAcc{
								row: repository.RoutingQualityRow{
									IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc,
									CandidateFingerprint: fp, BucketMinute: minute,
								},
								hist: make([]int64, 10),
							}
							roll[k] = acc
						}
						a := acc.row
						a.Attempts += row.Attempts
						a.Successes += row.Successes
						a.Count429 += row.Count429
						a.CountOrdinary4xx += row.CountOrdinary4xx
						a.Count5xx += row.Count5xx
						a.CountNetwork += row.CountNetwork
						a.TTFTN += row.TTFTN
						a.TTFTSumLogQ32 += row.TTFTSumLogQ32
						a.TTFTSumSqLogQ32 += row.TTFTSumSqLogQ32
						a.InputTokens += row.InputTokens
						a.OutputTokens += row.OutputTokens
						a.CacheReadTokens += row.CacheReadTokens
						a.CacheCreateTokens += row.CacheCreateTokens
						a.Calls += row.Calls
						a.Images += row.Images
						acc.row = a
						for j := range hist {
							acc.hist[j] += hist[j]
						}
					}
				}
			}
		}
		seq++
	}
	for k, acc := range roll {
		_, err := pool.Exec(ctx, `INSERT INTO shard_equiv_quality_rollup
			(identity_version, route_class_id, quality_class_id, candidate_fingerprint, bucket_minute,
			 attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network,
			 ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist,
			 input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images)
			VALUES (1,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
			[]byte(k.rc), []byte(k.qc), []byte(k.fp), k.min,
			acc.row.Attempts, acc.row.Successes, acc.row.Count429, acc.row.CountOrdinary4xx,
			acc.row.Count5xx, acc.row.CountNetwork, acc.row.TTFTN, acc.row.TTFTSumLogQ32,
			acc.row.TTFTSumSqLogQ32, acc.hist,
			acc.row.InputTokens, acc.row.OutputTokens, acc.row.CacheReadTokens,
			acc.row.CacheCreateTokens, acc.row.Calls, acc.row.Images)
		require.NoError(t, err)
	}

	// Flow 双灌：两分片同边不同 chain/generation，读侧必须 SUM/MIN 折叠。
	// 新路径走生产 UpsertFlowSnapshot，旧路径写入同值对照行。
	flowFrom := m.Add(-8 * time.Minute)
	// 注意：UpsertFlowSnapshot 是**按 (terminal_minute, instance_src) 整片替换**的快照写
	// （见 TestRoutingFlowEmptySnapshotPG：更高序号的空快照会删空该片）。故两条路由类
	// **不能共用同一批分钟**，否则后写的那条会把前一条的片整片删掉、读回 0 行。
	// 这里让每个路由类各占 8 个互不重叠的分钟，并把 flow 读窗口放宽到覆盖两者。
	flowWinFrom := m.Add(-20 * time.Minute)
	for ri, rc := range rcs {
		for mi, m0 := range minutes {
			minute := m0.Add(-time.Duration(8*ri) * time.Minute)
			for _, inst := range insts {
				chain1, gen1 := int64(5), int64(3)
				if inst == "shard-B" {
					chain1, gen1 = 7, 2
				}
				rows := []repository.RoutingFlowRow{{
					IdentityVersion: 1, RouteClassID: rc,
					Ordinal: 1, Lane: "primary", AccountID: int64(10 + ri),
					PreviousOutcome: "", TransitionReason: "init", Outcome: "success",
					IsTerminal: false, Generation: gen1, ChainCount: chain1,
				}}
				if mi%2 == 0 {
					prev := int64(10 + ri)
					chain2, gen2 := int64(3), int64(4)
					if inst == "shard-B" {
						chain2, gen2 = 1, 5
					}
					rows = append(rows, repository.RoutingFlowRow{
						IdentityVersion: 1, RouteClassID: rc,
						Ordinal: 2, Lane: "retry", AccountID: int64(20 + ri),
						PreviousAccountID: &prev, PreviousOutcome: "success",
						TransitionReason: "retry", Outcome: "success",
						IsTerminal: true, Generation: gen2, ChainCount: chain2,
					})
				}
				require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, inst, minute, 1, seq, rows))
				for _, r := range rows {
					_, err := pool.Exec(ctx, `INSERT INTO shard_equiv_flow_rollup
						(identity_version, route_class_id, terminal_minute, ordinal, lane, account_id,
						 previous_account_id, previous_outcome, transition_reason, outcome, is_terminal,
						 instance_src, min_generation, chain_count)
						VALUES (1,$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
						rc[:], minute, r.Ordinal, r.Lane, r.AccountID, r.PreviousAccountID,
						r.PreviousOutcome, r.TransitionReason, r.Outcome, r.IsTerminal,
						inst, r.Generation, r.ChainCount)
					require.NoError(t, err)
				}
			}
		}
		seq++
	}

	hotKeys := make([]repository.WindowHotKey, 0, len(rcs)*len(fps))
	for _, rc := range rcs {
		for _, fp := range fps {
			hotKeys = append(hotKeys, repository.WindowHotKey{RouteClassID: rc, Fingerprint: fp})
		}
	}
	rcHex := make([]string, 0, len(hotKeys))
	fpHex := make([]string, 0, len(hotKeys))
	for _, k := range hotKeys {
		rcHex = append(rcHex, hex.EncodeToString(k.RouteClassID[:]))
		fpHex = append(fpHex, hex.EncodeToString(k.Fingerprint[:]))
	}
	baseFrom, baseTo := m.Add(-domain.BaselineLookback), m.Add(-domain.CurrentWindowLen)

	// 读 1：编译器当前窗 [M-5m, M)。9 个度量逐字段精确相等。
	newCur, err := repos.Partitions.QueryCurrentWindowStats(ctx, m)
	require.NoError(t, err)
	oldCurRows, err := pool.Query(ctx, shardEquivOldCurrentSQL, m.Add(-domain.CurrentWindowLen), m)
	require.NoError(t, err)
	oldCur := map[string]repository.WindowCurrentStat{}
	for oldCurRows.Next() {
		var s repository.WindowCurrentStat
		var rt, fp []byte
		require.NoError(t, oldCurRows.Scan(&rt, &fp,
			&s.Attempts, &s.Successes, &s.TTFTN, &s.TTFTSumLogQ32, &s.TTFTSumSqLogQ32,
			&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheCreateTokens))
		copy(s.RouteClassID[:], rt)
		copy(s.Fingerprint[:], fp)
		oldCur[string(s.RouteClassID[:])+string(s.Fingerprint[:])] = s
	}
	require.NoError(t, oldCurRows.Err())
	oldCurRows.Close()
	newCurMap := map[string]repository.WindowCurrentStat{}
	for _, s := range newCur {
		newCurMap[string(s.RouteClassID[:])+string(s.Fingerprint[:])] = s
	}
	require.Len(t, newCurMap, len(rcs)*len(fps), "current window must return every (route, candidate)")
	require.Len(t, oldCur, len(newCurMap))
	for k, want := range oldCur {
		got, ok := newCurMap[k]
		require.True(t, ok, "current window lost candidate")
		require.Equal(t, want, got, "current window must match field-by-field")
	}

	// 读 2：编译器基线窗 [M-24h, M-5m)，新路径带内层折叠，旧路径无折叠但底表已是折叠粒度。
	// 两边逐字段精确相等；绊线候选另有硬编码金值 (38,16)。
	newBase, err := repos.Partitions.QueryBaselineTruncated(ctx, m, hotKeys)
	require.NoError(t, err)
	oldBaseRows, err := pool.Query(ctx, shardEquivOldBaselineSQL, rcHex, fpHex, baseFrom, baseTo)
	require.NoError(t, err)
	type basePair struct{ att, succ int64 }
	oldBase := map[string]basePair{}
	for oldBaseRows.Next() {
		var rt, fp []byte
		var att, succ int64
		require.NoError(t, oldBaseRows.Scan(&rt, &fp, &att, &succ))
		var rc domain.RouteClassIDVal
		var f domain.CandidateFingerprintVal
		copy(rc[:], rt)
		copy(f[:], fp)
		oldBase[string(rc[:])+string(f[:])] = basePair{att, succ}
	}
	require.NoError(t, oldBaseRows.Err())
	oldBaseRows.Close()
	newBaseMap := map[string]basePair{}
	for _, s := range newBase {
		newBaseMap[string(s.RouteClassID[:])+string(s.Fingerprint[:])] = basePair{s.Attempts, s.Successes}
	}
	require.Len(t, newBaseMap, len(hotKeys), "baseline must return every hot key")
	require.Len(t, oldBase, len(newBaseMap))
	for k, want := range oldBase {
		got, ok := newBaseMap[k]
		require.True(t, ok, "baseline lost hot key")
		require.Equal(t, want, got, "baseline must match field-by-field")
	}
	tripKey := string(rcs[0][:]) + string(fps[0][:])
	require.Equal(t, basePair{38, 16}, newBaseMap[tripKey],
		"straddle candidate must hit the frozen golden (merged boundary minute kept)")

	// 阴性对照：去掉折叠的探针在同一事实表上必须给出不同值，否则 harness 空转。
	// 绊线候选的金值是 (33,14)——跨界分钟第二片被丢弃；且总体至少一处分叉。
	rawRows, err := pool.Query(ctx, shardEquivUnaggregatedSQL, rcHex, fpHex, baseFrom, baseTo)
	require.NoError(t, err)
	rawBase := map[string]basePair{}
	for rawRows.Next() {
		var rt, fp []byte
		var att, succ int64
		require.NoError(t, rawRows.Scan(&rt, &fp, &att, &succ))
		var rc domain.RouteClassIDVal
		var f domain.CandidateFingerprintVal
		copy(rc[:], rt)
		copy(f[:], fp)
		rawBase[string(rc[:])+string(f[:])] = basePair{att, succ}
	}
	require.NoError(t, rawRows.Err())
	rawRows.Close()
	require.Equal(t, basePair{33, 14}, rawBase[tripKey],
		"un-aggregated probe must hit the frozen golden (boundary shard row dropped)")
	require.NotEqual(t, newBaseMap[tripKey], rawBase[tripKey],
		"negative control must differ on the straddle candidate")
	diverged := 0
	for k, v := range newBaseMap {
		if rawBase[k] != v {
			diverged++
		}
	}
	require.GreaterOrEqual(t, diverged, 1, "negative control must diverge on at least one hot key")

	// 读 3：frontier 窗 [M-8m, M)。16 个整数度量精确相等，ttft_hist 逐元素相等
	// （长度恒 10、尾元素非零，截断与尾部丢数都会被抓住）。
	for _, rc := range rcs {
		newStats, err := repos.Partitions.QueryQualityFactStats(ctx, rc, flowFrom, m)
		require.NoError(t, err)
		oldRows, err := pool.Query(ctx, shardEquivOldFrontierSQL, rc[:], flowFrom, m)
		require.NoError(t, err)
		oldStats := map[string]repository.RoutingQualityStat{}
		for oldRows.Next() {
			var s repository.RoutingQualityStat
			var rt, qc, fp []byte
			var histText string
			require.NoError(t, oldRows.Scan(&rt, &qc, &fp,
				&s.Attempts, &s.Successes, &s.Count429, &s.CountOrdinary4xx, &s.Count5xx, &s.CountNetwork,
				&s.TTFTN, &s.TTFTSumLogQ32, &s.TTFTSumSqLogQ32,
				&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheCreateTokens, &s.Calls, &s.Images,
				&histText))
			copy(s.RouteClassID[:], rt)
			copy(s.QualityClassID[:], qc)
			copy(s.CandidateFingerprint[:], fp)
			s.TTFTHist = shardEquivParseHist(t, histText)
			oldStats[string(s.RouteClassID[:])+string(s.QualityClassID[:])+string(s.CandidateFingerprint[:])] = s
		}
		require.NoError(t, oldRows.Err())
		oldRows.Close()
		newMap := map[string]repository.RoutingQualityStat{}
		for _, s := range newStats {
			newMap[string(s.RouteClassID[:])+string(s.QualityClassID[:])+string(s.CandidateFingerprint[:])] = s
		}
		require.Len(t, newMap, 3*2, "frontier must return every (candidate, quality class)")
		require.Len(t, oldStats, len(newMap))
		for k, want := range oldStats {
			got, ok := newMap[k]
			require.True(t, ok, "frontier lost identity")
			require.Equal(t, want.Attempts, got.Attempts)
			require.Equal(t, want.Successes, got.Successes)
			require.Equal(t, want.Count429, got.Count429)
			require.Equal(t, want.CountOrdinary4xx, got.CountOrdinary4xx)
			require.Equal(t, want.Count5xx, got.Count5xx)
			require.Equal(t, want.CountNetwork, got.CountNetwork)
			require.Equal(t, want.TTFTN, got.TTFTN)
			require.Equal(t, want.TTFTSumLogQ32, got.TTFTSumLogQ32)
			require.Equal(t, want.TTFTSumSqLogQ32, got.TTFTSumSqLogQ32)
			require.Equal(t, want.InputTokens, got.InputTokens)
			require.Equal(t, want.OutputTokens, got.OutputTokens)
			require.Equal(t, want.CacheReadTokens, got.CacheReadTokens)
			require.Equal(t, want.CacheCreateTokens, got.CacheCreateTokens)
			require.Equal(t, want.Calls, got.Calls)
			require.Equal(t, want.Images, got.Images)
			require.Len(t, got.TTFTHist, 10, "ttft_hist length must be 10, not truncated")
			require.Len(t, want.TTFTHist, 10)
			require.NotEqual(t, int64(0), got.TTFTHist[9], "ttft_hist tail must be non-zero")
			for j := range want.TTFTHist {
				require.Equal(t, want.TTFTHist[j], got.TTFTHist[j], "ttft_hist element must match")
			}
		}
	}

	// 读 4：flow 窗 [M-8m, M)。chain_count 跨分片 SUM、min_generation 跨分片 MIN，
	// 新旧 SQL 除表名与版本谓词外逐字一致，整行精确相等（含 ORDER BY 定序）。
	for _, rc := range rcs {
		newFlow, err := repos.Partitions.QueryFlowFactStats(ctx, rc, flowWinFrom, m)
		require.NoError(t, err)
		oldFlowRows, err := pool.Query(ctx, shardEquivOldFlowSQL, rc[:], flowWinFrom, m)
		require.NoError(t, err)
		oldFlow := []repository.RoutingFlowStat{}
		for oldFlowRows.Next() {
			var s repository.RoutingFlowStat
			var rt []byte
			var prev sql.NullInt64
			require.NoError(t, oldFlowRows.Scan(&rt, &s.Ordinal, &s.Lane, &s.AccountID, &prev, &s.PreviousOutcome,
				&s.TransitionReason, &s.Outcome, &s.IsTerminal, &s.MinGeneration, &s.ChainCount))
			copy(s.RouteClassID[:], rt)
			if prev.Valid {
				v := prev.Int64
				s.PreviousAccountID = &v
			}
			oldFlow = append(oldFlow, s)
		}
		require.NoError(t, oldFlowRows.Err())
		oldFlowRows.Close()
		require.Len(t, newFlow, 2, "flow window must return both edges")
		require.Equal(t, oldFlow, newFlow, "flow window must match row-for-row (same ORDER BY)")
		// 折叠真实发生：ord1 边在两分片 chain 5+7=12/分钟 × 8 分钟 = 96，
		// min_generation = min(3,2) = 2。
		require.Equal(t, int64(96), newFlow[0].ChainCount)
		require.Equal(t, int64(2), newFlow[0].MinGeneration)
	}
}

// shardEquivParseHist 解析 bigint[] 字面量（"{1,2,…}"），与生产 parseRoutingHist 同语义，
// 只为旧 frontier 读的 hist_text 列服务。
func shardEquivParseHist(t *testing.T, text string) []int64 {
	t.Helper()
	require.True(t, strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}"), "unexpected hist literal %q", text)
	trim := text[1 : len(text)-1]
	if trim == "" {
		return nil
	}
	parts := strings.Split(trim, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		require.NoError(t, err)
		out = append(out, v)
	}
	return out
}
