// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestPGScanUsageAgg 批量账号 usage_logs 区间聚合 roundtrip（/admin/accounts/
// usage 数据面 spec 2026-08-18）：多账号多行 SUM/COUNT 正确、raw 与 cost 分离
// （乘倍率前 vs 计费成本）、无记录账号无键（补零由 service 层按 ids 组装）、
// 时间过滤边界（created_at == from 含、== to 不含——半开区间）。
func TestPGScanUsageAgg(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)
	a1 := seedPGAccount(t, repos, tpl.ID, "agg-a1")
	a2 := seedPGAccount(t, repos, tpl.ID, "agg-a2")
	a3 := seedPGAccount(t, repos, tpl.ID, "agg-a3") // 无记录账号

	from := time.Now().UTC().Truncate(time.Second)
	mid := from.Add(5 * time.Minute)
	to := from.Add(10 * time.Minute)
	logs := []*domain.UsageLog{
		{RequestID: "agg-r1", AccountID: a1.ID, Model: "m", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 100, Cost: 1000, RawCost: 2000, CreatedAt: from}, // == from 含
		{RequestID: "agg-r2", AccountID: a1.ID, Model: "m", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 50, Cost: 500, RawCost: 700, CreatedAt: mid},
		{RequestID: "agg-r3", AccountID: a2.ID, Model: "m", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 100, RawCost: 300, CreatedAt: mid},
		{RequestID: "agg-r4", AccountID: a2.ID, Model: "m", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 100, RawCost: 300, CreatedAt: to}, // == to 不含
		{RequestID: "agg-r5", AccountID: a1.ID, Model: "m", Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 1, Cost: 1, RawCost: 2, CreatedAt: to.Add(-time.Second)},
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, logs))

	aggs, err := repos.ScanUsageAgg(ctx, []int64{a1.ID, a2.ID, a3.ID}, from, to)
	require.NoError(t, err)
	require.Len(t, aggs, 2, "无记录账号无键（补零归 service 层）")

	g1 := aggs[a1.ID]
	require.NotNil(t, g1)
	require.Equal(t, a1.ID, g1.AccountID)
	require.Equal(t, int64(3), g1.Requests, "a1 COUNT=3（r1==from 含 + r2 + r5）")
	require.Equal(t, int64(1501), g1.Cost, "a1 SUM(cost)")
	require.Equal(t, int64(2702), g1.RawCost, "a1 SUM(raw_cost)——raw 与 cost 分离（乘倍率前）")
	require.Equal(t, int64(151), g1.TotalTokens, "a1 SUM(total_tokens)")

	g2 := aggs[a2.ID]
	require.NotNil(t, g2)
	require.Equal(t, int64(1), g2.Requests, "a2 COUNT=1（r3 含；r4==to 不含）")
	require.Equal(t, int64(100), g2.Cost)
	require.Equal(t, int64(300), g2.RawCost)
	require.Equal(t, int64(10), g2.TotalTokens)

	// 账号缺省（无 any 命中）→ 空 map 不报错
	empty, err := repos.ScanUsageAgg(ctx, []int64{999999}, from, to)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestPGScanUsageAggPartitionPruning 分区剪枝命中（验收）：created_at 半开区间
// 谓词在计划期裁剪到当日分区——EXPLAIN 命中关系仅当日分区（无其他分区无父表
// 全扫；时间窗完全落在当日分区内 → 单分区）。种子 5000 行 + ANALYZE 防计划
// 形态 flake（pg_cursor 先例同款纪律）。
func TestPGScanUsageAggPartitionPruning(t *testing.T) {
	_ = newPGRepos(t) // 建 schema + 分区 bootstrap（种子经 pool 直插）
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	// 500ms 间隔 × 5000 = 41.7min，全部落在当日分区（pg_cursor 先例同款——
	// 超出已建分区 → 23514）。
	pgExec(t, pool, `INSERT INTO usage_logs (request_id, format, created_at, account_id)
		SELECT 'aggex-' || g, 'openai-chat', $1::timestamptz - g * interval '500 milliseconds', 1
		FROM generate_series(1, 5000) g`, today)
	pgExec(t, pool, `ANALYZE usage_logs`)

	// 时间窗完全落在当日分区 [00:00, 24:00) 内 → 剪枝到单分区。时间戳内联
	//（参数化 EXPLAIN 用 generic plan 不剪枝——pg_cursor 先例同款）。
	from := today.Add(-time.Hour).Format(time.RFC3339)
	to := today.Add(2 * time.Hour).Format(time.RFC3339)
	var planJSON string
	err := pool.QueryRow(ctx, fmt.Sprintf(`EXPLAIN (FORMAT JSON) SELECT account_id,
		count(*), sum(cost), sum(raw_cost), sum(total_tokens)
		FROM usage_logs WHERE account_id = ANY($1) AND created_at >= '%s' AND created_at < '%s'
		GROUP BY account_id`, from, to), []int64{1, 2}).Scan(&planJSON)
	require.NoError(t, err)

	var plan []struct {
		Plan *planNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(planJSON), &plan))
	require.Len(t, plan, 1)
	relations := map[string]bool{}
	walkPlan(plan[0].Plan, func(n *planNode) {
		if n.RelationName != "" {
			relations[n.RelationName] = true
		}
	})
	require.Len(t, relations, 1, "created_at 谓词剪枝到单分区: %v", relations)
	require.True(t, relations["usage_logs_"+today.Format("20060102")],
		"命中分区 = 当日分区 %s（实际 %v）", "usage_logs_"+today.Format("20060102"), relations)

	// pool 未注入（New 构造）→ 显式错误不静默降级（与 StatRepo 同纪律）。
	reposNoPool := newPGReposNoPool(t)
	_, err = reposNoPool.ScanUsageAgg(ctx, []int64{1}, today, today.Add(time.Hour))
	require.Error(t, err, "未注入 pgx 池（New）→ 显式错误")
}
