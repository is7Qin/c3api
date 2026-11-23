// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 请求链读面的 PG 证据（spec 2026-11-18 retry-observability §4/§6 S3-10）。
//
// 两个用例，各自只证明一件事：
//
//	TestAttemptChainQueryLegsPG      —— 三条腿的列映射、窗口边界、LIMIT N+1 的
//	                                    truncated 判定（每源独立）。
//	TestAttemptChainErrLegScanCostPG —— **err_logs 腿的扫描代价**：该表没有
//	                                    request_id 索引，只能靠 created_at 窗口做
//	                                    分区裁剪 + 范围扫描，再按 request_id 过滤。
//	                                    本用例把真实规模（当日 5 万行 + 前 6 日各 8 千行）
//	                                    灌进去，用生产 SQL 常量跑 EXPLAIN
//	                                    (ANALYZE, BUFFERS)，把耗时写进日志供报告引用；
//	                                    同时钉住「窗口外的分区不参与扫描」。
//
// 本包 PG 测试串行（AGENTS.md 全仓禁用并行测试）；基座 = CloneEmpty + 版本化迁移
// （分区由 EnsureAllTablePartitions 补齐，与生产 `-migrate up` 尾步一致）。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// openAttemptChainDB 空库 + 版本化迁移 + 当日/明日分区（生产 `-migrate up` 的尾步）。
func openAttemptChainDB(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.CloneEmpty(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	res, err := migrate.Up(ctx, db, migrate.UpOptions{})
	require.NoError(t, err)
	require.NotZero(t, res.To, "空库必须真的应用了迁移")
	repos, err := NewWithoutMigrate(ctx, entsql.OpenDB(dialect.Postgres, db), pool)
	require.NoError(t, err)
	// 分区归 runtime：迁移只建父表（0001 六张 + 0003 的 attempt_logs）。
	now := time.Now().UTC()
	require.NoError(t, repos.Partitions.EnsureAllTablePartitions(ctx, now, now.AddDate(0, 0, 1)))
	return repos, pool
}

// ensureWritePartitions 预建夹具会写到的三张「近期写入」分区表在 [earliest, now+1h]
// 的每日分区。pgtest 克隆只保证 [now, now+1d]（对齐生产 -migrate up 的尾步 / 每小时
// 预建），而夹具常按「相对 now 的分钟/小时偏移」造历史行：跨 UTC 零点时这些行落在
// 昨天，缺分区就是 SQLSTATE 23514。
//
// 约定：历史行是夹具的一部分，**谁造谁建**（同 TestAttemptChainErrLegScanCostPG:348、
// routing_quality_window_test.go:68）。不要靠给克隆加默认的「昨天」分区来兜——
// 那会让每个用例都背一个不可见的 1 天边界，且 CloneEmpty 路径根本不经那条 hook。
func ensureWritePartitions(t *testing.T, ctx context.Context, repos *Repository, earliest time.Time) {
	t.Helper()
	until := time.Now().UTC().Add(time.Hour)
	require.NoError(t, repos.Partitions.EnsureAttemptLogPartitions(ctx, earliest, until), "attempt_logs 分区")
	require.NoError(t, repos.Partitions.EnsureErrLogPartitions(ctx, earliest, until), "err_logs 分区")
	require.NoError(t, repos.Partitions.EnsureUsageLogPartitions(ctx, earliest, until), "usage_logs 分区")
}

// attemptChainAttemptRow 造一条 attempt_logs 行（字段有区分度，列错位即红）。
func attemptChainAttemptRow(reqID string, ordinal int16, accountID int64, lane string, at time.Time) *domain.AttemptLog {
	return &domain.AttemptLog{
		RequestID: reqID, Ordinal: ordinal, AccountID: accountID, Lane: lane,
		Format: "openai-chat", StatusCode: 502, Outcome: "5xx", ErrorType: "5xx",
		LatencyMS: 12, CreatedAt: at,
	}
}

// attemptChainUsageRow 造一条 usage_logs 终态行（commit_state 非空：证明该列
// 在三源 DTO 上真的被映射，NULL 与有值两种语义在 err 腿上另有用例覆盖）。
func attemptChainUsageRow(reqID string, accountID int64, at time.Time) *domain.UsageLog {
	commit := "response_started"
	return &domain.UsageLog{
		RequestID: reqID, AccountID: accountID, Format: domain.FormatOpenAIChat,
		StatusCode: 200, ErrorType: domain.ErrNone, CommitState: &commit,
		LatencyMS: 900, CreatedAt: at,
	}
}

// TestAttemptChainQueryLegsPG 三条腿各自的列映射与窗口边界。
//
// 注意：本用例**不**断言去重（那是 service 纯函数 + internal/proxy 真实 failover
// 用例的职责）。这里手写的行只服务读面：列序、NULL 语义、ORDER BY、窗口裁剪、
// truncated。拿手搓行证明去重恰好是 spec §4 点名要避免的假绿。
func TestAttemptChainQueryLegsPG(t *testing.T) {
	ctx := context.Background()
	repos, pool := openAttemptChainDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	from, to := base, base.Add(time.Hour)
	// 行最早到 base-1m，跨 UTC 零点时它落在昨天：历史行是夹具，分区自己建。
	ensureWritePartitions(t, ctx, repos, base.Add(-time.Minute))

	// attempt 腿：ordinal 1/2（lane/outcome 有区分度，错位即红）。
	require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, []*domain.AttemptLog{
		attemptChainAttemptRow("chain-a", 1, 11, "primary", base.Add(1*time.Minute)),
		attemptChainAttemptRow("chain-a", 2, 12, "secondary", base.Add(2*time.Minute)),
		// 同 request 但窗口外：不得出现。
		attemptChainAttemptRow("chain-a", 3, 13, "primary", base.Add(-time.Minute)),
		// 其它 request 在窗口内：不得出现。
		attemptChainAttemptRow("chain-other", 1, 14, "primary", base.Add(3*time.Minute)),
	}))

	// err 腿：一条带 ordinal（会被 attempt 行吸收，读面照常返回）、一条 NULL
	// ordinal（本地拒绝），外加窗口外/其它 request 各一行。
	for _, stmt := range []struct {
		reqID   string
		ordinal any
		at      time.Time
		status  int
	}{
		{"chain-a", int16(2), base.Add(2*time.Minute + 30*time.Second), 502},
		{"chain-a", nil, base.Add(30 * time.Second), 401},
		{"chain-a", nil, base.Add(-time.Minute), 401},
		{"chain-other", nil, base.Add(4 * time.Minute), 401},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO err_logs
			(request_id, attempt_ordinal, format, status_code, error_type, error_message, latency_ms, created_at)
			VALUES ($1, $2, 'openai-chat', $3, 'auth', 'detail', 5, $4)`,
			stmt.reqID, stmt.ordinal, stmt.status, stmt.at)
		require.NoError(t, err)
	}

	// usage 腿：请求级终态一条。
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		attemptChainUsageRow("chain-a", 11, base.Add(5*time.Minute)),
		attemptChainUsageRow("chain-a", 11, base.Add(-time.Minute)),
		attemptChainUsageRow("chain-other", 14, base.Add(6*time.Minute)),
	}))

	got, err := repos.QueryAttemptChain(ctx, "chain-a", from, to)
	require.NoError(t, err)

	require.Len(t, got.Attempt.Rows, 2, "窗口内只有 ordinal 1/2 两行（窗口外与其它 request 被裁掉）")
	require.Equal(t, []int16{1, 2}, []int16{*got.Attempt.Rows[0].Ordinal, *got.Attempt.Rows[1].Ordinal},
		"attempt 腿按 (ordinal, created_at) 出序")
	require.Equal(t, "primary", got.Attempt.Rows[0].Lane)
	require.Equal(t, "secondary", got.Attempt.Rows[1].Lane)
	require.Equal(t, int64(11), got.Attempt.Rows[0].AccountID)
	require.Equal(t, 502, got.Attempt.Rows[0].StatusCode)
	require.Equal(t, "5xx", got.Attempt.Rows[0].Outcome)
	require.Equal(t, "5xx", got.Attempt.Rows[0].ErrorType)
	require.False(t, got.Attempt.Truncated)

	require.Len(t, got.Err.Rows, 2, "窗口内两条 err（一条带 ordinal、一条 NULL）")
	require.Nil(t, got.Err.Rows[0].Ordinal, "按 created_at 出序：拒绝行在前")
	require.Equal(t, 401, got.Err.Rows[0].StatusCode)
	require.NotNil(t, got.Err.Rows[0].ErrorMessage)
	require.Equal(t, "detail", *got.Err.Rows[0].ErrorMessage)
	require.NotNil(t, got.Err.Rows[1].Ordinal)
	require.Equal(t, int16(2), *got.Err.Rows[1].Ordinal)
	require.Equal(t, 502, got.Err.Rows[1].StatusCode, "err 腿的 status_code 与 attempt 腿不同值：列映射错位即红")
	require.False(t, got.Err.Truncated)

	require.Len(t, got.Usage.Rows, 1, "usage 腿只取窗口内那一行")
	require.Nil(t, got.Usage.Rows[0].Ordinal, "usage_logs 没有序号列")
	require.Equal(t, int64(11), got.Usage.Rows[0].AccountID)
	require.Equal(t, "none", got.Usage.Rows[0].ErrorType)
	require.NotNil(t, got.Usage.Rows[0].CommitState)
	require.False(t, got.Usage.Truncated)

	// 空窗口 ⇒ 三腿皆空（不是错误）。
	empty, err := repos.QueryAttemptChain(ctx, "chain-missing", from, to)
	require.NoError(t, err)
	require.Empty(t, empty.Attempt.Rows)
	require.Empty(t, empty.Err.Rows)
	require.Empty(t, empty.Usage.Rows)
}

// TestAttemptChainTruncatedIsExplicitPG 每源 LIMIT N+1 的判定：129 行 ⇒ 返回 128 行
// 且 truncated=true；128 行 ⇒ 原样返回、不置位。静默截断会让「重试了 129 次」
// 看起来像 128 次。
func TestAttemptChainTruncatedIsExplicitPG(t *testing.T) {
	ctx := context.Background()
	repos, _ := openAttemptChainDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	from, to := base, base.Add(time.Hour)
	// 行从 base 起写，跨 UTC 零点时 base 在昨天：历史行是夹具，分区自己建。
	ensureWritePartitions(t, ctx, repos, base)

	seed := func(reqID string, n int) {
		rows := make([]*domain.AttemptLog, 0, n)
		for i := 1; i <= n; i++ {
			rows = append(rows, attemptChainAttemptRow(reqID, int16(i), int64(i), "primary",
				base.Add(time.Duration(i)*time.Second)))
		}
		require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, rows))
	}
	seed("chain-full", attemptChainSourceLimit)
	seed("chain-over", attemptChainSourceLimit+1)

	full, err := repos.QueryAttemptChain(ctx, "chain-full", from, to)
	require.NoError(t, err)
	require.Len(t, full.Attempt.Rows, attemptChainSourceLimit)
	require.False(t, full.Attempt.Truncated, "恰好 N 行不截断")

	over, err := repos.QueryAttemptChain(ctx, "chain-over", from, to)
	require.NoError(t, err)
	require.Len(t, over.Attempt.Rows, attemptChainSourceLimit, "返回量封顶在 N")
	require.True(t, over.Attempt.Truncated, "第 N+1 行存在 ⇒ 必须显式标记（不静默丢行）")
}

// TestAttemptChainWindowIsHalfOpenPG 窗口是**半开** [from, to)：created_at == from
// 的行必须命中，created_at == to 的行必须排除。**三条腿各一组**——三腿的 SQL 常量
// 各自带一份时间谓词，任何一处把 `>=` 写成 `>` 或 `<` 写成 `<=`，这里就红
// （只钉 attempt 腿时 err/usage 腿的变异全绿——那是 momus 抓到的洞）。
func TestAttemptChainWindowIsHalfOpenPG(t *testing.T) {
	ctx := context.Background()
	repos, pool := openAttemptChainDB(t)
	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	to := from.Add(time.Hour)
	before := from.Add(-time.Second)
	// before 比 from 早 1s，跨 UTC 零点时落在昨天：历史行是夹具，分区自己建。
	ensureWritePartitions(t, ctx, repos, before)

	// attempt 腿。
	require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, []*domain.AttemptLog{
		attemptChainAttemptRow("chain-edge-attempt", 1, 11, "primary", from),   // 命中（下界含）
		attemptChainAttemptRow("chain-edge-attempt", 2, 12, "primary", to),     // 排除（上界不含）
		attemptChainAttemptRow("chain-edge-attempt", 3, 13, "primary", before), // 排除（早于下界）
	}))

	// err 腿（本地拒绝行：NULL ordinal；status_code 逐行不同，命中的哪一行一目了然）。
	for i, at := range []time.Time{from, to, before} {
		_, err := pool.Exec(ctx, `INSERT INTO err_logs
			(request_id, attempt_ordinal, format, status_code, error_type, error_message, latency_ms, created_at)
			VALUES ($1, NULL, 'openai-chat', $2, 'auth', 'detail', 5, $3)`,
			"chain-edge-err", 401+i, at)
		require.NoError(t, err)
	}

	// usage 腿（account_id 逐行不同，同上）。
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		attemptChainUsageRow("chain-edge-usage", 11, from),   // 命中（下界含）
		attemptChainUsageRow("chain-edge-usage", 12, to),     // 排除（上界不含）
		attemptChainUsageRow("chain-edge-usage", 13, before), // 排除（早于下界）
	}))

	attemptLeg, err := repos.QueryAttemptChain(ctx, "chain-edge-attempt", from, to)
	require.NoError(t, err)
	require.Len(t, attemptLeg.Attempt.Rows, 1, "attempt 腿：只有 created_at == from 的那一行命中")
	require.Equal(t, int16(1), *attemptLeg.Attempt.Rows[0].Ordinal)
	require.True(t, attemptLeg.Attempt.Rows[0].CreatedAt.Equal(from), "下界必须含（>=），且时间不被截断")

	errLeg, err := repos.QueryAttemptChain(ctx, "chain-edge-err", from, to)
	require.NoError(t, err)
	require.Len(t, errLeg.Err.Rows, 1, "err 腿：只有 created_at == from 的那一行命中（把 >= 改成 > 这里就红）")
	require.True(t, errLeg.Err.Rows[0].CreatedAt.Equal(from))
	require.Equal(t, 401, errLeg.Err.Rows[0].StatusCode, "命中的必须是下界那一行")

	usageLeg, err := repos.QueryAttemptChain(ctx, "chain-edge-usage", from, to)
	require.NoError(t, err)
	require.Len(t, usageLeg.Usage.Rows, 1, "usage 腿：只有 created_at == from 的那一行命中（把 < 改成 <= 这里就红）")
	require.True(t, usageLeg.Usage.Rows[0].CreatedAt.Equal(from))
	require.Equal(t, int64(11), usageLeg.Usage.Rows[0].AccountID, "命中的必须是下界那一行")
}

// TestAttemptChainTruncatedUsageLegPG usage 腿自己的 LIMIT N+1 判定。正常流量下
// 不可能（usage_logs 每请求至多一条），但读面契约对三腿同形：任一源超限都必须显式
// 置 truncated，绝不能因为「这条腿实际上不会超」就漏判。造 129 行靠 created_at
// 各不相同绕开唯一索引 (request_id, created_at)。
func TestAttemptChainTruncatedUsageLegPG(t *testing.T) {
	ctx := context.Background()
	repos, _ := openAttemptChainDB(t)
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	from, to := base, base.Add(2*time.Hour)
	// 129 行从 base 起写，跨 UTC 零点时 base 在昨天：历史行是夹具，分区自己建。
	ensureWritePartitions(t, ctx, repos, base)

	seed := func(reqID string, n int) {
		rows := make([]*domain.UsageLog, 0, n)
		for i := 0; i < n; i++ {
			rows = append(rows, attemptChainUsageRow(reqID, int64(i+1), base.Add(time.Duration(i)*time.Second)))
		}
		require.NoError(t, repos.Usages.InsertBatch(ctx, rows))
	}
	seed("chain-usage-full", attemptChainSourceLimit)
	seed("chain-usage-over", attemptChainSourceLimit+1)

	full, err := repos.QueryAttemptChain(ctx, "chain-usage-full", from, to)
	require.NoError(t, err)
	require.Len(t, full.Usage.Rows, attemptChainSourceLimit)
	require.False(t, full.Usage.Truncated, "恰好 N 行不截断")

	over, err := repos.QueryAttemptChain(ctx, "chain-usage-over", from, to)
	require.NoError(t, err)
	require.Len(t, over.Usage.Rows, attemptChainSourceLimit, "返回量封顶在 N")
	require.True(t, over.Usage.Truncated, "第 N+1 行存在 ⇒ usage 腿也必须显式标记（不静默丢行）")
}

// TestAttemptChainWideWindowCoversLongRequestPG 请求可以跨小时：attempt 行落在
// created_at−2h，created_at ±24h 的窗口必须仍把它查回来；而 ±1h 的老窗口会漏掉它
// （这正是把对话框窗口从 ±1h 放宽到 ±24h 的原因）。
func TestAttemptChainWideWindowCoversLongRequestPG(t *testing.T) {
	ctx := context.Background()
	repos, _ := openAttemptChainDB(t)
	created := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	earlier := created.Add(-2 * time.Hour)
	// earlier 是 now-5h，跨 UTC 零点时落在昨天：历史行是夹具，分区自己建。
	ensureWritePartitions(t, ctx, repos, earlier)

	require.NoError(t, repos.AttemptLogs.InsertBatch(ctx, []*domain.AttemptLog{{
		RequestID: "chain-long", Ordinal: 1, AccountID: 11, Lane: "primary",
		Format: "openai-chat", StatusCode: 502, Outcome: "5xx", ErrorType: "5xx",
		LatencyMS: 12, CreatedAt: earlier,
	}}))

	wide, err := repos.QueryAttemptChain(ctx, "chain-long", created.Add(-24*time.Hour), created.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, wide.Attempt.Rows, 1, "±24h 窗口必须查到 2 小时前的那一跳")
	require.True(t, wide.Attempt.Rows[0].CreatedAt.Equal(earlier))

	narrow, err := repos.QueryAttemptChain(ctx, "chain-long", created.Add(-time.Hour), created.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, narrow.Attempt.Rows, "±1h 窗口确实漏掉它——放宽不是审美问题")
}

// explainPlanNode 是 EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) 的计划节点（只取本
// 用例要断言的字段）。
type explainPlanNode struct {
	NodeType            string             `json:"Node Type"`
	RelationName        string             `json:"Relation Name"`
	ActualRows          float64            `json:"Actual Rows"`
	ActualTotalTimeMS   float64            `json:"Actual Total Time"`
	RowsRemovedByFilter float64            `json:"Rows Removed by Filter"`
	SharedHitBlocks     float64            `json:"Shared Hit Blocks"`
	SharedReadBlocks    float64            `json:"Shared Read Blocks"`
	Plans               []*explainPlanNode `json:"Plans"`
}

func walkExplainPlan(n *explainPlanNode, visit func(*explainPlanNode)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.Plans {
		walkExplainPlan(c, visit)
	}
}

// TestAttemptChainErrLegScanCostPG err_logs 腿的扫描代价（spec §4bis 的「必须在
// 真实 PG 上量出来」）。
//
// 数据规模：当日 50_000 行 + 前 6 日各 8_000 行（共 98_000；单请求目标行 3 条，
// 与噪声同分布）。三个窗口各跑一次 EXPLAIN (ANALYZE, BUFFERS)：
//   - 默认窗口 now ∓ 24h（端点缺省值）；
//   - 日对齐窗口 [今日 00:00, 明日 00:00)（只看一个分区）；
//   - 7 天窗口 [今日−6d, 明日 00:00)（保留期上限）。
//
// 断言（可判定的部分）：窗口外的分区不参与扫描、目标行仍被取到、计划真的执行了
// 扫描；**耗时只记录不断言**（ns 级门禁在负载下必飘，见 test-discipline）。
func TestAttemptChainErrLegScanCostPG(t *testing.T) {
	ctx := context.Background()
	repos, pool := openAttemptChainDB(t)
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	// 前 6 日的分区（跑完迁移只有当日/明日）：让 7 天窗口真的有 7 个分区可裁剪。
	require.NoError(t, repos.Partitions.EnsureTablePartitions(ctx, "err_logs", today.AddDate(0, 0, -6), today.AddDate(0, 0, 1)))

	const (
		todayRows = 50_000
		dayRows   = 8_000
	)
	target := "chain-target"
	// 目标行：今日 01:00 一条。三个测量窗口都完整覆盖 [今日 00:00, 明日 00:00)
	// ⇒ 每次 EXPLAIN 恰好取回 1 行（Actual Rows = 1 才是「查到且只查到它」的证据）。
	_, err := pool.Exec(ctx, `INSERT INTO err_logs
		(request_id, format, status_code, error_type, error_message, latency_ms, created_at)
		VALUES ($1, 'openai-chat', 502, '5xx', 'target detail', 7, $2)`,
		target, today.Add(time.Hour))
	require.NoError(t, err)
	// 当日噪声 50_000 行（均匀铺在 00:00..13:53 之间）。
	_, err = pool.Exec(ctx, `INSERT INTO err_logs
		(request_id, format, status_code, error_type, error_message, latency_ms, created_at)
		SELECT 'noise-' || g, 'openai-chat', 500, '5xx', 'boom', 9, $1::timestamptz + (g * interval '1 second')
		FROM generate_series(1, $2::int) AS g`, today, todayRows)
	require.NoError(t, err)
	// 前 6 日噪声：每日 8_000 行。
	for d := 1; d <= 6; d++ {
		day := today.AddDate(0, 0, -d)
		_, err := pool.Exec(ctx, `INSERT INTO err_logs
			(request_id, format, status_code, error_type, error_message, latency_ms, created_at)
			SELECT 'noise-d' || $1::int::text || '-' || g, 'openai-chat', 500, '5xx', 'boom', 9, $2::timestamptz + (g * interval '1 second')
			FROM generate_series(1, $3::int) AS g`, d, day, dayRows)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `ANALYZE err_logs`)
	require.NoError(t, err)

	var total int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM err_logs`).Scan(&total))
	require.EqualValues(t, 1+todayRows+6*dayRows, total, "夹具必须真有行（空表下计划断言无鉴别力）")

	type measurement struct {
		name     string
		allowed  []time.Time // 允许出现在计划里的分区日期
		from, to time.Time
	}
	retainedDays := make([]time.Time, 0, 7)
	for d := 0; d <= 6; d++ {
		retainedDays = append(retainedDays, today.AddDate(0, 0, -d))
	}
	measurements := []measurement{
		// now ∓ 24h 会跨过今日两侧的分区边界（昨天末尾 + 明天开头），两者都允许
		// 出现在计划里——裁剪是按区间的，不是按行的。
		{name: "default_pm_1d", from: now.Add(-24 * time.Hour), to: now.Add(24 * time.Hour),
			allowed: []time.Time{today.AddDate(0, 0, -1), today, today.AddDate(0, 0, 1)}},
		{name: "day_aligned", from: today, to: today.AddDate(0, 0, 1), allowed: []time.Time{today}},
		{name: "retention_7d", from: today.AddDate(0, 0, -6), to: today.AddDate(0, 0, 1),
			allowed: retainedDays},
	}
	allowedSet := func(days []time.Time) map[string]struct{} {
		out := make(map[string]struct{}, len(days))
		for _, d := range days {
			out[d.Format("20060102")] = struct{}{}
		}
		return out
	}

	for _, m := range measurements {
		var planJSON string
		started := time.Now()
		require.NoError(t, pool.QueryRow(ctx,
			`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+attemptChainErrSQL,
			target, m.from, m.to, attemptChainSourceLimit+1).Scan(&planJSON))
		wall := time.Since(started)
		var plans []struct {
			Plan          *explainPlanNode `json:"Plan"`
			ExecutionTime float64          `json:"Execution Time"`
		}
		require.NoError(t, json.Unmarshal([]byte(planJSON), &plans))
		require.Len(t, plans, 1)

		allowed := allowedSet(m.allowed)
		var (
			scanned    []string
			removed    float64
			sharedHit  float64
			sharedRead float64
		)
		walkExplainPlan(plans[0].Plan, func(n *explainPlanNode) {
			if n.RelationName == "" || n.NodeType == "Append" {
				return
			}
			scanned = append(scanned, fmt.Sprintf("%s(%s,out=%.0f,removed=%.0f)",
				n.RelationName, n.NodeType, n.ActualRows, n.RowsRemovedByFilter))
			removed += n.RowsRemovedByFilter
			sharedHit += n.SharedHitBlocks
			sharedRead += n.SharedReadBlocks
			if day := errLogPartitionDay(n.RelationName); day != "" {
				_, ok := allowed[day]
				require.True(t, ok, "%s: scanned partition %s is outside window [%s, %s)",
					m.name, n.RelationName, m.from.Format(time.RFC3339), m.to.Format(time.RFC3339))
			}
		})
		require.NotEmpty(t, scanned, "%s: 计划里必须有真实的扫描节点（空计划 = 夹具没被查到）", m.name)
		require.EqualValues(t, 1, plans[0].Plan.ActualRows, "%s: 目标 request_id 必须恰好取到窗口内的行", m.name)
		t.Logf("err_logs leg %s: window=[%s, %s) execution=%s wall=%s removed_by_filter=%.0f buffers(hit=%.0f read=%.0f) scanned=%v",
			m.name, m.from.Format(time.RFC3339), m.to.Format(time.RFC3339),
			time.Duration(plans[0].ExecutionTime*float64(time.Millisecond)).Round(time.Microsecond),
			wall.Round(time.Microsecond), removed, sharedHit, sharedRead, scanned)

		if m.name == "day_aligned" {
			// 扫描量与**日分区行数**成正比、与匹配数（1）无关：这就是「没有
			// request_id 索引 ⇒ 只能按窗口扫 + 过滤」的代价本体。
			require.EqualValues(t, todayRows, removed,
				"day_aligned: 当日分区 50001 行里 50000 行被 request_id 过滤掉（扫描量 = 分区行数）")
		}
	}
}

// errLogPartitionDay 从 err_logs_YYYYMMDD 里取日期后缀（非分区名 ⇒ 空串）。
func errLogPartitionDay(relation string) string {
	const prefix = "err_logs_"
	if len(relation) != len(prefix)+8 || relation[:len(prefix)] != prefix {
		return ""
	}
	return relation[len(prefix):]
}
