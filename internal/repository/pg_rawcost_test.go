// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// raw_cost 列（spec 2026-08-18：乘倍率前原始成本）真实 PG 测试：bootstrap 建表
// 含列（bigint NOT NULL DEFAULT 0）+ ent 建行路径（InsertBatch → buildUsageLog
// Create）四态落库 roundtrip（QueryUsages 读回）+ SQL 直查落库值 + DEFAULT 锚
// （SQL 直插缺省列 → 0——历史行语义）。
//
// 基座约定同 pg_partition_test.go：newPGRepos 每测试 DROP SCHEMA 重建 +
// migrate（钩子跳过 usagelog）+ 分区 bootstrap（DDL 含 raw_cost 列）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestUsageLogRawCostRoundtripPG raw_cost 四态 roundtrip（行为契约 spec
// 2026-08-18）：billed 双值（倍率 ≠×1——raw 原文、cost 乘后）/ 免费组（cost=0
// raw>0）/ 非 billed 行（UserID==0 但 bill 装配）raw 照填 / bill 未装配 raw=0。
// ent 建行路径（buildUsageLogCreate SetRawCost 恒落）经 InsertBatch 全量覆盖。
func TestUsageLogRawCostRoundtripPG(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	pool := pgTestPool(t)

	// 列存在性 + 类型 + NOT NULL + DEFAULT 0 锚（建表 DDL 事实源落库等价）
	var dataType, isNullable, def string
	err := pool.QueryRow(ctx, `
		SELECT data_type, is_nullable, column_default FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'usage_logs' AND column_name = 'raw_cost'`).
		Scan(&dataType, &isNullable, &def)
	require.NoError(t, err, "bootstrap 建表必须含 raw_cost 列")
	require.Equal(t, "bigint", dataType)
	require.Equal(t, "NO", isNullable)
	require.Equal(t, "0", def, "历史行/缺省 = 0（fresh setup 不迁移）")

	// 四态行（同一批 InsertBatch——ent 建行路径）
	now := time.Now().UTC()
	l1 := usageLogFor("raw-billed", now) // billed：cost 乘后 750、raw 原文 500（倍率 ×1.5）
	l1.Cost, l1.RawCost = 750, 500
	l2 := usageLogFor("raw-free", now) // 免费组（m=0）：cost=0、raw>0
	l2.RawCost = 500
	l3 := usageLogFor("raw-nonbilled", now) // 非 billed 行（UserID==0 但 bill 装配）：raw 照填
	l3.RawCost = 500
	l4 := usageLogFor("raw-unset", now) // bill 未装配 → raw 恒 0（域内缺省）
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l1, l2, l3, l4}))

	rows, err := repos.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 4)
	byReq := map[string]*domain.UsageLog{}
	for _, r := range rows {
		byReq[r.RequestID] = r
	}
	require.Equal(t, int64(750), byReq["raw-billed"].Cost)
	require.Equal(t, int64(500), byReq["raw-billed"].RawCost, "billed 行 raw=倍率前原文")
	require.Equal(t, int64(0), byReq["raw-free"].Cost)
	require.Equal(t, int64(500), byReq["raw-free"].RawCost, "免费组 cost=0 但 raw 有值")
	require.Equal(t, int64(500), byReq["raw-nonbilled"].RawCost, "非 billed 行 raw 照填")
	require.Zero(t, byReq["raw-unset"].RawCost, "bill 未装配 raw 恒 0")

	// SQL 层直查确认落库值（不经 domain 映射）
	for _, c := range []struct {
		reqID             string
		wantCost, wantRaw int64
	}{
		{"raw-billed", 750, 500}, {"raw-free", 0, 500}, {"raw-nonbilled", 0, 500}, {"raw-unset", 0, 0},
	} {
		var cost, raw int64
		err = pool.QueryRow(ctx, `SELECT cost, raw_cost FROM usage_logs WHERE request_id = $1`, c.reqID).
			Scan(&cost, &raw)
		require.NoError(t, err)
		require.Equal(t, c.wantCost, cost, "%s cost 落库值", c.reqID)
		require.Equal(t, c.wantRaw, raw, "%s raw_cost 落库值", c.reqID)
	}

	// DEFAULT 锚：SQL 直插缺省列 → 0（历史行/未装配语义）
	pgExec(t, pool, `INSERT INTO usage_logs (request_id, model, format, error_type, created_at)
		VALUES ('raw-default', 'm', 'openai-chat', 'none', $1)`, now)
	require.Equal(t, int64(0), pgCount(t, pool,
		`SELECT raw_cost FROM usage_logs WHERE request_id = 'raw-default'`),
		"缺省 raw_cost = 0（DEFAULT 锚）")
}
