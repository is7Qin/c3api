// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestRoutingFlowRollupAggregatesMultiInstancePG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-agg", "", "", "", false, "", "", "", "")
	// Two instances publish identical edge for same terminalMinute with different chain_count
	rowA := repository.RoutingFlowRow{
		IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary",
		AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1,
		CandidateFingerprint: fp, InstanceSrc: "src-A", ChainCount: 2,
	}
	rowB := repository.RoutingFlowRow{
		IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary",
		AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1,
		CandidateFingerprint: fp, InstanceSrc: "src-B", ChainCount: 3,
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A", now, 1, 1, []repository.RoutingFlowRow{rowA}))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 1, []repository.RoutingFlowRow{rowB}))
	// barrier: both instance rows inserted distinct via instance_src
	var instCnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_instance_minute WHERE terminal_minute=$1`, now).Scan(&instCnt))
	require.Equal(t, int64(2), instCnt)
	require.NoError(t, repos.Partitions.RollupFlow(ctx, now, 1))
	var rollCnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&rollCnt))
	require.Equal(t, int64(1), rollCnt, "identical edges must deduplicate to one rollup row")
	var chainSum int64
	var absSeq int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT chain_count, absolute_sequence FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&chainSum, &absSeq))
	require.Equal(t, int64(5), chainSum, "conservation: sum of chain_count across instances must be retained")
	require.Equal(t, int64(1), absSeq, "absolute_sequence must be max (both 1)")

	// Second scenario: same edge repeated with distinct seq, ensure max seq retained and still deduped
	rowA2 := rowA
	rowA2.ChainCount = 4
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A", now, 1, 2, []repository.RoutingFlowRow{rowA2}))
	require.NoError(t, repos.Partitions.RollupFlow(ctx, now, 1))
	require.NoError(t, pool.QueryRow(ctx, `SELECT chain_count, absolute_sequence FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&chainSum, &absSeq))
	require.Equal(t, int64(7), chainSum, "after A updates to 4 + B 3 => 7")
	require.Equal(t, int64(2), absSeq, "max absolute_sequence must be 2")
}

func TestRoutingFlowRollupDedupAndConservationPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	fp1 := mustFPVal(t, 1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-dedup1", "", "", "", false, "", "", "", "")
	fp2 := mustFPVal(t, 2, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-dedup2", "", "", "", false, "", "", "", "")
	// src-A provides two distinct edges, src-B provides one overlapping edge + one new
	rowsA := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp1, InstanceSrc: "src-A", ChainCount: 2},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "primary", AccountID: 20, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp2, InstanceSrc: "src-A", ChainCount: 5},
	}
	rowsB := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp1, InstanceSrc: "src-B", ChainCount: 3},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 30, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, CandidateFingerprint: fp1, InstanceSrc: "src-B", ChainCount: 7},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A", now, 1, 1, rowsA))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 1, rowsB))
	require.NoError(t, repos.Partitions.RollupFlow(ctx, now, 1))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(3), cnt, "overlapping edge deduped: 2+2 distinct but one overlap => 3 rollup rows")
	var sum int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT SUM(chain_count) FROM routing_flow_rollup WHERE terminal_minute=$1`, now).Scan(&sum))
	require.Equal(t, int64(2+3+5+7), sum, "total chain_count conserved via SUM")
}
