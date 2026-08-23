// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// 真实 PostgreSQL 测试基座（评审 B1：本任务 repository 新增测试一律真实 PG，
// 既有 pgxmock 测试保留不动）。
//
// 启动方式：deploy/test-compose.yml 起 postgres:18，然后
//   TEST_DATABASE_URL=postgres://postgres:c3api@localhost:15432/c3api_test go test ./internal/repository/ -run PG -v
//
// 未设置 TEST_DATABASE_URL → t.Skip（不炸本地/CI 无库环境）。
// ---------------------------------------------------------------------------

func newPGRepos(tb testing.TB) *repository.Repository {
	tb.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		tb.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(tb, err)
	tb.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	tb.Cleanup(func() { _ = db.Close() })
	// 每测试重建 schema（AutoMigrate 幂等；DROP 保证表间无残留）
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`)
	require.NoError(tb, err)
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool) // pool 注入 Stats（Upsert COPY 两阶段真实路径）
	require.NoError(tb, err)
	// T4.5 + 分表设计 + 用户裁决 2026-08-11：四张分区表（usage_logs/err_logs/
	// usage_stats/usage_entity_stats）已从 ent migrate 列表排除
	// （migrateHookExcludesPartitioned），分区表由 bootstrap 独占建表——所有 PG
	// 测试共用同一分区表基座（含 usage_stats 分区上的 Upsert 真实路径）。
	require.NoError(tb, repos.EnsureUsageLogPartitioned(ctx, time.Now()))
	require.NoError(tb, repos.EnsureErrLogPartitioned(ctx, time.Now()))
	require.NoError(tb, repos.EnsureUsageStatsPartitioned(ctx, time.Now()))
	require.NoError(tb, repos.EnsureUsageEntityStatsPartitioned(ctx, time.Now()))
	require.NoError(tb, repos.EnsurePriceVariantsEffectCheck(ctx))
	return repos
}

// newPGReposNoPool 同一 schema 上的无池仓库（热点修复 A 扩：DeductAndLog 双
// 路径 A/B 与等价性测试用）——pool == nil → ent CreateBulk 回落路径；与
// newPGRepos（pool → pgx COPY 路径）共享同一测试 schema（必须先于本函数调用
// newPGRepos 完成建表）。
func newPGReposNoPool(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	db := pgTestDB(t) // 独立池 → *sql.DB（ent driver 用）
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)
	return repos
}

// seedPGTemplate 建模板（accounts.template_id 有外键，必先建）。
func seedPGTemplate(t *testing.T, repos *repository.Repository) *domain.Template {
	t.Helper()
	tpl, err := repos.Templates.CreateTemplate(context.Background(), &domain.Template{
		Name: "t", BaseURL: "https://u/v1",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	return tpl
}

func seedPGGroup(t *testing.T, repos *repository.Repository, name string) *domain.Group {
	t.Helper()
	g, err := repos.Groups.CreateGroup(context.Background(), &domain.Group{Name: name, Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	return g
}

func seedPGAccount(t *testing.T, repos *repository.Repository, tplID int64, name string) *domain.Account {
	t.Helper()
	a, err := repos.Accounts.CreateAccount(context.Background(), &domain.Account{
		Name: name, TemplateID: tplID, UpstreamKey: "sk-" + name, Weight: 1, MaxConcurrency: 8,
	})
	require.NoError(t, err)
	return a
}

// TestAccountGroupsPG 账号侧分组的真实 PG 语义（替换/清空/不变/缺失 404/
// 批量；读取经 GetAccountGroups 与 LoadGroupAccounts 双向核对）。
func TestAccountGroupsPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	g1 := seedPGGroup(t, repos, "g1")
	g2 := seedPGGroup(t, repos, "g2")

	t.Run("set replace clear", func(t *testing.T) {
		acc := seedPGAccount(t, repos, tpl.ID, "a1")
		// 设置两个分组
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g1.ID, g2.ID}))
		got, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{g1.ID, g2.ID}, got)
		// 组侧读取一致（LoadGroupAccounts 是调度器数据源）
		members, err := repos.Groups.LoadGroupAccounts(ctx, g1.ID)
		require.NoError(t, err)
		require.Len(t, members, 1)
		require.Equal(t, acc.ID, members[0].ID)
		// 替换：只剩 g2
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g2.ID}))
		got, err = repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{g2.ID}, got)
		members, err = repos.Groups.LoadGroupAccounts(ctx, g1.ID)
		require.NoError(t, err)
		require.Empty(t, members, "替换后 g1 不再含该账号")
		// 清空：空数组
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{}))
		got, err = repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("missing group 404", func(t *testing.T) {
		acc := seedPGAccount(t, repos, tpl.ID, "a2")
		err := repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{999})
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.Contains(t, err.Error(), "999")
		got, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Empty(t, got, "404 后绑定不变")
	})

	t.Run("batch replace unchanged clear", func(t *testing.T) {
		a1 := seedPGAccount(t, repos, tpl.ID, "b1")
		a2 := seedPGAccount(t, repos, tpl.ID, "b2")
		// 预置绑定：a1→g1, a2→g2
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, a1.ID, []int64{g1.ID}))
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, a2.ID, []int64{g2.ID}))
		// 批量替换为同一组
		require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID},
			repository.AccountPatch{GroupIDs: &[]int64{g1.ID}}))
		for _, id := range []int64{a1.ID, a2.ID} {
			got, err := repos.Accounts.GetAccountGroups(ctx, id)
			require.NoError(t, err)
			require.Equal(t, []int64{g1.ID}, got, "批量替换后全部只属 g1")
		}
		// 不变（GroupIDs nil）：仅改 name，绑定不动
		name := "renamed"
		require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID},
			repository.AccountPatch{Name: &name}))
		got, err := repos.Accounts.GetAccountGroups(ctx, a1.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{g1.ID}, got, "nil = 不变")
		// 批量清空（[]）
		require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID},
			repository.AccountPatch{GroupIDs: &[]int64{}}))
		for _, id := range []int64{a1.ID, a2.ID} {
			got, err := repos.Accounts.GetAccountGroups(ctx, id)
			require.NoError(t, err)
			require.Empty(t, got, "批量清空后无分组")
		}
	})

	t.Run("batch missing group rolls back", func(t *testing.T) {
		a1 := seedPGAccount(t, repos, tpl.ID, "c1")
		a2 := seedPGAccount(t, repos, tpl.ID, "c2")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, a1.ID, []int64{g1.ID}))
		err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID},
			repository.AccountPatch{GroupIDs: &[]int64{g1.ID, 999}})
		require.ErrorIs(t, err, repository.ErrNotFound)
		require.Contains(t, err.Error(), "999")
		got, err := repos.Accounts.GetAccountGroups(ctx, a1.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{g1.ID}, got, "事务回滚：a1 绑定不变")
	})

	t.Run("read path not eager-loaded", func(t *testing.T) {
		acc := seedPGAccount(t, repos, tpl.ID, "d1")
		require.NoError(t, repos.Accounts.SetAccountGroups(ctx, acc.ID, []int64{g1.ID, g2.ID}))
		// GetAccount/ListAccounts 不 eager-load groups：domain.GroupIDs 保持 nil
		got, err := repos.Accounts.GetAccount(ctx, acc.ID)
		require.NoError(t, err)
		require.Nil(t, got.GroupIDs, "读路径不填充 GroupIDs（回显走 GetAccountGroups）")
		rows, _, err := repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
		require.NoError(t, err)
		for _, row := range rows {
			require.Nil(t, row.GroupIDs)
		}
	})

	t.Run("GetAccountGroups round-trip", func(t *testing.T) {
		acc := seedPGAccount(t, repos, tpl.ID, "e1")
		got, err := repos.Accounts.GetAccountGroups(ctx, acc.ID)
		require.NoError(t, err)
		require.Empty(t, got, "新账号无分组")
	})
}
