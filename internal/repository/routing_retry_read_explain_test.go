// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

// 重试聚合（/routing/retry）的多 route 读面必须命中既有索引
// routing_flow_fact_read (route_class_id, terminal_minute)：ANY($1) 只是把
// 等值谓词换成「逐元素范围」，不得因此退化成全表 Seq Scan。
//
// 夹具与 A14（routing_flow_merged_read_explain_test.go）同构：窗口覆盖整整
// 360 分钟且噪声在同一窗口内均匀铺开 ⇒ terminal_minute 范围对表不具选择性，
// route_class_id 集合才是唯一有选择性的访问路径。引用生产 SQL 常量
// flowFactStatsMultiSQL（同包），故计划断言不会与发布 SQL 漂移。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

func TestRoutingRetryMultiReadPlanPG(t *testing.T) {
	dsn := pgtest.CloneEmpty(t)
	ctx := context.Background()
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)

	m := time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))

	const (
		windowMinutes = 360
		instances     = 2
		noiseClasses  = 60
	)
	from := m.Add(-time.Duration(windowMinutes) * time.Minute)
	hotRC := make([]byte, 32)
	hotRC[0] = 0xC1
	instancesSrc := []string{"src-M1", "src-M2"}
	batch := &pgx.Batch{}
	for inst := range instances {
		for min := range windowMinutes {
			minute := from.Add(time.Duration(min) * time.Minute)
			batch.Queue(`INSERT INTO routing_flow_fact
				(route_class_id, terminal_minute, ordinal, lane, account_id,
				 previous_outcome, transition_reason, outcome, is_terminal, instance_src,
				 min_generation, chain_count, updated_at)
				VALUES ($1, $2, 1, 'primary', 10, '', 'init', 'success', true, $3, 1, 5, now())`,
				hotRC, minute, instancesSrc[inst])
			for c := range noiseClasses {
				rc := make([]byte, 32)
				rc[0] = 0xD0
				rc[1] = byte(c)
				rc[2] = byte(c >> 8)
				batch.Queue(`INSERT INTO routing_flow_fact
					(route_class_id, terminal_minute, ordinal, lane, account_id,
					 previous_outcome, transition_reason, outcome, is_terminal, instance_src,
					 min_generation, chain_count, updated_at)
					VALUES ($1, $2, 1, 'primary', 10, '', 'init', 'success', true, $3, 1, 5, now())`,
					rc, minute, instancesSrc[inst])
			}
		}
	}
	br := pool.SendBatch(ctx, batch)
	_, err = br.Exec()
	require.NoError(t, br.Close())
	_, err = pool.Exec(ctx, `ANALYZE routing_flow_fact`)
	require.NoError(t, err)

	// 行数前置断言：「不是 Seq Scan + 用了父索引」在**空表**上无条件成立。
	// 先证明夹具真的插进了声明规模的行，否则分区日期漂移 / 批量写失败时，
	// 下面的计划断言会静默退化成空表也过。
	const insertedRows = instances * windowMinutes * (noiseClasses + 1)
	var windowRows int64
	err = pool.QueryRow(ctx, `SELECT count(*) FROM routing_flow_fact
		WHERE terminal_minute >= $1 AND terminal_minute < $2`, from, m).Scan(&windowRows)
	require.NoError(t, err)
	require.Equal(t, int64(insertedRows), windowRows,
		"夹具必须真有行：窗口内行数 == 插入量（空窗口下计划断言无鉴别力）")

	// 生产形态：route 集合作为 bytea[] 参数（[][]byte → ANY($1)）。
	// EXPLAIN ANALYZE 才能拿到 Actual Rows：执行本身只读，且聚合 720 行很便宜。
	classIDs := [][]byte{hotRC}
	var planJSON string
	err = pool.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+flowFactStatsMultiSQL,
		classIDs, from, m).Scan(&planJSON)
	require.NoError(t, err)
	t.Logf("retry multi read plan: %s", planJSON)
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
	require.False(t, seqScan, "multi-route flow read must not seq-scan routing_flow_fact")
	require.NotEmpty(t, indexNames, "multi-route flow read must go through an index")
	// 计划真的产出了行（空表时顶层聚合返回 0 行、扫描节点也读 0 行）。
	require.Greater(t, plan[0].Plan.ActualRows, float64(0),
		"EXPLAIN ANALYZE 顶层必须返回行；0 行 = 夹具空或窗口漂移")
	var maxActualRows float64
	walkMergedReadPlan(plan[0].Plan, func(n *mergedReadPlanNode) {
		if n.ActualRows > maxActualRows {
			maxActualRows = n.ActualRows
		}
	})
	require.Greater(t, maxActualRows, float64(0), "计划里至少一个节点必须真的读到行")
	// 分区子索引名不含父索引名，故经 pg_inherits 解析回父索引再断言。
	require.Contains(t, parentIndexNames(t, ctx, pool, indexNames), "routing_flow_fact_read",
		"multi-route flow read must use routing_flow_fact_read; used %v", indexNames)
}
