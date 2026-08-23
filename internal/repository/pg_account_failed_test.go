// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// ---------------------------------------------------------------------------
// SDK 接入 T1：account 失效字段（failed_at——用户裁决 2026-08-13 仅此一列，
// 失效原因复用既有 last_error）真实 PG 测试：建列断言 + 幂等首写 + 与 status
// 语义分离。基座见 pg_account_groups_test.go 的 newPGRepos（DROP SCHEMA 重建）。
// ---------------------------------------------------------------------------

// TestAccountFailedColumnsPG ent schema 建列断言：accounts 表存在 failed_at
// （timestamptz NULL）一列；不新增 failed_reason（用户裁决——原因复用 last_error）。
func TestAccountFailedColumnsPG(t *testing.T) {
	repos := newPGRepos(t) // 迁移建表（ent AutoMigrate 按 migrate/schema.go 建列）
	ctx := context.Background()
	require.NotNil(t, repos)

	// 独立 pgx 连接直查 information_schema（ent 无客户端入口）。
	dsn := os.Getenv("TEST_DATABASE_URL")
	pool, err := repository.OpenPG(ctx, dsn, 1)
	require.NoError(t, err)
	defer pool.Close()

	rows, err := pool.Query(ctx, `SELECT column_name, is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'accounts' AND column_name = 'failed_at'`)
	require.NoError(t, err)
	defer rows.Close()
	type colRow struct{ Name, Nullable string }
	var cols []colRow
	for rows.Next() {
		var c colRow
		require.NoError(t, rows.Scan(&c.Name, &c.Nullable))
		cols = append(cols, c)
	}
	require.NoError(t, rows.Err())
	require.Len(t, cols, 1, "failed_at 恰一列（不新增 failed_reason）")
	require.Equal(t, "failed_at", cols[0].Name)
	require.Equal(t, "YES", cols[0].Nullable)
}

// TestSetAccountFailedPG 失效 roundtrip + 幂等（重复上报不重复写——首写生效
// 保持首次失效时刻与原因）+ 失效原因复用 last_error + 与 status.disabled 语义
// 分离（不触碰 status）。
func TestSetAccountFailedPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a := seedPGAccount(t, repos, tpl.ID, "f1")

	first := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Accounts.SetAccountFailed(ctx, a.ID, first, "auth permanently revoked"))

	got, err := repos.Accounts.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FailedAt)
	require.True(t, got.FailedAt.Equal(first), "failed_at 落库")
	require.NotNil(t, got.LastError)
	require.Equal(t, "auth permanently revoked", *got.LastError, "失效原因复用 last_error")
	require.Equal(t, domain.StatusActive, got.Status, "失效字段不触碰 status（与 disabled 语义分离）")

	// 幂等：再次上报（不同时刻/原因）→ 0 行不覆盖，首次值保持
	later := first.Add(1 * time.Hour)
	require.NoError(t, repos.Accounts.SetAccountFailed(ctx, a.ID, later, "different reason"))
	got2, err := repos.Accounts.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, got2.FailedAt.Equal(first), "重复上报不重复写——首次失效时刻保持")
	require.Equal(t, "auth permanently revoked", *got2.LastError, "首次失效原因保持")

	// 未知账号 → 0 行不报错（审计性写入，无对象可写）
	require.NoError(t, repos.Accounts.SetAccountFailed(ctx, 999999, later, "x"))
}

// TestSetAccountFailedEmptyReasonPG 空 reason 不清旧值（P3-2 评审）：空原因
// 上报不触碰既有 last_error（保持"最近错误"审计语义——调度回写携带的快照旧值
// 与 DB 一致，不互相覆盖）。
func TestSetAccountFailedEmptyReasonPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a := seedPGAccount(t, repos, tpl.ID, "f2")

	// 预置既有 last_error（最近错误审计）
	old := "previous upstream error"
	require.NoError(t, repos.Accounts.UpdateAccountStatus(ctx, a.ID, domain.StatusActive, nil, &old, nil))

	require.NoError(t, repos.Accounts.SetAccountFailed(ctx, a.ID, time.Now(), ""))
	got, err := repos.Accounts.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FailedAt, "failed_at 照常写入")
	require.NotNil(t, got.LastError)
	require.Equal(t, old, *got.LastError, "空 reason 不清旧值——既有 last_error 保持")
}
