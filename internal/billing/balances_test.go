// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeBalLoader 余额 + 倍率快照测试 loader（failLoad 注入失败——fail-safe 断言）。
type fakeBalLoader struct {
	m      map[int64]int64       // 余额
	am     map[AssignmentKey]int // 用户-组专属倍率（仅已设置行）
	gm     map[int64]int         // 组倍率
	failAt int                   // 1 = LoadBalances 失败；2 = LoadGroupMultipliers 失败；3 = LoadAssignmentMultipliers 失败
}

func (f fakeBalLoader) LoadBalances(ctx context.Context) (map[int64]int64, error) {
	if f.failAt == 1 {
		return nil, errors.New("db down")
	}
	return f.m, nil
}

func (f fakeBalLoader) LoadGroupMultipliers(ctx context.Context) (map[int64]int, error) {
	if f.failAt == 2 {
		return nil, errors.New("db down")
	}
	return f.gm, nil
}

func (f fakeBalLoader) LoadAssignmentMultipliers(ctx context.Context) (map[AssignmentKey]int, error) {
	if f.failAt == 3 {
		return nil, errors.New("db down")
	}
	return f.am, nil
}

// TestBalancesReloadFailSafe Reload 失败 fail-safe：Warn（log 注入）+ 保留旧
// 快照不替换（预检继续用旧值，条件扣 DB 兜底）；空初始快照 → 全部缺失。
func TestBalancesReloadFailSafe(t *testing.T) {
	// 先成功加载 → 再失败 → 旧快照保留
	b := NewBalances(fakeBalLoader{m: map[int64]int64{3: 77}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	b.loader = fakeBalLoader{failAt: 1}
	require.Error(t, b.Reload(context.Background()))
	bal, ok := b.BalanceOf(3)
	require.True(t, ok)
	require.Equal(t, int64(77), bal, "Reload 失败保留旧快照")

	// 初始失败（空快照）→ 全部缺失（预检 402 拒绝，安全侧）
	b2 := NewBalances(fakeBalLoader{failAt: 1}, nil)
	require.Error(t, b2.Reload(context.Background()))
	_, ok = b2.BalanceOf(1)
	require.False(t, ok)
}

// TestBalancesReloadGroupFailSafe 组倍率加载失败 → 余额与倍率两路都保留旧值
// （快照内自洽：不出现新余额 + 旧倍率错配）。
func TestBalancesReloadGroupFailSafe(t *testing.T) {
	b := NewBalances(fakeBalLoader{
		m: map[int64]int64{1: 100}, am: map[AssignmentKey]int{{1, 1}: 20000}, gm: map[int64]int{1: 15000},
	}, nil)
	require.NoError(t, b.Reload(context.Background()))
	// 余额加载成功、组倍率失败 → 整体不替换
	b.loader = fakeBalLoader{m: map[int64]int64{1: 999}, failAt: 2}
	require.Error(t, b.Reload(context.Background()))
	bal, ok := b.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(100), bal, "组倍率失败 → 余额也保留旧值")
	require.Equal(t, 15000, b.EffectiveMultiplier(2, 1), "组倍率保留旧值")
}

// TestBalancesReloadAssignmentFailSafe assignment 倍率加载失败 → 三路都保留
// 旧值（快照内自洽）。
func TestBalancesReloadAssignmentFailSafe(t *testing.T) {
	b := NewBalances(fakeBalLoader{
		m: map[int64]int64{1: 100}, am: map[AssignmentKey]int{{1, 1}: 20000}, gm: map[int64]int{1: 15000},
	}, nil)
	require.NoError(t, b.Reload(context.Background()))
	b.loader = fakeBalLoader{m: map[int64]int64{1: 999}, failAt: 3}
	require.Error(t, b.Reload(context.Background()))
	require.Equal(t, 20000, b.EffectiveMultiplier(1, 1), "assignment 倍率失败 → 保留旧值")
	bal, ok := b.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(100), bal, "assignment 倍率失败 → 余额也保留旧值")
}

// TestBalancesSet O1 O(1) 语义：Set 命中已存在条目原地 Store（零拷贝）；目标
// 用户即时可见，其余用户不受影响。
func TestBalancesSet(t *testing.T) {
	b := NewBalances(fakeBalLoader{m: map[int64]int64{1: 100, 2: 200}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	b.Set(1, 40)
	bal, ok := b.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(40), bal, "Set 后命中新值")
	bal, ok = b.BalanceOf(2)
	require.True(t, ok)
	require.Equal(t, int64(200), bal, "其余用户不受影响")
}

// TestBalancesSetMissingIgnored O1 语义：Set 缺失条目忽略（仅限已存在用户的
// 余额变更——PUT/Redeem/flush 回写的用户预检时已在快照内恒命中）；新用户
// 经全量 Reload 进快照，不走 Set。
func TestBalancesSetMissingIgnored(t *testing.T) {
	b := NewBalances(fakeBalLoader{m: map[int64]int64{1: 100}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	b.Set(9, 1) // 快照缺失用户 → 忽略（此前补入行为取消）
	_, ok := b.BalanceOf(9)
	require.False(t, ok, "Set 缺失条目忽略（用户创建走 Reload）")
}

// TestBalancesSetAfterReload 用户创建路径（评审 M-2）：fake store 插入新用户
// → 全量 Reload → 新用户即刻可读（预检 402 窗口关闭）；Set 其扣费回写命中。
func TestBalancesSetAfterReload(t *testing.T) {
	b := NewBalances(fakeBalLoader{m: map[int64]int64{1: 100}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	_, ok := b.BalanceOf(9)
	require.False(t, ok, "创建前缺失 → 402 窗口（显式暴露，不用 sleep 掩盖）")

	b.loader = fakeBalLoader{m: map[int64]int64{1: 100, 9: 50000}} // 用户 9 创建入库
	require.NoError(t, b.Reload(context.Background()))
	bal, ok := b.BalanceOf(9)
	require.True(t, ok)
	require.Equal(t, int64(50000), bal, "Reload 后新用户即刻可读")

	b.Set(9, 49900) // 扣费回写命中（条目已存在）
	bal, ok = b.BalanceOf(9)
	require.True(t, ok)
	require.Equal(t, int64(49900), bal, "Reload 后 Set 定向刷新生效")
}

// TestReloadMultipliers O2 组 + assignment 倍率定向刷新：两路都换（小表单查，
// 非全量 Reload——assignment 倍率变更走此路，不依赖全量 Reload）；失败
// fail-safe 保留旧倍率快照。
func TestReloadMultipliers(t *testing.T) {
	b := NewBalances(fakeBalLoader{
		m: map[int64]int64{1: 100}, am: map[AssignmentKey]int{{1, 1}: 20000}, gm: map[int64]int{1: 15000},
	}, nil)
	require.NoError(t, b.Reload(context.Background()))

	// 组倍率变更（g2 从 10000 → 30000）+ assignment 倍率变更（(1,1) 20000 → 0）
	b.loader = fakeBalLoader{
		m:  map[int64]int64{1: 100},
		am: map[AssignmentKey]int{{1, 1}: 0, {9, 3}: 5000},
		gm: map[int64]int{1: 15000, 2: 30000},
	}
	require.NoError(t, b.ReloadMultipliers(context.Background()))
	require.Equal(t, 30000, b.EffectiveMultiplier(9, 2), "新组倍率即刻生效")
	require.Equal(t, 0, b.EffectiveMultiplier(1, 1), "assignment 倍率变更即刻生效（0 免费）")
	require.Equal(t, 5000, b.EffectiveMultiplier(9, 3), "新 assignment 倍率即刻生效")
	require.Equal(t, 15000, b.EffectiveMultiplier(9, 1), "既有组倍率不变")
	require.Equal(t, 10000, b.EffectiveMultiplier(8, 9), "无 assignment 无组 → ×1")

	// 失败 fail-safe：保留旧倍率快照
	b.loader = fakeBalLoader{failAt: 3}
	require.Error(t, b.ReloadMultipliers(context.Background()))
	require.Equal(t, 0, b.EffectiveMultiplier(1, 1), "assignment 失败保留旧倍率快照")
	require.Equal(t, 30000, b.EffectiveMultiplier(9, 2), "组倍率也保留旧值（两路原子换）")
}

// TestBalancesBalanceOfMissing 缺失 → (0, false)（预检 402 语义：无快照 =
// 拒绝，不按 0 放行）。
func TestBalancesBalanceOfMissing(t *testing.T) {
	b := NewBalances(fakeBalLoader{m: map[int64]int64{}}, nil)
	require.NoError(t, b.Reload(context.Background()))
	_, ok := b.BalanceOf(42)
	require.False(t, ok)
	bal, ok := b.BalanceOf(1)
	require.False(t, ok)
	require.Zero(t, bal)
}

// TestEffectiveMultiplier 有效倍率表驱动（T3.5 修正：按组查序 assignment 专属
// → 组倍率 → 10000）：assignment 覆盖组（含 0 免费/×10 上限）；仅组；均缺。
func TestEffectiveMultiplier(t *testing.T) {
	cases := []struct {
		name        string
		assignments map[AssignmentKey]int
		groups      map[int64]int
		userID, gid int64
		want        int
	}{
		{"assignment 覆盖组", map[AssignmentKey]int{{1, 1}: 20000}, map[int64]int{1: 15000}, 1, 1, 20000},
		{"assignment 免费覆盖组", map[AssignmentKey]int{{1, 1}: 0}, map[int64]int{1: 15000}, 1, 1, 0},
		{"assignment ×10 上限", map[AssignmentKey]int{{1, 1}: 100000}, nil, 1, 1, 100000},
		{"同用户不同组不同倍率", map[AssignmentKey]int{{1, 1}: 20000, {1, 2}: 5000}, map[int64]int{1: 15000}, 1, 2, 5000},
		{"仅组倍率", nil, map[int64]int{1: 15000}, 1, 1, 15000},
		{"组免费（用户未设置）", nil, map[int64]int{2: 0}, 1, 2, 0},
		{"均缺默认×1", nil, nil, 1, 1, 10000},
		{"无组无用户", nil, nil, 99, 0, 10000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := NewBalances(fakeBalLoader{m: map[int64]int64{}, am: c.assignments, gm: c.groups}, nil)
			require.NoError(t, b.Reload(context.Background()))
			require.Equal(t, c.want, b.EffectiveMultiplier(c.userID, c.gid))
		})
	}
	// 未 Reload（倍率快照空）→ 默认 ×1
	b := NewBalances(fakeBalLoader{m: map[int64]int64{}}, nil)
	require.Equal(t, 10000, b.EffectiveMultiplier(1, 1))
}
