// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package identitypool

// 七条并发不变量（P5b v2 修正 2 的判据形态）+ 五个确定性注入点（修正 3）。
//
// 为什么必须靠注入点而不是"跑很多 goroutine 看结果"：
//   - 本包所有跨 goroutine 状态都是 atomic.Bool / atomic.Uint64 ⇒ **-race 结构性
//     沉默**（它不报原子操作的双租，那是纯逻辑不变量）；
//   - 统计式用例在 GOMAXPROCS=1 时 100% 假绿（两份 racer 根本不会重叠）；
//   - "最终值" 型断言（在途数最后是不是 0、map 最后是不是还在）在变异下恒真。
//
// 所以每条判据都带**正控**（先证明观测面非空），交错一律用通道屏障而非 sleep。

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// exclusiveTable 是 I1/I2 的**池侧只读旁表**：per-session 独占计数器。
//
// 为什么不放进 Session 字段：40B→48B 会改布局（testdata/layout.golden 会红），
// 而测试面本来就枚举得到池的槽位——旁表建一次、只读、无锁，池的热路径一行都不碰。
type exclusiveTable map[*Session]*atomic.Int32

func newExclusiveTable(p *Pool) exclusiveTable {
	t := make(exclusiveTable, p.Len())
	for i := range p.Len() {
		t[p.Slot(i)] = new(atomic.Int32)
	}
	return t
}

// enter/leave 是持有者的进出账：唯一的合法迁移是 0→1（认领成功）与 1→0（归还）。
// 返回值即判据：CAS 失败 = 同一会话同一时刻有两个持有者，也就是双租。
func (t exclusiveTable) enter(s *Session) bool { return t[s].CompareAndSwap(0, 1) }
func (t exclusiveTable) leave(s *Session) bool { return t[s].CompareAndSwap(1, 0) }

// waitFor 在 deadline 内等一个通道关闭（确定性交错的死锁兜底：不睡、不轮询）。
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s 未在 deadline 内到达（屏障未放行）", what)
	}
}

// TestI1SessionExclusiveUnderReserveBarrier I1（会话独占，确定性屏障）：
// 单槽 + 两个 racer 在「已判空闲、尚未写 busy」处对齐，然后同时越过判定——
// 恰好一个取得会话，且旁表上的 0→1 / 1→0 迁移各自只成功一次。
//
// 为什么不是"64 goroutine 跑很久"：GOMAXPROCS=1 时那种写法 100% 假绿，而
// `-race` 对全 atomic 状态一条 DATA RACE 都不会报（M7 变异下实测 0 条）。
func TestI1SessionExclusiveUnderReserveBarrier(t *testing.T) {
	require.GreaterOrEqual(t, runtime.GOMAXPROCS(0), 2,
		"GOMAXPROCS=1 时 CAS 争用不可复现：本用例不许静默假绿，请提高 GOMAXPROCS 再跑")
	pool := newTestPool(1, testPolicy())
	excl := newExclusiveTable(pool)
	require.Len(t, excl, 1, "正控：独占旁表非空（否则下面的 CAS 断言全是空转）")

	const racers = 2
	var arrived sync.WaitGroup
	arrived.Add(racers)
	release := make(chan struct{})
	pool.hooks = &hooks{reserve: func() {
		arrived.Done()
		<-release
	}}
	go func() { arrived.Wait(); close(release) }()

	type outcome struct {
		session *Session
		double  bool
	}
	results := make(chan outcome, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Go(func() {
			s := pool.Claim()
			if s == nil {
				results <- outcome{}
				return
			}
			if !excl.enter(s) { // 0→1 必须成功；失败 = 双租
				results <- outcome{session: s, double: true}
				return
			}
			results <- outcome{session: s}
		})
	}
	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()
	waitFor(t, allDone, "两个 racer 的 claim")
	// 屏障使命结束：后续单线程 claim 不得再进钩子（arrived 已计满）。
	pool.hooks = nil
	close(results)

	var winners []*Session
	for r := range results {
		require.False(t, r.double, "同一会话不得被两个 racer 同时持有（双租）")
		if r.session != nil {
			winners = append(winners, r.session)
		}
	}
	require.Len(t, winners, 1, "单槽只能被一个 goroutine 取得（双租 = M7 现形）")
	held := winners[0]
	require.Equal(t, int32(1), excl[held].Load(), "持有者在旁表上计到 1（那次 0→1 的 CAS 成功）")
	require.Nil(t, pool.Claim(), "同一时刻第二个持有者必失败（独占只由 CAS 保证）")
	require.Equal(t, int32(1), excl[held].Load(), "失败的认领不得改计数")
	require.True(t, excl.leave(held), "归还前 1→0 的 CAS 必须成功")
	held.Release()
	require.Equal(t, int32(0), excl[held].Load(), "归还后归零")
	require.Same(t, held, pool.Claim(), "归还后立刻可再认领（同一会话对象）")
}

// TestI2ClaimNoOversellAndBidirectionalNil I2（不超卖 + 双向 nil 判定）：
// ① 并发相位：在途量峰值 ≤ K，且同一会话不得有第二个持有者；
// ② 确定性相位："全忙 ⇒ nil" 与 "非全忙 ⇒ 非 nil" 两个方向都断。
//
// 为什么用峰值而不是最终值：最终值恒为 0，`0 ≤ K` 永远真（真空断言）；
// 为什么两个方向都要：只验"全忙⇒nil"时，"claim 恒返 nil" 的变异全绿。
func TestI2ClaimNoOversellAndBidirectionalNil(t *testing.T) {
	const k = 4
	pool := newTestPool(k, testPolicy())
	excl := newExclusiveTable(pool)
	require.Len(t, excl, k, "正控：旁表覆盖全部槽位")

	var inFlight, maxInFlight, claims, doubles atomic.Int64
	const workers = 32
	const rounds = 200
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range rounds {
				s := pool.Claim()
				if s == nil {
					continue
				}
				claims.Add(1)
				// Add/Sub 严格在临界区内（认领成功之后、归还之前）：界外读到的
				// inFlight 是"账"，不是"在途量"。
				n := inFlight.Add(1)
				for {
					m := maxInFlight.Load()
					if n <= m || maxInFlight.CompareAndSwap(m, n) {
						break
					}
				}
				if !excl.enter(s) {
					doubles.Add(1)
				}
				runtime.Gosched() // 撑开临界区：双租才有机会被第二个 worker 撞上
				if !excl.leave(s) {
					doubles.Add(1)
				}
				inFlight.Add(-1)
				s.Release()
			}
		})
	}
	wg.Wait()

	require.Positive(t, claims.Load(), "并发至少成功认领一次（否则用例空转）")
	require.Positive(t, maxInFlight.Load(), "正控：峰值 > 0（并发确实重叠过）")
	require.LessOrEqual(t, maxInFlight.Load(), int64(k), "在途峰值不得超过 K（不超卖）")
	require.Zero(t, doubles.Load(), "同一会话不得被两个 worker 同时持有")
	require.Zero(t, inFlight.Load(), "全部归还后账目归零")

	// 确定性相位：两个方向。
	held := make([]*Session, 0, k)
	for i := range k {
		s := pool.Claim()
		require.NotNil(t, s, "非全忙（第 %d 个）⇒ 必非 nil", i)
		require.True(t, excl.enter(s), "第 %d 个持有者独占", i)
		held = append(held, s)
	}
	require.Nil(t, pool.Claim(), "全忙 ⇒ 必 nil")
	require.False(t, excl.enter(held[0]), "全忙时不存在第二个持有者（旁表证明不是空转）")
	held[0].Release()
	require.True(t, excl.leave(held[0]), "归还的那一个 1→0")
	again := pool.Claim()
	require.NotNil(t, again, "非全忙 ⇒ 必非 nil")
	again.Release()
	for _, s := range held[1:] {
		s.Release()
	}
}

// TestI3ResizeLeavesInFlightSessionAlone I3（迁移不碰在飞会话）：
// K=4 占住一个位次后 shrink 到 2、grow 到 8——迁移的位次是**同一个指针**
// （值拷贝会红），在飞位次既不被迁移也不被退役。
//
// 判据为什么是 `require.Same` 而不是"threadKey/句柄不变"：`ThreadHandle` 是
// `struct{s *threadState}` 别名，`==` 只比指针；在在飞会话上推一次 `Step` 能让
// 窗口前进而 key/句柄全等 ⇒ 逐字段比较在现有 sdkbridge 上抓不住状态损坏。
func TestI3ResizeLeavesInFlightSessionAlone(t *testing.T) {
	spy := &retireSpy{}
	pool := newTestPoolWith(4, policyA, spy.fn)
	inFlight := pool.Claim()
	require.NotNil(t, inFlight)
	idx := slotIndex(pool, inFlight)
	require.GreaterOrEqual(t, idx, 0, "正控：认领到的会话确实在池里")
	key := inFlight.Key()

	shrunk := Resize(pool, Spec{AccountID: 7, K: 2, InstallationID: "inst", Policy: policyA, Retire: spy.fn}, &Reclaim{})
	require.Equal(t, 2, shrunk.Len())
	for i := range shrunk.Len() {
		if i == idx {
			require.NotSame(t, inFlight, shrunk.Slot(i), "在飞位次新开会话（不交接）")
			continue
		}
		require.Same(t, pool.Slot(i), shrunk.Slot(i), "同策略：迁移的是**会话指针**（值拷贝变异必红）")
		require.Equal(t, policyA, shrunk.Slot(i).Policy(), "迁移不改写策略")
	}
	require.True(t, inFlight.Busy(), "在飞会话仍被持有（迁移方不碰）")
	require.Equal(t, key, inFlight.Key(), "在飞会话身份不被改写")
	require.Empty(t, spy.snapshot(), "在飞会话的线程不退役（留给 SDK idle TTL）")

	grown := Resize(pool, Spec{AccountID: 7, K: 8, InstallationID: "inst", Policy: policyA, Retire: spy.fn}, &Reclaim{})
	require.Equal(t, 8, grown.Len())
	for i := range 4 {
		if i == idx {
			continue
		}
		require.Same(t, pool.Slot(i), grown.Slot(i), "扩容保留位次仍迁移同一指针")
	}
	require.NotSame(t, inFlight, grown.Slot(idx), "同一个在飞位次在扩容时同样不交接")
	for i := 4; i < 8; i++ {
		require.NotSame(t, inFlight, grown.Slot(i), "新增位次与在飞会话无关")
	}
	require.True(t, inFlight.Busy())
	require.Equal(t, key, inFlight.Key())
	require.Empty(t, spy.snapshot(), "两次 resize 都没退役在飞会话的线程")
	inFlight.Release()
}

// TestI4PolicyChangeRebuildsIdleAndLeavesInFlight I4（策略变化 ⇒ 换会话）：
// 空闲槽位**全部**退役，在飞槽位既不被退役也不被复用——它的 `Policy()` 仍是旧值、
// 指针不变（旧池/旧会话不被改写）；新池每个会话都按新策略构造。
//
// 与实现的边界一致：策略变更时在飞会话**故意**不退役（CAS 失败即跳过），
// 所以判据只能是"空闲全退役 + 在飞原样"。
func TestI4PolicyChangeRebuildsIdleAndLeavesInFlight(t *testing.T) {
	spy := &retireSpy{}
	pool := newTestPoolWith(4, policyA, spy.fn)
	inFlight := pool.Claim()
	require.NotNil(t, inFlight)
	idx := slotIndex(pool, inFlight)
	require.GreaterOrEqual(t, idx, 0)

	reclaim := &Reclaim{}
	next := Resize(pool, Spec{AccountID: 7, K: 4, InstallationID: "inst", Policy: policyB, Retire: spy.fn}, reclaim)
	require.Equal(t, 4, next.Len())
	require.Len(t, reclaim.Threads, pool.Len()-1, "空闲槽位全部退役（K-1 条，正控）")
	require.Empty(t, spy.snapshot(), "只收集不执行")
	reclaim.Run(spy.fn)
	require.Len(t, spy.snapshot(), pool.Len()-1, "Run 后恰好执行收集数")

	require.Same(t, inFlight, pool.Slot(idx), "旧池仍指向同一会话（resize 不写已发布会话）")
	require.Equal(t, policyA, inFlight.Policy(), "在飞会话策略仍是旧值")
	require.True(t, inFlight.Busy(), "在飞会话仍被持有")
	for i := range next.Len() {
		require.NotSame(t, inFlight, next.Slot(i), "策略变化不得复用旧会话（换的是会话，不是策略）")
		require.Equal(t, policyB, next.Slot(i).Policy(), "新会话按新策略构造")
	}
	require.Equal(t, policyB, next.RotatePolicy())
	require.Equal(t, policyA, pool.RotatePolicy(), "旧池策略恒定")
	inFlight.Release()
}

// TestI5RetireCollectedOnlyThenRun I5（退役只收集）：三条收集路径都必须是
// "清单变长、回调不动"，Run 之后回调才恰好是清单长度，且同一线程只出现一次。
//
// 为什么三重正控：只断言 spy==0 时，"函数开头 return"（什么都不做）也全绿；
// 只断言清单长度时，"收集了但没跑"与"根本没收集"分不开。
func TestI5RetireCollectedOnlyThenRun(t *testing.T) {
	spy := &retireSpy{}
	pool := newTestPoolWith(4, policyA, spy.fn)

	// ① 缩容丢槽
	reclaim := &Reclaim{}
	shrunk := Resize(pool, Spec{AccountID: 7, K: 2, InstallationID: "inst", Policy: policyA, Retire: spy.fn}, reclaim)
	require.Equal(t, 2, shrunk.Len())
	require.Len(t, reclaim.Threads, 2, "缩容丢槽：两条线程被收集（正控）")
	require.Empty(t, spy.snapshot(), "Resize 只收集，不执行")
	requireNoDuplicates(t, threadKeys(reclaim.Threads), "同一线程不得二次退役")
	reclaim.Run(spy.fn)
	require.Len(t, spy.snapshot(), 2, "Run 后恰好执行收集数")

	// ② CollectThreads（池消失/被替换时的空闲侧）
	reclaim2 := &Reclaim{}
	shrunk.CollectThreads(reclaim2)
	require.Len(t, reclaim2.Threads, 2, "空闲槽位全部收集（正控）")
	require.Empty(t, spy.snapshot()[2:], "CollectThreads 只收集，不执行")
	requireNoDuplicates(t, threadKeys(reclaim2.Threads), "同一线程不得二次退役")
	reclaim2.Run(spy.fn)
	require.Len(t, spy.snapshot(), 4, "Run 后清单长度 = 新增回调数")

	// ③ CollectRetiredPools（池从注册表消失）
	vanish := newTestPoolWith(3, policyA, spy.fn)
	reclaim3 := &Reclaim{}
	CollectRetiredPools(
		&Registry{Pools: map[int64]*Pool{7: vanish}},
		&Registry{Pools: map[int64]*Pool{}},
		reclaim3,
	)
	require.Len(t, reclaim3.Threads, 3, "池消失：三条线程被收集（正控）")
	require.Empty(t, spy.snapshot()[4:], "CollectRetiredPools 只收集，不执行")
	requireNoDuplicates(t, threadKeys(reclaim3.Threads), "同一线程不得二次退役")
	reclaim3.Run(spy.fn)
	require.Len(t, spy.snapshot(), 7)

	// 退役汇为 nil（本批次没有退役出口）⇒ 一条都不收集、不 panic：与改前
	// `reclaim.retire == nil ⇒ 直接返回` 等价（否则收集期会多出一轮假忙 CAS 窗口，
	// 并发请求会看到"池突然全忙"）。调度器用 retireSink() 把这件事传成裸 nil 接口。
	CollectRetiredPools(&Registry{Pools: map[int64]*Pool{7: vanish}}, nil, nil)
	require.Empty(t, spy.snapshot()[7:], "无退役出口时不得执行任何退役")
}

// requireNoDuplicates 集合无重复：同一线程被退役两次的话，I1–I7 没有一条抓得到
// （每条不变量都只看"最终状态"，而重复退役改的是回调次数）。
func requireNoDuplicates(t *testing.T, keys []string, msg string) {
	t.Helper()
	seen := make(map[string]int, len(keys))
	for _, k := range keys {
		seen[k]++
		require.Equal(t, 1, seen[k], "%s：线程键 %q 出现 %d 次", msg, k, seen[k])
	}
}

// threadKeys 把批次摊成线程键列表（去重断言用）。
func threadKeys(ts []ThreadRetire) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ThreadKey
	}
	return out
}

// TestI6RegistryImmutableWhileCollecting I6（注册表发布后不可变）：读者 goroutine
// 在 `CollectRetiredPools` 进行中循环 `Lookup`，期望在调用**之前**独立构造——
// 拿调用后的 map 当期望就是真空（它当然等于它自己）。
func TestI6RegistryImmutableWhileCollecting(t *testing.T) {
	vanish := newTestPool(2, policyA)
	shared := newTestPool(2, policyA)
	fresh := newTestPool(2, policyA)
	prev := &Registry{Pools: map[int64]*Pool{7: vanish, 3: shared}}
	next := &Registry{Pools: map[int64]*Pool{3: shared, 9: fresh}}

	// 期望在调用前独立构造（不是调用后的快照）。
	wantPrev := map[int64]*Pool{7: vanish, 3: shared}
	wantNext := map[int64]*Pool{3: shared, 9: fresh}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	vanish.hooks = &hooks{registry: func(phase string) {
		if phase != "enter" {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	}}

	// 每个读者先跑满一遍再报就绪——钩子等就绪，于是"收集进行中"是**构造出来的**，
	// 不是碰巧同时。
	var ready sync.WaitGroup
	ready.Add(4)
	stop := make(chan struct{})
	var violations atomic.Int64
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			first := true
			for {
				if prev.Lookup(7) != vanish || prev.Lookup(3) != shared {
					violations.Add(1)
				}
				if next.Lookup(3) != shared || next.Lookup(9) != fresh {
					violations.Add(1)
				}
				if len(prev.Pools) != 2 || len(next.Pools) != 2 {
					violations.Add(1)
				}
				if first {
					first = false
					ready.Done()
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		})
	}

	done := make(chan *Reclaim, 1)
	go func() {
		r := &Reclaim{}
		CollectRetiredPools(prev, next, r)
		done <- r
	}()
	ready.Wait()
	waitFor(t, entered, "收集方进入循环体")
	require.Zero(t, violations.Load(), "收集进行中注册表已被改写（读者看见了不变量违反）")
	close(release)
	r := <-done
	close(stop)
	readers.Wait()

	require.Len(t, r.Threads, 2, "池消失只收集它自己的线程（正控）")
	require.Zero(t, violations.Load(), "读者在收集全程观察到的键集/值指针必须不变")
	require.Len(t, prev.Pools, 2, "prev.Pools 键集不变")
	require.Len(t, next.Pools, 2, "next.Pools 键集不变")
	for id, want := range wantPrev {
		require.Same(t, want, prev.Pools[id], "prev.Pools[%d] 值指针不变", id)
	}
	for id, want := range wantNext {
		require.Same(t, want, next.Pools[id], "next.Pools[%d] 值指针不变", id)
	}
}

// TestI7MigrateHookSingleHolder I7-a（migrate 注入点）：resize 迁移分支在
// 「CAS 成功、Store(false) 之前」停住持有会话——此刻旧视图认领必须失败。
// 这是"安全来自会话对象上的共享 CAS，不靠发布时机"那段注释唯一可执行的形式。
func TestI7MigrateHookSingleHolder(t *testing.T) {
	pool := newTestPool(1, policyA)
	os := pool.Slot(0)
	require.NotNil(t, os)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	pool.hooks = &hooks{migrate: func(i int) {
		once.Do(func() { close(entered) })
		<-release
	}}

	done := make(chan *Pool, 1)
	go func() { done <- Resize(pool, Spec{AccountID: 7, K: 1, InstallationID: "inst", Policy: policyA}, nil) }()
	waitFor(t, entered, "resizer 停在迁移点")
	require.True(t, os.Busy(), "resizer 此刻持有旧会话（CAS 已成功）")
	require.Nil(t, pool.Claim(), "旧视图同时 claim 必失败：独占只由共享 CAS 保证")
	close(release)
	np := <-done
	require.Same(t, os, np.Slot(0), "迁移的是同一指针（不是副本）")
	require.False(t, os.Busy(), "Store(false) 之后空闲")
	require.Same(t, os, pool.Claim(), "旧池认领到的仍是同一会话对象")
}

// TestI7ShrinkHookCollectsOnly I7-b（shrink 注入点）：缩容在「CAS 成功后、
// 收集退役前」停住——此刻被丢位次已独占（并发 claim 拿不到），且 retire 还没发生。
func TestI7ShrinkHookCollectsOnly(t *testing.T) {
	spy := &retireSpy{}
	pool := newTestPoolWith(2, policyA, spy.fn)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	pool.hooks = &hooks{shrink: func(i int) {
		once.Do(func() { close(entered) })
		<-release
	}}

	reclaim := &Reclaim{}
	done := make(chan *Pool, 1)
	go func() {
		done <- Resize(pool, Spec{AccountID: 7, K: 1, InstallationID: "inst", Policy: policyA, Retire: spy.fn}, reclaim)
	}()
	waitFor(t, entered, "resizer 停在缩容点")
	require.NotNil(t, pool.Claim(), "保留位次仍空闲可认领")
	require.Nil(t, pool.Claim(), "被丢位次此刻被缩容方持有 ⇒ 全忙")
	require.Empty(t, spy.snapshot(), "收集期不执行 retire（只收集）")
	close(release)
	<-done
	require.Len(t, reclaim.Threads, 1, "被丢位次的线程已收集（正控）")
	require.Empty(t, spy.snapshot(), "Run 之前回调恒 0")
	reclaim.Run(spy.fn)
	require.Len(t, spy.snapshot(), 1, "Run 后恰好一次")
}

// TestI7CollectHookExcludesConcurrentClaim I7-c（collect 注入点）：收集方在
// 「CAS 成功后、Store(false) 之前」停住时，并发 claim 拿不到那个会话——
// 半成品不对外可见。
func TestI7CollectHookExcludesConcurrentClaim(t *testing.T) {
	pool := newTestPool(2, policyA)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	pool.hooks = &hooks{collect: func(i int) {
		if i != 0 {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	}}

	reclaim := &Reclaim{}
	done := make(chan struct{})
	go func() { pool.CollectThreads(reclaim); close(done) }()
	waitFor(t, entered, "收集方停在 collect 点")
	got := pool.Claim()
	require.NotNil(t, got, "另一槽位仍空闲")
	require.Same(t, pool.Slot(1), got, "收集中的槽位不可被认领（只拿到另一个）")
	require.Nil(t, pool.Claim(), "收集中的槽位已独占 ⇒ 全忙")
	got.Release()
	close(release)
	<-done
	require.Len(t, reclaim.Threads, 2, "两个空闲槽位都收集到（正控）")
}
