// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// testPolicy 无退休（WMaxHi 0）——池机制测试用（身份演化另测）。
func testPolicy() codexsdk.RotatePolicy {
	return codexsdk.RotatePolicy{}
}

// retireSpy 记录线程退役调用（G2/G10/G11 的「旧线程被摘」观测面）。
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
func newTestPool(k int, p codexsdk.RotatePolicy) *identityPool {
	return newTestPoolWith(k, p, nil)
}

func newTestPoolWith(k int, p codexsdk.RotatePolicy, retire func(int64, string)) *identityPool {
	return resizeForTest(nil, poolSpec{
		accountID: 7, k: k, installationID: "inst", policy: p, retire: retire,
	})
}

// resizeForTest 跑一次 resize 并**立即执行**收集到的退役动作。生产里退役由持锁方
// 在解锁后跑 `poolReclaim.run`（N1）；单测没有锁，所以这里当场消费——断言面相同。
func resizeForTest(old *identityPool, spec poolSpec) *identityPool {
	reclaim := &poolReclaim{retire: spec.retire}
	np := resizeIdentityPool(old, spec, reclaim)
	reclaim.run()
	return np
}

// syncForTest 同 resizeForTest（复用路径不产生退役动作，但仍需要批次出口）。
func syncForTest(prev *identityPool, spec poolSpec) *identityPool {
	reclaim := &poolReclaim{retire: spec.retire}
	np := syncIdentityPool(prev, spec, reclaim)
	reclaim.run()
	return np
}

// TestSelectionCodexIdentityFromSession 身份四元组一次给全（Session + Meta 同源），
// installation 取自会话（单一来源）——与账号 ext 无关；无会话 → 零值。
func TestSelectionCodexIdentityFromSession(t *testing.T) {
	session := newCodexSession("slot-inst", testPolicy())
	session.turnState = "T1"
	sel := &Selection{
		session: session,
		Ext:     codexExt("ext-inst"), // 故意与会话不一致：注入必须取会话
	}
	id := sel.IdentityFor([16]byte{}, true)
	require.Equal(t, session.thread, id.Session.SessionID)
	require.Equal(t, session.thread, id.Session.ThreadID)
	require.Equal(t, session.windowID, id.Session.WindowID)
	require.Equal(t, "slot-inst", id.Meta.InstallationID, "installation 取会话，非 ext")
	require.Equal(t, id.Session.SessionID, id.Meta.SessionID)
	require.Equal(t, id.Session.ThreadID, id.Meta.ThreadID)
	require.Equal(t, id.Session.WindowID, id.Meta.WindowID)
	require.Equal(t, "T1", id.TurnState, "ts 来自会话")
	require.Same(t, session, id.Sink, "Sink 指向会话本身（ts 落点）")
	require.Equal(t, session.threadKey, threadKeyOf("slot-inst", session.thread), "threadKey 与 SDK 同构")

	// 无会话（非 codex / 池缺席）→ 零值（不注入）。
	empty := &Selection{Ext: codexExt("ext-inst")}
	require.Zero(t, empty.IdentityFor([16]byte{}, true))
	var nilSel *Selection
	require.Zero(t, nilSel.IdentityFor([16]byte{}, true))
}

// TestSessionStepAdvancesWindow 推进只由「完成轮数」驱动：turns 每步 +1；跨
// nextAt ⇒ windowN+1、windowID 重算、nextAt 叠加下一个 span（G1 的会话面）。
func TestSessionStepAdvancesWindow(t *testing.T) {
	session := newCodexSession("inst", testPolicy())
	firstWindow := session.windowID
	require.Equal(t, session.thread+":0", firstWindow, "windowID = thread:0")

	firstNextAt := session.nextAt
	require.GreaterOrEqual(t, firstNextAt, uint64(48))
	require.LessOrEqual(t, firstNextAt, uint64(96))

	for i := uint64(1); i < firstNextAt; i++ {
		require.Empty(t, session.step(), "窗口内不退休")
		require.Equal(t, i, session.turns)
		require.Equal(t, uint64(0), session.windowN)
		require.Equal(t, firstWindow, session.windowID, "窗口内 windowID 不变（不重算、不分配）")
	}
	require.Empty(t, session.step(), "跨窗口不退休")
	require.Equal(t, uint64(1), session.windowN)
	require.Equal(t, session.thread+":1", session.windowID, "跨窗口 ⇒ windowID 递增")
	require.Greater(t, session.nextAt, firstNextAt, "阈值随窗口推进")
	require.Equal(t, firstNextAt+windowSpanOf(session.thread, 1, firstNextAt), session.nextAt)
}

// windowSpanOf 用 SDK 的公开纯值类型复算 (thread, windowIndex) 的 span：造一个
// "刚好跨过本窗口阈值"的状态，Step 一步读到阈值增量（独立于实现内部）。
func windowSpanOf(threadID string, windowIndex, nextAt uint64) uint64 {
	cur := codexsdk.IdentityState{
		ThreadID: threadID, Turns: nextAt - 1, WindowN: windowIndex - 1,
		NextWindowAt: nextAt, WMax: 0,
	}
	next := codexsdk.Step(cur, codexsdk.RotatePolicy{})
	return next.NextWindowAt - nextAt
}

// TestSessionRetireClearsTurnStateAndReportsOldThreadKey 退休（§3.2）：
// ① 旧 threadKey 被报出（调用方锁外退役连接）② ts 清空 ③ 新线程 + 计数归零 +
// windowID/threadKey 重算 + wmax 重抽。
func TestSessionRetireClearsTurnStateAndReportsOldThreadKey(t *testing.T) {
	spy := &retireSpy{}
	pool := newTestPoolWith(1, codexsdk.RotatePolicy{WMaxLo: 1, WMaxHi: 1}, spy.fn)
	session := pool.claim()
	require.NotNil(t, session)
	oldThread, oldKey := session.thread, session.threadKey
	session.SetTurnState("T-old")

	sel := &Selection{session: session, pool: pool}
	retiredAt := 0
	for i := 1; i <= 96; i++ {
		sel.StepIdentity()
		if session.thread != oldThread {
			retiredAt = i
			break
		}
	}
	require.GreaterOrEqual(t, retiredAt, 48, "48 轮前不退休（span≥48）")
	require.LessOrEqual(t, retiredAt, 96, "96 轮内必退休（span≤96）")

	require.Equal(t, []string{fmt.Sprintf("7|%s", oldKey)}, spy.snapshot(), "退休报出**旧** threadKey（锁外退役入口）")
	require.Empty(t, session.turnState, "退休清 ts（M3：不清就带旧 ts 进新线程）")
	require.NotEqual(t, oldThread, session.thread, "退休换新线程")
	require.Equal(t, uint64(0), session.turns, "退休归零轮数")
	require.Equal(t, uint64(0), session.windowN, "退休归零窗口")
	require.Equal(t, session.thread+":0", session.windowID, "windowID 重算")
	require.Equal(t, threadKeyOf("inst", session.thread), session.threadKey, "threadKey 重算")
	require.Equal(t, uint64(1), session.wmax, "wmax 按策略重抽（WMax=1 固定）")
	require.Equal(t, session.thread+":0", sel.IdentityFor([16]byte{}, true).Session.WindowID, "下一个请求用新线程")

	// 退休后旧 threadKey 永不复用（UUIDv7 新线程）；新会话首请求不带 ts。
	require.NotEqual(t, oldKey, session.threadKey)
	require.Empty(t, sel.IdentityFor([16]byte{}, true).TurnState)
}

// TestResizeMigratesOwnedSessions 容量变化按位次迁移（沿用线程/窗口计数/ts）；
// 空闲旧会话被缩容丢弃 ⇒ 立即退役旧线程（G10 的空闲侧）。
func TestResizeMigratesOwnedSessions(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(4, testPolicy(), spy.fn)
	threads := make([]string, 4)
	keys := make([]string, 4)
	for i, s := range old.slots {
		threads[i], keys[i] = s.thread, s.threadKey
		old.slots[i].SetTurnState(fmt.Sprintf("T%d", i))
		old.slots[i].turns = uint64(10 + i)
	}

	shrunk := resizeForTest(old, poolSpec{accountID: 7, k: 2, installationID: "inst", policy: testPolicy(), retire: spy.fn})
	require.Len(t, shrunk.slots, 2)
	for i := range 2 {
		require.Equal(t, threads[i], shrunk.slots[i].thread, "保留位次沿用线程")
		require.Equal(t, fmt.Sprintf("T%d", i), shrunk.slots[i].turnState, "保留位次不丢 ts")
		require.Equal(t, uint64(10+i), shrunk.slots[i].turns, "保留位次不丢计数器")
		require.False(t, shrunk.slots[i].busy.Load(), "迁移后空闲（新池尚未发布）")
	}
	require.Equal(t, []string{fmt.Sprintf("7|%s", keys[2]), fmt.Sprintf("7|%s", keys[3])}, spy.snapshot(),
		"被丢弃且预留成功的旧会话立即退役旧线程")

	grown := resizeForTest(shrunk, poolSpec{accountID: 7, k: 4, installationID: "inst", policy: testPolicy(), retire: spy.fn})
	require.Len(t, grown.slots, 4)
	require.Equal(t, threads[0], grown.slots[0].thread)
	require.Equal(t, threads[1], grown.slots[1].thread)
	require.NotEmpty(t, grown.slots[2].thread, "扩容位次开新线程")
	require.NotEmpty(t, grown.slots[3].thread)
	require.NotEqual(t, grown.slots[2].thread, grown.slots[3].thread)
	for i := range 2 {
		require.Equal(t, fmt.Sprintf("T%d", i), grown.slots[i].turnState, "扩容不丢保留位次的 ts")
	}
	for i := 2; i < 4; i++ {
		require.Empty(t, grown.slots[i].turnState, "新会话无 ts（G10：K 2→4 首请求不带 ts）")
		id := (&Selection{session: grown.slots[i], pool: grown}).IdentityFor([16]byte{}, true)
		require.Empty(t, id.TurnState)
		require.Equal(t, grown.slots[i].thread+":0", id.Session.WindowID, "新会话 window 从 0 起")
	}
}

// TestResizeDroppedInFlightSessionNotHandedOver 缩容丢弃的**在飞**会话不做任何
// 交接（§3.2/§5.2-5）：迁移方不读它的字段（那是数据竞争）、不退役它的线程——
// 残留 idle 由 SDK 的 idle TTL 时间有界回收。
func TestResizeDroppedInFlightSessionNotHandedOver(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(4, testPolicy(), spy.fn)
	inFlight := old.slots[3]
	require.True(t, inFlight.busy.CompareAndSwap(false, true), "模拟持者在飞")
	inFlightThread := inFlight.thread

	droppedIdle := old.slots[2].threadKey
	shrunk := resizeForTest(old, poolSpec{accountID: 7, k: 2, installationID: "inst", policy: testPolicy(), retire: spy.fn})
	require.Len(t, shrunk.slots, 2)
	require.Equal(t, []string{"7|" + droppedIdle}, spy.snapshot(),
		"只退役空闲的丢槽；在飞丢槽不做任何交接（不读其状态）")
	require.Equal(t, inFlightThread, inFlight.thread, "持者的会话状态不被迁移方改写")
	require.True(t, inFlight.busy.Load(), "持者仍持有 busy（收尾照常）")
	for _, s := range shrunk.slots {
		require.NotSame(t, inFlight, s, "在飞会话不进入新池")
	}
	// 持者收尾：正常 Step（这里不退休）后释放，不影响新池。
	inFlight.release()
	require.False(t, inFlight.busy.Load())
}

// TestResizeInstallationChangeRebuildsAllSessions installation 变 ⇒ 该账号**全部
// 会话重建**：旧线程逐个退役、ts 全清、新会话首请求不带 ts（G11 的会话面）。
func TestResizeInstallationChangeRebuildsAllSessions(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(3, testPolicy(), spy.fn)
	oldThreads := make([]string, 3)
	oldKeys := make([]string, 3)
	for i, s := range old.slots {
		oldThreads[i], oldKeys[i] = s.thread, s.threadKey
		s.SetTurnState("T-stale")
	}

	np := resizeForTest(old, poolSpec{accountID: 7, k: 3, installationID: "inst-new", policy: testPolicy(), retire: spy.fn})
	require.Len(t, np.slots, 3)
	require.Equal(t, []string{
		"7|" + oldKeys[0], "7|" + oldKeys[1], "7|" + oldKeys[2],
	}, spy.snapshot(), "逐个退役旧线程")
	for i, s := range np.slots {
		require.NotEqual(t, oldThreads[i], s.thread, "全部会话重建（新线程）")
		require.Equal(t, "inst-new", s.installation, "installation 取本次生效值")
		require.Equal(t, threadKeyOf("inst-new", s.thread), s.threadKey, "threadKey 随 installation 重算")
		require.Empty(t, s.turnState, "ts 全清（新 owner 不重放旧 ts）")
		require.Empty(t, (&Selection{session: s, pool: np}).IdentityFor([16]byte{}, true).TurnState)
	}
}

// TestSyncIdentityPoolReusesWhenUnchanged 容量/安装 ID/策略均未变 → 原样复用
// （保留在途 busy 与会话身份）；任一变化才重建。
func TestSyncIdentityPoolReusesWhenUnchanged(t *testing.T) {
	p := testPolicy()
	pool := newTestPool(3, p)
	spec := func(k int, installation string, policy codexsdk.RotatePolicy) poolSpec {
		return poolSpec{accountID: 7, k: k, installationID: installation, policy: policy}
	}
	require.Same(t, pool, syncForTest(pool, spec(3, "inst", p)), "未变复用")
	require.NotSame(t, pool, syncForTest(pool, spec(4, "inst", p)), "容量变化重建")
	require.NotSame(t, pool, syncForTest(pool, spec(3, "other", p)), "安装 ID 变化重建")
	require.NotSame(t, pool, syncForTest(pool, spec(3, "inst", codexsdk.RotatePolicy{WMaxLo: 2, WMaxHi: 4})), "策略变化重建")
}

// TestIdentityPoolClaimReleaseNoCrossSession 认领 K 个会话互不相同（不串会话）、
// 全忙 claim 返回 nil、归还可再认领；复用不换身份。
func TestIdentityPoolClaimReleaseNoCrossSession(t *testing.T) {
	pool := newTestPool(3, testPolicy())
	seen := map[*codexSession]bool{}
	threads := map[string]bool{}
	for range 3 {
		s := pool.claim()
		require.NotNil(t, s)
		require.False(t, seen[s], "会话不得重复认领")
		seen[s] = true
		require.True(t, s.busy.Load())
		threads[s.thread] = true
	}
	require.Len(t, threads, 3, "K 个会话身份互不相同（不串台）")
	require.Nil(t, pool.claim(), "全忙 → nil（兜底在外层）")

	pool.slots[1].release()
	require.False(t, pool.slots[1].busy.Load())
	thread := pool.slots[1].thread
	again := pool.claim()
	require.Same(t, pool.slots[1], again, "唯一空闲会话被认领")
	require.Equal(t, thread, again.thread, "复用（占用）不改身份")
	again.release()
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
	claimed := map[*codexSession]int{}
	success := 0
	for range g {
		done.Go(func() {
			start.Wait() // 屏障：全部就绪后同时认领，最大化 CAS 争用
			s := pool.claim()
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
	require.Nil(t, pool.claim(), "全忙 → nil（兜底在外层）")
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
	pool.reserveHook = func() {
		arrived.Done()
		<-release
	}
	// 放行条件 = 两个 racer 都已到达屏障（通道 + deadline，无 sleep）。
	var closeOnce sync.Once
	go func() {
		arrived.Wait()
		closeOnce.Do(func() { close(release) })
	}()

	results := make(chan *codexSession, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Go(func() { results <- pool.claim() })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("claim 未在 deadline 内返回（屏障未放行）")
	}
	close(results)

	var got []*codexSession
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
	busy := busyPool.claim()
	require.NotNil(t, busy)
	busyIdx := -1
	for i, s := range busyPool.slots {
		if s == busy {
			busyIdx = i
		}
	}
	require.NotEqual(t, -1, busyIdx)
	rebuilt := resizeForTest(busyPool, poolSpec{accountID: 7, k: 2, installationID: "inst", policy: testPolicy()})
	for _, s := range rebuilt.slots {
		require.NotSame(t, busy, s, "在飞会话不迁移（不与在途请求共享身份）")
	}
	require.NotSame(t, busy, rebuilt.slots[busyIdx], "在飞位次新开会话")
	require.Equal(t, busy.thread, busyPool.slots[busyIdx].thread, "持者身份不被改写")
}

// TestIdentityPoolRotatePolicyImmutable 轮换策略是**池的不可变构造参数**：池与会话
// 各持同一份只读副本，池的读取不依赖任何会话——兜底路径因此不需要租借。
func TestIdentityPoolRotatePolicyImmutable(t *testing.T) {
	p := codexsdk.RotatePolicy{WMaxLo: 1, WMaxHi: 3}
	pool := newTestPool(3, p)
	require.Equal(t, p, pool.rotatePolicy(), "池字段 = 构造参数")
	for _, sl := range pool.slots {
		require.Equal(t, p, sl.policy, "会话持有建会话时的只读副本")
	}
	require.Equal(t, codexsdk.RotatePolicy{}, (&identityPool{}).rotatePolicy(),
		"空池也按字段回答（不读 slot）")
}

// TestClaimCodexSessionOnlyCodexAndFallback 仅 codex 凭据认领；全忙兜底临时身份
// 不落池；非 codex / 无池 → nil。
func TestClaimCodexSessionOnlyCodexAndFallback(t *testing.T) {
	pool := newTestPool(2, testPolicy())
	s := &Scheduler{}
	reg := &identityRegistry{pools: map[int64]*identityPool{7: pool}}

	// 非 codex → nil
	session, owner := s.claimCodexSession(reg, 7, credential.TypeAPIKey)
	require.Nil(t, session)
	require.Nil(t, owner)
	// 无池 → nil
	session, owner = s.claimCodexSession(reg, 99, credential.TypeCodexOAuth)
	require.Nil(t, session)
	require.Nil(t, owner)

	// 认领入池会话
	session, owner = s.claimCodexSession(reg, 7, credential.TypeCodexOAuth)
	require.NotNil(t, session)
	require.Same(t, pool, owner, "返回所属池（代际/退役出口）")
	require.True(t, session.busy.Load())

	// 全忙兜底：临时身份不落池、不计 busy、无池。
	s.claimCodexSession(reg, 7, credential.TypeCodexPAT) // 占满第二会话
	ephem, ephPool := s.claimCodexSession(reg, 7, credential.TypeCodexPAT)
	require.NotNil(t, ephem)
	require.False(t, ephem.busy.Load(), "兜底临时身份不落池（不计 busy）")
	require.Nil(t, ephPool, "兜底身份无池（无代际/退役面）")
	for _, sl := range pool.slots {
		require.NotSame(t, ephem, sl)
	}
	require.NotEmpty(t, ephem.thread)
}

// TestSessionCredGenSurvivesPendingPoolPublication 凭据 owner 失效**不依赖池的发布
// 时机**，也不依赖"被租到的那一条会话"（B2 回归，替代原名
// TestClearTurnStatesAdvancesGeneration）：
//
//	① 两个会话都已对齐 owner A 并各自持有 ts=T-old；
//	② 重载建好新池（会话按位次迁移——ts/owner 都是**会话字段**），但尚未发布；
//	③ 发布窗口内发生凭据替换：旧池仍被旧视图持有者使用，**两条会话各租借一次**都会
//	   对齐 owner B；
//	④ 新池发布后：**每个**会话的下一租借都必须是空 ts。
//
// 旧实现（池级 credGen + resize 时 `np.credGen.Store(old.credGen.Load())` 值拷贝）在
// ②③④ 之间丢掉失效：清 ts 推进的是旧池，新池发布后会话看不到刚推进的代际，T-old 被
// 带进新 owner 的上游会话（oracle B1-路径二）。
//
// ③④ 的位次是**判别力**的关键（momus N3）：③ 必须把两条会话都租一遍（这样"只清被
// 租到的那一条"的池级实现会让另一条带着 T-old 存活），④ 必须**逆序**访问（先访问
// ③ 最后清过的那条）——否则池级 owner 在 ④ 的第一次访问时已经"顺手"清了两条，用例
// 对池级失效源仍然是绿的。
func TestSessionCredGenSurvivesPendingPoolPublication(t *testing.T) {
	ownerA, ownerB := [16]byte{0x0a}, [16]byte{0x0b}
	old := newTestPool(2, testPolicy())

	// ① 两条会话各自完成一轮（拿到 T-old）并已对齐 owner A。
	for _, s := range old.slots {
		require.True(t, s.busy.CompareAndSwap(false, true))
		// 请求准备顺序：先对齐 owner（会话刚建，ts 尚空），请求结束后 Sink 才落 ts。
		(&Selection{session: s, pool: old}).IdentityFor(ownerA, true)
		s.SetTurnState("T-old")
		s.release()
	}

	// ② 重载：新池已建（两条会话按位次迁移，不重建），尚未发布。
	next := resizeForTest(old, poolSpec{accountID: 7, k: 3, installationID: "inst", policy: testPolicy()})
	require.Same(t, old.slots[0], next.slots[0], "会话对象跨池迁移（owner/ts 都是会话字段）")
	require.Same(t, old.slots[1], next.slots[1], "会话对象跨池迁移（位次 1）")

	// ③ 发布窗口内的凭据替换：旧池仍可见，**两条会话各租借一次**（每次都要在租借内
	// 先清 ts——失效源是会话字段时两条都会被清；克隆到池上时只会清被租到的那一条）。
	for i := range 2 {
		stale := old.claim()
		require.NotNil(t, stale, "①已全部归还 ⇒ 窗口内两次租借都应成功（%d）", i)
		(&Selection{session: stale, pool: old}).IdentityFor(ownerB, true)
		require.Empty(t, stale.turnState, "凭据替换 ⇒ 租借内先清 ts（位次 %d）", i)
		stale.release()
	}

	// ④ 新池发布后到来的请求（同一份凭据 = owner B）：**每条**会话都不得再带回 T-old。
	// 逆序访问：先走被 ③ 最后清过的那条，让「池级失效源」在第二次访问时已无清 ts 动作。
	for i, held := range []*codexSession{next.slots[1], next.slots[0]} {
		require.True(t, held.busy.CompareAndSwap(false, true))
		sel := &Selection{session: held, pool: next}
		sel.IdentityFor(ownerB, true)
		require.Empty(t, sel.IdentityFor([16]byte{}, true).TurnState,
			"逆序第 %d 条：失效必须跨「新池已建、尚未发布」窗口存活，且覆盖该账号全部会话", i)
		held.release()
	}
}

// TestResizeWithoutOwnerChangeKeepsTurnState 正控（momus N2）：owner 没有变化的重载
// **不得过度清理**——resize 后的首次租借必须保留 ts。
//
// 这一条是 pending owner 那一类变异的对偶：把 owner 搬回池上、让 resize 重建失效源，
// 会让"每次重载都清 ts"，而只看"凭据替换后必须清"的用例抓不到这种过度清理。
func TestResizeWithoutOwnerChangeKeepsTurnState(t *testing.T) {
	owner := [16]byte{0x0c}
	old := newTestPool(1, testPolicy())
	held := old.slots[0]
	require.True(t, held.busy.CompareAndSwap(false, true))
	(&Selection{session: held, pool: old}).IdentityFor(owner, true)
	held.SetTurnState("T-keep")
	held.release()

	next := resizeForTest(old, poolSpec{accountID: 7, k: 2, installationID: "inst", policy: testPolicy()})
	require.Same(t, held, next.slots[0], "会话对象跨池迁移（同一 owner 的会话身份不变）")

	require.True(t, held.busy.CompareAndSwap(false, true))
	sel := &Selection{session: held, pool: next}
	sel.IdentityFor(owner, true)
	require.Equal(t, "T-keep", sel.IdentityFor([16]byte{}, true).TurnState,
		"owner 未变 ⇒ 重载后的首次租借保留 ts（不允许无差别清空）")
	held.release()
}

// TestReleaseFreesSessionBeforeConcurrencyDecrement 释放窗口内 claim 恒成功
// （P0-4）：旧序（先减并发计数、后归还会话）下新预留可放行而 K 会话全忙 →
// claim()==nil → 兜底临时身份（不落池）。
func TestReleaseFreesSessionBeforeConcurrencyDecrement(t *testing.T) {
	const k = 2
	a := codexAcc(1, domain.FormatOpenAIResponses, "m", k, "inst-1")
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {a}}))
	route := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})

	pool := s.View().static.identityPools.pools[1]
	require.NotNil(t, pool)
	inPool := make(map[*codexSession]bool, k)
	for _, sl := range pool.slots {
		inPool[sl] = true
	}

	var ephemeral, successes int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 2000 {
				sel, err := s.Select(10, domain.FormatOpenAIResponses, "m")
				if err != nil {
					continue // 门禁满（预期 ErrNoAvailable）
				}
				atomic.AddInt64(&successes, 1)
				if sel.session == nil || !inPool[sel.session] {
					atomic.AddInt64(&ephemeral, 1)
				}
				sel.Release()
			}
		})
	}
	wg.Wait()
	require.Positive(t, atomic.LoadInt64(&successes), "并发至少成功预留一次（否则用例空转）")
	require.Zero(t, atomic.LoadInt64(&ephemeral), "释放窗口内 claim 不得落池外临时身份")
}

func codexExt(installationID string) *domain.AccountExt {
	return &domain.AccountExt{CodexIdentity: &domain.CodexIdentity{InstallationID: installationID}}
}

// codexAcc 构造带 codex 凭据模板 + ext 的账号（池建池需要 tpl.CredentialType
// 为 codex 且 Ext 非 nil）。
func codexAcc(id int64, format domain.RequestFormat, model string, maxConc int, installationID string) *domain.Account {
	t := tpl(id, format, []string{model})
	t.CredentialType = credential.TypeCodexOAuth
	a := acc(id, t, maxConc)
	a.Ext = codexExt(installationID)
	return a
}

// TestInvalidateGroupMaintainsIdentityPool 组级定向重载按新 byID 维护会话池，
// 与门禁上限同发布点（P0-3）：调高 max_concurrency 后池 K 与静态叶一致；新增
// 账号入池、删除账号池随之 GC——杜绝「门禁上限已变、池仍旧」。
func TestInvalidateGroupMaintainsIdentityPool(t *testing.T) {
	a1 := codexAcc(1, domain.FormatOpenAIResponses, "gpt-5", 2, "inst-1")
	byGroup := map[int64][]*domain.Account{10: {a1}}
	m := newMemLoader(byGroup)
	s := newSched(t, m)

	require.Len(t, s.View().static.identityPools.pools[1].slots, 2, "初始池 K = max_concurrency=2")

	// 管理面调高 max_concurrency（账号变更生产路由 = InvalidateGroup）→ 池同源扩容。
	m.mu.Lock()
	m.byGroup[10][0].MaxConcurrency = 5
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()

	v := s.View()
	require.Equal(t, 5, v.static.byID[1].static.Load().acc.MaxConcurrency, "门禁上限更新")
	require.Len(t, v.static.identityPools.pools[1].slots, 5, "池 K 与门禁同发布点更新")

	// 新增 codex 账号（组内）→ 立即有池（此前缺失 → 不注入身份）。
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], codexAcc(2, domain.FormatOpenAIResponses, "gpt-5", 3, "inst-2"))
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()
	require.Len(t, s.View().static.identityPools.pools[2].slots, 3, "新增账号经 InvalidateGroup 即建池")

	// 删除账号 → 池随注册表替换 GC（有界泄漏修复）。
	m.mu.Lock()
	m.byGroup[10] = m.byGroup[10][:1]
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()
	require.NotContains(t, s.View().static.identityPools.pools, int64(2), "删除账号池被回收")
}

// TestIdentityPoolCapacityClamped codex 账号的 max_concurrency 上界在装载期钳制：
// 池 K 与门禁上限同源取钳后值，reload 锁内 UUID 分配有界；非 codex 账号不受
// 上界影响（不建池、门禁照旧）。
func TestIdentityPoolCapacityClamped(t *testing.T) {
	huge := maxIdentityPoolSlots + 5000
	codex := codexAcc(1, domain.FormatOpenAIResponses, "gpt-5", huge, "inst-1")
	plain := acc(2, tpl(2, domain.FormatOpenAIChat, []string{"gpt-4o"}), huge) // api_key：无池
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {codex, plain}}))

	v := s.View()
	require.Len(t, v.static.identityPools.pools[1].slots, maxIdentityPoolSlots, "codex 池 K 钳到上界")
	require.Equal(t, maxIdentityPoolSlots, v.static.byID[1].static.Load().acc.MaxConcurrency, "门禁上限同源钳制")
	require.Nil(t, v.static.identityPools.pools[2], "非 codex 账号不建池")
	require.Equal(t, huge, v.static.byID[2].static.Load().acc.MaxConcurrency, "非 codex 账号门禁不受上界影响")
}

// TestReloadPublishesPoolWithView 全量 reload 的池挂在与门禁同读的静态根上：
// 同一 RoutingView.static 既给门禁 limit（byID 叶）又给池 K（identityPools）。
func TestReloadPublishesPoolWithView(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {codexAcc(1, domain.FormatOpenAIResponses, "gpt-5", 4, "inst-1")}})
	s := newSched(t, m)
	v := s.View()
	require.NotNil(t, v.static.identityPools, "池随静态根发布（非独立发布点）")
	require.Len(t, v.static.identityPools.pools[1].slots, 4)
	require.Same(t, v.static, s.View().static, "同一静态根")
}

// TestClaimAndIdentityZeroAlloc §4 预算：租借 + Identity 构造稳态 0 allocs
// （windowID/threadKey 预计算在会话上；Sink 是指针接口不装箱）。
func TestClaimAndIdentityZeroAlloc(t *testing.T) {
	pool := newTestPool(1, testPolicy())
	var sink codexsdk.TurnStateSink
	var window string
	allocs := testing.AllocsPerRun(1000, func() {
		session := pool.claim()
		if session == nil {
			t.Fatal("单槽空闲时 claim 必须成功")
		}
		id := session.identity()
		sink, window = id.Sink, id.Session.WindowID // 保留引用，防被优化掉
		session.release()
	})
	require.Zero(t, allocs, "租借 + 身份构造不得分配（allocs=%v）", allocs)
	require.NotNil(t, sink)
	require.NotEmpty(t, window)
}
