// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
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
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	for _, tbl := range []string{"routing_quality_instance_minute", "routing_flow_instance_minute", "routing_quality_rollup", "routing_flow_rollup"} {
		parted, err := repos.Partitions.IsTablePartitioned(ctx, tbl)
		require.NoError(t, err)
		require.True(t, parted, "%s partitioned", tbl)
	}
	today := now.UTC().Truncate(24 * time.Hour)
	for _, tbl := range []string{"routing_quality_instance_minute", "routing_flow_instance_minute", "routing_quality_rollup", "routing_flow_rollup"} {
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
	for _, col := range []string{"previous_outcome", "transition_reason", "absolute_sequence"} {
		require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name='routing_flow_instance_minute' AND column_name=$1`, col).Scan(&n))
		require.Equal(t, int64(1), n, "missing flow col %s", col)
	}
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
	fp := mustFPVal(t, 3, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-y", "", "", "", false, "", "", "", "")
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 3, PreviousOutcome: "", TransitionReason: "initial", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp, InstanceSrc: "src-B", ChainCount: 1},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "primary", AccountID: 4, PreviousAccountID: ptrInt64(3), PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: mustFPVal(t, 4, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-z", "", "", "", false, "", "", "", ""), InstanceSrc: "src-B", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 10, rows))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(2), cnt)
	// equal sequence divergent must not mutate
	rowsDiv := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "degraded", AccountID: 99, PreviousOutcome: "", TransitionReason: "initial", Outcome: "fail", IsTerminal: true, Generation: 1, CandidateFingerprint: fp, InstanceSrc: "src-B", ChainCount: 99},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 10, rowsDiv))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "equal sequence must not replace")
	var lane string
	require.NoError(t, pool.QueryRow(ctx, `SELECT lane FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2 AND ordinal=1`, "src-B", now).Scan(&lane))
	require.Equal(t, "primary", lane)
	// greater sequence replaces complete set (deletes omitted stale edges)
	rows2 := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "explore", AccountID: 5, PreviousOutcome: "", TransitionReason: "initial", Outcome: "success", IsTerminal: true, Generation: 2, CandidateFingerprint: fp, InstanceSrc: "src-B", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 11, rows2))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(1), cnt, "greater sequence must replace with new edge set")
	require.NoError(t, pool.QueryRow(ctx, `SELECT lane FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&lane))
	require.Equal(t, "explore", lane)
	// lower sequence must not mutate
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 9, rows))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(1), cnt, "lower sequence must not mutate")
}

func TestRoutingFlowDimensionUniquenessPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-a", "", "", "", false, "i", "s", "t", "w")
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp, InstanceSrc: "src-C", ChainCount: 1},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp, InstanceSrc: "src-C", ChainCount: 1},
	}
	// these two edges differ only in previous_outcome/transition_reason, must both persist (no collapse)
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-C", now, 1, 5, rows))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src='src-C' AND terminal_minute=$1`, now).Scan(&cnt))
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
		for _, tbl := range []string{"routing_quality_instance_minute", "routing_flow_instance_minute", "routing_quality_rollup", "routing_flow_rollup"} {
			pgExec(t, pool, `CREATE TABLE `+tbl+`_`+d+` PARTITION OF `+tbl+` FOR VALUES FROM ('`+mustISODate(d)+` 00:00:00+00') TO ('`+mustNextISODate(d)+` 00:00:00+00')`)
		}
	}
	n, err := repos.Partitions.DropRoutingQualityInstanceBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = repos.Partitions.DropRoutingFlowInstanceBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
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
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-e", "", "", "", false, "", "", "", "")
	rows10 := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp, InstanceSrc: "src-E", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 10, rows10))
	var cnt int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(1), cnt)
	// empty snapshot with higher sequence should delete all
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 11, nil))
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(0), cnt, "empty higher seq must delete")
	// stale seq10 must not repopulate
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 10, rows10))
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(0), cnt, "stale seq10 must remain empty")
	// seq12 repopulates
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", now, 1, 12, rows10))
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE instance_src='src-E' AND terminal_minute=$1`, now).Scan(&cnt)
	require.Equal(t, int64(1), cnt, "seq12 must repopulate")
}

func TestRoutingQualityRollbackPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-poison", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-P", BucketMinute: now, AbsoluteSequence: 1, Attempts: -1}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	// rollup should fail due to poison and rollback dirty/watermark/output
	err := repos.Partitions.RollupQuality(ctx, now, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "poison")
	dirty, _ := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.True(t, dirty, "dirty must stay true after rollback")
	_, err = repos.Partitions.GetWatermark(ctx, "quality", 1)
	require.Error(t, err, "watermark must not advance after poison")
	var rollCnt int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_quality_rollup WHERE bucket_minute=$1`, now).Scan(&rollCnt)
	require.Equal(t, int64(0), rollCnt, "rollup output must be rolled back")
	// fix poison and retry
	row.Attempts = 5
	row.AbsoluteSequence = 2
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, now, 1))
	dirty, _ = repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.False(t, dirty)
}

func TestRoutingFlowRollbackPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-fp", "", "", "", false, "", "", "", "")
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "poison", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp, InstanceSrc: "src-PF", ChainCount: 1},
	}
	// insert poison flow snapshot directly via UpsertFlowSnapshot with poison lane, then rollup should detect poison
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-PF", now, 1, 1, rows))
	err := repos.Partitions.RollupFlow(ctx, now, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "poison")
	dirty, _ := repos.Partitions.IsDirty(ctx, "flow", 1, now)
	require.True(t, dirty, "dirty must stay true after flow poison rollback")
	var rollCnt int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&rollCnt)
	require.Equal(t, int64(0), rollCnt)
}

func TestRoutingPartitionWriterRollupBarrierPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	qc := mustQualityClassVal(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-barrier", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-BA", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	// deterministic overlap: rollup locks dirty row FOR UPDATE before reading, writer blocks on same row and re-dirties after rollup commits
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT dirty FROM routing_dirty_minute WHERE kind='quality' AND identity_version=1 AND bucket_minute=$1 FOR UPDATE`, now)
	require.NoError(t, err)
	rollupLocked := make(chan struct{})
	writerDone := make(chan error)
	go func() {
		close(rollupLocked)
		// writer will try to upsert same bucket with higher sequence; its dirty upsert will block on the row lock held by tx
		row2 := row
		row2.AbsoluteSequence = 2
		row2.Attempts = 2
		writerDone <- repos.Partitions.UpsertQualityAndMarkDirty(context.Background(), row2)
	}()
	<-rollupLocked
	time.Sleep(100 * time.Millisecond)
	// rollup does its work and clears dirty, then commits (releasing lock)
	_, err = tx.Exec(ctx, `UPDATE routing_dirty_minute SET dirty=false, updated_at=now() WHERE kind='quality' AND identity_version=1 AND bucket_minute=$1`, now)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	select {
	case err := <-writerDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not complete after rollup")
	}
	dirty, _ := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.True(t, dirty, "writer-after-read must leave dirty true for next rollup")
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
