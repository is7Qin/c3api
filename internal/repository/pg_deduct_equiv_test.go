// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 事务载体等价性测试（spec-f2-ledger-cursor）：DeductOnlyAndMark 双载体——
// pool → pgx 直连事务 vs nil pool → ent txDriver 回落——同输入终态逐字段一致
// （FEFO/透支/幽灵用户三场景矩阵），防两载体行为漂移。usage flusher InsertBatch
// 是 usage_logs 唯一写者。

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/ent/tempbalance"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// deductState 单次 DeductOnlyAndMark 后的可观测终态（双载体对比面）。
type deductState struct {
	balance     int64
	od          bool
	quarantined bool
	// temp 该用户剩余临时额度金额（升序；FEFO 扣减顺序对比）。
	temp []int64
	// unbilled 该用户剩余未标记行数（0 = 扣减与标记同事务原子完成）。
	unbilled int64
}

// TestPGDeductOnlyCarrierEquivalent 双载体等价：3 场景矩阵（FEFO 条件扣成功 /
// 透支 / 用户缺失隔离）逐场景对比终态。
func TestPGDeductOnlyCarrierEquivalent(t *testing.T) {
	reposCopy := newPGRepos(t)      // pool → pgx 直连事务载体
	reposEnt := newPGReposNoPool(t) // nil pool → ent txDriver 载体（同 schema）
	ctx := context.Background()

	scenarios := []struct {
		name    string
		cost    int64
		ghost   bool
		balance int64   // 预置余额（ghost 忽略）
		temps   []int64 // 预置永久临时额度
	}{
		{
			name:    "conditional success with FEFO temp balances",
			cost:    400_000,
			balance: 1_000_000,
			temps:   []int64{150_000, 300_000},
		},
		{
			name:    "overdraft",
			cost:    400_000,
			balance: 10_000,
		},
		{
			name:  "user missing quarantined",
			cost:  400_000,
			ghost: true,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// 两载体各自独立用户/行（email/id 带 tag 防撞），无需清行——状态
			// 采集全部按 user 过滤。
			stCopy := runDeductOnlyScenario(t, sc.name, "copy", reposCopy, ctx, sc.cost, sc.ghost, sc.balance, sc.temps)
			stEnt := runDeductOnlyScenario(t, sc.name, "ent", reposEnt, ctx, sc.cost, sc.ghost, sc.balance, sc.temps)
			require.Equal(t, stEnt, stCopy, "ent 载体与 pgx 载体终态必须逐字段一致")
		})
	}
}

// runDeductOnlyScenario 在指定载体上执行一个场景并采集终态（ghost = 不建用户，
// 固定不存在 uid；email 带载体 tag 防撞 users_email_key）。
func runDeductOnlyScenario(t *testing.T, name, tag string, repos *repository.Repository, ctx context.Context,
	cost int64, ghost bool, balance int64, temps []int64) deductState {
	t.Helper()
	uid := int64(900000 + len(tag))
	if !ghost {
		u := seedPGUser(t, repos, fmt.Sprintf("equivonly-%s-%s@example.com", name, tag))
		uid = u.ID
		require.NoError(t, repos.UpdateUserBalance(ctx, uid, balance))
		for _, amt := range temps {
			seedTempBalance(t, repos, uid, amt, nil)
		}
	}
	l := fullLogFor(uid, "eqo-"+tag)
	l.Cost = cost
	seedUnbilled(t, repos, l)
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)

	bal, od, q, err := repos.DeductOnlyAndMark(ctx, uid, cost, ledgerRowIDs(rows))
	require.NoError(t, err)

	st := deductState{balance: bal, od: od, quarantined: q}
	trows, err := repos.Client.TempBalance.Query().
		Where(tempbalance.UserIDEQ(uid)).All(ctx)
	require.NoError(t, err)
	for _, r := range trows {
		st.temp = append(st.temp, r.Amount)
	}
	sort.Slice(st.temp, func(i, j int) bool { return st.temp[i] < st.temp[j] })
	n, err := repos.Client.UsageLog.Query().
		Where(usagelog.UserIDEQ(uid), usagelog.BilledEQ(false)).Count(ctx)
	require.NoError(t, err)
	st.unbilled = int64(n)
	return st
}
