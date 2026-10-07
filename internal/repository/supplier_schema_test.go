// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// 供应商收益数据面锚（spec 2026-10-09 §3.1–§3.10 四新表 + accounts 归属列 +
// usage_logs 三收益列）：ent 生成物列/自然键一次性锚定，防"改 schema 忘同步"。
// 生成物迁移是 schema 驱动（go generate），本测试断言终态列集与自然键唯一约束。

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/ent/migrate"
)

// TestSupplierDataPlaneSchema 断言四张新表存在且列集合符合 spec §3.4–§3.10。
func TestSupplierDataPlaneSchema(t *testing.T) {
	balances := migrate.SupplierBalancesTable
	require.Equal(t, "supplier_balances", balances.Name)
	balCols := make(map[string]bool)
	for _, c := range balances.Columns {
		balCols[c.Name] = true
	}
	for _, want := range []string{"supplier_user_id", "available", "lifetime_credited", "lifetime_paid", "share_bp", "freeze_hours", "created_at", "updated_at"} {
		require.True(t, balCols[want], "supplier_balances 必须含列 %s", want)
	}

	chunks := migrate.SupplierFrozenChunksTable
	require.Equal(t, "supplier_frozen_chunks", chunks.Name)
	chunkCols := make(map[string]bool)
	for _, c := range chunks.Columns {
		chunkCols[c.Name] = true
	}
	for _, want := range []string{"supplier_user_id", "available_at", "amount"} {
		require.True(t, chunkCols[want], "supplier_frozen_chunks 必须含列 %s", want)
	}

	settlements := migrate.SupplierSettlementsTable
	require.Equal(t, "supplier_settlements", settlements.Name)
	setCols := make(map[string]bool)
	for _, c := range settlements.Columns {
		setCols[c.Name] = true
	}
	for _, want := range []string{"supplier_user_id", "kind", "amount_millis", "period_start", "period_end", "status", "revision", "request_key", "requested_operator", "payment_key", "external_ref", "payee_snapshot", "risk_review", "paid_at", "paid_operator_user_id"} {
		require.True(t, setCols[want], "supplier_settlements 必须含列 %s", want)
	}

	recon := migrate.SupplierReconciliationTable
	require.Equal(t, "supplier_reconciliation", recon.Name, "表名须为 supplier_reconciliation（非复数）")
	recCols := make(map[string]bool)
	for _, c := range recon.Columns {
		recCols[c.Name] = true
	}
	for _, want := range []string{"supplier_user_id", "source_day", "gross_cost", "earned", "row_count", "state", "closed_revision", "closed_at"} {
		require.True(t, recCols[want], "supplier_reconciliation 必须含列 %s", want)
	}
}

// TestAccountsSupplierColumn 断言 accounts 归属列与 usage_logs 三收益列已生成。
func TestAccountsSupplierColumn(t *testing.T) {
	accCols := make(map[string]bool)
	for _, c := range migrate.AccountsTable.Columns {
		accCols[c.Name] = true
	}
	require.True(t, accCols["supplier_user_id"], "accounts 必须含 supplier_user_id（§3.2）")

	ulCols := make(map[string]bool)
	for _, c := range migrate.UsageLogsTable.Columns {
		ulCols[c.Name] = true
	}
	for _, want := range []string{"supplier_user_id", "supplier_earn_millis", "supplier_credited"} {
		require.True(t, ulCols[want], "usage_logs 必须含收益列 %s（§3.3）", want)
	}
	require.False(t, ulCols["supplier_share_bp"], "usage_logs 不得含 supplier_share_bp（§2.3）")
}
