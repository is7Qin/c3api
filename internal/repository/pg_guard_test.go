// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

// TestGuardLegacyTransmitPG R-2 启动哨兵真实 PG 测试（串行、无 t.Parallel）：
//   (a) rules 表无 transmit 列 → guard 通过
//   (b) ALTER TABLE 追加 dummy transmit 列 → guard 失败且文案含 fresh setup/no migration
// 未设 TEST_DATABASE_URL → t.Skip（与仓内其余 pg_*_test.go 同纪律）。
func TestGuardLegacyTransmitPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	// (a) 新库：ent 已移除 transmit 列，哨兵应通过
	require.NoError(t, repository.EnsureNoLegacyTransmitColumn(ctx, pool), "fresh DB without transmit column must pass")

	// also exercise via repos path: table exists without column → pass
	// (guard is pool-based; pool shares same DB as repos)
	_ = repos

	// (b) 模拟旧库残留：追加 dummy 列 → 哨兵必须 fail-fast 且文案显式
	_, err := pool.Exec(ctx, `ALTER TABLE rules ADD COLUMN transmit TEXT`)
	require.NoError(t, err, "add dummy transmit column")

	err = repository.EnsureNoLegacyTransmitColumn(ctx, pool)
	require.Error(t, err, "legacy transmit column present must fail")
	require.Contains(t, err.Error(), "transmit")
	require.Contains(t, err.Error(), "fresh setup")
	require.Contains(t, err.Error(), "no migration")

	// cleanup: remove dummy so后续同进程其他测试不受影响（DROP SCHEMA per-test 已隔离，
	// 但同 test 内额外断言移除后恢复通过）
	_, err = pool.Exec(ctx, `ALTER TABLE rules DROP COLUMN transmit`)
	require.NoError(t, err)
	require.NoError(t, repository.EnsureNoLegacyTransmitColumn(ctx, pool), "after dropping transmit column guard must pass again")
}

// TestGuardLegacyTransmitNoTablePG 表尚不存在（全新库 migrate 前）→ guard 通过
// （information_schema 零行，不因缺表报错）。
func TestGuardLegacyTransmitNoTablePG(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	// 新建独立 schema 模拟表不存在场景：DROP 后仅查 information_schema 应 0 行
	_, err := pool.Exec(ctx, `DROP TABLE IF EXISTS rules CASCADE`)
	require.NoError(t, err)
	// rules 已删，但 guard 查的是 information_schema.columns → 应 pass（0 行）
	require.NoError(t, repository.EnsureNoLegacyTransmitColumn(ctx, pool))
}
