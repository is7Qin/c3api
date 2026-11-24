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

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// testPolicy 无退休（WMaxHi 0）——池机制测试用（身份演化另测）。
// sessionThreadKey 会话当前线程键（不解析：只用于"是否换了线程"的比较）。
func sessionThreadKey(s *codexSession) string { return s.threads.Key(s.h) }

func testPolicy() sdkbridge.RotatePolicy {
	return sdkbridge.RotatePolicy{}
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
func newTestPool(k int, p sdkbridge.RotatePolicy) *identityPool {
	return newTestPoolWith(k, p, nil)
}

func newTestPoolWith(k int, p sdkbridge.RotatePolicy, retire func(int64, string)) *identityPool {
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

// TestSessionRetireReportsOldThreadKey 退休（§3.2）：
// ① 旧 threadKey 被报出（调用方锁外退役连接）② 新线程 + 计数归零 +
// windowID/threadKey/context_window_id 重算 + wmax 重抽。
func TestSessionRetireReportsOldThreadKey(t *testing.T) {
	spy := &retireSpy{}
	pool := newTestPoolWith(1, sdkbridge.RotatePolicy{WMaxLo: 1, WMaxHi: 1}, spy.fn)
	session := pool.claim()
	require.NotNil(t, session)
	oldKey := sessionThreadKey(session)

	sel := &selection{session: session, pool: pool}
	retiredAt := 0
	for i := 1; i <= 96; i++ {
		sel.StepIdentity()
		if sessionThreadKey(session) != oldKey {
			retiredAt = i
			break
		}
	}
	require.GreaterOrEqual(t, retiredAt, 48, "48 轮前不退休（span≥48）")
	require.LessOrEqual(t, retiredAt, 96, "96 轮内必退休（span≤96）")

	require.Equal(t, []string{fmt.Sprintf("7|%s", oldKey)}, spy.snapshot(), "退休报出**旧** threadKey（锁外退役入口）")
	require.NotEqual(t, oldKey, sessionThreadKey(session), "退休换新线程")
	require.Equal(t, sessionThreadKey(session), session.threadKey, "池缓存的线程键与句柄一致")
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

	shrunk := resizeForTest(old, poolSpec{accountID: 7, k: 2, installationID: "inst", policy: testPolicy(), retire: spy.fn})
	require.Len(t, shrunk.slots, 2)
	for i := range 2 {
		require.Equal(t, threads[i], sessionThreadKey(shrunk.slots[i]), "保留位次沿用线程")
		require.False(t, shrunk.slots[i].busy.Load(), "迁移后空闲（新池尚未发布）")
	}
	require.Equal(t, []string{fmt.Sprintf("7|%s", keys[2]), fmt.Sprintf("7|%s", keys[3])}, spy.snapshot(),
		"被丢弃且预留成功的旧会话立即退役旧线程")

	grown := resizeForTest(shrunk, poolSpec{accountID: 7, k: 4, installationID: "inst", policy: testPolicy(), retire: spy.fn})
	require.Len(t, grown.slots, 4)
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
// `*codexSession`——同一个句柄、同一份协议状态。剩下只需再验一句"迁移后状态机还活着"。
func TestResizeSamePolicyKeepsThreadProgress(t *testing.T) {
	policy := sdkbridge.RotatePolicy{WMaxLo: 2, WMaxHi: 2} // 每线程活 2 个窗口即退休

	pool := newTestPool(1, policy)
	session := pool.claim()
	require.NotNil(t, session)
	key := sessionThreadKey(session)

	const walked = 7
	for i := 0; i < walked; i++ {
		require.Empty(t, session.step(), "前 %d 步不该退休", walked)
	}
	session.release() // 归还后 resize：空闲会话才按位次迁移

	np := resizeForTest(pool, poolSpec{accountID: 7, k: 1, installationID: "inst", policy: policy})
	require.Same(t, session, np.slots[0], "同策略 resize 迁移会话指针（协议状态随对象走，不重建）")
	require.Equal(t, key, sessionThreadKey(np.slots[0]), "迁移后仍是同一条线程")

	// 状态机迁移后仍然活着：能继续推进并在有界步数内退休。
	migrated := np.slots[0]
	retired := false
	for i := 1; i <= 500; i++ {
		if migrated.step() != "" {
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
	shrunk := resizeForTest(old, poolSpec{accountID: 7, k: 2, installationID: "inst", policy: testPolicy(), retire: spy.fn})
	require.Len(t, shrunk.slots, 2)
	require.Equal(t, []string{"7|" + droppedIdle}, spy.snapshot(),
		"只退役空闲的丢槽；在飞丢槽不做任何交接（不读其状态）")
	require.Equal(t, inFlightThread, sessionThreadKey(inFlight), "持者的会话状态不被迁移方改写")
	require.True(t, inFlight.busy.Load(), "持者仍持有 busy（收尾照常）")
	for _, s := range shrunk.slots {
		require.NotSame(t, inFlight, s, "在飞会话不进入新池")
	}
	// 持者收尾：正常 Step（这里不退休）后释放，不影响新池。
	inFlight.release()
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

	np := resizeForTest(old, poolSpec{accountID: 7, k: 3, installationID: "inst-new", policy: testPolicy(), retire: spy.fn})
	require.Len(t, np.slots, 3)
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
	spec := func(k int, installation string, policy sdkbridge.RotatePolicy) poolSpec {
		return poolSpec{accountID: 7, k: k, installationID: installation, policy: policy}
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
	seen := map[*codexSession]bool{}
	threads := map[string]bool{}
	for range 3 {
		s := pool.claim()
		require.NotNil(t, s)
		require.False(t, seen[s], "会话不得重复认领")
		seen[s] = true
		require.True(t, s.busy.Load())
		threads[sessionThreadKey(s)] = true
	}
	require.Len(t, threads, 3, "K 个会话身份互不相同（不串台）")
	require.Nil(t, pool.claim(), "全忙 → nil（兜底在外层）")

	pool.slots[1].release()
	require.False(t, pool.slots[1].busy.Load())
	thread := sessionThreadKey(pool.slots[1])
	again := pool.claim()
	require.Same(t, pool.slots[1], again, "唯一空闲会话被认领")
	require.Equal(t, thread, sessionThreadKey(again), "复用（占用）不改身份")
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
	require.Equal(t, sessionThreadKey(busy), sessionThreadKey(busyPool.slots[busyIdx]), "持者身份不被改写")
}

// TestIdentityPoolRotatePolicyImmutable 轮换策略是**池的不可变构造参数**：池与会话
// 各持同一份只读副本，池的读取不依赖任何会话——兜底路径因此不需要租借。
func TestIdentityPoolRotatePolicyImmutable(t *testing.T) {
	p := sdkbridge.RotatePolicy{WMaxLo: 1, WMaxHi: 3}
	pool := newTestPool(3, p)
	require.Equal(t, p, pool.rotatePolicy(), "池字段 = 构造参数")
	for _, sl := range pool.slots {
		require.Equal(t, p, sl.threads.Policy(), "会话持有建会话时的只读副本")
	}
	require.Equal(t, sdkbridge.RotatePolicy{}, (&identityPool{}).rotatePolicy(),
		"空池也按字段回答（不读 slot）")
}

// TestClaimCodexSessionOnlyCodexAndFallback 仅 codex **通道**认领（入参是预留装配点
// 算好的 credential.Channel，不再是凭据类型谓词）；全忙兜底临时身份不落池；
// 非 codex 通道 / 无池 → nil。
func TestClaimCodexSessionOnlyCodexAndFallback(t *testing.T) {
	pool := newTestPool(2, testPolicy())
	s := &Scheduler{}
	reg := &identityRegistry{pools: map[int64]*identityPool{7: pool}}

	// 非 codex 通道 → nil（BPS 通道同样不认领：它没有 codex 会话池）
	session, owner := s.claimCodexSession(reg, 7, credential.ChannelStaticKey)
	require.Nil(t, session)
	require.Nil(t, owner)
	// 无池 → nil
	session, owner = s.claimCodexSession(reg, 99, credential.ChannelCodex)
	require.Nil(t, session)
	require.Nil(t, owner)

	// 认领入池会话
	session, owner = s.claimCodexSession(reg, 7, credential.ChannelCodex)
	require.NotNil(t, session)
	require.Same(t, pool, owner, "返回所属池（代际/退役出口）")
	require.True(t, session.busy.Load())

	// 全忙兜底：临时身份不落池、不计 busy、无池。
	s.claimCodexSession(reg, 7, credential.ChannelCodex) // 占满第二会话
	ephem, ephPool := s.claimCodexSession(reg, 7, credential.ChannelCodex)
	require.NotNil(t, ephem)
	require.False(t, ephem.busy.Load(), "兜底临时身份不落池（不计 busy）")
	require.Nil(t, ephPool, "兜底身份无池（无代际/退役面）")
	for _, sl := range pool.slots {
		require.NotSame(t, ephem, sl)
	}
	require.NotEmpty(t, sessionThreadKey(ephem))
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
				if h := sel.Handle.(*selection); h.session == nil || !inPool[h.session] {
					atomic.AddInt64(&ephemeral, 1)
				}
				sel.Handle.Release()
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
	// BPS 是**独立 provider**：它有 account_ext 与身份面（很容易被误当成 codex 族），
	// 但没有 codex 会话池，因此 K 的上界（= 会话池容量）不适用于它。
	bps := bpsAccWith(3, huge, &domain.AccountExt{
		AccountID: 3, CredentialType: credential.TypeBPS,
		BPSProfile: []byte(`{"profile_id":"p"}`),
	})
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {codex, plain, bps}}))

	v := s.View()
	require.Len(t, v.static.identityPools.pools[1].slots, maxIdentityPoolSlots, "codex 池 K 钳到上界")
	require.Equal(t, maxIdentityPoolSlots, v.static.byID[1].static.Load().acc.MaxConcurrency, "门禁上限同源钳制")
	require.Nil(t, v.static.identityPools.pools[2], "非 codex 账号不建池")
	require.Equal(t, huge, v.static.byID[2].static.Load().acc.MaxConcurrency, "非 codex 账号门禁不受上界影响")
	require.Nil(t, v.static.identityPools.pools[3], "BPS 不建 codex 会话池")
	require.Equal(t, huge, v.static.byID[3].static.Load().acc.MaxConcurrency,
		"BPS 的 max_concurrency 不得被钳到 maxIdentityPoolSlots（它没有会话池，钳它等于凭空砍并发）")
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

// TestClaimAndThreadZeroAlloc §4 预算：认领 + 取线程句柄必须稳态 0 allocs。
// 句柄是值类型（两个机器字），Key 直接返回预计算字符串——热路径不做拼接、不分配。
func TestClaimAndThreadZeroAlloc(t *testing.T) {
	pool := newTestPool(1, testPolicy())
	sel := &selection{pool: pool}
	var key string
	allocs := testing.AllocsPerRun(1000, func() {
		session := pool.claim()
		if session == nil {
			t.Fatal("独占空档时 claim 必须成功")
		}
		sel.session = session
		key = session.threads.Key(sel.Thread())
		session.release()
	})
	require.Zero(t, allocs, "认领 + 取句柄不得分配（allocs=%v）", allocs)
	require.NotEmpty(t, key, "线程键由池预计算，取用零分配")
}
