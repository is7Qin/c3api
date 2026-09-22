// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// err_logs 分区生命周期（用户裁决：与 usage_logs 同分区路线，独立保留期）真实
// PG 测试：bootstrap 幂等 / 跨日插入路由 / 独立保留期 DROP 边界（与 usage_logs
// 不同 cutoff）/ 并发 bootstrap 幂等。
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试 DROP SCHEMA 重建 + migrate
//（钩子跳过两表）+ 分区 bootstrap（EnsureUsageLogPartitioned +
// EnsureErrLogPartitioned）。本包 PG 测试串行（无 t.Parallel），无表级冲突。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// pgErrPartitionNames 当前 err_logs 分区名列表（pgPartitionNames 是 usage_logs
// 专用——同构 SQL 按表参数化）。
func pgErrPartitionNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT c.relname FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
		 JOIN pg_class p ON p.oid = i.inhparent JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE p.relname = 'err_logs' AND n.nspname = current_schema()`)
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

// TestErrLogPartitionBootstrapPG err_logs bootstrap 幂等 + 分区表结构：二次
// bootstrap 不重建（数据保留），预建当日/明日分区 + 3 个查询索引（
// created_at + (group_id, created_at) + (user_id, created_at)）。
func TestErrLogPartitionBootstrapPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	parted, err := repos.Partitions.IsErrLogPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted, "migrate 后 bootstrap 必须建 err_logs 分区表")

	// 数据保留验证幂等：插入一行 → 二次 bootstrap → 行仍在、仍分区
	now := time.Now().UTC()
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{errLogFor("err-idem", now)}))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))
	parted, err = repos.Partitions.IsErrLogPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted)
	rows, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1, "二次 bootstrap 不重建（数据保留）")
	require.Equal(t, "err-idem", rows[0].RequestID)

	// 预建分区：当日 + 明日；索引齐（3 个非唯一 + 主键）
	names := pgErrPartitionNames(t, pool)
	today := now.Truncate(24 * time.Hour)
	require.Contains(t, names, "err_logs_"+today.Format("20060102"))
	require.Contains(t, names, "err_logs_"+today.AddDate(0, 0, 1).Format("20060102"))
	var n int64
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'err_logs' AND indexname IN ('errlog_created_at','errlog_group_id_created_at','errlog_user_id_created_at')`).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, int64(3), n, "bootstrap 建齐 3 个查询索引")
}

// TestErrLogPartitionRoutingPG 跨日边界插入路由：InsertErrLogBatch 不指定 id →
// 序列生成 + 按 created_at 路由到正确分区；QueryErrLogs 跨分区返回。
func TestErrLogPartitionRoutingPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{
		errLogFor("err-today-1", today),
		errLogFor("err-tomorrow-1", tomorrow),
		errLogFor("err-today-2", today.Add(time.Hour)),
	}))

	for _, tc := range []struct {
		part, reqID string
		want        int64
	}{
		{"err_logs_" + today.Format("20060102"), "err-today-", 2},
		{"err_logs_" + tomorrow.Format("20060102"), "err-tomorrow-", 1},
	} {
		got := pgCount(t, pool, `SELECT COUNT(*) FROM `+tc.part)
		require.Equal(t, tc.want, got, "分区 %s 落库行数", tc.part)
	}

	rows, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	from := today.Add(-time.Hour)
	to := tomorrow.Add(24 * time.Hour)
	rows, err = repos.QueryErrLogs(ctx, repository.ErrLogQuery{From: &from, To: &to, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 3, "时间范围过滤跨分区查询")
}

// TestErrLogPartitionRetentionPG 独立保留期 DROP 边界：err_logs cutoff 与
// usage_logs 独立——同一日期分区，err_logs 早 7 天可删而 usage_logs 30 天保留
// 期未到（核心断言：两表各自 cutoff 不互相影响）。
func TestErrLogPartitionRetentionPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	// 构造 8 天前的分区数据（两表同日期——边界差异检验）
	old := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -8)
	require.NoError(t, repos.EnsureUsageLogPartitions(ctx, old, old)) // 预建历史分区（幂等）
	require.NoError(t, repos.EnsureErrLogPartitions(ctx, old, old))
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor("u-old", old)}))
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{errLogFor("e-old", old)}))

	// err_logs 保留 7 天：cutoff = now-7d → 8 天前分区该删
	n, err := repos.DropErrLogPartitionsBefore(ctx, now.AddDate(0, 0, -7))
	require.NoError(t, err)
	require.Equal(t, 1, n, "err_logs 8 天前分区该删（7 天保留期）")
	require.Zero(t, pgCount(t, pool, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace WHERE ns.nspname = current_schema() AND c.relname = 'err_logs_`+old.Format("20060102")+`'`),
		"err_logs 历史分区已 DROP（DROP 后直接查询分区表 → 42P01，按 pg_class 存在性断言）")

	// usage_logs 保留 30 天：cutoff = now-30d → 8 天前分区保留（同日期独立边界）
	n, err = repos.DropUsageLogPartitionsBefore(ctx, now.AddDate(0, 0, -30))
	require.NoError(t, err)
	require.Zero(t, n, "usage_logs 8 天前分区保留（30 天保留期）")
	require.Equal(t, int64(1), pgCount(t, pool, `SELECT COUNT(*) FROM usage_logs_`+old.Format("20060102")), "usage_logs 同日期数据未受影响")

	// 两表数据互不可见（分表）
	rows, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Limit: 100})
	require.NoError(t, err)
	require.Empty(t, rows, "err_logs 查询只见 err_logs 数据（e-old 已 DROP）")
	rows, err = repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "u-old", rows[0].RequestID)
}

// TestErrLogPartitionConcurrentBootstrapPG 并发 bootstrap 幂等（多实例
// 语义）：两实例同时 EnsureErrLogPartitioned——42P07/23505 容忍收敛，无错误。
func TestErrLogPartitionConcurrentBootstrapPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repos.EnsureErrLogPartitioned(ctx, time.Now())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "并发 bootstrap 第 %d 个必须成功", i)
	}
	parted, err := repos.Partitions.IsErrLogPartitioned(ctx)
	require.NoError(t, err)
	require.True(t, parted)
	// 二次（串行）幂等：数据保留
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{errLogFor("concurrent-ok", time.Now().UTC())}))
	require.NoError(t, repos.EnsureErrLogPartitioned(ctx, time.Now()))
	rows, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
