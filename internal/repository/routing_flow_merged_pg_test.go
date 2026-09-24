// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// mergedKeyColumns 从 catalog 读回合并层唯一索引的列序（不是 Go 侧期望值的
// 回显：索引定义来自 pg_get_indexdef，故能真正钉住 §4 新键）。
func mergedKeyColumns(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return indexColumns(t, pool, "routing_flow_fact_uniq")
}

// flowFactColumnSet 返回合并层表的列名集合（catalog 事实）。
func flowFactColumnSet(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'routing_flow_fact'`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		out[name] = true
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, out)
	return out
}

// A1：S2′ 键精确 = §4 新键。同边不同 generation 两笔输入合一行（chain_count
// 相加、min_generation 取小）；candidate_fingerprint 换值不增行（该维度已从
// flow 身份整体删除，故在构造上不可能再乘行——由 catalog 断言钉住）；
// 不同 instance_src 各一行。
func TestRoutingFlowMergedKeyExactPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)

	older := repository.RoutingFlowRow{
		IdentityVersion: 1, RouteClassID: rc, TerminalMinute: m, Ordinal: 1, Lane: "primary",
		AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success",
		IsTerminal: true, Generation: 5, ChainCount: 10,
	}
	newer := older
	newer.Generation = 7
	newer.ChainCount = 7

	// 同分片同边、不同代际：写面按新键折叠为一行。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-K1", m, 1, 1, []repository.RoutingFlowRow{older, newer}))

	var rowCount, chainSum, minGen int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(chain_count), 0), COALESCE(MIN(min_generation), 0) FROM routing_flow_fact WHERE terminal_minute=$1 AND instance_src=$2`,
		m, "src-K1").Scan(&rowCount, &chainSum, &minGen))
	require.Equal(t, int64(1), rowCount, "same edge with two generations must collapse into one row")
	require.Equal(t, int64(17), chainSum, "chain_count must sum across generations within the shard")
	require.Equal(t, int64(5), minGen, "min_generation must be the minimum generation of the row")

	// 不同 instance_src = 不同分片身份 → 各一行（分片独立，不合并）。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-K2", m, 1, 1, []repository.RoutingFlowRow{newer}))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM routing_flow_fact WHERE terminal_minute=$1`, m).Scan(&rowCount))
	require.Equal(t, int64(2), rowCount, "different instance_src must stay separate rows")

	// 键精确性（catalog）：唯一索引列序 = §4 新键——旧边身份减 generation、
	// candidate_fingerprint，加 instance_src。
	require.Equal(t, []string{
		"terminal_minute", "instance_src", "route_class_id", "ordinal",
		"lane", "account_id", "previous_account_id", "previous_outcome", "transition_reason",
		"outcome", "is_terminal",
	}, mergedKeyColumns(t, pool), "merged-layer unique key drifted from the specified identity")

	cols := flowFactColumnSet(t, pool)
	require.NotContains(t, cols, "candidate_fingerprint",
		"flow identity must not carry the fingerprint dimension (it can no longer add rows)")
	require.NotContains(t, cols, "generation", "generation must be demoted to the aggregate min_generation column")
	require.Contains(t, cols, "min_generation")
	require.Contains(t, cols, "instance_src")
}

// A7：行数下降。夹具钉死为单实例且每边恰一个 generation、一个 candidate_fingerprint
// （多代际夹具会低于 50%，故必须钉死）。同夹具 S2′ 行数恒 =（边, 实例）组合数；
// 单实例夹具恰为基线两表行数和的 50%——基线实例行按全保留期留存，故被消除的
// 那一半是重复副本。
//
// 基线行数用基线 schema 的副本表实测（列/唯一键抄自
// `git show 8037f31:internal/repository/routing.go` 的
// routingFlowInstanceColumnDefs/IndexDDLs 与 routingFlowRollupColumnDefs/IndexDDLs，
// 仅略去当时的常量版本列——它不改变任何行数或唯一性，而本夹具度量的是两表 → 单表
// 的行数减半），而不是用 Go 侧算式回显，故 50% 是测量而非断言我自己的算术。
func TestRoutingFlowMergedRowCountDropPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)

	// 夹具：单实例、每边恰一个 generation、一个 fingerprint。
	const (
		fixtureMinutes   = 3
		fixtureEdges     = 4
		fixtureInstances = 1
		fixtureGen       = 3
	)
	fixtureFP := make([]byte, 32)
	fixtureFP[0] = 0x7A
	edges := make([]repository.RoutingFlowRow, 0, fixtureEdges)
	for e := 0; e < fixtureEdges; e++ {
		edges = append(edges, repository.RoutingFlowRow{
			IdentityVersion: 1, RouteClassID: rc, Ordinal: int16(e + 1), Lane: "primary",
			AccountID: int64(100 + e), PreviousOutcome: "", TransitionReason: "init",
			Outcome: "success", IsTerminal: true, Generation: fixtureGen, ChainCount: 5,
		})
	}
	for i := 0; i < fixtureMinutes; i++ {
		minute := m.Add(time.Duration(i) * time.Minute)
		require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A7", minute, 1, 1, edges))
	}

	var merged int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact`).Scan(&merged))
	require.Equal(t, int64(fixtureEdges*fixtureMinutes*fixtureInstances), merged,
		"merged rows must be exactly the (edge, instance) combinations")

	// 基线两表副本（列与唯一键 = 基线 DDL 口径）。
	_, err := pool.Exec(ctx, `CREATE TABLE baseline_flow_instance (
		terminal_minute timestamptz NOT NULL,
		ordinal smallint NOT NULL,
		lane text NOT NULL,
		account_id bigint NOT NULL,
		previous_account_id bigint NULL,
		previous_outcome text NOT NULL DEFAULT '',
		transition_reason text NOT NULL,
		outcome text NOT NULL,
		is_terminal boolean NOT NULL,
		generation bigint NOT NULL,
		candidate_fingerprint bytea NOT NULL,
		instance_src text NOT NULL,
		route_class_id bytea NOT NULL,
		chain_count bigint NOT NULL DEFAULT 0,
		UNIQUE NULLS NOT DISTINCT (instance_src, terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, route_class_id)
	)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `CREATE TABLE baseline_flow_rollup (
		terminal_minute timestamptz NOT NULL,
		ordinal smallint NOT NULL,
		lane text NOT NULL,
		account_id bigint NOT NULL,
		previous_account_id bigint NULL,
		previous_outcome text NOT NULL DEFAULT '',
		transition_reason text NOT NULL,
		outcome text NOT NULL,
		is_terminal boolean NOT NULL,
		generation bigint NOT NULL,
		candidate_fingerprint bytea NOT NULL,
		route_class_id bytea NOT NULL,
		chain_count bigint NOT NULL DEFAULT 0,
		UNIQUE NULLS NOT DISTINCT (terminal_minute, ordinal, lane, account_id, previous_account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, route_class_id)
	)`)
	require.NoError(t, err)
	for i := 0; i < fixtureMinutes; i++ {
		minute := m.Add(time.Duration(i) * time.Minute)
		for e := 0; e < fixtureEdges; e++ {
			_, err = pool.Exec(ctx, `INSERT INTO baseline_flow_instance
				(terminal_minute, ordinal, lane, account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, instance_src, route_class_id, chain_count)
				VALUES ($1,$2,'primary',$3,'','init','success',true,$4,$5,'src-A7',$6,5)`,
				minute, int16(e+1), int64(100+e), fixtureGen, fixtureFP, rc[:])
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO baseline_flow_rollup
				(terminal_minute, ordinal, lane, account_id, previous_outcome, transition_reason, outcome, is_terminal, generation, candidate_fingerprint, route_class_id, chain_count)
				VALUES ($1,$2,'primary',$3,'','init','success',true,$4,$5,$6,5)`,
				minute, int16(e+1), int64(100+e), fixtureGen, fixtureFP, rc[:])
			require.NoError(t, err)
		}
	}
	var baselineInstanceRows, baselineRollupRows int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM baseline_flow_instance`).Scan(&baselineInstanceRows))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM baseline_flow_rollup`).Scan(&baselineRollupRows))
	require.Equal(t, int64(fixtureEdges*fixtureMinutes), baselineInstanceRows)
	require.Equal(t, int64(fixtureEdges*fixtureMinutes), baselineRollupRows)
	require.Equal(t, 2*merged, baselineInstanceRows+baselineRollupRows,
		"single-instance pinned fixture: the merged layer is exactly 50% of the baseline two-table sum")

	// 字节数前后对比（测量义务，非阻塞）。
	var mergedBytes, baselineBytes int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(SUM(pg_relation_size(c.oid)), 0)
		FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
		WHERE i.inhparent = 'routing_flow_fact'::regclass`).Scan(&mergedBytes))
	require.NoError(t, pool.QueryRow(ctx, `SELECT pg_relation_size('baseline_flow_instance') + pg_relation_size('baseline_flow_rollup')`).Scan(&baselineBytes))
	require.Greater(t, baselineBytes, int64(0))
	t.Logf("A7 heap bytes: merged=%d baseline_two_tables=%d ratio=%.3f", mergedBytes, baselineBytes, float64(mergedBytes)/float64(baselineBytes))
}

// A9：分钟对齐。`from` 整分钟行 included、`to` 整分钟行 excluded（半开
// [from,to)）；非对齐输入按分钟截断后判定。
func TestRoutingFlowRollupWindowAlignmentPG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, base))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)

	// 同一条边在相邻三分钟各写一笔（chain_count 1/2/4）——读面按边身份跨分钟
	// SUM，故和值直接显示哪几分钟被纳入窗口。
	edge := func(minute time.Time, chain int64) repository.RoutingFlowRow {
		return repository.RoutingFlowRow{
			IdentityVersion: 1, RouteClassID: rc, TerminalMinute: minute, Ordinal: 1, Lane: "primary",
			AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success",
			IsTerminal: true, Generation: 1, ChainCount: chain,
		}
	}
	for i, chain := range []int64{1, 2, 4} {
		minute := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-Align", minute, 1, 1, []repository.RoutingFlowRow{edge(minute, chain)}))
	}

	// 非对齐输入 → 截断到分钟：from=base+30s → base，to=base+2m+30s → base+2m。
	stats, err := repos.Partitions.QueryFlowFactStats(ctx, rc, 1, base.Add(30*time.Second), base.Add(2*time.Minute+30*time.Second))
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, int64(3), stats[0].ChainCount,
		"from row included (1) + middle row (2); the to-minute row (4) must be excluded")

	// from 整分钟行 included；to 整分钟行 excluded。
	stats, err = repos.Partitions.QueryFlowFactStats(ctx, rc, 1, base.Add(time.Minute), base.Add(3*time.Minute))
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, int64(6), stats[0].ChainCount, "from-minute row (2) + next row (4)")

	// 单分钟窗口 [base+2m, base+3m)：恰含最后一行。
	stats, err = repos.Partitions.QueryFlowFactStats(ctx, rc, 1, base.Add(2*time.Minute), base.Add(3*time.Minute))
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, int64(4), stats[0].ChainCount)

	// 空窗口（to == from）不报错，返回空。
	stats, err = repos.Partitions.QueryFlowFactStats(ctx, rc, 1, base, base)
	require.NoError(t, err)
	require.Empty(t, stats)
}

// A15：B 的陈旧布尔精确。同边混代际（gen5+gen7）两笔输入合一行后
// min_generation=5，plan_generation=7 时布尔为真；仅 gen7 时为假。
// **断言布尔值而非链数**——链数在该行不可精确拆分（{gen5:10,gen7:5} 折叠为
// chain_count=22，而实际陈旧只有 10），这正是退役占比的原因。
func TestRoutingFlowMergedStaleGenerationPredicatePG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	m := time.Date(2026, 9, 16, 11, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	const planGeneration = int64(7)

	row := func(gen, chain int64) repository.RoutingFlowRow {
		return repository.RoutingFlowRow{
			IdentityVersion: 1, RouteClassID: rc, TerminalMinute: m, Ordinal: 1, Lane: "primary",
			AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success",
			IsTerminal: true, Generation: gen, ChainCount: chain,
		}
	}

	// 混代际：gen5 + gen7 折叠为一行。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-Stale", m, 1, 1, []repository.RoutingFlowRow{row(5, 10), row(7, 5)}))
	stats, err := repos.Partitions.QueryFlowFactStats(ctx, rc, 1, m, m.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, int64(5), stats[0].MinGeneration)
	require.True(t, stats[0].MinGeneration != planGeneration,
		"mixed-generation row (min 5) must read as stale for plan generation 7")

	// 仅当前代际：同一分片以新序号整分片重写为单一 gen7 行。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-Stale", m, 1, 2, []repository.RoutingFlowRow{row(7, 5)}))
	stats, err = repos.Partitions.QueryFlowFactStats(ctx, rc, 1, m, m.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, stats, 1)
	require.Equal(t, int64(7), stats[0].MinGeneration)
	require.False(t, stats[0].MinGeneration != planGeneration,
		"current-generation-only row must not read as stale")
}
