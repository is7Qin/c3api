// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

// Test-only real-PG atomicity proof for the versioned snapshot/ack handoff:
// UpsertFlowSnapshot replaces the complete edge set atomically — a mid-
// replacement row failure rolls back to the prior sequence untouched, and the
// retry replaces fully. Repository production files are untouched by this
// change; this file is the sole repository-tree exception.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestRoutingFlowSnapshotAtomicRollbackPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
			src := "src-atomic"

	// Seed seq1 full rows.
	seq1 := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 3, TransitionReason: "initial", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: src, ChainCount: 1},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "primary", AccountID: 4, PreviousAccountID: ptrInt64(3), PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: src, ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, src, now, 1, 1, seq1))

	// Temporary trigger: fail the sentinel second row mid-replacement, after
	// the DELETE and the first INSERT of the seq2 attempt.
	_, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION tmp_flow_fail_sentinel() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.chain_count = 777 THEN RAISE EXCEPTION 'tmp sentinel failure'; END IF; RETURN NEW; END;$$`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DROP TRIGGER IF EXISTS tmp_flow_fail ON routing_flow_rollup`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `CREATE TRIGGER tmp_flow_fail BEFORE INSERT ON routing_flow_rollup FOR EACH ROW WHEN (NEW.chain_count = 777) EXECUTE FUNCTION tmp_flow_fail_sentinel()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS tmp_flow_fail ON routing_flow_rollup`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS tmp_flow_fail_sentinel()`)
	})

	// seq2 replacement fails on the sentinel row: the whole transaction,
	// including the DELETE and the partial first insert, must roll back.
	seq2 := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "explore", AccountID: 5, TransitionReason: "initial", Outcome: "success", IsTerminal: true, Generation: 2, InstanceSrc: src, ChainCount: 2},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "explore", AccountID: 6, TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 2, InstanceSrc: src, ChainCount: 777},
	}
	require.Error(t, repos.Partitions.UpsertFlowSnapshot(ctx, src, now, 1, 2, seq2), "mid-replacement row failure must surface")

	// Old seq1 rows and highest_sequence=1 remain unchanged.
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, src, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "failed replacement must leave old rows untouched")
	var accounts []int64
	rows, err := pool.Query(ctx, `SELECT account_id FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2 ORDER BY ordinal`, src, now)
	require.NoError(t, err)
	for rows.Next() {
		var a int64
		require.NoError(t, rows.Scan(&a))
		accounts = append(accounts, a)
	}
	rows.Close()
	require.Equal(t, []int64{3, 4}, accounts)
	var highest int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT highest_sequence FROM routing_flow_snapshot_state WHERE terminal_minute=$1 AND instance_src=$2 AND identity_version=1`, now, src).Scan(&highest))
	require.Equal(t, int64(1), highest, "failed replacement must not advance durable sequence")

	// Drop the trigger and retry seq2 without the sentinel: full replacement
	// with highest_sequence=2.
	_, err = pool.Exec(ctx, `DROP TRIGGER tmp_flow_fail ON routing_flow_rollup`)
	require.NoError(t, err)
	seq2[1].ChainCount = 3
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, src, now, 1, 2, seq2))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, src, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt)
	accounts = nil
	rows, err = pool.Query(ctx, `SELECT account_id FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2 ORDER BY ordinal`, src, now)
	require.NoError(t, err)
	for rows.Next() {
		var a int64
		require.NoError(t, rows.Scan(&a))
		accounts = append(accounts, a)
	}
	rows.Close()
	require.Equal(t, []int64{5, 6}, accounts, "retry replaces with the full new edge set")
	require.NoError(t, pool.QueryRow(ctx, `SELECT highest_sequence FROM routing_flow_snapshot_state WHERE terminal_minute=$1 AND instance_src=$2 AND identity_version=1`, now, src).Scan(&highest))
	require.Equal(t, int64(2), highest)
}
