// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/redisx"
)

// newRecoverRegressionScheduler 装配一个「真实健康面 + 真 latch + 真规则引擎」的
// 调度器，**不**启动 RuntimeHealth.Start（即不注入 probeFn、不跑 probeLoop）。
// 这正是回归场景的关键前提：探针环是可选的，恢复的生效不得依赖它。
func newRecoverRegressionScheduler(t *testing.T) (*Scheduler, *RuntimeHealth, *latch.LatchStore, *memLoader) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })

	h := NewRuntimeHealth(c, "self-a", nil, nil) // 未 Start ⇒ probeFn == nil
	ls := latch.NewLatchStore()
	hub := latch.NewHub()
	_ = NewLatchSink(h, ls, hub, nil)
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, h, nil, ls, hub)
	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()
	return s, h, ls, m
}

// TestRecoverLeavesAccountSelectable_NoProbeLoop 是「recover 后账号还是不可选」的
// 端到端回归。
//
// 缺陷成因（修复前的结构）：恢复链路在清掉健康记录之后**又写了一条通配 PROBING**。
// 健康状态里 PROBING 是唯一必须由探针推进才能离开的状态，而选号门要求
// EffectiveState == StateReady。于是「恢复是否生效」被绑死在一个**可选组件**
// （探针环）上：探针没跑 / 没选到本实例 / permit 饥饿 / 返回 stale，账号就永久
// 不可选，且没有任何告警。
//
// 本测试不启动探针环，因此修复前的实现必然卡在 PROBING —— 会在
// 「恢复后必须能被选中」这一条上变红。
func TestRecoverLeavesAccountSelectable_NoProbeLoop(t *testing.T) {
	s, h, ls, m := newRecoverRegressionScheduler(t)
	ctx := context.Background()

	src := m.byGroup[10][0]
	fp, err := candidateFingerprint(src)
	require.NoError(t, err)
	K := src.IdentityRevision

	// 前提 1：账号处于运行时失效（failed_at 置位 ⇒ 快照 disabled ⇒ 不可选）。
	failedAt := time.Now()
	src.FailedAt = &failedAt
	require.NoError(t, s.reload(ctx))
	s.compileOnce()
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.Error(t, err, "前提：失效账号不得被选中")

	// 前提 2：同时存在一条真实冷却判决（OPEN），让恢复有实际东西要清。
	_, err = h.Throttle(ctx, HealthKey{AccountID: 1, Quality: "*", Identity: fp, IdentityRevision: K}, StateOPEN, 10*time.Minute)
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "m", fp, K), "前提：冷却已就位")

	// —— 执行恢复的健康面动作序（与 service.RecoverAccount 的落地步骤同构）——
	// 清 DB 侧失效事实（等价 RecoverAccountCAS 清 failed_at）
	src.FailedAt = nil
	require.NoError(t, s.reload(ctx))
	s.compileOnce()
	ls.Clear(1)
	_, err = h.ClearAccount(ctx, 1)
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))

	// 断言 1：恢复后健康面**无任何记录** ⇒ EffectiveState 判 READY。
	require.Equal(t, StateReady, h.EffectiveState(1, "m", fp, K),
		"恢复只清除、不写新记录：无记录即 READY")
	_, ok := h.AccountHealth(1)
	require.False(t, ok, "恢复后不得残留任何健康记录（含 PROBING）")

	// 断言 2（核心）：探针环完全没跑，账号依然可选。
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err,
		"恢复后账号必须立即可选：探针环是可选的，不得成为恢复生效的必要条件")
	require.Equal(t, int64(1), sel.AccountID)
	sel.Handle.Release()
}

// TestRecoverDoesNotCreateProbingRecord 单独钉住「恢复不得创建 PROBING」这条
// 结构约束：PROBING 只能来自 Sync 的保留转换（OPEN/RETRY_AFTER 自然到期）。
//
// 为什么值得单独一条：只要恢复链路能造出 PROBING，选号门就会长时间拒绝这个账号
// （PROBING 的离开条件完全在探针手里）。这条测试不依赖探针、不依赖选号，直接看
// 健康视图的键空间——是「生成机制是否被移除」的最直接证据。
func TestRecoverDoesNotCreateProbingRecord(t *testing.T) {
	_, h, _, _ := newRecoverRegressionScheduler(t)
	ctx := context.Background()

	const K = int64(1)
	// 先造一条真实冷却，模拟「冷却中的账号」。
	k := HealthKey{AccountID: 1, Quality: "m", Identity: testIdentity, IdentityRevision: K}
	_, err := h.Throttle(ctx, k, StateRetryAfter, time.Minute)
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))
	require.NotEmpty(t, h.View(), "前提：存在冷却记录")

	// 恢复的健康面动作：清空。
	_, err = h.ClearAccount(ctx, 1)
	require.NoError(t, err)
	require.NoError(t, h.Sync(ctx))

	require.Empty(t, h.View(), "恢复后健康视图必须为空（无记录 = READY）")

	// 再等两个同步周期，确认没有任何东西被「还原」成 PROBING。
	for range 2 {
		require.NoError(t, h.Sync(ctx))
		require.Empty(t, h.View(), "恢复后的同步周期不得重新造出健康记录")
	}
}
