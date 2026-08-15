// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- concurrencyGate 单元测试（内存原子语义） ---

// 两级 acquire（user → key）与两步回滚（评审 I-3）：user 成功 key 失败 →
// user 计数复原，防泄漏。
func TestGateAcquireReleaseAndRollback(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 2, KeyMaxConc: 1}
	g.upsert(meta) // 种子：计数器存在后才参与门禁
	lvl1, ok := g.acquire(meta)
	require.True(t, ok)
	require.Equal(t, 3, lvl1, "user+key 两层全占")

	// 第二请求：user 层可占（2 内）但 key 层超限 → 整体失败 + user 回滚
	lvl2, ok := g.acquire(meta)
	require.False(t, ok)
	require.Zero(t, lvl2)
	snap := g.store.Load()
	require.Equal(t, int64(1), snap.users[1].Load(), "user 计数已回滚（1 非 2）")
	require.Equal(t, int64(1), snap.keys[1].Load())

	g.release(meta, lvl1)
	require.Equal(t, int64(0), snap.users[1].Load())
	require.Equal(t, int64(0), snap.keys[1].Load())

	lvl3, ok := g.acquire(meta)
	require.True(t, ok, "释放后可再占")
	g.release(meta, lvl3)
}

func TestGateUserLimit(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 1}
	g.upsert(meta)
	lvl, ok := g.acquire(meta)
	require.True(t, ok)
	require.Equal(t, 1, lvl, "仅 user 层")
	_, ok = g.acquire(meta)
	require.False(t, ok, "user 并发超限")
	g.release(meta, lvl)
	_, ok = g.acquire(meta)
	require.True(t, ok, "release 后可再占")
}

// 快照换入换出：在途并发/额度跨 reload 继承（复用 scheduler reload 继承教训；
// 评审提醒②：quota_used 内存值同走继承）。
func TestGateReloadInheritsInflight(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, UserMaxConc: 4, KeyMaxConc: 4, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"h": meta}) // 种子
	lvl, ok := g.acquire(meta)
	require.True(t, ok)
	g.deductQuota(1, 30)

	g.reload(map[string]domain.KeyMeta{"h": meta})
	snap := g.store.Load()
	require.Equal(t, int64(1), snap.users[1].Load(), "user 在途继承")
	require.Equal(t, int64(1), snap.keys[1].Load(), "key 在途继承")
	require.Equal(t, int64(30), snap.quotas[1].consumed.Load(), "额度在途继承")

	// 跨 reload 的 release/deduct 命中新快照的继承值
	g.release(meta, lvl)
	g.deductQuota(1, 10)
	require.Equal(t, int64(0), snap.users[1].Load())
	require.Equal(t, int64(40), snap.quotas[1].consumed.Load())
}

// 额度：检查无计数副作用、后扣、无额度 key 零成本短路（无复核能力 → 预算
// 耗尽即 429，与单实例现状语义一致）。
func TestGateQuotaCheckAndDeduct(t *testing.T) {
	g := newConcurrencyGate(nil)
	noQuota := domain.KeyMeta{KeyID: 2, HasQuota: false}
	require.False(t, g.quotaExhausted(noQuota), "无额度 key 短路")
	g.deductQuota(2, 50) // 无条目 → no-op（恒 0）

	quota := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"q": quota}) // 种子（额度条目存在）
	require.False(t, g.quotaExhausted(quota))
	g.deductQuota(1, 60)
	require.False(t, g.quotaExhausted(quota))
	g.deductQuota(1, 50)
	require.True(t, g.quotaExhausted(quota), "后扣超限拦下一个请求")

	// 检查无副作用：quotaExhausted 不改变 consumed 计数
	before := g.store.Load().quotas[1].consumed.Load()
	_ = g.quotaExhausted(quota)
	require.Equal(t, before, g.store.Load().quotas[1].consumed.Load(), "检查不改变消耗计数（评审提醒①）")
}

// reload 后无额度 key 不建 quota 条目；reload 新 key 从快照值起算。
func TestGateReloadQuotaBase(t *testing.T) {
	g := newConcurrencyGate(nil)
	noQuota := domain.KeyMeta{KeyID: 2, HasQuota: false}
	g.reload(map[string]domain.KeyMeta{"nq": noQuota})
	snap := g.store.Load()
	_, ok := snap.quotas[2]
	require.False(t, ok, "无额度 key 无条目")

	withQuota := domain.KeyMeta{KeyID: 3, HasQuota: true, Quota: 50, QuotaUsed: 7}
	g.reload(map[string]domain.KeyMeta{"q": withQuota})
	require.Equal(t, int64(7), g.store.Load().quotas[3].consumed.Load(), "新 key 基线 = DB 快照值")
}

// upsert/delete 增量：新 key 建条目、已存在不动在途值、删除清条目。
func TestGateUpsertDelete(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1, HasQuota: true, Quota: 10, QuotaUsed: 3}
	g.upsert(meta)
	require.Equal(t, int64(3), g.store.Load().quotas[1].consumed.Load(), "新 key 额度基线")
	g.deductQuota(1, 5)
	g.upsert(meta)
	require.Equal(t, int64(8), g.store.Load().quotas[1].consumed.Load(), "upsert 不动在途额度")

	g.delete(1)
	snap := g.store.Load()
	_, ok := snap.keys[1]
	require.False(t, ok, "key 计数移除")
	_, ok = snap.quotas[1]
	require.False(t, ok, "额度条目移除")
}

// UserMaxConc=0（不限并发）路径覆盖（spec 2026-08-15 A-2；修复前 0 路径无
// 测试）：计数与限流解耦——acquire 无条件计数 +1、level user 位恒置、release
// 归零、快照遍历（InFlightUsers 同型读法）能读到在途值。
func TestGateUnlimitedUserCounts(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 1, UserID: 1} // UserMaxConc=0：不限并发
	g.upsert(meta)                              // 种子：计数器存在后才参与门禁

	lvl, ok := g.acquire(meta)
	require.True(t, ok, "不限并发恒放行")
	require.Equal(t, 1, lvl, "user 计数位恒置（release 按位释放对称）")

	inflight := func() map[int64]int64 { // InFlightUsers 同型读法（遍历 + 原子读）
		snap := g.store.Load()
		out := make(map[int64]int64, len(snap.users))
		for uid, c := range snap.users {
			if c != nil {
				out[uid] = c.Load()
			}
		}
		return out
	}
	require.Equal(t, int64(1), inflight()[1], "acquire 无条件计数 +1（排行数据源）")

	lvl2, ok2 := g.acquire(meta) // 不限并发：多请求全部放行，计数累加
	require.True(t, ok2)
	require.Equal(t, int64(2), inflight()[1])

	g.release(meta, lvl)
	require.Equal(t, int64(1), inflight()[1], "release 按位减")
	g.release(meta, lvl2)
	require.Equal(t, int64(0), inflight()[1], "全部 release 归零")
}

// 混合并发（spec 2026-08-15 A-2）：不限用户（UserMaxConc=0）与限 8 用户真实
// goroutine 并发 acquire/release——两用户计数都在且恒非负；超限者 429 且回滚
// 后计数正确（回滚竞态闭合验证：占满后 N 并发全回滚 → 计数净 0，不误减持锁者）。
func TestGateMixedUnlimitedLimitedConcurrent(t *testing.T) {
	g := newConcurrencyGate(nil)
	unl := domain.KeyMeta{KeyID: 1, UserID: 1}                 // 不限并发
	lim := domain.KeyMeta{KeyID: 2, UserID: 2, UserMaxConc: 8} // 限 8
	g.reload(map[string]domain.KeyMeta{"u": unl, "l": lim})

	// 限 8 用户占满 8 槽（持锁不释放）
	held := make([]int, 0, 8)
	for i := 0; i < 8; i++ {
		lvl, ok := g.acquire(lim)
		require.True(t, ok)
		held = append(held, lvl)
	}

	const (
		nUnl  = 16 // 不限用户并发（全部放行）
		nOver = 32 // 超限用户并发（占满后全部 429 回滚）
	)

	// 并发期间计数恒非负监控（acquire/release 交错下任何时刻不为负）
	done := make(chan struct{})
	var neg atomic.Bool
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			snap := g.store.Load()
			for _, uid := range []int64{1, 2} {
				if c := snap.users[uid]; c != nil && c.Load() < 0 {
					neg.Store(true)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	var unlOK, overOK atomic.Int32
	for i := 0; i < nUnl; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lvl, ok := g.acquire(unl); ok {
				unlOK.Add(1)
				g.release(unl, lvl)
			}
		}()
	}
	for i := 0; i < nOver; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lvl, ok := g.acquire(lim); ok {
				overOK.Add(1)
				g.release(lim, lvl)
			}
		}()
	}
	wg.Wait()
	close(done)

	require.False(t, neg.Load(), "并发期间计数恒非负")
	require.Equal(t, int32(nUnl), unlOK.Load(), "不限用户全部放行")
	require.Zero(t, overOK.Load(), "占满后全部 429（超限不误放行）")
	require.Equal(t, int64(8), g.store.Load().users[2].Load(),
		"N 并发全回滚净 0：超限风暴后计数仍 = 8（不误减持锁者）")

	// 释放全部 → 归零
	for _, lvl := range held {
		g.release(lim, lvl)
	}
	require.Equal(t, int64(0), g.store.Load().users[2].Load())
	require.Equal(t, int64(0), g.store.Load().users[1].Load())
}

// 缺 key 的 key 层失败（reload 后无该 key 计数器 → 该层跳过，不误拒）。
func TestGateMissingCounterFailOpen(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 99, UserID: 99, UserMaxConc: 1, KeyMaxConc: 1}
	lvl, ok := g.acquire(meta)
	require.True(t, ok, "计数器缺失层跳过（fail-open）")
	g.release(meta, lvl)
}

// --- 多实例本地预算（#14 T3b §3.2） ---

// fakeInstances 固定 N 的 InstancesProvider 测试桩。
type fakeInstances int

func (n fakeInstances) ClusterInstances() int { return int(n) }

// fakeQuotaReader 可编程复核读：used 返回值/错误 + 调用计数 + 可选阻塞
//（单飞并发测试用）。线程安全。
type fakeQuotaReader struct {
	mu      sync.Mutex
	used    int64
	err     error
	calls   int
	blockCh chan struct{} // 非 nil → 每次复核先等放行（测并发单飞）
}

func (f *fakeQuotaReader) QuotaUsed(ctx context.Context, keyID int64) (int64, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.blockCh != nil {
		<-f.blockCh
	}
	f.mu.Lock()
	u, e := f.used, f.err
	f.mu.Unlock()
	return u, e
}

func (f *fakeQuotaReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeQuotaReader) set(used int64, err error) {
	f.mu.Lock()
	f.used, f.err = used, err
	f.mu.Unlock()
}

// 预算分摊分配/消耗：budget = consumed + ceil(remaining/N)；预算内放行，
// 耗尽触发复核（无复核能力 → 429，单实例语义）。
func TestGateBudgetSplitByN(t *testing.T) {
	g := newConcurrencyGate(nil)
	g.SetInstancesProvider(fakeInstances(3))
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100, QuotaUsed: 10}
	g.reload(map[string]domain.KeyMeta{"q": meta})
	q := g.store.Load().quotas[1]
	require.Equal(t, int64(10), q.consumed.Load())
	require.Equal(t, int64(10+ceilDiv(90, 3)), q.budget.Load(), "budget = consumed + ceil(remaining/N)")

	// 预算内消耗：放行
	for i := 0; i < int(ceilDiv(90, 3))-1; i++ {
		g.deductQuota(1, 1)
		require.False(t, g.quotaExhausted(meta))
	}
	// 预算耗尽：无复核能力 → 429
	g.deductQuota(1, 1)
	require.True(t, g.quotaExhausted(meta))
}

// N=1 单实例等价回归：无复核能力时 budget = 剩余额（精确），消耗到快照剩余即
// 429，与现状单实例语义同点拒绝。注意（评审 I-1）：此"同点"仅在无复核能力或
// 快照值恰好等于 DB 时成立；生产 N=1（真 reclaimer）见
// TestGateN1ReclaimerLagNoOverrun——429 点由 DB quota_used 决定。
func TestGateN1EquivalentToSingleInstance(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100, QuotaUsed: 10}
	g.reload(map[string]domain.KeyMeta{"q": meta})
	q := g.store.Load().quotas[1]
	require.Equal(t, int64(100), q.budget.Load(), "N=1：budget = 10 + ceil(90/1) = 100")
	for i := 0; i < 90; i++ {
		g.deductQuota(1, 1)
	}
	require.True(t, g.quotaExhausted(meta), "消耗 90 后与单实例同点 429（无复核能力）")
}

// #37 P1：N=1 + 真 reclaimer，DB quota_used 滞后于本地消耗（模拟 usage.Recorder
// flush 滞后）。复核认领扣除本地未反映消耗（unreported = consumed - 上次复核
// 基线）→ 本地消耗满 quota 即复核确认真尽 429——不再凭滞后 DB 值续额超跑
// （修复前：每次复核重新分配 full remaining → 滞后窗口超跑；压测实证复核循环
// 无限续额 14 倍）。收敛语义：本实例总放行 ≤ 初始份额 + 滞后差。
func TestGateN1ReclaimerLagNoOverrun(t *testing.T) {
	g := newConcurrencyGate(nil)
	reader := &fakeQuotaReader{used: 0} // DB 滞后：本地已扣尚未回写
	g.setReclaimer(reader)
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"q": meta})
	q := g.store.Load().quotas[1]
	require.Equal(t, int64(100), q.budget.Load(), "N=1 初始 budget = quota（快照相等）")

	for i := 0; i < 100; i++ {
		g.deductQuota(1, 1)
	}
	// 本地预算耗尽 → 复核：DB 仍显示 used=0（flush 滞后）→ unreported =
	// consumed(100) - 基线(0) = 100 扣尽剩余 → 确认真尽 429（无滞后超跑）
	require.True(t, g.quotaExhausted(meta), "复核扣除本地未反映消耗 → 真尽 429")
	require.Equal(t, 1, reader.callCount())
	require.True(t, q.exhausted.Load())
}

// 复核成功续额（#37 P1 收敛修正）：预算耗尽 → DB 复核（quota_used=30）→
// 认领扣除本地未反映消耗——首次复核 unreported = consumed(34) - 基线(0) = 34
// → remaining_eff = 100-30-34 = 36 → budget = 34 + ceil(36/3) = 46（修复前：
// 34 + ceil(70/3) = 58，复核循环续额不收敛）；二次耗尽复核（基线前移至 30）
// → unreported = 46-30 = 16 → remaining_eff = 54 → budget = 64；DB 追上真尽
// → 429 短路。
func TestGateReclaimReplenishes(t *testing.T) {
	g := newConcurrencyGate(nil)
	reader := &fakeQuotaReader{used: 30} // DB：已用 30（含他实例），剩余 70
	g.setReclaimer(reader)
	g.SetInstancesProvider(fakeInstances(3))
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"q": meta}) // 快照 quota_used=0 → budget = ceil(100/3) = 34

	for i := 0; i < 34; i++ {
		g.deductQuota(1, 1)
	}
	require.False(t, g.quotaExhausted(meta), "复核成功续额放行")
	require.Equal(t, 1, reader.callCount())
	q := g.store.Load().quotas[1]
	require.Equal(t, int64(34+ceilDiv(36, 3)), q.budget.Load(),
		"budget = consumed + ceil((剩余 - 本地未反映消耗)/N)")
	require.False(t, q.exhausted.Load())

	// 续额后继续放行至二次耗尽（12 笔）→ 复核（DB 仍 30）→ 续额收敛
	for i := 0; i < int(ceilDiv(36, 3))-1; i++ {
		g.deductQuota(1, 1)
		require.False(t, g.quotaExhausted(meta))
	}
	g.deductQuota(1, 1) // consumed 46 ≥ budget 46 → 二次复核
	require.False(t, g.quotaExhausted(meta), "复核继续收敛续额（非 429）")
	require.Equal(t, 2, reader.callCount())
	q = g.store.Load().quotas[1]
	require.Equal(t, int64(46+ceilDiv(54, 3)), q.budget.Load(),
		"unreported = 46-30 = 16 → remaining_eff = 100-30-16 = 54")

	// 二次续额内放行（17 笔，剩 1 笔触界）；DB 追上（真尽）→ 触界复核确认真尽
	for i := 0; i < int(ceilDiv(54, 3))-1; i++ {
		g.deductQuota(1, 1)
		require.False(t, g.quotaExhausted(meta))
	}
	reader.set(100, nil) // DB 复核读到真尽
	g.deductQuota(1, 1)  // consumed 64 ≥ budget 64 → 复核
	require.True(t, g.quotaExhausted(meta), "复核确认真尽 → 429")
	require.Equal(t, 3, reader.callCount())
}

// 复核确认真尽：exhausted 短路——后续请求 429 且不再发起复核（不双倍认领）。
func TestGateReclaimTrueExhaustionShortCircuit(t *testing.T) {
	g := newConcurrencyGate(nil)
	reader := &fakeQuotaReader{used: 100} // DB：已用 100 → 剩余 0
	g.setReclaimer(reader)
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"q": meta})

	for i := 0; i < 100; i++ {
		g.deductQuota(1, 1)
	}
	require.True(t, g.quotaExhausted(meta), "复核确认真尽 → 429")
	require.Equal(t, 1, reader.callCount())
	for i := 0; i < 5; i++ {
		require.True(t, g.quotaExhausted(meta))
	}
	require.Equal(t, 1, reader.callCount(), "exhausted 短路后不再发起复核")
}

// 并发复核单飞：同 key 并发到达耗尽点，只有一个进 DB（block 卡住复核者），
// 其余按旧预算 429；复核完成后预算续额，后续请求放行。
func TestGateReclaimSingleFlight(t *testing.T) {
	g := newConcurrencyGate(nil)
	block := make(chan struct{})
	reader := &fakeQuotaReader{used: 50, blockCh: block}
	g.setReclaimer(reader)
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"q": meta})
	for i := 0; i < 100; i++ {
		g.deductQuota(1, 1)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.quotaExhausted(meta)
		}()
	}
	// 复核者已进 DB（被 block 卡住），其余并发到达按旧预算判定返回
	require.Eventually(t, func() bool { return reader.callCount() == 1 }, time.Second, 10*time.Millisecond)
	close(block)
	wg.Wait()
	require.Equal(t, 1, reader.callCount(), "同 key 并发复核单飞去重（不双倍认领）")
	// #37 P1：复核认领扣除本地未反映消耗——consumed(100) - 基线(0) = 100 →
	// remaining_eff = 100-50-100 ≤ 0 → 确认真尽（DB 滞后 50 ≠ 还有额度）
	require.True(t, g.quotaExhausted(meta), "本地已消耗满 quota → 复核确认真尽 429")
}

// 复核失败（DB 错）策略：Warn + 本请求放行 + 预算补 1 + 退避 10s；退避期内
// 预算耗尽按 429（不重复复核防风暴）；退避过期重试；DB 恢复后复核认领（#37
// P1：本地已超 quota → 确认真尽，见尾部）。
func TestGateReclaimDBErrorAllowAndRetry(t *testing.T) {
	g := newConcurrencyGate(nil)
	reader := &fakeQuotaReader{err: errors.New("db down")}
	g.setReclaimer(reader)
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100}
	g.reload(map[string]domain.KeyMeta{"q": meta})
	for i := 0; i < 100; i++ {
		g.deductQuota(1, 1)
	}

	// DB 错：软门禁不误伤——本请求放行（扣费恒条件 UPDATE，DB 错时同样失败，
	// 无错计费），预算补 1
	require.False(t, g.quotaExhausted(meta))
	require.Equal(t, 1, reader.callCount())
	q := g.store.Load().quotas[1]
	require.Equal(t, int64(101), q.budget.Load(), "DB 错 → 预算补 1（短暂放行）")

	// 本请求扣减后预算再耗尽 → 退避期内保守 429，且不重复复核
	g.deductQuota(1, 1)
	require.True(t, g.quotaExhausted(meta), "退避期内保守 429")
	require.Equal(t, 1, reader.callCount(), "退避期内不重复复核（防 DB 风暴）")

	// 退避过期 → 重试复核（仍错 → 放行）
	q.retryAt.Store(0)
	require.False(t, g.quotaExhausted(meta))
	require.Equal(t, 2, reader.callCount())

	// #37 P1：DB 恢复（读 used=50，仍滞后于本地）→ 复核认领扣除本地未反映
	// 消耗——consumed(102) - 基线(0，复核从未成功过) = 102 → remaining_eff =
	// 100-50-102 ≤ 0 → 确认真尽 429（本地已超 quota，不再凭滞后 DB 值续额）
	g.deductQuota(1, 1)
	reader.set(50, nil)
	q.retryAt.Store(0)
	require.True(t, g.quotaExhausted(meta), "本地消耗已超 quota → 复核确认真尽 429")
	require.Equal(t, 3, reader.callCount())
	require.True(t, g.store.Load().quotas[1].exhausted.Load())
}

// #37 P1 核心回归（镜像压测场景）：N=2 单 key quota=20000，fake 复核读固定
// quota_used=3000（DB 滞后——usage.Recorder 每 quota_flush_interval 批写一次，
// 两次回写间复核读到的 DB 值恒定）。修复前：每次复核重新分配 ceil(remaining/N)
// → 复核循环无限续额（压测实证超跑 14 倍 ≈283,740 token）。修复后：复核认领
// 扣除本地未反映消耗（unreported = consumed - 上次复核基线）→ 多次复核后总
// 放行收敛 ≤ quota + 滞后差（实际收敛到 quota 本身），随后确认真尽 429。
func TestGateReclaimConvergesWithLag(t *testing.T) {
	g := newConcurrencyGate(nil)
	g.SetInstancesProvider(fakeInstances(2))
	reader := &fakeQuotaReader{used: 3000} // DB 滞后固定值（一直不追上的最坏形态）
	g.setReclaimer(reader)
	const quota = int64(20000)
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: quota}
	g.reload(map[string]domain.KeyMeta{"q": meta}) // 快照 quota_used=0 → budget = ceil(20000/2) = 10000

	var allowed int64
	// 复核循环模拟：预算耗尽 → 复核（DB 恒定滞后）→ 续额 → 继续消耗，直到
	// 复核确认真尽（收敛）；allowed 超 2 倍 quota 视为未收敛（修复前必触发）。
	for allowed = 0; !g.quotaExhausted(meta) && allowed <= quota*2; {
		g.deductQuota(1, 1)
		allowed++
	}
	q := g.store.Load().quotas[1]
	require.True(t, q.exhausted.Load(), "复核循环收敛：确认真尽（修复前无限续额）")
	require.LessOrEqual(t, allowed, quota, "总放行收敛 ≤ quota（滞后差内不超配额）")
	require.GreaterOrEqual(t, reader.callCount(), 2, "复核多次（验证循环收敛而非一次封顶）")
	require.LessOrEqual(t, reader.callCount(), 100, "复核次数有界（收敛非震荡）")
}

// Reload 重建预算保留在途：consumed 跨 reload 继承，预算按最新快照重分配；
// 真尽快照 → exhausted。
func TestGateReloadRebuildBudgetKeepsInflight(t *testing.T) {
	g := newConcurrencyGate(nil)
	g.SetInstancesProvider(fakeInstances(2))
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100, QuotaUsed: 40}
	g.reload(map[string]domain.KeyMeta{"q": meta})
	q := g.store.Load().quotas[1]
	require.Equal(t, int64(40), q.consumed.Load(), "新 key 基线 = DB 快照值")
	require.Equal(t, int64(40+ceilDiv(60, 2)), q.budget.Load())
	g.deductQuota(1, 10)

	// reload 重建：consumed 在途继承，预算按新快照重分配（quota_used 前移）
	g.reload(map[string]domain.KeyMeta{"q": domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100, QuotaUsed: 55}})
	q = g.store.Load().quotas[1]
	require.Equal(t, int64(50), q.consumed.Load(), "在途继承（40+10）")
	require.Equal(t, int64(50+ceilDiv(45, 2)), q.budget.Load(), "预算按最新快照重分配")

	// 真尽快照 → exhausted 短路
	g.reload(map[string]domain.KeyMeta{"q": domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100, QuotaUsed: 100}})
	q = g.store.Load().quotas[1]
	require.True(t, q.exhausted.Load())
	require.True(t, g.quotaExhausted(domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 100, QuotaUsed: 100}))
}

// upsert 预算随最新 meta 重分配（额度调整即时生效）：quota 上调 → budget 前移、
// 在途 consumed 不动；quota 取消（→0）→ 门禁条目移除不再拦截。
func TestGateUpsertReallocBudget(t *testing.T) {
	g := newConcurrencyGate(nil)
	meta := domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 10, QuotaUsed: 3}
	g.upsert(meta)
	q := g.store.Load().quotas[1]
	require.Equal(t, int64(3), q.consumed.Load())
	require.Equal(t, int64(10), q.budget.Load(), "N=1：budget = 3 + ceil(7/1)")
	g.deductQuota(1, 5)

	// 额度调整（10 → 50）：预算重分配、在途 consumed 不动
	g.upsert(domain.KeyMeta{KeyID: 1, HasQuota: true, Quota: 50, QuotaUsed: 8})
	q = g.store.Load().quotas[1]
	require.Equal(t, int64(8), q.consumed.Load(), "在途不动")
	require.Equal(t, int64(50), q.budget.Load(), "budget = consumed + ceil((50-8)/1)")

	// 额度取消（quota→0）：门禁条目移除，不再拦截
	g.upsert(domain.KeyMeta{KeyID: 1, HasQuota: false})
	_, ok := g.store.Load().quotas[1]
	require.False(t, ok, "quota→0 移除门禁条目")
	require.False(t, g.quotaExhausted(domain.KeyMeta{KeyID: 1, HasQuota: false}))
}

// Auth.SetInstancesProvider：N 注入即触发预算重算（幂等 reload，在途继承）。
func TestAuthSetInstancesRebuildsBudget(t *testing.T) {
	a := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"h": {KeyID: 1, HasQuota: true, Quota: 100, QuotaUsed: 20},
	}}, noopUserLoader{}, nil)
	require.NoError(t, a.Reload(context.Background())) // 构造不再自载——测试显式首刷（快照注册表单一入口）
	q := a.gate.store.Load().quotas[1]
	require.Equal(t, int64(20+ceilDiv(80, 1)), q.budget.Load(), "N=1 初始：budget = 20 + ceil(80/1)")
	a.SetInstancesProvider(fakeInstances(4))
	q = a.gate.store.Load().quotas[1]
	require.Equal(t, int64(20+ceilDiv(80, 4)), q.budget.Load(), "N 变更立即重算预算（§3.4）")
	require.Equal(t, int64(20), q.consumed.Load(), "在途 consumed 不动")
}
