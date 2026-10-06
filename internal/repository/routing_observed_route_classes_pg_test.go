// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

// A8（真实 PG）：QueryObservedRouteClasses 的功能正确性与访问路径。
//  - 多 route 多分钟事实去重集合正确；
//  - 半开窗口 [from,to)：`to` 边界行排除、`from` 前一行排除；
//  - 空表 → 非 nil 空切片；
//  - 窄窗口 EXPLAIN：无 Seq Scan 且命中 routing_flow_fact_uniq；
//  - EXPLAIN ANALYZE：记录扫描行数上界（噪声类数 × 查询分钟数）。

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func observedRouteClass(fill byte) domain.RouteClassIDVal {
	var v domain.RouteClassIDVal
	v[0] = fill
	return v
}

// insertObservedFlowFact 插一条最小完整链事实（分区表 routing_flow_fact）。
func insertObservedFlowFact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rc domain.RouteClassIDVal, minute time.Time, src string) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO routing_flow_fact
		(route_class_id, terminal_minute, ordinal, lane, account_id,
		 previous_outcome, transition_reason, outcome, is_terminal, instance_src,
		 min_generation, chain_count, updated_at)
		VALUES ($1, $2, 1, 'primary', 10, '', 'init', 'success', true, $3, 1, 5, now())`,
		rc[:], minute, src)
	require.NoError(t, err)
}

func openObservedPG(t *testing.T) (context.Context, *pgxpool.Pool, *Repository) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	ctx := context.Background()
	pool, err := OpenPG(ctx, dsn, 2)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
	require.NoError(t, err)
	repos, err := NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	return ctx, pool, repos
}

func TestQueryObservedRouteClassesPG(t *testing.T) {
	ctx, pool, repos := openObservedPG(t)

	m := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))

	from, to := m.Add(-2*time.Minute), m
	a, b, c, d := observedRouteClass(0x01), observedRouteClass(0x02), observedRouteClass(0x03), observedRouteClass(0x04)

	// routeA：窗口内两分钟共三行（同分钟不同 src → 仍去重为一条）。
	insertObservedFlowFact(t, ctx, pool, a, from, "src-1")
	insertObservedFlowFact(t, ctx, pool, a, from, "src-2")
	insertObservedFlowFact(t, ctx, pool, a, from.Add(time.Minute), "src-1")
	// routeB：窗口内最后一分钟命中（半开的上端不含 `to`）。
	insertObservedFlowFact(t, ctx, pool, b, from.Add(time.Minute), "src-1")
	// routeC：恰在 `to` → 排除（半开）。
	insertObservedFlowFact(t, ctx, pool, c, to, "src-1")
	// routeD：在 `from` 前一分钟 → 排除。
	insertObservedFlowFact(t, ctx, pool, d, from.Add(-time.Minute), "src-1")

	got, err := repos.Partitions.QueryObservedRouteClasses(ctx, from, to)
	require.NoError(t, err)
	require.Equal(t, []domain.RouteClassIDVal{a, b}, got, "半开窗口去重集合（按 route_class_id 升序）")
}

func TestQueryObservedRouteClassesPG_Empty(t *testing.T) {
	ctx, _, repos := openObservedPG(t)

	m := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))

	got, err := repos.Partitions.QueryObservedRouteClasses(ctx, m.Add(-time.Hour), m)
	require.NoError(t, err)
	require.NotNil(t, got, "空集必须是非 nil 空切片")
	require.Empty(t, got)
}

func TestQueryObservedRouteClassesPlanPG(t *testing.T) {
	ctx, pool, repos := openObservedPG(t)

	m := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))

	// 噪声：单个日分区内铺满 windowMinutes 分钟 × noiseClasses 个路由类，使
	// terminal_minute 范围谓词在窄查询窗下高度选择性 → 唯一可用访问路径是
	// routing_flow_fact_uniq（terminal_minute 首列，可 Index Only Scan）。
	const (
		windowMinutes = 720
		noiseClasses  = 100
		queryMinutes  = 2
	)
	from := m.Add(-time.Duration(windowMinutes) * time.Minute)
	batch := &pgx.Batch{}
	for min := 0; min < windowMinutes; min++ {
		minute := from.Add(time.Duration(min) * time.Minute)
		for c := 0; c < noiseClasses; c++ {
			rc := observedRouteClass(0xD0)
			rc[1] = byte(c)
			rc[2] = byte(c >> 8)
			batch.Queue(`INSERT INTO routing_flow_fact
				(route_class_id, terminal_minute, ordinal, lane, account_id,
				 previous_outcome, transition_reason, outcome, is_terminal, instance_src,
				 min_generation, chain_count, updated_at)
				VALUES ($1, $2, 1, 'primary', 10, '', 'init', 'success', true, $3, 1, 5, now())`,
				rc[:], minute, "src-M1")
		}
	}
	br := pool.SendBatch(ctx, batch)
	_, err := br.Exec()
	require.NoError(t, br.Close())
	_, err = pool.Exec(ctx, `ANALYZE routing_flow_fact`)
	require.NoError(t, err)

	qFrom, qTo := m.Add(-time.Duration(queryMinutes)*time.Minute), m

	var planJSON string
	err = pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+observedRouteClassesSQL, qFrom, qTo).Scan(&planJSON)
	require.NoError(t, err)
	t.Logf("observed route classes plan: %s", planJSON)
	var plan []struct {
		Plan *mergedReadPlanNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(planJSON), &plan))
	require.Len(t, plan, 1)

	var seqScan bool
	var indexNames []string
	walkMergedReadPlan(plan[0].Plan, func(n *mergedReadPlanNode) {
		if n.NodeType == "Seq Scan" {
			seqScan = true
		}
		if n.IndexName != "" {
			indexNames = append(indexNames, n.IndexName)
		}
	})
	// 本夹具为窄窗口，规划器稳定命中 routing_flow_fact_uniq（Index Only Scan）；宽窗口下
	// 规划器可合法退化为 Seq Scan（spec §4.2 已承认）——故此处仅对窄窗口夹具断言，不弱化。
	require.False(t, seqScan, "narrow-window observed route lookup must not seq-scan routing_flow_fact")
	require.NotEmpty(t, indexNames, "narrow-window observed route lookup must go through an index")
	require.Contains(t, parentIndexNames(t, ctx, pool, indexNames), "routing_flow_fact_uniq",
		"observed route lookup must use routing_flow_fact_uniq; used %v", indexNames)

	// EXPLAIN ANALYZE：扫描行数上界 = 噪声类数 × 查询分钟数（每 (类, 分钟) 恰一行）。
	var planAnalyze string
	err = pool.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+observedRouteClassesSQL, qFrom, qTo).Scan(&planAnalyze)
	require.NoError(t, err)
	var analyzed []struct {
		Plan *mergedReadPlanNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(planAnalyze), &analyzed))
	require.Len(t, analyzed, 1)
	var scanned float64
	walkMergedReadPlan(analyzed[0].Plan, func(n *mergedReadPlanNode) {
		switch n.NodeType {
		case "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
			scanned += n.ActualRows
		}
	})
	bound := float64(noiseClasses * queryMinutes)
	require.Greater(t, scanned, float64(0), "the narrow window must actually read rows")
	require.LessOrEqual(t, scanned, bound, "scanned rows must stay within classes × window minutes")
	t.Logf("observed route classes scan: scanned=%v bound=%v", scanned, bound)
}
