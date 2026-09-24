// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// RED (TestRoutingQualityWindowPlanPG): Q2 must use the fact identity
// index with no Seq Scan on a seeded multi-candidate fixture, and the Q2 row
// count stays bounded by the hot-key count. References the production SQL
// consts directly so plan assertions can never drift from shipped SQL.

type windowPlanNode struct {
	NodeType     string            `json:"Node Type"`
	RelationName string            `json:"Relation Name"`
	IndexName    string            `json:"Index Name"`
	Plans        []*windowPlanNode `json:"Plans"`
}

func walkWindowPlan(n *windowPlanNode, visit func(*windowPlanNode)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.Plans {
		walkWindowPlan(c, visit)
	}
}

func TestRoutingQualityWindowPlanPG(t *testing.T) {
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

	m := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-25*time.Hour), m.Add(24*time.Hour)))

	// 5000 candidates × 12 rows across ~20h + ANALYZE: the hot set (400)
	// is selective against the table, mirroring production where the join
	// must probe the fact identity index instead of seq-scanning the table.
	const ncand, nhot, nrows = 5000, 400, 12
	rc := make([]byte, 32)
	rc[0] = 0xA1
	qc := make([]byte, 32)
	qc[0] = 0xB2
	hotRC, hotFP := make([]string, 0, nhot), make([]string, 0, nhot)
	batch := &pgx.Batch{}
	for i := 0; i < ncand; i++ {
		fp := make([]byte, 32)
		fp[0] = byte(i)
		fp[1] = byte(i >> 8)
		fp[2] = 0xC3
		if i < nhot {
			hotRC = append(hotRC, hex.EncodeToString(rc))
			hotFP = append(hotFP, hex.EncodeToString(fp))
		}
		for h := 0; h < nrows; h++ {
			minute := m.Add(-time.Duration(6+h*110) * time.Minute)
			batch.Queue(
				`INSERT INTO routing_quality_fact
				 (route_class_id, quality_class_id, candidate_fingerprint,
				  instance_src, bucket_minute, absolute_sequence, attempts, successes, updated_at)
				 VALUES ($1, $2, $3, 'src-plan', $4, 1, 10, 8, now())`,
				rc, qc, fp, minute)
		}
	}
	br := pool.SendBatch(ctx, batch)
	_, err = br.Exec()
	require.NoError(t, br.Close())
	_, err = pool.Exec(ctx, `ANALYZE routing_quality_fact`)
	require.NoError(t, err)

	var planJSON string
	err = pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+routingQualityWindowBaselineSQL,
		hotRC, hotFP, m.Add(-24*time.Hour), m.Add(-5*time.Minute)).Scan(&planJSON)
	require.NoError(t, err)
	t.Logf("Q2 plan: %s", planJSON)
	var plan []struct {
		Plan *windowPlanNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(planJSON), &plan))
	require.Len(t, plan, 1)
	var seqScan, identityIndex bool
	var indexNames []string
	walkWindowPlan(plan[0].Plan, func(n *windowPlanNode) {
		if n.NodeType == "Seq Scan" {
			seqScan = true
		}
		if n.IndexName != "" {
			indexNames = append(indexNames, n.IndexName)
		}
		// Partitioned-table auto index names derive per-partition from the
		// column list, not the parent name: the plan reports e.g.
		// routing_quality_fact_20260820_route_class_id_candidate_fing_idx for the
		// parent identity index routing_quality_fact_uniq (whose canonical name is
		// pinned by the catalog test). Match table + identity-key leading columns
		// so the bucket-only index (…_bucket_minute_idx) cannot satisfy it.
		if strings.Contains(n.IndexName, "routing_quality_fact") && strings.Contains(n.IndexName, "route_class_id_candidate") {
			identityIndex = true
		}
	})
	require.False(t, seqScan, "Q2 must not seq-scan routing_quality_fact")
	require.True(t, identityIndex, "Q2 must use the fact identity index, saw %v", indexNames)

	// Row-count bound: one row per hot (route, fp) at most.
	keys := make([]WindowHotKey, 0, nhot)
	for i := 0; i < nhot; i++ {
		var rcV domain.RouteClassIDVal
		copy(rcV[:], rc)
		fpb, err := hex.DecodeString(hotFP[i])
		require.NoError(t, err)
		var fpV domain.CandidateFingerprintVal
		copy(fpV[:], fpb)
		keys = append(keys, WindowHotKey{RouteClassID: rcV, Fingerprint: fpV})
	}
	got, err := repos.Partitions.QueryBaselineTruncated(ctx, m, keys)
	require.NoError(t, err)
	require.LessOrEqual(t, len(got), nhot)
}
