// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// A14：读支撑索引生效。跨分片读的访问路径是
// (route_class_id, identity_version, terminal_minute 范围)；新唯一索引以
// terminal_minute 起头不服务该路径，故必须显式建
// routing_flow_fact_read。本测试以 EXPLAIN 断言该索引被选中且无 Seq Scan，
// 并以 EXPLAIN ANALYZE 实测跨分片扇出 ≤ 实例数 × 窗口分钟数。
//
// 引用生产 SQL 常量 flowFactStatsSQL（同包），故计划断言不会与发布 SQL 漂移。
type mergedReadPlanNode struct {
	NodeType     string                `json:"Node Type"`
	RelationName string                `json:"Relation Name"`
	IndexName    string                `json:"Index Name"`
	ActualRows   float64               `json:"Actual Rows"`
	Plans        []*mergedReadPlanNode `json:"Plans"`
}

func walkMergedReadPlan(n *mergedReadPlanNode, visit func(*mergedReadPlanNode)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.Plans {
		walkMergedReadPlan(c, visit)
	}
}

// parentIndexNames 把 EXPLAIN 报出的索引名解析回父索引名（分区表子索引经
// pg_inherits 挂在父索引下；非分区索引回落到自身）。catalog 事实，非硬编码名。
func parentIndexNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool, names []string) []string {
	t.Helper()
	out := make([]string, 0, len(names))
	for _, name := range names {
		var parent string
		err := pool.QueryRow(ctx, `SELECT COALESCE(pi.indexrelid::regclass::text, ci.indexrelid::regclass::text)
			FROM pg_index ci
			LEFT JOIN pg_inherits inh ON inh.inhrelid = ci.indexrelid
			LEFT JOIN pg_index pi ON pi.indexrelid = inh.inhparent
			WHERE ci.indexrelid = $1::regclass`, name).Scan(&parent)
		require.NoError(t, err, "resolve index %s", name)
		out = append(out, parent)
	}
	return out
}

func TestRoutingFlowMergedReadPlanPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	ctx := context.Background()
	pool, err := OpenPG(ctx, dsn, 2)
	require.NoError(t, err)
	defer pool.Close()
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
	require.NoError(t, err)
	repos, err := NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)

	m := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))

	// 夹具：查询窗覆盖整整 360 分钟且噪声在同一窗口内均匀铺开——于是
	// terminal_minute 范围对表不具选择性（唯一索引无用武之地），
	// route_class_id 等值才是唯一有选择性的访问路径。
	const (
		windowMinutes = 360
		instances     = 2
		noiseClasses  = 100
	)
	from := m.Add(-time.Duration(windowMinutes) * time.Minute)
	hotRC := make([]byte, 32)
	hotRC[0] = 0xC1
	instancesSrc := []string{"src-M1", "src-M2"}
	batch := &pgx.Batch{}
	for inst := 0; inst < instances; inst++ {
		for min := 0; min < windowMinutes; min++ {
			minute := from.Add(time.Duration(min) * time.Minute)
			// 热类：每 (实例, 分钟) 恰一条边身份——扇出上界 = 实例数 × 窗口分钟数。
			batch.Queue(`INSERT INTO routing_flow_fact
				(identity_version, route_class_id, terminal_minute, ordinal, lane, account_id,
				 previous_outcome, transition_reason, outcome, is_terminal, instance_src,
				 min_generation, chain_count, updated_at)
				VALUES (1, $1, $2, 1, 'primary', 10, '', 'init', 'success', true, $3, 1, 5, now())`,
				hotRC, minute, instancesSrc[inst])
			// 噪声：其余路由类在同一窗口内同样铺满。
			for c := 0; c < noiseClasses; c++ {
				rc := make([]byte, 32)
				rc[0] = 0xD0
				rc[1] = byte(c)
				rc[2] = byte(c >> 8)
				batch.Queue(`INSERT INTO routing_flow_fact
					(identity_version, route_class_id, terminal_minute, ordinal, lane, account_id,
					 previous_outcome, transition_reason, outcome, is_terminal, instance_src,
					 min_generation, chain_count, updated_at)
					VALUES (1, $1, $2, 1, 'primary', 10, '', 'init', 'success', true, $3, 1, 5, now())`,
					rc, minute, instancesSrc[inst])
			}
		}
	}
	br := pool.SendBatch(ctx, batch)
	_, err = br.Exec()
	require.NoError(t, br.Close())
	_, err = pool.Exec(ctx, `ANALYZE routing_flow_fact`)
	require.NoError(t, err)

	var planJSON string
	err = pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+flowFactStatsSQL,
		hotRC, int16(1), from, m).Scan(&planJSON)
	require.NoError(t, err)
	t.Logf("merged read plan: %s", planJSON)
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
	require.False(t, seqScan, "cross-shard flow read must not seq-scan routing_flow_fact")
	require.NotEmpty(t, indexNames, "cross-shard flow read must go through an index")
	// 分区表的子索引名由「分区表名 + 列名」自动派生，不含父索引名，故必须把
	// 用到的子索引经 pg_inherits 解析回父索引再断言（父名才是 DDL 承诺的那一个）。
	require.Contains(t, parentIndexNames(t, ctx, pool, indexNames), "routing_flow_fact_read",
		"cross-shard flow read must use routing_flow_fact_read; used %v", indexNames)

	// 扇出实测：窗口内每个 (实例, 分钟) 恰一行热类边，故堆访问行数上界 =
	// 实例数 × 窗口分钟数（实例越多、分钟越多 → 扇出线性，绝无实例间的二次放大）。
	var planAnalyze string
	err = pool.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+flowFactStatsSQL,
		hotRC, int16(1), from, m).Scan(&planAnalyze)
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
			if strings.Contains(n.RelationName, "routing_flow_fact") {
				scanned += n.ActualRows
			}
		}
	})
	bound := float64(instances * windowMinutes)
	require.Greater(t, scanned, float64(0), "the hot class must actually be read")
	require.LessOrEqual(t, scanned, bound, "cross-shard fan-out must stay within instances × window minutes")
	t.Logf("A14 fan-out: scanned=%v bound=%v", scanned, bound)
}
