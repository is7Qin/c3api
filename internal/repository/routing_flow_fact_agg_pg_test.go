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

// A6 晚到证伪（分片独立性回归）：A 写 M 边 E chain=10 seq=1；B 迟到写 M
// 边 E chain=7 seq=1 → 读 E==17 且非负；A 以 seq=2 chain=12 重写 → 读
// E==19（B 分片完好）。本测试是分片独立性回归，不是新旧判别：旧重算道
// 对全实例重 SUM 同样得出 19，不得声称"旧代码必红"。
func TestRoutingFlowMergedAggregatesMultiInstancePG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	// Two instances publish identical edge for same terminalMinute with different chain_count
	rowA := repository.RoutingFlowRow{
		IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary",
		AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1,
		InstanceSrc: "src-A", ChainCount: 10,
	}
	rowB := repository.RoutingFlowRow{
		IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary",
		AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1,
		InstanceSrc: "src-B", ChainCount: 7,
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A", now, 1, 1, []repository.RoutingFlowRow{rowA}))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 1, []repository.RoutingFlowRow{rowB}))
	// barrier: both shard rows inserted distinct via instance_src
	var instCnt int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1`, now).Scan(&instCnt))
	require.Equal(t, int64(2), instCnt)
	// 读恒为跨分片聚合：SUM(chain_count)=17，MIN(min_generation)=1，非负。
	stats, err := repos.Partitions.QueryFlowFactStats(ctx, rc, 1, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, int64(17), stats[0].ChainCount)
	require.Equal(t, int64(1), stats[0].MinGeneration)
	require.GreaterOrEqual(t, stats[0].ChainCount, int64(0))

	// A 以 seq=2 chain=12 重写己分片 → 读 E==19（B 分片完好）。
	rowA2 := rowA
	rowA2.ChainCount = 12
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A", now, 1, 2, []repository.RoutingFlowRow{rowA2}))
	stats, err = repos.Partitions.QueryFlowFactStats(ctx, rc, 1, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, int64(19), stats[0].ChainCount, "A rewrite must not touch B shard")
}

func TestRoutingFlowMergedDedupAndConservationPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	// src-A provides two distinct edges, src-B provides one overlapping edge + one new
	rowsA := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-A", ChainCount: 2},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 2, Lane: "primary", AccountID: 20, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-A", ChainCount: 5},
	}
	rowsB := []repository.RoutingFlowRow{
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-B", ChainCount: 3},
		{IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary", AccountID: 30, PreviousOutcome: "", TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, InstanceSrc: "src-B", ChainCount: 7},
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A", now, 1, 1, rowsA))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", now, 1, 1, rowsB))
	// 读面跨分片聚合：overlap 边合一行（2+3），其余各一行 → 3 行，总和守恒。
	stats, err := repos.Partitions.QueryFlowFactStats(ctx, rc, 1, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, stats, 3, "overlapping edge merged: 2+2 distinct but one overlap => 3 rows")
	var sum int64
	for _, s := range stats {
		sum += s.ChainCount
	}
	require.Equal(t, int64(2+3+5+7), sum, "total chain_count conserved via SUM")
}
