// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 计费结算语句测量基准（三车道拓扑适配，spec-f2opt-settlement）：SettleBalanceBatch /
// SettleFefoBatch 单语句耗时构成测量。真实 PG（TEST_DATABASE_URL，独立库）+
// pg_stat_statements（容器需 -c shared_preload_libraries=pg_stat_statements 启动）：
//   - 每次调用整语句 Go 侧耗时（含网络往返），≥5 轮取中位数（首轮预热）；
//   - 运行窗口内 pg_stat_statements 按语句类型拆分服务器侧耗时与往返次数
//     （batch 取批 / 条件 UPDATE / 透支补刀 / billed 标记 / COMMIT——单语句一体）。
//
// 三车道形态：取批/扣减/标记单语句单事务一体（每窗口一次往返）；usage flusher
// InsertBatch 是唯一写者——种子 unbilled 行先行（不计入结算计时窗口）。
//
// 用法（默认跳过，显式开启）：
//   HOTFIX_BENCH=1 TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15433/c3api_test_hotfix \
//     go test ./internal/repository/ -run TestDeductBenchComposition -v

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// deductBenchRounds 每场景测量轮数（≥5 轮 + 首轮预热，取中位数）。
const deductBenchRounds = 7

// deductBenchLogsPerTx 每窗口标记行数 = fetchBatchLimit（billing/flusher.go 车道
// 窗口上限）——单周期单用户满档形态。
const deductBenchLogsPerTx = 500

// benchLogFor 生产形态计费日志（fullLogFor 全列：列集合锚定回归锚）。
func benchLogFor(userID int64, requestID string) *domain.UsageLog {
	return fullLogFor(userID, requestID)
}

// TestDeductBenchComposition 结算语句单窗口耗时构成（含各环节往返次数与耗时）。
// 结果打印为 medians + pg_stat_statements 拆分明细。
func TestDeductBenchComposition(t *testing.T) {
	if os.Getenv("HOTFIX_BENCH") == "" {
		t.Skip("HOTFIX_BENCH not set; skipping benchmark (use: HOTFIX_BENCH=1 ... -run TestDeductBenchComposition)")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	require.NotEmpty(t, dsn, "TEST_DATABASE_URL must be set (dedicated benchmark PG)")
	repos := newPGRepos(t)
	ctx := context.Background()

	// pg_stat_statements 窗口清零（重置在本测试全部 schema 建立之后——统计面
	// 只含结算语句）。扩展装在 public schema，newPGRepos 的 DROP SCHEMA public
	// CASCADE 会连带删掉扩展对象——此处幂等重建。
	statPool, err := repository.OpenPG(ctx, dsn, 2)
	require.NoError(t, err)
	defer statPool.Close()
	_, err = statPool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`)
	require.NoError(t, err)
	_, err = statPool.Exec(ctx, `SELECT pg_stat_statements_reset()`)
	require.NoError(t, err)

	// 主场景（双载体对比）：500 行/窗口（fetchBatchLimit 满档，≥5 轮取中位数）
	// ——pgx 直连载体（pool）vs ent txDriver 载体（no-pool），同 schema 同环境
	// 交错测量。
	durationsCopy := benchDeductRounds(t, "copy", repos, 0, deductBenchLogsPerTx, deductBenchRounds)
	printBenchReport(t, "pgx 载体: balance 车道 500 行/窗口", durationsCopy)
	reposEnt := newPGReposNoPool(t)
	durationsEnt := benchDeductRounds(t, "ent", reposEnt, 0, deductBenchLogsPerTx, deductBenchRounds)
	printBenchReport(t, "ent txDriver 载体: balance 车道 500 行/窗口", durationsEnt)

	// 临时额度形态（temp-active 用户 → FEFO 车道；窗口函数消费 3 行 temp）——
	// 测量"有临时额度时 FEFO 车道的窗口成本"。
	durationsTB := benchDeductRounds(t, "copytb", repos, 3, deductBenchLogsPerTx, 3)
	printBenchReport(t, "pgx 载体: fefo 车道 500 行/窗口 + temp balances (3 行)", durationsTB)

	// 网络往返延迟直接测量（"往返次数多"假说的决定性实验）：同一连接上 25 次
	// 顺序 SELECT 1。
	durationsRT := benchRoundTrips(t, statPool, 25, 5)
	printBenchReport(t, "client: 25× SELECT 1 顺序往返", durationsRT)

	printStatBreakdown(t, statPool, ctx)
}

// benchDeductRounds 跑 n 轮结算语句（每轮独立用户；tag 保证同 schema 上多载体
// 测量用户不撞 users_email_key；tempRows 为预插的临时额度行数——>0 时用户
// temp-active 走 FEFO 车道，否则走 Balance 车道；perTx 为每窗口标记行数），返回
// 逐轮耗时。种子 unbilled 行先行（usage flusher 单写点形态，InsertBatch 不计入
// 结算事务计时窗口）。
func benchDeductRounds(t *testing.T, tag string, repos *repository.Repository, tempRows, perTx, n int) []time.Duration {
	t.Helper()
	ctx := context.Background()
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		u := seedPGUser(t, repos, fmt.Sprintf("bench-%s-%d-%d-%d@example.com", tag, tempRows, perTx, i))
		require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000_000))
		for k := 0; k < tempRows; k++ {
			seedTempBalance(t, repos, u.ID, 100_000, nil)
		}
		logs := make([]*domain.UsageLog, 0, perTx)
		for j := 0; j < perTx; j++ {
			logs = append(logs, benchLogFor(u.ID, fmt.Sprintf("bench-%s-%d-%d-%d-%d", tag, tempRows, perTx, i, j)))
		}
		require.NoError(t, repos.Usages.InsertBatch(ctx, logs)) // 种子 unbilled 行先行
		rows, err := repos.FetchUnbilledBatch(ctx, perTx)
		require.NoError(t, err)
		require.Len(t, rows, perTx)

		start := time.Now()
		var res domain.SettlementSummary
		if tempRows > 0 { // temp-active：FEFO 车道谓词命中
			res, err = repos.SettleFefoBatch(ctx, perTx)
		} else {
			res, err = repos.SettleBalanceBatch(ctx, perTx)
		}
		d := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, int64(perTx), res.Marked, "整窗口恰标记一次")
		if tempRows == 0 {
			require.NotEmpty(t, res.Balances, "balance 车道返回定向余额对")
			require.Greater(t, res.Balances[0].Balance, int64(0))
		}
		durations = append(durations, d)
	}
	return durations
}

// benchRoundTrips 网络往返延迟测量：单连接上 n 次顺序 SELECT 1。
func benchRoundTrips(t *testing.T, pool *pgxpool.Pool, n, rounds int) []time.Duration {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	durations := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		for j := 0; j < n; j++ {
			var one int
			require.NoError(t, conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
		}
		durations = append(durations, time.Since(start))
	}
	return durations
}

// printBenchReport 逐轮耗时 + 中位数（排序后中间值）。
func printBenchReport(t *testing.T, label string, durations []time.Duration) {
	t.Helper()
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	t.Logf("== %s: %d 轮 中位数 %s", label, len(durations), median)
	for i, d := range durations {
		t.Logf("   round %d: %s", i, d)
	}
}

// printStatBreakdown pg_stat_statements 拆分明细（服务器侧耗时 + 往返次数）：
// 窗口内全部语句按类型分类汇总——专用容器，无其他会话干扰。
func printStatBreakdown(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT query, calls, total_exec_time, rows
		FROM pg_stat_statements ORDER BY total_exec_time DESC`)
	require.NoError(t, err)
	defer rows.Close()

	type stat struct {
		calls     int64
		totalTime float64 // ms
		totalRows int64
	}
	byType := map[string]*stat{}
	order := []string{}
	for rows.Next() {
		var query string
		var calls, r int64
		var total float64
		require.NoError(t, rows.Scan(&query, &calls, &total, &r))
		if calls == 0 {
			continue
		}
		typ := classifyBenchQuery(query)
		s, ok := byType[typ]
		if !ok {
			s = &stat{}
			byType[typ] = s
			order = append(order, typ)
		}
		s.calls += calls
		s.totalTime += total
		s.totalRows += r
	}
	require.NoError(t, rows.Err())
	t.Logf("== pg_stat_statements 拆分明细（服务器侧耗时）:")
	for _, typ := range order {
		s := byType[typ]
		mean := s.totalTime / float64(s.calls)
		t.Logf("   %-22s calls=%5d total=%8.2fms mean=%7.3fms rows=%d", typ, s.calls, s.totalTime, mean, s.totalRows)
	}
}

// classifyBenchQuery 按语句特征分类（结算语句构成环节 + 种子面）。
func classifyBenchQuery(query string) string {
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "temp_balances"):
		if strings.Contains(q, "update") {
			return "UPDATE temp_balances (FEFO drawn)"
		}
		return "SELECT temp_balances (FEFO pool)"
	case strings.Contains(q, `"users"`):
		if strings.Contains(q, "update") {
			return "UPDATE users (条件扣/透支)"
		}
		return "SELECT users (余额)"
	case strings.Contains(q, "usage_logs"):
		if strings.Contains(q, "update") {
			return "UPDATE usage_logs (billed 标记)"
		}
		return "INSERT/SELECT usage_logs (种子/取批)"
	case strings.Contains(q, "begin"):
		return "BEGIN"
	case strings.Contains(q, "commit"):
		return "COMMIT"
	case strings.Contains(q, "rollback"):
		return "ROLLBACK"
	default:
		return "other"
	}
}
