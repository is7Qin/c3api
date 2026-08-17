// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// client_ip（S-E 2026-08-17）真实 PG roundtrip：两表（usage_logs + err_logs）
// bootstrap 建表含 client_ip 列（text，紧随 request_id）；NULL + 有值两态
// 插/查（QueryUsages/QueryErrLogs 回填映射红绿——gate M3）；billing COPY
// 路径（DeductAndLog）落库行带 client_ip（全列对比见 TestPGDeductCopyPathEquivalent
// 的 fullLogFor.ClientIP，本文件 SQL 层直查断言）。
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试 DROP SCHEMA 重建 +
// migrate（钩子跳过分区表）+ 分区 bootstrap（两表 DDL 含新列）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestUsageLogClientIPRoundtripPG usage_logs client_ip 有值/NULL roundtrip
// （QueryUsages 回填）+ SQL 层直查确认 NULL 语义 + 分区表建表含新列。
func TestUsageLogClientIPRoundtripPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	var dataType, isNullable string
	err := pool.QueryRow(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = 'client_ip'`).
		Scan(&dataType, &isNullable)
	require.NoError(t, err, "bootstrap 建表必须含 client_ip 列")
	require.Equal(t, "text", dataType)
	require.Equal(t, "YES", isNullable)

	// 有值 + 未设置（NULL）两态
	l1 := usageLogFor("cip-u-1", time.Now().UTC())
	l1.ClientIP = "9.9.9.9"
	l2 := usageLogFor("cip-u-2", time.Now().UTC())
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l1, l2}))

	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	got := map[string]*domain.UsageLog{}
	for _, r := range rows {
		got[r.RequestID] = r
	}
	require.Equal(t, "9.9.9.9", got["cip-u-1"].ClientIP, "有值必须读回（QueryUsages 回填）")
	require.Empty(t, got["cip-u-2"].ClientIP, "未设置 → NULL → 回填空")

	var raw *string
	err = pool.QueryRow(ctx, `SELECT client_ip FROM usage_logs WHERE request_id = 'cip-u-2'`).Scan(&raw)
	require.NoError(t, err)
	require.Nil(t, raw, "DB 层 client_ip 为 NULL")
	var rawVal string
	err = pool.QueryRow(ctx, `SELECT client_ip FROM usage_logs WHERE request_id = 'cip-u-1'`).Scan(&rawVal)
	require.NoError(t, err)
	require.Equal(t, "9.9.9.9", rawVal)
}

// TestErrLogClientIPRoundtripPG err_logs client_ip 有值/NULL roundtrip
// （QueryErrLogs 回填）+ 分区表建表含新列。
func TestErrLogClientIPRoundtripPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	var dataType, isNullable string
	err := pool.QueryRow(ctx, `
		SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'err_logs' AND column_name = 'client_ip'`).
		Scan(&dataType, &isNullable)
	require.NoError(t, err, "bootstrap 建表必须含 client_ip 列")
	require.Equal(t, "text", dataType)
	require.Equal(t, "YES", isNullable)

	// 有值（拒绝行恒带）+ 未设置（NULL）两态
	l1 := errLogFor("cip-e-1", time.Now().UTC())
	l1.ClientIP = "9.9.9.9"
	l2 := errLogFor("cip-e-2", time.Now().UTC())
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{l1, l2}))

	rows, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	got := map[string]*domain.UsageLog{}
	for _, r := range rows {
		got[r.RequestID] = r
	}
	require.Equal(t, "9.9.9.9", got["cip-e-1"].ClientIP, "有值必须读回（QueryErrLogs 回填）")
	require.Empty(t, got["cip-e-2"].ClientIP, "未设置 → NULL → 回填空")
}

// TestBillingDeductAndLogCarriesClientIPPG billing DeductAndLog（COPY 路径——
// newPGRepos 带 pool）落库行带 client_ip（SQL 层直查；全列双路径等价对比见
// TestPGDeductCopyPathEquivalent）。
func TestBillingDeductAndLogCarriesClientIPPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	u := seedPGUser(t, repos, "cip-bill@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))
	l := fullLogFor(u.ID, "cip-bill-1") // fullLogFor 带 ClientIP = "9.9.9.9"
	od, bal, err := repos.DeductAndLog(ctx, u.ID, 100_000, []*domain.UsageLog{l})
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(900_000), bal)

	var raw string
	err = pool.QueryRow(ctx, `SELECT client_ip FROM usage_logs WHERE request_id = 'cip-bill-1'`).Scan(&raw)
	require.NoError(t, err)
	require.Equal(t, "9.9.9.9", raw, "DeductAndLog（COPY 路径）落库行必须带 client_ip")
}
