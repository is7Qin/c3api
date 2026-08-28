// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func mustRouteClass(t *testing.T, gid int64, cf domain.RequestFormat, model string, op domain.OperationTag) []byte {
	t.Helper()
	id, err := domain.RouteClassID(gid, cf, model, op)
	require.NoError(t, err)
	b := make([]byte, 32)
	copy(b, id[:])
	return b
}
func mustQualityClass(t *testing.T, ck domain.CallerKind, uf domain.RequestFormat, model string, op domain.OperationTag) []byte {
	t.Helper()
	id, err := domain.QualityClassID(ck, uf, model, op)
	require.NoError(t, err)
	b := make([]byte, 32)
	copy(b, id[:])
	return b
}
func mustFingerprint(t *testing.T, accID, tplID int64, ct credential.Type, origin, sk, pat, email, acc string, strip bool, inst, sess, thr, win string) []byte {
	t.Helper()
	id, err := domain.CandidateFingerprint(accID, tplID, ct, origin, sk, pat, email, acc, strip, inst, sess, thr, win)
	require.NoError(t, err)
	b := make([]byte, 32)
	copy(b, id[:])
	return b
}

func TestRoutingPartitionBootstrapPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	now := time.Now().UTC()
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	// idempotent second call
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
	// non-partitioned tables exist
	var n int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_dirty_minute`).Scan(&n))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_compiler_state`).Scan(&n))
	// indexes
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE tablename='routing_quality_instance_minute' AND indexname='routing_quality_instance_minute_uniq'`).Scan(&n))
	require.Equal(t, int64(1), n)
}

func TestRoutingQualityReplayPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	qc := mustQualityClass(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rc := mustRouteClass(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFingerprint(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-one", "", "", "", false, "inst", "sess", "thr", "win")
	row := repository.RoutingQualityRow{
		IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp,
		InstanceSrc: "host-1-abc", BucketMinute: now, AbsoluteSequence: 10, Attempts: 5, Successes: 3, Failures: 2, TTFTSumMS: 100, TTFTCount: 3,
	}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	// duplicate absolute replay should not double
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
	// fingerprint change isolates quality: different fp => separate row
	fp2 := mustFingerprint(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-two", "", "", "", false, "inst", "sess", "thr", "win")
	row3 := row
	row3.CandidateFingerprint = fp2
	row3.InstanceSrc = "host-1-abc"
	row3.AbsoluteSequence = 10
	row3.Attempts = 7
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row3))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_quality_instance_minute WHERE bucket_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "fingerprint change must create separate row")
	// version isolation: version 2 row separate, version 1 query still returns v1
	rowV2 := row
	rowV2.IdentityVersion = 2
	rowV2.AbsoluteSequence = 20
	rowV2.Attempts = 99
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, rowV2))
	qrow, err := repos.Partitions.QueryQualityRow(ctx, "host-1-abc", now, fp, qc, 1)
	require.NoError(t, err)
	require.Equal(t, int64(8), qrow.Attempts, "version 1 row must remain")
	qrow2, err := repos.Partitions.QueryQualityRow(ctx, "host-1-abc", now, fp, qc, 2)
	require.NoError(t, err)
	require.Equal(t, int64(99), qrow2.Attempts)
	require.NotEqual(t, qrow.Attempts, qrow2.Attempts)
}

func TestRoutingQualityDirtyPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	qc := mustQualityClass(t, domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rc := mustRouteClass(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFingerprint(t, 2, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-x", "", "", "", false, "", "", "", "")
	row := repository.RoutingQualityRow{IdentityVersion: 1, RouteClassID: rc, QualityClassID: qc, CandidateFingerprint: fp, InstanceSrc: "src-A", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1}
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row))
	dirty, err := repos.Partitions.IsDirty(ctx, now)
	require.NoError(t, err)
	require.True(t, dirty, "writer must mark dirty in same tx")
}

func TestRoutingFlowReplayPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClass(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFingerprint(t, 3, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-y", "", "", "", false, "", "", "", "")
	row := repository.RoutingFlowRow{
		IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 3, Outcome: "success", Reason: "ok", IsTerminal: true, Generation: 1, CandidateFingerprint: fp, InstanceSrc: "src-B", ChainCount: 5,
	}
	require.NoError(t, repos.Partitions.UpsertFlowAndMarkDirty(ctx, row))
	require.NoError(t, repos.Partitions.UpsertFlowAndMarkDirty(ctx, row))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT chain_count FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(5), cnt, "flow absolute replay must not double")
	row2 := row
	row2.ChainCount = 10
	require.NoError(t, repos.Partitions.UpsertFlowAndMarkDirty(ctx, row2))
	require.NoError(t, pool.QueryRow(ctx, `SELECT chain_count FROM routing_flow_instance_minute WHERE instance_src=$1 AND terminal_minute=$2`, "src-B", now).Scan(&cnt))
	require.Equal(t, int64(10), cnt)
}

func TestRoutingPartitionRetentionPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)
	now := time.Now().UTC()
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	// create historical partitions
	for _, d := range []string{"20260728", "20260729"} {
		for _, tbl := range []string{"routing_quality_instance_minute", "routing_flow_instance_minute"} {
			pgExec(t, pool, `CREATE TABLE `+tbl+`_`+d+` PARTITION OF `+tbl+` FOR VALUES FROM ('`+mustISODate(d)+` 00:00:00+00') TO ('`+mustNextISODate(d)+` 00:00:00+00')`)
		}
	}
	n, err := repos.Partitions.DropRoutingQualityInstanceBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	m, err := repos.Partitions.DropRoutingFlowInstanceBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, m)
}

func TestRoutingPartitionAbsencePG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	// query non-existent partition still works due to parent table; but dropping future partition and inserting should auto-route if partition exists, else fail. Test that Ensure creates future.
	future := now.AddDate(0, 0, 5)
	require.NoError(t, repos.Partitions.EnsureRoutingInstancePartitions(ctx, future, future))
	var exists bool
	require.NoError(t, poolQueryExists(ctx, pgTestPool(t), "routing_quality_instance_minute_"+future.Format("20060102"), &exists))
	require.True(t, exists)
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
