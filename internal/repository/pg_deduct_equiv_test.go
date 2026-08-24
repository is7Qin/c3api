// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 两车道并集 vs 总账守恒 harness（spec-f2opt-settlement D8 改写，替代 legacy
// DeductOnlyAndMark 双载体等价族）：混合种群（temp-active FEFO 多行 + 余额-only
// 条件扣 + 透支补刀 + 幽灵用户 + 匿名行 + 零价吸收）经三车道排空后——
// Σdrawn(temp) + Σ|Δbalance| + Σ隔离行 cost == Σbilled cost 精确容差 0；每行恰
// 标记一次；od 列按用户整组对齐。双载体（pool → pgx 直连 / nil pool → ent
// txDriver）各跑一遍终态逐字段一致，防载体行为漂移。

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// conservationState 单次守恒场景的可观测终态（双载体对比面；balances 以角色名
// 为键——两载体各自建用户，真实 uid 序列不同，按 uid 键无法逐字段对比）。
type conservationState struct {
	balances      map[string]int64 // fefo/rich/poor 角色终态余额
	tempRemaining []int64          // fefo 用户剩余临时额度（升序）
	billedRows    int
	billedCost    int64
	overdraftTrue int // od=true 行数（poor 整组）
	unbilled      int
}

// TestPGSettleLanesConservation 两车道并集守恒 + 双载体等价。
func TestPGSettleLanesConservation(t *testing.T) {
	reposCopy := newPGRepos(t)      // pool → pgx 直连事务载体
	reposEnt := newPGReposNoPool(t) // nil pool → ent txDriver 载体（同 schema）

	stCopy := runConservationScenario(t, "copy", reposCopy)
	stEnt := runConservationScenario(t, "ent", reposEnt)
	require.Equal(t, stEnt, stCopy, "ent 载体与 pgx 载体终态必须逐字段一致")
}

// runConservationScenario 在指定载体上执行混合种群三车道排空并采集终态
// （email 带载体 tag 防撞 users_email_key；状态采集全部按本场景 id 过滤）。
func runConservationScenario(t *testing.T, tag string, repos *repository.Repository) conservationState {
	t.Helper()
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	fefo := seedPGUser(t, repos, fmt.Sprintf("cons-fefo-%s@example.com", tag))
	rich := seedPGUser(t, repos, fmt.Sprintf("cons-rich-%s@example.com", tag))
	poor := seedPGUser(t, repos, fmt.Sprintf("cons-poor-%s@example.com", tag))
	require.NoError(t, repos.UpdateUserBalance(ctx, fefo.ID, 100_000))
	require.NoError(t, repos.UpdateUserBalance(ctx, rich.ID, 1_000_000))
	require.NoError(t, repos.UpdateUserBalance(ctx, poor.ID, 50_000))

	// temp-active 种群：FEFO 车道独占（balance 车道谓词排除）
	e1 := seedTempBalance(t, repos, fefo.ID, 30_000, ptrTime(time.Now().Add(time.Hour)))
	e2 := seedTempBalance(t, repos, fefo.ID, 50_000, ptrTime(time.Now().Add(24*time.Hour)))
	ep := seedTempBalance(t, repos, fefo.ID, 70_000, nil)

	seedCursorRows(t, repos, fefo.ID, 3, 60_000) // delta 180000 → drawn 150000 + spill 30000
	z := cursorLog(fefo.ID, 0)                   // 零价行：sweep 车道吸收
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{z}))
	seedCursorRows(t, repos, rich.ID, 2, 130_000)          // 条件扣 260000
	seedCursorRows(t, repos, poor.ID, 2, 40_000)           // 透支 80000
	const ghostUID = int64(910000001)                      // 幽灵：从不建行
	seedCursorRows(t, repos, ghostUID, 2, 90_000)          // 隔离 180000
	seedCursorRows(t, repos, 0, 1, 70_000)                 // 匿名 NULL user_id：隔离 70000

	all := fetchAllUnbilled(t, repos)
	require.Len(t, all, 11)
	ids := ledgerIDsOf(all)

	drained, quarantined := drainBillingCursor(t, repos)
	require.Equal(t, int64(11), drained, "全部行退出游标")
	require.Equal(t, int64(3), quarantined, "ghost×2 + 匿名×1 隔离零扣费")

	st := conservationState{
		balances: map[string]int64{
			"fefo": cursorBalance(t, repos, fefo.ID),
			"rich": cursorBalance(t, repos, rich.ID),
			"poor": cursorBalance(t, repos, poor.ID),
		},
	}
	// 语义断言（双载体各自成立）：逐用户 Δ余额精确 + temp 全耗尽
	require.Equal(t, int64(70_000), st.balances["fefo"], "FEFO drawn 150000 + spill 30000 条件扣")
	require.Equal(t, int64(740_000), st.balances["rich"], "条件扣精确")
	require.Equal(t, int64(-30_000), st.balances["poor"], "透支补刀负余额精确")
	for _, tid := range []int64{e1, e2, ep} {
		st.tempRemaining = append(st.tempRemaining, tempBalanceAmount(t, repos, tid))
	}
	sort.Slice(st.tempRemaining, func(i, j int) bool { return st.tempRemaining[i] < st.tempRemaining[j] })
	require.Equal(t, []int64{0, 0, 0}, st.tempRemaining, "FEFO 全档消耗")

	// 每行恰标记一次（无复活无丢失）+ od 列按用户整组对齐
	rows, err := repos.Client.UsageLog.Query().Where(usagelog.IDIn(ids...)).All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 11)
	for _, r := range rows {
		require.True(t, r.Billed, "id=%d 恰标记一次", r.ID)
		st.billedCost += r.Cost
		if r.Overdraft {
			st.overdraftTrue++
		}
	}
	st.billedRows = len(rows)
	require.Equal(t, 2, st.overdraftTrue, "仅 poor 整组 od=true")
	n, err := repos.Client.UsageLog.Query().
		Where(usagelog.IDIn(ids...), usagelog.BilledEQ(false)).Count(ctx)
	require.NoError(t, err)
	st.unbilled = n
	require.Zero(t, st.unbilled)

	// 守恒恒等式（spec §四：精确容差 0）：
	// Σdrawn(temp) + Σ|Δbalance| + Σ隔离行 cost == Σbilled cost
	drawnTemp := int64(150_000)
	deltaBal := (100_000 - st.balances["fefo"]) +
		(1_000_000 - st.balances["rich"]) +
		(50_000 - st.balances["poor"])
	isolatedCost := int64(2*90_000 + 70_000)
	require.Equal(t, drawnTemp+deltaBal+isolatedCost, st.billedCost,
		"两车道并集 vs 总账守恒——精确容差 0")
	require.Equal(t, int64(770_000), st.billedCost)
	return st
}
