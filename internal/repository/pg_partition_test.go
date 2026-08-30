// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// usagelog 按日分区（Phase 5 T4.5，用户决策 2026-08-09）真实 PG 测试：
// bootstrap 幂等 / 普通表升级重建（该删删）/ 跨日边界插入路由 / ent 查询跨
// 分区 / DROP 保留边界 / ent migrate 二次启动兼容（主键 diff 实测结论）。
//
// 基座约定：newPGRepos 每测试 DROP SCHEMA 重建 + migrate（钩子跳过 usagelog）
// + 分区 bootstrap——本包 PG 测试串行（无 t.Parallel），无表级冲突。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/repository"
)

// pgTestPool 额外开一个直连池（分区级断言 SQL 用；newPGRepos 的池随测试
// 关闭，且不暴露 *sql.DB）。
func pgTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	pool, err := repository.OpenPG(context.Background(), dsn, 2)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// pgTestDB 直连 *sql.DB（ent 无钩子 migrate / 二次启动模拟用）。
func pgTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := stdlib.OpenDBFromPool(pgTestPool(t))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func pgExec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), query, args...)
	require.NoError(t, err)
}

func pgCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(context.Background(), query, args...).Scan(&n)
	require.NoError(t, err)
	return n
}

// pgPartitionNames 当前 usagelog 分区名列表。
func pgPartitionNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
		 JOIN pg_class p ON p.oid = i.inhparent JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE p.relname = 'usage_logs' AND n.nspname = current_schema()`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

func usageLogFor(reqID string, at time.Time) *domain.UsageLog {
	return &domain.UsageLog{
		RequestID: reqID, Model: "m", Format: domain.FormatOpenAIChat,
		StatusCode: 200, ErrorType: domain.ErrNone, TotalTokens: 10, CreatedAt: at,
	}
}

// TestUsageLogPartitionBootstrapPG bootstrap 幂等 + 分区表结构：
// newPGRepos 已建分区表（migrate 钩子 + bootstrap），二次 bootstrap 不重建
// （既有数据保留），预建当日/明日分区 + 索引齐全。
func TestUsageLogPartitionBootstrapPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	parted, err := repos.Partitions.IsUsageLogPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted, "migrate 后 bootstrap 必须建分区表")

	// 数据保留验证幂等：插入一行 → 二次 bootstrap → 行仍在、仍分区
	now := time.Now().UTC()
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor("idem", now)}))
	require.NoError(t, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	parted, err = repos.Partitions.IsUsageLogPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted)
	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1, "二次 bootstrap 不重建（数据保留）")
	require.Equal(t, "idem", rows[0].RequestID)

	// 预建分区：当日 + 明日；索引与 ent schema 同名同列（5 个非唯一 + 1 个
	// 唯一幂等键 usagelog_request_id_created_at + 主键）
	names := pgPartitionNames(t, pool)
	today := now.Truncate(24 * time.Hour)
	require.Contains(t, names, "usage_logs_"+today.Format("20060102"))
	require.Contains(t, names, "usage_logs_"+today.AddDate(0, 0, 1).Format("20060102"))
	var n int64
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'usage_logs' AND indexname IN ('usagelog_created_at','usagelog_group_id_created_at','usagelog_account_id_created_at','usagelog_user_id_created_at','usagelog_key_id_created_at','usagelog_request_id_created_at')`).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, int64(6), n, "bootstrap 建齐 5 个查询索引 + 1 个幂等键唯一索引")

	// 幂等键唯一索引行为锚定（方向 A 批次 1a）：同 (request_id, created_at)
	// 重复插入 → DO NOTHING 幂等成功（P1：InsertBatch 固定冲突目标幂等）
	dupAt := now.Add(time.Second)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor("dup-key", dupAt)}))
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor("dup-key", dupAt)}))
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs WHERE request_id = 'dup-key'`))

	// start 边界由传入 now 推导（评审 I-2）：now=+3 天 → 预建 +3/+4 天分区，
	// 而非仅当日/明日（内部 time.Now() 语义下该调用不可能建出未来分区）
	injected := time.Now().UTC().AddDate(0, 0, 3)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, injected, injected.AddDate(0, 0, 1)))
	names = pgPartitionNames(t, pool)
	require.Contains(t, names, "usage_logs_"+injected.Format("20060102"))
	require.Contains(t, names, "usage_logs_"+injected.AddDate(0, 0, 1).Format("20060102"))
}

// TestUsageLogPartitionRoutingPG 跨日边界插入路由：InsertBatch（buildUsageLogCreate
// 路径）不指定 id → 序列生成 + 按 created_at 路由到正确分区；ent QueryLogs
// 跨分区正常返回。
func TestUsageLogPartitionRoutingPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		usageLogFor("today-1", today),
		usageLogFor("tomorrow-1", tomorrow),
		usageLogFor("today-2", today.Add(time.Hour)),
	}))

	for _, tc := range []struct {
		part, reqID string
		want        int64
	}{
		{"usage_logs_" + today.Format("20060102"), "today-", 2},
		{"usage_logs_" + tomorrow.Format("20060102"), "tomorrow-", 1},
	} {
		got := pgCount(t, pool, `SELECT COUNT(*) FROM `+tc.part)
		require.Equal(t, tc.want, got, "分区 %s 落库行数", tc.part)
	}

	// ent 查询跨分区（无 created_at 过滤扫全部分区；带范围过滤命中 1~2 分区）
	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	from := today.Add(-time.Hour)
	to := tomorrow.Add(24 * time.Hour)
	rows, err = repos.QueryUsages(ctx, repository.UsageQuery{From: &from, To: &to, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 3, "时间范围过滤跨分区查询")
	got := map[string]bool{}
	for _, r := range rows {
		got[r.RequestID] = true
	}
	require.True(t, got["today-1"] && got["today-2"] && got["tomorrow-1"])

	// 精确日界路由（评审 I-4）：PG RANGE 分区下界含（INCLUSIVE）上界不含
	// （EXCLUSIVE）——
	//   today 00:00:00.000000（= 分区 FROM）      → 今日分区
	//   today 23:59:59.999999（= 明日 FROM 前 1µs）→ 今日分区
	//   tomorrow 00:00:00.000000（= 今日 TO）     → 明日分区
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lastMicro := midnight.Add(24*time.Hour - time.Microsecond)
	tomorrowMidnight := midnight.Add(24 * time.Hour)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		usageLogFor("bound-low-incl", midnight),
		usageLogFor("bound-up-excl", lastMicro),
		usageLogFor("bound-next-day", tomorrowMidnight),
	}))
	for _, tc := range []struct {
		part string
		want int64
	}{
		{"usage_logs_" + midnight.Format("20060102"), 4},         // today-1/2 + low-incl + up-excl
		{"usage_logs_" + tomorrowMidnight.Format("20060102"), 2}, // tomorrow-1 + next-day
	} {
		require.Equal(t, tc.want, pgCount(t, pool, `SELECT COUNT(*) FROM `+tc.part),
			"日界路由：分区 %s 落库行数（下界含/上界不含）", tc.part)
	}
}

// TestUsageLogPartitionRetentionPG DROP 保留边界：删除分区下界 < cutoff，
// 保留 >= cutoff（分区名日期判定，O(1)）；被删分区的数据从父表消失。
func TestUsageLogPartitionRetentionPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	// 补建两个历史分区（模拟运行多日后的存量分区；分区名 = 日期 YYYYMMDD）
	for _, d := range []string{"20260728", "20260729"} {
		pgExec(t, pool, fmt.Sprintf(
			`CREATE TABLE usage_logs_%s PARTITION OF usage_logs FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')`,
			d, mustISODate(d), mustNextISODate(d)))
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		usageLogFor("old-28", time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)),
		usageLogFor("old-29", time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)),
		usageLogFor("today", time.Now().UTC()),
	}))

	// cutoff 在 29 日 12:00 → 分区 28 日 < trunc(cutoff) 删除，29 日保留
	n, err := repos.DropUsageLogPartitionsBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, 1, n, "仅 28 日分区过期")
	names := pgPartitionNames(t, pool)
	require.NotContains(t, names, "usage_logs_20260728")
	require.Contains(t, names, "usage_logs_20260729")

	from := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	to := time.Now().UTC().Add(24 * time.Hour)
	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{From: &from, To: &to, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2, "28 日数据随分区 DROP 消失（29 + 当日保留）")

	// DROP 幂等：再跑一次 0 个可删
	n, err = repos.DropUsageLogPartitionsBefore(ctx, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestEntMigrateSecondRunPG ent migrate × 分区表兼容性（本任务首要验证项，
// 真实 PG 实测结论 2026-08-09）：
//   - 带钩子路径（生产启动顺序）：分区表已存在时二次 ent migrate 必须成功
//     （钩子把 usagelog 从迁移列表过滤，ent 永不 diff 该表）；
//   - 无钩子对照路径：atlas v0.36.2 能识别分区表（分区键属性），与 ent
//     schema 的普通表定义 diff 时在规划期直接报错
//     "sql/schema: partition key cannot be dropped from \"usage_logs\""——
//     migrate 必失败，没有任何 migrate 选项可容忍（ent 无禁用主键/分区键
//     diff 的选项）。断言该失败以锁定兼容性依据。
func TestEntMigrateSecondRunPG(t *testing.T) {
	repos := newPGRepos(t) // 第一次启动：migrate(钩子) + bootstrap → 分区表
	_ = repos
	ctx := context.Background()

	db := pgTestDB(t)
	// 二次启动：同一 schema 上再跑 migrate（钩子）+ bootstrap 幂等
	repos2, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err, "二次启动 ent migrate 必须容忍分区表（钩子过滤 usagelog）")
	require.NoError(t, repos2.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(t, repos2.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor("second-boot", time.Now().UTC())}))
	rows, err := repos2.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "second-boot", rows[0].RequestID)

	// 对照：无钩子 migrate 对分区表必然失败（实测：atlas 规划期拒绝分区键 diff）
	naive := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	err = naive.Schema.Create(ctx)
	require.Error(t, err, "无钩子 migrate 对分区表必须失败")
	require.Contains(t, err.Error(), "partition key cannot be dropped",
		"失败模式 = atlas 规划期拒绝分区键 diff（实测文案，稳定断言）")
}

// TestUsageLogPartitionConcurrentBootstrapPG 多实例并发 bootstrap（评审 I-1）：
// 两实例同时启动（barrier 对齐，双方都通过 is-partitioned=false 判定）→
// CREATE TABLE/索引/日分区撞名 42P07 → 容忍后幂等收敛，双方都不 fatal；收敛
// 后插入路由正常。每轮重建 schema 保证双方从同一初始状态出发（3 轮跑
// 叠加重压撞名路径）。
func TestUsageLogPartitionConcurrentBootstrapPG(t *testing.T) {
	ctx := context.Background()
	db := pgTestDB(t)
	_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
	require.NoError(t, err)
	// 与生产启动顺序一致：migrate（钩子）建其余表，bootstrap 由并发调用承担
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		if i > 0 { // 首轮 schema 已清空；后续轮次重建
			_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
			require.NoError(t, err)
			repos, err = repository.New(entsql.OpenDB(dialect.Postgres, db), true)
			require.NoError(t, err)
		}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				errs[g] = repos.EnsureUsageLogPartitioned(ctx, time.Now())
			}(g)
		}
		close(start)
		wg.Wait()
		require.NoError(t, errs[0], "实例 A bootstrap 不得 fatal")
		require.NoError(t, errs[1], "实例 B 撞 42P07 必须容忍后成功")

		parted, err := repos.Partitions.IsUsageLogPartitioned(ctx)
		require.NoError(t, err)
		require.True(t, parted, "并发 bootstrap 收敛为分区表")
		require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor("concurrent", time.Now().UTC())}))
		rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
		require.NoError(t, err)
		require.Len(t, rows, 1, "收敛后插入路由正常")
		require.Equal(t, "concurrent", rows[0].RequestID)
	}
}

// mustISODate 把 YYYYMMDD 转 ISO 日期（分区边界构造用）。
func mustISODate(d string) string {
	t, err := time.Parse("20060102", d)
	if err != nil {
		panic(err)
	}
	return t.Format("2006-01-02")
}

// mustNextISODate 返回 YYYYMMDD 的后一天 ISO 日期。
func mustNextISODate(d string) string {
	t, err := time.Parse("20060102", d)
	if err != nil {
		panic(err)
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02")
}
