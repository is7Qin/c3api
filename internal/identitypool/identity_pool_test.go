// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package identitypool

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/sdkbridge"
)

// 本文件是 `internal/scheduler/identity_pool{,_resize_policy}_test.go` 里**只读池**
// 的那 16 个用例的搬家产物（G1 对照表见报告）：会话/池语义断言逐条保留，只改
// 类型名与"调度器视图"那一层的调用（`selection`/`Scheduler` 留在调度器）。
// 白盒（package identitypool）因此不需要为测试导出任何东西。

var (
	policyA = sdkbridge.RotatePolicy{WMaxLo: 1, WMaxHi: 3}
	policyB = sdkbridge.RotatePolicy{WMaxLo: 4, WMaxHi: 9}
)

// testPolicy 无退休（WMaxHi 0）——池机制测试用（身份演化另测）。
func testPolicy() sdkbridge.RotatePolicy {
	return sdkbridge.RotatePolicy{}
}

// sessionThreadKey 会话当前线程键（不解析：只用于"是否换了线程"的比较）。
// 与 `Session.Key()` 走同一条派生（这里直接穿句柄，是为了让"缓存副本 == 句柄派生"
// 这条断言仍然是**两个独立来源**）。
func sessionThreadKey(s *Session) string { return s.threads.Key(s.h) }

// retireSpy 记录线程退役调用（「旧线程被摘」的观测面）。
type retireSpy struct {
	mu    sync.Mutex
	calls []string // "accountID|threadKey"
}

func (r *retireSpy) fn(accountID int64, threadKey string) {
	r.mu.Lock()
	r.calls = append(r.calls, fmt.Sprintf("%d|%s", accountID, threadKey))
	r.mu.Unlock()
}

func (r *retireSpy) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// newTestPool 构造容量 k 的池（installation "inst"，账号 7）。
func newTestPool(k int, p sdkbridge.RotatePolicy) *Pool {
	return newTestPoolWith(k, p, nil)
}

func newTestPoolWith(k int, p sdkbridge.RotatePolicy, retire func(int64, string)) *Pool {
	return resizeForTest(nil, Spec{
		AccountID: 7, K: k, InstallationID: "inst", Policy: p, Retire: retire,
	})
}

// resizeForTest 跑一次 resize 并**立即执行**收集到的退役动作。生产里退役由持锁方
// 在解锁后跑 `poolReclaim.run`；单测没有锁，所以这里当场消费——断言面相同。
func resizeForTest(old *Pool, spec Spec) *Pool {
	reclaim := &Reclaim{}
	np := Resize(old, spec, reclaim)
	reclaim.Run(spec.Retire)
	return np
}

// syncForTest 同 resizeForTest（复用路径不产生退役动作，但仍需要批次出口）。
func syncForTest(prev *Pool, spec Spec) *Pool {
	reclaim := &Reclaim{}
	np := Sync(prev, spec, reclaim)
	reclaim.Run(spec.Retire)
	return np
}

// slotIndex 会话在池里的位次（-1 = 不在池里）。
func slotIndex(p *Pool, s *Session) int {
	for i := range p.Len() {
		if p.Slot(i) == s {
			return i
		}
	}
	return -1
}

// TestSessionRetireReportsOldThreadKey 退休（§3.2）：
// ① 旧 threadKey 被报出（调用方锁外退役连接）② 新线程 + 计数归零 +
// windowID/threadKey/context_window_id 重算 + wmax 重抽。
//
// 池侧两步 = `Session.Step()` + `Pool.Retire(旧键)`——调度器的 `StepIdentity`
// 就是这两行的唯一调用点。
func TestSessionRetireReportsOldThreadKey(t *testing.T) {
	spy := &retireSpy{}
	pool := newTestPoolWith(1, sdkbridge.RotatePolicy{WMaxLo: 1, WMaxHi: 1}, spy.fn)
	session := pool.Claim()
	require.NotNil(t, session)
	oldKey := sessionThreadKey(session)

	retiredAt := 0
	for i := 1; i <= 96; i++ {
		if old := session.Step(); old != "" {
			pool.Retire(old)
			retiredAt = i
			break
		}
	}
	require.GreaterOrEqual(t, retiredAt, 48, "48 轮前不退休（span≥48）")
	require.LessOrEqual(t, retiredAt, 96, "96 轮内必退休（span≤96）")

	require.Equal(t, []string{fmt.Sprintf("7|%s", oldKey)}, spy.snapshot(), "退休报出**旧** threadKey（锁外退役入口）")
	require.NotEqual(t, oldKey, sessionThreadKey(session), "退休换新线程")
	require.Equal(t, sessionThreadKey(session), session.threadKey, "池缓存的线程键与句柄一致")
	require.Equal(t, session.threadKey, session.Key(), "Key() 与缓存副本同源（零分配读）")
}

// TestResizeMigratesOwnedSessions 容量变化按位次迁移（沿用线程/窗口计数）；
// 空闲旧会话被缩容丢弃 ⇒ 立即退役旧线程（G10 的空闲侧）。
func TestResizeMigratesOwnedSessions(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(4, testPolicy(), spy.fn)
	threads := make([]string, 4)
	keys := make([]string, 4)
	for i, s := range old.slots {
		threads[i], keys[i] = sessionThreadKey(s), s.threadKey
	}

	shrunk := resizeForTest(old, Spec{AccountID: 7, K: 2, InstallationID: "inst", Policy: testPolicy(), Retire: spy.fn})
	require.Equal(t, 2, shrunk.Len())
	for i := range 2 {
		require.Equal(t, threads[i], sessionThreadKey(shrunk.slots[i]), "保留位次沿用线程")
		require.False(t, shrunk.slots[i].busy.Load(), "迁移后空闲（新池尚未发布）")
	}
	require.Equal(t, []string{fmt.Sprintf("7|%s", keys[2]), fmt.Sprintf("7|%s", keys[3])}, spy.snapshot(),
		"被丢弃且预留成功的旧会话立即退役旧线程")

	grown := resizeForTest(shrunk, Spec{AccountID: 7, K: 4, InstallationID: "inst", Policy: testPolicy(), Retire: spy.fn})
	require.Equal(t, 4, grown.Len())
	require.Equal(t, threads[0], sessionThreadKey(grown.slots[0]))
	require.Equal(t, threads[1], sessionThreadKey(grown.slots[1]))
	require.NotEmpty(t, sessionThreadKey(grown.slots[2]), "扩容位次开新线程")
	require.NotEmpty(t, sessionThreadKey(grown.slots[3]))
	require.NotEqual(t, sessionThreadKey(grown.slots[2]), sessionThreadKey(grown.slots[3]))
	for i := 2; i < 4; i++ {
		require.NotEmpty(t, sessionThreadKey(grown.slots[i]), "扩容位次开新线程")
		require.NotEqual(t, sessionThreadKey(grown.slots[i]), sessionThreadKey(grown.slots[0]), "新线程互不相同")
	}
}

// TestResizeSamePolicyKeepsThreadProgress 同策略 resize 迁移的是**会话对象本身**：
// 状态随对象走，进度不可能被重置。
//
// 为什么断言指针身份而不是"剩余步数"：`windowSpan` 由 (threadID, windowIndex) 确定性
// 派生，**不同线程的跨度不同**，跨会话比步数是错的尺子（第一版就这么写错了，
// 179 步 vs 97 步）。真正的判据是结构性的一条：迁移后 `np.slots[i]` 就是原来那个
// `*Session`——同一个句柄、同一份协议状态。剩下只需再验一句"迁移后状态机还活着"。
func TestResizeSamePolicyKeepsThreadProgress(t *testing.T) {
	policy := sdkbridge.RotatePolicy{WMaxLo: 2, WMaxHi: 2} // 每线程活 2 个窗口即退休

	pool := newTestPool(1, policy)
	session := pool.Claim()
	require.NotNil(t, session)
	key := sessionThreadKey(session)

	const walked = 7
	for i := 0; i < walked; i++ {
		require.Empty(t, session.Step(), "前 %d 步不该退休", walked)
	}
	session.Release() // 归还后 resize：空闲会话才按位次迁移

	np := resizeForTest(pool, Spec{AccountID: 7, K: 1, InstallationID: "inst", Policy: policy})
	require.Same(t, session, np.slots[0], "同策略 resize 迁移会话指针（协议状态随对象走，不重建）")
	require.Equal(t, key, sessionThreadKey(np.slots[0]), "迁移后仍是同一条线程")

	// 状态机迁移后仍然活着：能继续推进并在有界步数内退休。
	migrated := np.slots[0]
	retired := false
	for i := 1; i <= 500; i++ {
		if migrated.Step() != "" {
			retired = true
			break
		}
	}
	require.True(t, retired, "迁移后窗口推进与退休仍工作（500 步内未退休）")
	require.NotEqual(t, key, sessionThreadKey(migrated), "退休后换新线程键")
}

// TestResizeDroppedInFlightSessionNotHandedOver 缩容丢弃的**在飞**会话不做任何
// 交接（§3.2/§5.2-5）：迁移方不读它的字段（那是数据竞争）、不退役它的线程——
// 残留 idle 由 SDK 的 idle TTL 时间有界回收。
func TestResizeDroppedInFlightSessionNotHandedOver(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(4, testPolicy(), spy.fn)
	inFlight := old.slots[3]
	require.True(t, inFlight.busy.CompareAndSwap(false, true), "模拟持者在飞")
	inFlightThread := sessionThreadKey(inFlight)

	droppedIdle := old.slots[2].threadKey
	shrunk := resizeForTest(old, Spec{AccountID: 7, K: 2, InstallationID: "inst", Policy: testPolicy(), Retire: spy.fn})
	require.Equal(t, 2, shrunk.Len())
	require.Equal(t, []string{"7|" + droppedIdle}, spy.snapshot(),
		"只退役空闲的丢槽；在飞丢槽不做任何交接（不读其状态）")
	require.Equal(t, inFlightThread, sessionThreadKey(inFlight), "持者的会话状态不被迁移方改写")
	require.True(t, inFlight.busy.Load(), "持者仍持有 busy（收尾照常）")
	for i := range shrunk.Len() {
		require.NotSame(t, inFlight, shrunk.slots[i], "在飞会话不进入新池")
	}
	// 持者收尾：正常 Step（这里不退休）后释放，不影响新池。
	inFlight.Release()
	require.False(t, inFlight.busy.Load())
}

// TestResizeInstallationChangeRebuildsAllSessions installation 变 ⇒ 该账号**全部
// 会话重建**：旧线程逐个退役、新会话用新 installation（G11 的会话面）。
func TestResizeInstallationChangeRebuildsAllSessions(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(3, testPolicy(), spy.fn)
	oldThreads := make([]string, 3)
	oldKeys := make([]string, 3)
	for i, s := range old.slots {
		oldThreads[i], oldKeys[i] = sessionThreadKey(s), s.threadKey
	}

	np := resizeForTest(old, Spec{AccountID: 7, K: 3, InstallationID: "inst-new", Policy: testPolicy(), Retire: spy.fn})
	require.Equal(t, 3, np.Len())
	require.Equal(t, []string{
		"7|" + oldKeys[0], "7|" + oldKeys[1], "7|" + oldKeys[2],
	}, spy.snapshot(), "逐个退役旧线程")
	for i, s := range np.slots {
		require.NotEqual(t, oldThreads[i], sessionThreadKey(s), "全部会话重建（新线程）")
		require.Equal(t, sessionThreadKey(s), s.threadKey, "池缓存的线程键与句柄一致")
		require.Equal(t, np.installationID, "inst-new", "池安装号取本次生效值")
	}
}

// TestSyncIdentityPoolReusesWhenUnchanged 容量/安装 ID/策略均未变 → 原样复用
// （保留在途 busy 与会话身份）；任一变化才重建。
func TestSyncIdentityPoolReusesWhenUnchanged(t *testing.T) {
	p := testPolicy()
	pool := newTestPool(3, p)
	spec := func(k int, installation string, policy sdkbridge.RotatePolicy) Spec {
		return Spec{AccountID: 7, K: k, InstallationID: installation, Policy: policy}
	}
	require.Same(t, pool, syncForTest(pool, spec(3, "inst", p)), "未变复用")
	require.NotSame(t, pool, syncForTest(pool, spec(4, "inst", p)), "容量变化重建")
	require.NotSame(t, pool, syncForTest(pool, spec(3, "other", p)), "安装 ID 变化重建")
	require.NotSame(t, pool, syncForTest(pool, spec(3, "inst", sdkbridge.RotatePolicy{WMaxLo: 2, WMaxHi: 4})), "策略变化重建")
}

// TestIdentityPoolClaimReleaseNoCrossSession 认领 K 个会话互不相同（不串会话）、
// 全忙 claim 返回 nil、归还可再认领；复用不换身份。
func TestIdentityPoolClaimReleaseNoCrossSession(t *testing.T) {
	pool := newTestPool(3, testPolicy())
	seen := map[*Session]bool{}
	threads := map[string]bool{}
	for range 3 {
		s := pool.Claim()
		require.NotNil(t, s)
		require.False(t, seen[s], "会话不得重复认领")
		seen[s] = true
		require.True(t, s.busy.Load())
		threads[sessionThreadKey(s)] = true
	}
	require.Len(t, threads, 3, "K 个会话身份互不相同（不串台）")
	require.Nil(t, pool.Claim(), "全忙 → nil（兜底在外层）")

	pool.slots[1].Release()
	require.False(t, pool.slots[1].busy.Load())
	thread := sessionThreadKey(pool.slots[1])
	again := pool.Claim()
	require.Same(t, pool.slots[1], again, "唯一空闲会话被认领")
	require.Equal(t, thread, sessionThreadKey(again), "复用（占用）不改身份")
	again.Release()
}

// TestIdentityPoolConcurrentClaimUnique 并发认领唯一性：G 个 goroutine 同时
// claim（屏障对齐起跑），成功者互不相同、成功数恰为 K、全忙后 nil。
func TestIdentityPoolConcurrentClaimUnique(t *testing.T) {
	const k = 8
	pool := newTestPool(k, testPolicy())
	const g = 64

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	var mu sync.Mutex
	claimed := map[*Session]int{}
	success := 0
	for range g {
		done.Go(func() {
			start.Wait() // 屏障：全部就绪后同时认领，最大化 CAS 争用
			s := pool.Claim()
			if s == nil {
				return
			}
			mu.Lock()
			claimed[s]++
			success++
			mu.Unlock()
		})
	}
	start.Done()
	done.Wait()

	require.Len(t, claimed, success, "并发认领的成功者互不相同（不串会话）")
	require.Equal(t, k, success, "K 个空闲会话恰好被全部认领一次")
	for s, n := range claimed {
		require.Equal(t, 1, n, "同一会话不得被重复认领")
		require.True(t, s.busy.Load())
	}
	require.Nil(t, pool.Claim(), "全忙 → nil（兜底在外层）")
}

// TestClaimExclusiveAcrossIdleBarrier 独占性只由 CAS 保证（M7 的骨架）：单槽
// fixture 在「已判空闲、尚未写 busy」处放屏障，两个 goroutine 同时越过空闲判定
// ⇒ 仍只有一个拿到会话（变异把 CAS 换成无判定的 Store(true) 时两者拿到同一个）。
func TestClaimExclusiveAcrossIdleBarrier(t *testing.T) {
	pool := newTestPool(1, testPolicy())
	const racers = 2

	var arrived sync.WaitGroup
	arrived.Add(racers)
	release := make(chan struct{})
	pool.hooks = &hooks{reserve: func() {
		arrived.Done()
		<-release
	}}
	// 放行条件 = 两个 racer 都已到达屏障（通道 + deadline，无 sleep）。
	var closeOnce sync.Once
	go func() {
		arrived.Wait()
		closeOnce.Do(func() { close(release) })
	}()

	results := make(chan *Session, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Go(func() { results <- pool.Claim() })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("claim 未在 deadline 内返回（屏障未放行）")
	}
	close(results)

	var got []*Session
	for s := range results {
		if s != nil {
			got = append(got, s)
		}
	}
	require.Len(t, got, 1, "单槽只能被一个 goroutine 取得（双租 = M7 现形）")
	require.True(t, got[0].busy.Load())
}

// TestIdentityPoolResizeBusySlotNotMigrated 在飞（busy）旧会话不迁移、不被读写：
// 迁移只碰**取得 busy 所有权**的位次（与 TestResizeMigratesOwnedSessions 互补）。
func TestIdentityPoolResizeBusySlotNotMigrated(t *testing.T) {
	busyPool := newTestPool(2, testPolicy())
	busy := busyPool.Claim()
	require.NotNil(t, busy)
	busyIdx := slotIndex(busyPool, busy)
	require.NotEqual(t, -1, busyIdx)
	rebuilt := resizeForTest(busyPool, Spec{AccountID: 7, K: 2, InstallationID: "inst", Policy: testPolicy()})
	for i := range rebuilt.Len() {
		require.NotSame(t, busy, rebuilt.slots[i], "在飞会话不迁移（不与在途请求共享身份）")
	}
	require.NotSame(t, busy, rebuilt.slots[busyIdx], "在飞位次新开会话")
	require.Equal(t, sessionThreadKey(busy), sessionThreadKey(busyPool.slots[busyIdx]), "持者身份不被改写")
}

// TestIdentityPoolRotatePolicyImmutable 轮换策略是**池的不可变构造参数**：池与会话
// 各持同一份只读副本，池的读取不依赖任何会话——兜底路径因此不需要租借。
func TestIdentityPoolRotatePolicyImmutable(t *testing.T) {
	p := sdkbridge.RotatePolicy{WMaxLo: 1, WMaxHi: 3}
	pool := newTestPool(3, p)
	require.Equal(t, p, pool.RotatePolicy(), "池字段 = 构造参数")
	for _, sl := range pool.slots {
		require.Equal(t, p, sl.threads.Policy(), "会话持有建会话时的只读副本")
		require.Equal(t, p, sl.Policy(), "Session.Policy 不穿句柄读协议状态以外的东西")
	}
	require.Equal(t, sdkbridge.RotatePolicy{}, (&Pool{}).RotatePolicy(),
		"空池也按字段回答（不读 slot）")
}

// TestClaimAndThreadZeroAlloc §4 预算：认领 + 取线程句柄必须稳态 0 allocs。
// 句柄是值类型（两个机器字），Key 直接返回预计算字符串——热路径不做拼接、不分配。
func TestClaimAndThreadZeroAlloc(t *testing.T) {
	pool := newTestPool(1, testPolicy())
	var key string
	var h sdkbridge.ThreadHandle
	allocs := testing.AllocsPerRun(1000, func() {
		session := pool.Claim()
		if session == nil {
			t.Fatal("独占空档时 claim 必须成功")
		}
		h = session.Thread()
		key = session.Key()
		session.Release()
	})
	require.Zero(t, allocs, "认领 + 取句柄不得分配（allocs=%v）", allocs)
	require.NotEmpty(t, key, "线程键由池预计算，取用零分配")
	require.NotEqual(t, sdkbridge.ThreadHandle{}, h, "句柄由池持有（值搬运，不分配）")
}

// TestResizeSamePolicyMigratesSessionPointers 容量变化（策略不变）只迁移会话指针：
// 线程/窗口计数全部随对象走，池不复制任何会话状态。
func TestResizeSamePolicyMigratesSessionPointers(t *testing.T) {
	old := newTestPool(2, policyA)
	threads := []string{sessionThreadKey(old.slots[0]), sessionThreadKey(old.slots[1])}

	grown := resizeForTest(old, Spec{AccountID: 7, K: 4, InstallationID: "inst", Policy: policyA})
	require.Equal(t, 4, grown.Len())
	for i := range 2 {
		require.Same(t, old.slots[i], grown.slots[i], "同一策略：按位次迁移会话指针")
		require.Equal(t, threads[i], sessionThreadKey(grown.slots[i]))
		require.Equal(t, policyA, grown.slots[i].threads.Policy())
	}
	for i := 2; i < 4; i++ {
		require.NotSame(t, old.slots[0], grown.slots[i], "扩容位次是新会话")
		require.Equal(t, policyA, grown.slots[i].threads.Policy())
	}

	shrunk := resizeForTest(grown, Spec{AccountID: 7, K: 1, InstallationID: "inst", Policy: policyA})
	require.Same(t, grown.slots[0], shrunk.slots[0], "缩容保留的位次仍迁移同一指针")
}

// TestResizePolicyChangeRebuildsSessions 策略变化 = 换会话：旧会话的线程不再属于
// 新配置（窗口/退休口径全变），必须退役旧线程并开新会话。旧池/旧会话的策略字段
// 保持原值——持旧视图的请求读到的仍是它建会话时的策略。
func TestResizePolicyChangeRebuildsSessions(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(2, policyA, spy.fn)

	next := resizeForTest(old, Spec{AccountID: 7, K: 2, InstallationID: "inst", Policy: policyB, Retire: spy.fn})
	require.Equal(t, 2, next.Len())
	for i := range 2 {
		require.NotSame(t, old.slots[i], next.slots[i], "策略变化 ⇒ 新会话")
		require.Equal(t, policyB, next.slots[i].threads.Policy(), "新会话按新策略构造")
		require.Equal(t, policyA, old.slots[i].threads.Policy(), "已发布池/会话的策略不被 resize 改写")
		require.Equal(t, policyA, old.RotatePolicy())
	}
	require.Equal(t, policyB, next.RotatePolicy())
	require.Len(t, spy.snapshot(), 2, "旧线程在锁外退役（每会话一条）")
}

// TestResizePolicyReadDuringResize resize 期间策略只读（§3.3b 第 4 条的并发判别
// 用例）：多个读者在租借之外反复读旧池策略，同时执行策略变更的 resize。策略只在
// 构造期写入 ⇒ `-race` 干净；旧实现（resize 写 os.threads.Policy()）在这里现红。
func TestResizePolicyReadDuringResize(t *testing.T) {
	const k = 4
	old := newTestPool(k, policyA)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := old.RotatePolicy(); got != policyA {
					t.Errorf("已发布池的策略被 resize 改写：%+v", got)
					return
				}
				runtime.Gosched()
			}
		}()
	}

	for range 32 {
		next := resizeForTest(old, Spec{AccountID: 7, K: k, InstallationID: "inst", Policy: policyB})
		require.Equal(t, policyB, next.RotatePolicy())
		require.Equal(t, policyA, old.RotatePolicy(), "旧池策略恒定")
		for _, sl := range old.slots {
			require.Equal(t, policyA, sl.threads.Policy(), "旧会话策略恒定（resize 不写共享会话）")
		}
	}
	close(stop)
	wg.Wait()
}

// TestFallbackSessionUsesPoolPolicy 全忙兜底的临时身份只读池的不可变字段——不读
// slot 0 的 policy，也不需要任何租借（调度器 `claimCodexSession` 的兜底分支就是
// `NewSession(pool.InstallationID(), pool.RotatePolicy())` 这一行）。
func TestFallbackSessionUsesPoolPolicy(t *testing.T) {
	pool := newTestPool(1, policyB)

	busy := pool.Claim()
	require.NotNil(t, busy)
	require.Nil(t, pool.Claim(), "全忙（兜底在调用方）")

	ephem := NewSession(pool.InstallationID(), pool.RotatePolicy())
	require.NotNil(t, ephem)
	require.False(t, ephem.Busy(), "兜底身份不落池（不计 busy）")
	require.Equal(t, policyB, ephem.Policy(), "兜底身份的策略取自池字段")
	require.Equal(t, "inst", pool.InstallationID(), "兜底身份的 installation 取自池字段")
	require.NotSame(t, busy, ephem, "兜底身份不是池内会话")
	require.NotEmpty(t, sessionThreadKey(ephem), "兜底身份也是完整会话（有线程键）")
}
