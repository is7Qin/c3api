// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func mustRouteClassVal(t *testing.T, gid int64, cf domain.RequestFormat, model string, op domain.OperationTag) domain.RouteClassIDVal {
	t.Helper()
	id, err := domain.RouteClassID(gid, cf, model, op)
	require.NoError(t, err)
	return id
}
func mustQualityClassVal(t *testing.T, ck domain.CallerKind, uf domain.RequestFormat, model string, op domain.OperationTag) domain.QualityClassIDVal {
	t.Helper()
	id, err := domain.QualityClassID(ck, uf, model, op)
	require.NoError(t, err)
	return id
}
func mustFPVal(t *testing.T, accID, tplID int64, ct credential.Type, origin, sk, pat, email, acc string, strip bool, inst, sess, thr, win string) domain.CandidateFingerprintVal {
	t.Helper()
	id, err := domain.CandidateFingerprint(accID, tplID, ct, origin, sk, pat, email, acc, strip, inst, sess, thr, win)
	require.NoError(t, err)
	return id
}

// indexColumns 从 catalog 读回某索引的列序（pg_get_indexdef 是事实源，
// 不是 Go 侧期望值的回显），供唯一键/探针键的列序断言共用。
func indexColumns(t *testing.T, pool *pgxpool.Pool, indexName string) []string {
	t.Helper()
	var def string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT pg_get_indexdef($1::regclass)`, indexName).Scan(&def))
	open := strings.Index(def, "(")
	closing := strings.LastIndex(def, ")")
	require.True(t, open >= 0 && closing > open, "unexpected index definition: %s", def)
	parts := strings.Split(def[open+1:closing], ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func newRoutingRepos(t *testing.T) (*repository.Repository, *pgxpool.Pool) {
	t.Helper()
	pool := pgTestPool(t)
	ctx := context.Background()
	db := stdlib.OpenDBFromPool(pool)
	_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
	require.NoError(t, err)
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureUsageStatsPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsureUsageEntityStatsPartitioned(ctx, time.Now()))
	require.NoError(t, repos.EnsurePriceVariantsEffectCheck(ctx))
	return repos, pool
}

func TestRoutingPartitionBootstrapPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	for _, tbl := range []string{"routing_quality_instance_minute", "routing_quality_rollup", "routing_quality_fact", "routing_flow_rollup"} {
		parted, err := repos.Partitions.IsTablePartitioned(ctx, tbl)
		require.NoError(t, err)
		require.True(t, parted, "%s partitioned", tbl)
	}
	today := now.UTC().Truncate(24 * time.Hour)
	for _, tbl := range []string{"routing_quality_instance_minute", "routing_quality_rollup", "routing_quality_fact", "routing_flow_rollup"} {
		rows, err := pool.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid=c.oid JOIN pg_class p ON p.oid=i.inhparent JOIN pg_namespace n ON n.oid=c.relnamespace WHERE p.relname=$1 AND n.nspname=current_schema()`, tbl)
		require.NoError(t, err)
		var names []string
		for rows.Next() {
			var n string
			require.NoError(t, rows.Scan(&n))
			names = append(names, n)
		}
		rows.Close()
		require.Contains(t, names, tbl+"_"+today.Format("20060102"))
		require.Contains(t, names, tbl+"_"+today.AddDate(0, 0, 1).Format("20060102"))
	}
	var n int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_dirty_minute`).Scan(&n))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_compiler_state`).Scan(&n))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_rollup_watermark`).Scan(&n))
	// checks for digest length
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint WHERE conrelid='routing_quality_instance_minute'::regclass AND contype='c'`).Scan(&n))
	require.GreaterOrEqual(t, n, int64(3))
	// columns existence for sufficient stats
	for _, col := range []string{"count_429", "count_ordinary_4xx", "count_5xx", "count_network", "ttft_n", "ttft_sum_log_q32", "ttft_hist", "input_tokens", "calls", "images"} {
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name='routing_quality_instance_minute' AND column_name=$1`, col).Scan(&n))
		require.Equal(t, int64(1), n, "missing col %s", col)
	}
	for _, col := range []string{"previous_outcome", "transition_reason", "absolute_sequence", "instance_src", "min_generation"} {
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name='routing_flow_rollup' AND column_name=$1`, col).Scan(&n))
		require.Equal(t, int64(1), n, "missing flow col %s", col)
	}
}

// TestRoutingQualityFactDDLPG 钉住 S1 事实表的列集合/列序与两条索引定义。
// 列定义事实源漂移（加列忘改、身份列序被改回、度量类型被换）必须在此失败；
// 索引列序取自 pg_get_indexdef，故能真正证明「身份键以
// (route_class_id, candidate_fingerprint, bucket_minute) 开头」——这正是基线窗
// LATERAL 探针与唯一性共用同一条索引的前提（改序则探针退化为另建含两个 bytea
// 的索引，实测 +16.6% B/行，见 routing-footprint 证据 §4）。
func TestRoutingQualityFactDDLPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)))

	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'routing_quality_fact' ORDER BY ordinal_position`)
	require.NoError(t, err)
	var got []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got = append(got, name)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Equal(t, []string{
		"id", "route_class_id", "candidate_fingerprint", "bucket_minute", "instance_src",
		"quality_class_id", "identity_version", "absolute_sequence", "attempts", "successes",
		"count_429", "count_ordinary_4xx", "count_5xx", "count_network", "ttft_n",
		"ttft_sum_log_q32", "ttft_sumsq_log_q32", "ttft_hist", "input_tokens", "output_tokens",
		"cache_read_tokens", "cache_create_tokens", "calls", "images", "updated_at",
	}, got, "routing_quality_fact column set/order drifted from the single-shard fact model")

	require.Equal(t,
		[]string{"route_class_id", "candidate_fingerprint", "bucket_minute", "instance_src", "quality_class_id", "identity_version"},
		indexColumns(t, pool, "routing_quality_fact_uniq"))
	require.Equal(t, []string{"bucket_minute"}, indexColumns(t, pool, "routing_quality_fact_bucket"))

	var uniq bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT indisunique FROM pg_index WHERE indexrelid = 'routing_quality_fact_uniq'::regclass`).Scan(&uniq))
	require.True(t, uniq, "identity index must enforce uniqueness")

	// ttft_hist 必须是 bigint[]（udt_name _int8）：跨分片逐元素求和依赖它，
	// 换成 int4[] 会静默改变溢出语义。
	var histType string
	require.NoError(t, pool.QueryRow(ctx, `SELECT udt_name FROM information_schema.columns WHERE table_name='routing_quality_fact' AND column_name='ttft_hist'`).Scan(&histType))
	require.Equal(t, "_int8", histType, "ttft_hist must stay bigint[]")
}

func TestRoutingQualityReplayPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-one", "", "", "", false, "inst", "sess", "thr", "win")
	row := repository.RoutingQualityRow{
		IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp,
		InstanceSrc: "host-1-abc", BucketMinute: now, AbsoluteSequence: 10, Attempts: 5, Successes: 3, Count429: 1, InputTokens: 100, Calls: 2,
	}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	var attempts int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_instance_minute WHERE instance_src=$1 AND bucket_minute=$2`, "host-1-abc", now).Scan(&attempts))
	require.Equal(t, int64(5), attempts, "absolute replay must not double")
	// larger sequence overwrites
	row2 := row
	row2.AbsoluteSequence = 11
	row2.Attempts = 8
	row2.Successes = 5
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row2))
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_instance_minute WHERE instance_src=$1 AND bucket_minute=$2`, "host-1-abc", now).Scan(&attempts))
	require.Equal(t, int64(8), attempts, "larger sequence must overwrite")
	// smaller sequence must not overwrite (stale replay)
	rowStale := row
	rowStale.AbsoluteSequence = 9
	rowStale.Attempts = 100
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, rowStale))
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_instance_minute WHERE instance_src=$1 AND bucket_minute=$2`, "host-1-abc", now).Scan(&attempts))
	require.Equal(t, int64(8), attempts, "stale sequence must be ignored")
	// equal divergent: same sequence but different attempts must NOT overwrite
	rowEqualDiverge := row2
	rowEqualDiverge.Attempts = 99
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, rowEqualDiverge))
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_instance_minute WHERE instance_src=$1 AND bucket_minute=$2`, "host-1-abc", now).Scan(&attempts))
	require.Equal(t, int64(8), attempts, "equal divergent must not overwrite")
	// fingerprint change isolates
	fp2 := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-two", "", "", "", false, "inst", "sess", "thr", "win")
	row3 := row
	row3.CandidateFingerprint = fp2
	row3.InstanceSrc = "host-1-abc"
	row3.AbsoluteSequence = 10
	row3.Attempts = 7
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row3))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_quality_instance_minute WHERE bucket_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "fingerprint change must create separate row")
}

func TestRoutingQualityDigestCheckPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	// direct SQL with malformed digest (3 bytes) must fail CHECK octet_length=32
	_, err := pool.Exec(ctx, `INSERT INTO routing_quality_instance_minute (identity_version, route_class_id, quality_class_id, candidate_fingerprint, instance_src, bucket_minute, absolute_sequence, updated_at) VALUES (1, '\x010203'::bytea, '\x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20'::bytea, '\x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20'::bytea, 'src', $1, 1, now())`, now)
	require.Error(t, err)
	require.Contains(t, err.Error(), "check constraint")
}

func TestRoutingQualityDirtyPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 2, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-x", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-A", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	dirty, err := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.NoError(t, err)
	require.True(t, dirty, "writer must mark dirty in same tx with kind/version")
}

func TestRoutingFlowSnapshotPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 3, PreviousOutcome: "", TransitionReason: "initial", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-B", ChainCount: 1},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "primary", AccountID: 4, PreviousAccountID: ptrInt64(3), PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-B", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 10, rows))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(2), cnt)
	// equal sequence divergent must not mutate
	rowsDiv := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "degraded", AccountID: 99, PreviousOutcome: "", TransitionReason: "initial", Outcome: "fail", IsTerminal: true, Generation: 1, InstanceSrc: "src-B", ChainCount: 99},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 10, rowsDiv))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "equal sequence must not replace")
	var lane string
	require.NoError(t, pool.QueryRow(ctx, `SELECT lane FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2 AND ordinal=1`, "src-B", now).Scan(&lane))
	require.Equal(t, "primary", lane)
	// greater sequence replaces complete set (deletes omitted stale edges)
	rows2 := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "explore", AccountID: 5, PreviousOutcome: "", TransitionReason: "initial", Outcome: "success", IsTerminal: true, Generation: 2, InstanceSrc: "src-B", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 11, rows2))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(1), cnt, "greater sequence must replace with new edge set")
	require.NoError(t, pool.QueryRow(ctx, `SELECT lane FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&lane))
	require.Equal(t, "explore", lane)
	// lower sequence must not mutate
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 9, rows))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(1), cnt, "lower sequence must not mutate")
}

func TestRoutingFlowDimensionUniquenessPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-C", ChainCount: 1},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-C", ChainCount: 1},
	}
	// these two edges differ only in previous_outcome/transition_reason, must both persist (no collapse)
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-C", now, 1, 5, rows))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src='src-C' AND terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "distinct edges must not collapse")
}

func TestRoutingPartitionWatermarkPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-wm", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-W", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, now, 1))
	dirty, _ := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.False(t, dirty, "rollup must clear dirty")
	var wm time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT watermark FROM routing_rollup_watermark WHERE kind='quality' AND identity_version=1`).Scan(&wm))
	require.True(t, wm.Equal(now), "watermark must advance")
	err := repos.Partitions.RollupQuality(ctx, now, 1)
	require.Error(t, err, "re-rollup without dirty must fail")
	row2 := row
	row2.AbsoluteSequence = 2
	row2.Attempts = 2
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row2))
	dirty, _ = repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.True(t, dirty, "writer after rollup must re-dirty")
}

func TestRoutingPartitionWatermarkDirtyRequiredPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	next := now.Add(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, next))
	err := repos.Partitions.RollupQuality(ctx, next, 1)
	require.Error(t, err, "rollup without dirty must fail")
	require.Contains(t, err.Error(), "dirty")
	_, err = repos.Partitions.GetWatermark(ctx, "quality", 1)
	require.Error(t, err, "watermark should not exist after failed rollup")
}

func TestRoutingPartitionConcurrencyPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fpA := mustFPVal(t, 10, 1, credential.TypeAPIKey, "https://api.openai.com", "sk-conc-a", "", "", "", false, "", "", "", "")
	fpB := mustFPVal(t, 11, 1, credential.TypeAPIKey, "https://api.openai.com", "sk-conc-b", "", "", "", false, "", "", "", "")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fpA, InstanceSrc: "src-conc-A", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1}
		errs[0] = repos.Partitions.UpsertQualityAndMarkDirty(ctx, row)
	}()
	go func() {
		defer wg.Done()
		row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fpB, InstanceSrc: "src-conc-B", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1}
		errs[1] = repos.Partitions.UpsertQualityAndMarkDirty(ctx, row)
	}()
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	pool := pgTestPool(t)
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_quality_instance_minute WHERE bucket_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "two sources must not serialize cluster-wide")
	// same source deterministic sequencing: two sequential writes same source with increasing sequence must both succeed
	row1 := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fpA, InstanceSrc: "src-conc-A", BucketMinute: now, AbsoluteSequence: 2, Attempts: 2}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row1))
	var attempts int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_instance_minute WHERE instance_src='src-conc-A' AND bucket_minute=$1`, now).Scan(&attempts))
	require.Equal(t, int64(2), attempts)
}

func TestRoutingPartitionRetentionPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	for _, d := range []string{"20260728", "20260729"} {
		for _, tbl := range []string{"routing_quality_instance_minute", "routing_quality_rollup", "routing_flow_rollup"} {
			pgExec(t, pool, `CREATE TABLE `+tbl+`_`+d+` PARTITION OF `+tbl+` FOR VALUES FROM ('`+mustISODate(d)+` 00:00:00+00') TO ('`+mustNextISODate(d)+` 00:00:00+00')`)
		}
	}
	n, err := repos.Partitions.DropRoutingQualityInstanceBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = repos.Partitions.DropRoutingQualityRollupBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = repos.Partitions.DropRoutingFlowRollupBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestRoutingPartitionMissingPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	future := now.AddDate(0, 0, 5)
	require.NoError(t, repos.Partitions.EnsureRoutingInstancePartitions(ctx, future, future))
	var exists bool
	require.NoError(t, poolQueryExists(ctx, pool, "routing_quality_instance_minute_"+future.Format("20060102"), &exists))
	require.True(t, exists)
}

func TestRoutingPartitionStartupPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	tomorrow := now.AddDate(0, 0, 1)
	require.NoError(t, repos.Partitions.EnsureRoutingInstancePartitions(ctx, tomorrow, tomorrow))
	require.NoError(t, repos.Partitions.EnsureRoutingRollupPartitions(ctx, tomorrow, tomorrow))
}

func TestRoutingQualityFullStatsPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 7, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 99, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-full", "", "", "", false, "i1", "s1", "t1", "w1")
	row := repository.RoutingQualityRow{
		IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp,
		InstanceSrc: "src-full", BucketMinute: now, AbsoluteSequence: 5, Attempts: 10, Successes: 6, Count429: 1, CountOrdinary4xx: 1, Count5xx: 1, CountNetwork: 1,
		TTFTN: 5, TTFTSumLogQ32: 12345, TTFTSumSqLogQ32: 67890, TTFTHist: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 300, CacheCreateTokens: 400, Calls: 7, Images: 3,
	}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	// query must require RouteClassID and return all stats including hist
	got, err := repos.Partitions.QueryQualityRow(ctx, "src-full", now, fp, qc, rc, 1)
	require.NoError(t, err)
	require.Equal(t, int64(10), got.Attempts)
	require.Equal(t, int64(1), got.Count429)
	require.Equal(t, int64(5), got.TTFTN)
	require.Equal(t, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, got.TTFTHist)
	require.Equal(t, int64(1000), got.InputTokens)
	// wrong route should not find
	rc2 := mustRouteClassVal(t, 8, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	_, err = repos.Partitions.QueryQualityRow(ctx, "src-full", now, fp, qc, rc2, 1)
	require.Error(t, err, "wrong route must not find")
	var histText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT ttft_hist::text FROM routing_quality_instance_minute WHERE instance_src='src-full' AND bucket_minute=$1`, now).Scan(&histText))
	require.Equal(t, "{1,2,3,4,5,6,7,8,9,10}", histText)
}

func TestRoutingFlowEmptySnapshotPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows10 := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-E", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 10, rows10))
	var cnt int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(1), cnt)
	// empty snapshot with higher sequence should delete all
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 11, nil))
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(0), cnt, "empty higher seq must delete")
	// stale seq10 must not repopulate
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 10, rows10))
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(0), cnt, "stale seq10 must remain empty")
	// seq12 repopulates
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 12, rows10))
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(1), cnt, "seq12 must repopulate")
}

func TestRoutingQualityRollbackPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-rollback", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-P", BucketMinute: now, AbsoluteSequence: 1, Attempts: 5, Successes: 3}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	_, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION fail_dirty_q() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'poison dirty %', NEW.bucket_minute; END; $$ LANGUAGE plpgsql;`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `CREATE TRIGGER poison_dirty_q_trg BEFORE UPDATE ON routing_dirty_minute FOR EACH ROW WHEN (NEW.dirty = false) EXECUTE FUNCTION fail_dirty_q();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Exec(ctx, `DROP TRIGGER IF EXISTS poison_dirty_q_trg ON routing_dirty_minute;`)
		pool.Exec(ctx, `DROP FUNCTION IF EXISTS fail_dirty_q();`)
	})
	err = repos.Partitions.RollupQuality(ctx, now, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "poison dirty")
	dirty, _ := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.True(t, dirty, "dirty must stay true after rollback")
	_, err = repos.Partitions.GetWatermark(ctx, "quality", 1)
	require.Error(t, err, "watermark must not advance after poison")
	var rollCnt int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_quality_rollup WHERE bucket_minute=$1`, now).Scan(&rollCnt)
	require.Equal(t, int64(0), rollCnt, "rollup output must be rolled back")
	_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS poison_dirty_q_trg ON routing_dirty_minute;`)
	_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS fail_dirty_q();`)
	require.NoError(t, repos.Partitions.RollupQuality(ctx, now, 1))
	dirty, _ = repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.False(t, dirty)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_quality_rollup WHERE bucket_minute=$1`, now).Scan(&rollCnt)
	require.Equal(t, int64(1), rollCnt)
	var attempts int64
	pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_rollup WHERE bucket_minute=$1`, now).Scan(&attempts)
	require.Equal(t, int64(5), attempts)
}

func TestRoutingFlowSnapshotDirectWritePG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-PF", ChainCount: 1},
	}
	// S3 起 flow 写入直达合并层：快照即聚合行，无 dirty、无 watermark、无
	// RollupFlow 中间道。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-PF", now, 1, 1, rows))
	var rollCnt int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&rollCnt)
	require.Equal(t, int64(1), rollCnt, "snapshot writes the merged row directly")
	var minGen int64
	pool.QueryRow(ctx, `SELECT min_generation FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&minGen)
	require.Equal(t, int64(1), minGen)
	dirty, _ := repos.Partitions.IsDirty(ctx, "flow", 1, now)
	require.False(t, dirty, "flow writes no dirty bit anymore")
}

func TestRoutingQualityRollupSuccessPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-roll", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{
		IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-RQ", BucketMinute: now, AbsoluteSequence: 1,
		Attempts: 10, Successes: 6, Count429: 1, CountOrdinary4xx: 1, Count5xx: 1, CountNetwork: 1,
		TTFTN: 5, TTFTSumLogQ32: 111, TTFTSumSqLogQ32: 222, TTFTHist: []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		InputTokens: 100, OutputTokens: 200, CacheReadTokens: 30, CacheCreateTokens: 40, Calls: 3, Images: 2,
	}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, now, 1))
	var attempts, successes, c429, c4xx, c5xx, cnet, ttn, sumLog, sumSq, inp, out, cr, cc, calls, images int64
	var histText string
	pool.QueryRow(ctx, `SELECT attempts, successes, count_429, count_ordinary_4xx, count_5xx, count_network, ttft_n, ttft_sum_log_q32, ttft_sumsq_log_q32, ttft_hist::text, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, calls, images FROM routing_quality_rollup WHERE bucket_minute=$1`, now).Scan(&attempts, &successes, &c429, &c4xx, &c5xx, &cnet, &ttn, &sumLog, &sumSq, &histText, &inp, &out, &cr, &cc, &calls, &images)
	require.Equal(t, int64(10), attempts)
	require.Equal(t, int64(1), c429)
	require.Equal(t, int64(5), ttn)
	require.Equal(t, "{1,1,1,1,1,1,1,1,1,1}", histText)
	require.Equal(t, int64(100), inp)
	dirty, _ := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.False(t, dirty)
	row.AbsoluteSequence = 2
	row.Attempts = 20
	row.Successes = 12
	row.Count429 = 2
	row.TTFTHist = []int64{2, 2, 2, 2, 2, 2, 2, 2, 2, 2}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, now, 1))
	pool.QueryRow(ctx, `SELECT attempts, count_429, ttft_hist::text FROM routing_quality_rollup WHERE bucket_minute=$1`, now).Scan(&attempts, &c429, &histText)
	require.Equal(t, int64(20), attempts)
	require.Equal(t, int64(2), c429)
	require.Equal(t, "{2,2,2,2,2,2,2,2,2,2}", histText)
}

func TestRoutingFlowSnapshotSuccessPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 10, PreviousAccountID: nil, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: false, Generation: 1, InstanceSrc: "src-RF", AbsoluteSequence: 1, ChainCount: 1},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "primary", AccountID: 20, PreviousAccountID: ptrInt64(10), PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-RF", AbsoluteSequence: 1, ChainCount: 1},
	}
	// S3 起快照直写合并层，无 RollupFlow 中间道。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-RF", now, 1, 1, rows))
	var cnt int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(2), cnt, "no collapse")
	var prevAcc sql.NullInt64
	var prevOut, trans, out string
	var isTerm bool
	var gen int64
	pool.QueryRow(ctx, `SELECT previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, min_generation FROM routing_flow_rollup WHERE terminal_minute=$1 AND ordinal=2`, now).Scan(&prevAcc, &prevOut, &trans, &out, &isTerm, &gen)
	require.True(t, prevAcc.Valid)
	require.Equal(t, int64(10), prevAcc.Int64)
	require.Equal(t, "success", prevOut)
	require.Equal(t, "retry", trans)
	require.Equal(t, "success", out)
	require.True(t, isTerm)
	require.Equal(t, int64(1), gen)
	dirty, _ := repos.Partitions.IsDirty(ctx, "flow", 1, now)
	require.False(t, dirty)
}

func TestRoutingQualityGateBarrierPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-gate-q", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-GQ", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	gateKey := int64(91001)
	// gate holder on dedicated pool to avoid blocking main pool's 2 connections (rollup uses ent driver sharing same pool)
	dsnGate := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, dsnGate)
	gatePoolCfg, err := pgxpool.ParseConfig(dsnGate)
	require.NoError(t, err)
	gatePoolCfg.MaxConns = 1
	gatePool, err := pgxpool.NewWithConfig(context.Background(), gatePoolCfg)
	require.NoError(t, err)
	t.Cleanup(func() { gatePool.Close() })
	gateConn, err := gatePool.Acquire(ctx)
	require.NoError(t, err)
	// holder transaction via gateConn
	gateTx, err := gateConn.Begin(ctx)
	require.NoError(t, err)
	_, err = gateTx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", gateKey)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "CREATE OR REPLACE FUNCTION gate_q_fn() RETURNS trigger AS $$ BEGIN PERFORM pg_advisory_xact_lock(91001::bigint); RETURN NEW; END; $$ LANGUAGE plpgsql;")
	require.NoError(t, err)
	partSuffix := now.Format("20060102")
	_, err = pool.Exec(ctx, fmt.Sprintf("CREATE TRIGGER gate_q_trg BEFORE INSERT ON routing_quality_rollup_%s FOR EACH ROW EXECUTE FUNCTION gate_q_fn();", partSuffix))
	require.NoError(t, err)
	var holderPid int
	_ = gateTx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPid)
	require.NotZero(t, holderPid)
	var gateReleased sync.Once
	releaseGate := func() {
		gateReleased.Do(func() {
			_ = gateTx.Rollback(context.Background())
			gateConn.Release()
		})
	}
	// register immediately: a failure before the late cleanup below would otherwise leave
	// gateConn acquired and hang gatePool.Close until the package timeout (masking the error)
	t.Cleanup(releaseGate)
	// failure cleanup must release gate before closing writerPool or joining writer; idempotent via sync.Once, cannot deadlock
	rollupDone := make(chan error, 1)
	go func() { rollupDone <- repos.Partitions.RollupQuality(context.Background(), now, 1) }()
	rollupPid := waitForAdvisoryWaiterForKey(t, pool, holderPid, gateKey)
	require.NotEqual(t, holderPid, rollupPid)
	// dedicated writer pool to obtain stable writer PID and preserve production SQL path
	dsn := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, dsn)
	writerPoolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	writerPoolCfg.MaxConns = 1
	writerPoolCfg.MinConns = 0
	if writerPoolCfg.ConnConfig.RuntimeParams == nil {
		writerPoolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	writerPool, err := pgxpool.NewWithConfig(context.Background(), writerPoolCfg)
	require.NoError(t, err)
	var writerDoneChan chan error
	t.Cleanup(func() {
		// idempotent gate release before pool close and before joining blocked writer, avoids deadlock
		releaseGate()
		if writerDoneChan != nil {
			select {
			case <-writerDoneChan:
			case <-time.After(2 * time.Second):
			}
		}
		select {
		case <-rollupDone:
		case <-time.After(2 * time.Second):
		}
		pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS gate_q_trg ON routing_quality_rollup_%s;", partSuffix))
		pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS gate_q_fn();")
		writerPool.Close()
	})
	// ensure writer pool can create partitions (already ensured)
	writerDB := stdlib.OpenDBFromPool(writerPool)
	writerRepos, err := repository.NewWithPG(context.Background(), entsql.OpenDB(dialect.Postgres, writerDB), true, writerPool)
	require.NoError(t, err)
	// obtain writer PID via dedicated connection
	writerConn, err := writerPool.Acquire(context.Background())
	require.NoError(t, err)
	var writerPid int
	require.NoError(t, writerConn.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&writerPid))
	writerConn.Release()
	require.NotEqual(t, holderPid, writerPid)
	require.NotEqual(t, rollupPid, writerPid)
	t.Logf("holder %d rollup %d writer %d distinct", holderPid, rollupPid, writerPid)
	writerDone := make(chan error, 1)
	writerDoneChan = writerDone
	go func() {
		row2 := row
		row2.AbsoluteSequence = 2
		row2.Attempts = 2
		writerDone <- writerRepos.Partitions.UpsertQualityAndMarkDirty(context.Background(), row2)
	}()
	waitForWriterLock(t, pool, holderPid, rollupPid, writerPid)
	releaseGate()
	select {
	case err := <-rollupDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("rollup did not complete after gate release")
	}
	select {
	case err := <-writerDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not complete after rollup")
	}
	dirty, _ := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.True(t, dirty, "writer-after-read must leave dirty true")
}

func TestRoutingFlowSnapshotGateBarrierPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-GF", ChainCount: 1}}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-GF", now, 1, 1, rows))
	gateKey := int64(91002)
	dsnGate2 := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, dsnGate2)
	gatePoolCfg2, err := pgxpool.ParseConfig(dsnGate2)
	require.NoError(t, err)
	gatePoolCfg2.MaxConns = 1
	gatePool2, err := pgxpool.NewWithConfig(context.Background(), gatePoolCfg2)
	require.NoError(t, err)
	t.Cleanup(func() { gatePool2.Close() })
	gateConn2, err := gatePool2.Acquire(ctx)
	require.NoError(t, err)
	gateTx, err := gateConn2.Begin(ctx)
	require.NoError(t, err)
	_, err = gateTx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", gateKey)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "CREATE OR REPLACE FUNCTION gate_f_fn() RETURNS trigger AS $$ BEGIN PERFORM pg_advisory_xact_lock(91002::bigint); RETURN NEW; END; $$ LANGUAGE plpgsql;")
	require.NoError(t, err)
	partSuffix2 := now.Format("20060102")
	_, err = pool.Exec(ctx, fmt.Sprintf("CREATE TRIGGER gate_f_trg BEFORE INSERT ON routing_flow_rollup_%s FOR EACH ROW EXECUTE FUNCTION gate_f_fn();", partSuffix2))
	require.NoError(t, err)
	var holderPid2 int
	_ = gateTx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPid2)
	require.NotZero(t, holderPid2)
	var gateReleased2 sync.Once
	releaseGate2 := func() {
		gateReleased2.Do(func() {
			_ = gateTx.Rollback(context.Background())
			gateConn2.Release()
		})
	}
	dsn2 := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, dsn2)
	writerPoolCfg2, err := pgxpool.ParseConfig(dsn2)
	require.NoError(t, err)
	writerPoolCfg2.MaxConns = 1
	writerPoolCfg2.MinConns = 0
	if writerPoolCfg2.ConnConfig.RuntimeParams == nil {
		writerPoolCfg2.ConnConfig.RuntimeParams = map[string]string{}
	}
	writerPool2, err := pgxpool.NewWithConfig(context.Background(), writerPoolCfg2)
	require.NoError(t, err)
	var writerDoneChan2 chan error
	t.Cleanup(func() {
		releaseGate2()
		if writerDoneChan2 != nil {
			select {
			case <-writerDoneChan2:
			case <-time.After(2 * time.Second):
			}
		}
		pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS gate_f_trg ON routing_flow_rollup_%s;", partSuffix2))
		pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS gate_f_fn();")
		writerPool2.Close()
	})
	writerDB2 := stdlib.OpenDBFromPool(writerPool2)
	writerRepos2, err := repository.NewWithPG(context.Background(), entsql.OpenDB(dialect.Postgres, writerDB2), true, writerPool2)
	require.NoError(t, err)
	// S3 起快照直写合并层，**没有下游重算车道**：旧版"rollup 车道 + writer 两个
	// 等待者"的屏障场景在构造上不再存在（flow 写面无 dirty、无下游 SELECT）。
	// 本用例保留仍然成立的不变式：写入被门挡住（真的在途阻塞，而非立即返回），
	// 门开后同一分片被更高序号完整替换。
	writerDoneChan2 = make(chan error, 1)
	go func() {
		rows2 := []repository.RoutingFlowRow{{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 2, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-GF", AbsoluteSequence: 2, ChainCount: 1}}
		writerDoneChan2 <- writerRepos2.Partitions.UpsertFlowSnapshot(context.Background(), "src-GF", now, 1, 2, rows2)
	}()
	// 门内等待者 = writer 自身的连接（在 INSERT 触发器上等同一把 advisory 锁）；
	// 它必须不是持门者，否则说明写入根本没被挡住。
	waiterPid2 := waitForAdvisoryWaiterForKey(t, pool, holderPid2, gateKey)
	require.NotEqual(t, holderPid2, waiterPid2)
	select {
	case err := <-writerDoneChan2:
		require.Failf(t, "flow writer must block on the gate", "writer returned early: %v", err)
	default:
	}
	releaseGate2()
	select {
	case err := <-writerDoneChan2:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("flow writer did not complete")
	}
	var acct int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT account_id FROM routing_flow_rollup WHERE terminal_minute=$1 AND instance_src='src-GF'`, now).Scan(&acct))
	require.Equal(t, int64(2), acct, "second snapshot replaced own shard after gate release")
	dirty, _ := repos.Partitions.IsDirty(ctx, "flow", 1, now)
	require.False(t, dirty)
}

func waitForAdvisoryWaiterForKey(t *testing.T, pool *pgxpool.Pool, holderPid int, gateKey int64) int {
	t.Helper()
	classID := int32(gateKey >> 32)
	objID := int32(gateKey)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-ticker.C:
			var waiterPid int
			err := pool.QueryRow(context.Background(), `SELECT pid FROM pg_locks WHERE locktype='advisory' AND classid=$2 AND objid=$3 AND objsubid=1 AND NOT granted AND granted = false AND pid != $1 LIMIT 1`, holderPid, classID, objID).Scan(&waiterPid)
			if err == nil && waiterPid != 0 {
				require.NotEqual(t, holderPid, waiterPid, "advisory waiter must be distinct from holder")
				t.Logf("advisory waiter pid %d holder %d gate %d classid %d objid %d", waiterPid, holderPid, gateKey, classID, objID)
				return waiterPid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("rollup did not block on advisory gate %d holder %d", gateKey, holderPid)
		}
	}
}

func waitForWriterLock(t *testing.T, pool *pgxpool.Pool, holderPid int, rollupPid int, writerPid int) {
	t.Helper()
	require.NotEqual(t, holderPid, rollupPid)
	require.NotEqual(t, holderPid, writerPid)
	require.NotEqual(t, rollupPid, writerPid)
	t.Logf("holder %d rollup %d writer %d distinct", holderPid, rollupPid, writerPid)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-ticker.C:
			var waitType sql.NullString
			var waitEvent sql.NullString
			_ = pool.QueryRow(context.Background(), `SELECT wait_event_type, wait_event FROM pg_stat_activity WHERE pid=$1`, writerPid).Scan(&waitType, &waitEvent)
			if waitType.Valid && waitType.String == "Lock" {
				t.Logf("writer %d wait %s:%s via pg_stat_activity", writerPid, waitType.String, waitEvent.String)
				return
			}
			var cnt int64
			_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM pg_locks WHERE pid=$1 AND NOT granted`, writerPid).Scan(&cnt)
			if cnt > 0 {
				t.Logf("writer %d ungranted pg_locks %d", writerPid, cnt)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer %d did not block holder %d rollup %d", writerPid, holderPid, rollupPid)
		}
	}
}

func poolQueryExists(ctx context.Context, pool *pgxpool.Pool, name string, out *bool) error {
	var n int64
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname=$1`, name).Scan(&n)
	if err != nil {
		return err
	}
	*out = n > 0
	return nil
}
func ptrInt64(v int64) *int64 { return &v }
