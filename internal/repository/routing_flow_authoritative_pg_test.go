// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestRoutingFlowAuthoritativeOuterPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	outerMinute := now
	outerSrc := "src-Auth"
	outerVersion := int16(1)
	rowMinute := now.Add(5 * time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, rowMinute))
	bogusRC := mustRouteClassVal(t, 99, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{
		{
			IdentityVersion:  99,
			RouteClassID:     bogusRC,
			TerminalMinute:   rowMinute,
			Ordinal:          1,
			Lane:             "primary",
			AccountID:        10,
			PreviousOutcome:  "",
			TransitionReason: "init",
			Outcome:          "success",
			IsTerminal:       true,
			Generation:       1,
			InstanceSrc:      "src-BAD",
			ChainCount:       1,
		},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, outerSrc, outerMinute, outerVersion, 10, rows))
	var gotMinute time.Time
	var gotSrc string
	var gotVer int16
	var gotRoute []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT terminal_minute, instance_src, identity_version, route_class_id FROM routing_flow_fact WHERE instance_src=$1 AND terminal_minute=$2`, outerSrc, outerMinute).Scan(&gotMinute, &gotSrc, &gotVer, &gotRoute))
	require.True(t, gotMinute.Equal(outerMinute), "terminal_minute must be authoritative outer")
	require.Equal(t, outerSrc, gotSrc, "instance_src must be authoritative outer")
	require.Equal(t, outerVersion, gotVer, "identity_version must be authoritative outer")
	var wantRoute [32]byte = bogusRC
	require.Equal(t, wantRoute[:], gotRoute, "route_class_id per edge must be preserved from row")
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE instance_src='src-BAD'`).Scan(&cnt))
	require.Equal(t, int64(0), cnt, "bad instance_src must not leak")
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1 AND instance_src='src-Auth'`, rowMinute).Scan(&cnt))
	require.Equal(t, int64(0), cnt, "bad terminal_minute must not leak")
}

func TestRoutingFlowRollupRemovesObsoletePG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rowsInitial := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-Obs", ChainCount: 1},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "primary", AccountID: 20, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-Obs", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-Obs", now, 1, 1, rowsInitial))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "initial snapshot must have 2 edges")
	rowsSmaller := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-Obs", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-Obs", now, 1, 2, rowsSmaller))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(1), cnt, "obsolete merged edge must be removed after replacement")
	var acct int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT account_id FROM routing_flow_fact WHERE terminal_minute=$1`, now).Scan(&acct))
	require.Equal(t, int64(10), acct)
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(1), cnt)
}

func TestRoutingFlowEmptySnapshotRollupPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 1, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-EmptyRoll", ChainCount: 1},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-EmptyRoll", now, 1, 1, rows))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(1), cnt)
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-EmptyRoll", now, 1, 2, nil))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(0), cnt, "empty snapshot must clear merged edges")
}

func TestRoutingFlowEdgeIdentityPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	rows := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 5, PreviousAccountID: ptrInt64(3), PreviousOutcome: "ok", TransitionReason: "retry", Outcome: "success", IsTerminal: false, Generation: 2, InstanceSrc: "src-Ident", ChainCount: 7},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 5, PreviousAccountID: ptrInt64(3), PreviousOutcome: "fail", TransitionReason: "fallback", Outcome: "success", IsTerminal: false, Generation: 2, InstanceSrc: "src-Ident", ChainCount: 7},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-Ident", now, 1, 1, rows))
	var cnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE instance_src='src-Ident' AND terminal_minute=$1`, now).Scan(&cnt))
	require.Equal(t, int64(2), cnt, "distinct edges must not collapse in merged layer")
	var prevOut1, trans1 string
	var isTerm bool
	var gen int64
	var chain int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT previous_outcome, transition_reason, is_terminal, min_generation, chain_count FROM routing_flow_fact WHERE instance_src='src-Ident' AND previous_outcome='ok'`).Scan(&prevOut1, &trans1, &isTerm, &gen, &chain))
	require.Equal(t, "retry", trans1)
	require.Equal(t, false, isTerm)
	require.Equal(t, int64(2), gen)
	require.Equal(t, int64(7), chain)
}
