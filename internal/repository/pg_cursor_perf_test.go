// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 游标分页性能基准（≥1M 行基准并入正式测试——此前仅 tmp 测量未入库，
// 提交内 EXPLAIN 是 1 行分区无 ANALYZE，实测计划与声称的 pkey backward 不符）。
// 真实 PG（TEST_DATABASE_URL）：当日分区灌 ≥1M 行（INSERT...SELECT 服务端生成，
// 零客户端往返）→ ANALYZE → 断言 keyset 游标页毫秒级 + 计划形态（pkey Index
// Scan Backward、无 Seq Scan、分区裁剪到当日分区），并与 OFFSET 深翻页对比
// （keyset 修复的核心：OFFSET 12ms 级 → 游标页 0.1ms 级）。
//
// 基准规模 1M 行在专用测试库上种子 ~2-4s + ANALYZE ~1-2s；毫秒级上界 20ms
// 是实测值（0.07-0.13ms）的 150 倍以上裕量——只防数量级回归，不卡机器噪音。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// pgCursorPerfRows ≥1M 行基准规模（验收线）。
const pgCursorPerfRows = 1_000_000

// TestPGCursorPerfBounded 游标页性能基准（验收）：1M 行单日分区 + ANALYZE 后
//   - 游标页（id < cursor + 时间窗 + LIMIT 21）端到端耗时毫秒级（实测 0.1ms 级；
//     上界 20ms）；
//   - 游标页显著快于同位置 OFFSET 深翻页（keyset 相对 offset 的收益对比）；
//   - EXPLAIN：pkey Index Scan Backward + 无 Seq Scan + 命中仅当日分区。
func TestPGCursorPerfBounded(t *testing.T) {
	_ = newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	// 种子：1M 行 × 20ms 间隔 = 5.6h，全部落在当日分区（12:00 起回退到 06:24）
	seedStart := time.Now()
	pgExec(t, pool, `
		INSERT INTO usage_logs (request_id, model, format, error_type, latency_ms, total_tokens, cost, created_at)
		SELECT 'perf-' || g, 'gpt-4o', 'openai-chat', 'none', 42, 100, 130, $1::timestamptz - g * interval '20 milliseconds'
		FROM generate_series(1, $2) g`, today, pgCursorPerfRows)
	t.Logf("seed %d rows: %s", pgCursorPerfRows, time.Since(seedStart))
	require.Equal(t, int64(pgCursorPerfRows), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs`),
		"种子行数必须 ≥1M")
	pgExec(t, pool, `ANALYZE usage_logs`)

	from := today.Add(-time.Hour).Format(time.RFC3339)
	to := today.Add(2 * time.Hour).Format(time.RFC3339)

	// cursor 取"第 100k 条最新行"的 id（OFFSET 查询顺带量出深翻页基线）
	var cursorID int64
	err := pool.QueryRow(ctx,
		`SELECT id FROM usage_logs WHERE created_at >= $1 AND created_at <= $2 ORDER BY id DESC OFFSET 100000 LIMIT 1`,
		from, to).Scan(&cursorID)
	require.NoError(t, err)

	// 两路对比：同窗口同位置——OFFSET 深翻 100k vs keyset 游标页（id < cursor）
	offsetDur := measureUsagePage(t, pool, `
		SELECT id, created_at FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2
		ORDER BY id DESC OFFSET 100000 LIMIT 21`, from, to, nil)
	cursorDur := measureUsagePage(t, pool, `
		SELECT id, created_at FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2 AND id < $3
		ORDER BY id DESC LIMIT 21`, from, to, &cursorID)
	t.Logf("offset 100k 页: %s | cursor 页: %s", offsetDur, cursorDur)

	require.Less(t, cursorDur, 20*time.Millisecond,
		"游标页毫秒级（实测 0.1ms 级；%s 是 150x 以上裕量）", cursorDur)
	require.Less(t, cursorDur, offsetDur,
		"keyset 游标页必须快于同位置 OFFSET 深翻页（%s vs %s）", cursorDur, offsetDur)

	// 计划形态：pkey Index Scan Backward + 无 Seq Scan + 分区裁剪到当日
	var planJSON string
	require.NoError(t, pool.QueryRow(ctx, `
		EXPLAIN (FORMAT JSON) SELECT id, created_at FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2 AND id < $3
		ORDER BY id DESC LIMIT 21`, from, to, cursorID).Scan(&planJSON))
	t.Logf("perf plan: %s", planJSON)
	var plan []struct {
		Plan *planNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(planJSON), &plan))
	require.Len(t, plan, 1)
	var pkeyBackward, seqScan bool
	relations := map[string]bool{}
	walkPlan(plan[0].Plan, func(n *planNode) {
		// 主键是 (id, created_at) 覆盖索引——planner 两态：Index Scan Backward
		// 或更优的 Index Only Scan（Backward，免 heap 取列）；二者都是 pkey
		// 逆序提前终止，按"索引名 *_pkey + 逆序"统一断言。
		if (n.NodeType == "Index Scan Backward" ||
			(n.NodeType == "Index Only Scan" && n.ScanDirection == "Backward")) &&
			strings.HasSuffix(n.IndexName, "_pkey") {
			pkeyBackward = true
		}
		if n.NodeType == "Seq Scan" {
			seqScan = true
		}
		if n.RelationName != "" {
			relations[n.RelationName] = true
		}
	})
	require.True(t, pkeyBackward, "1M 行 ANALYZE 后计划必须 pkey 逆序扫描")
	require.False(t, seqScan, "无全分区 Seq Scan")
	require.Len(t, relations, 1, "时间窗裁剪到 1 个分区")
	require.True(t, relations["usage_logs_"+today.Format("20060102")],
		"命中分区 = 当日分区 usage_logs_%s", today.Format("20060102"))
}

// measureUsagePage 测一页查询端到端耗时（含行扫描解码）：5 轮取最小值
// （首轮冷页/GC 噪音由 min 排除）；cursor 为 nil 时查询不带 id 谓词（OFFSET 路）。
func measureUsagePage(t *testing.T, pool *pgxpool.Pool, q string, from, to string, cursor *int64) time.Duration {
	t.Helper()
	best := time.Duration(1 << 62)
	for i := 0; i < 5; i++ {
		args := []any{from, to}
		if cursor != nil {
			args = append(args, *cursor)
		}
		start := time.Now()
		rows, err := pool.Query(context.Background(), q, args...)
		require.NoError(t, err)
		n := 0
		var lastID int64
		for rows.Next() {
			var id int64
			var at time.Time
			require.NoError(t, rows.Scan(&id, &at))
			lastID = id
			n++
		}
		require.NoError(t, rows.Err())
		rows.Close()
		d := time.Since(start)
		require.LessOrEqual(t, n, 21, "探测语义：页 ≤ limit+1 行")
		if cursor != nil {
			require.Less(t, lastID, *cursor, "游标页全部行 id < cursor")
		}
		if d < best {
			best = d
		}
	}
	return best
}
