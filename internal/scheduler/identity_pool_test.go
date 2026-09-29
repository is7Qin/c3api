// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"sync"
	"sync/atomic"
	"testing"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// testPolicy 无退休（WMaxHi 0）+ Total 口径——池机制测试用（身份演化另测）。
func testPolicy() codexsdk.RotatePolicy {
	return codexsdk.RotatePolicy{Scope: codexsdk.ScopeTotal}
}

// newTestPool 构造容量 k 的池（installation "inst"）。
func newTestPool(k int, p codexsdk.RotatePolicy) *identityPool {
	pool := &identityPool{policy: p, installationID: "inst", slots: make([]*identitySlot, k)}
	for i := 0; i < k; i++ {
		pool.slots[i] = newIdentitySlot("inst", p)
	}
	return pool
}

// TestSelectionCodexIdentityFromSlot 伪装身份四元组一次给全（Selection.
// CodexIdentity），且 installation 取自槽 state（单一来源）——与账号 ext 无关。
func TestSelectionCodexIdentityFromSlot(t *testing.T) {
	slot := newIdentitySlot("slot-inst", testPolicy())
	sel := &Selection{
		identitySlot: slot,
		Ext:          codexExt("ext-inst"), // 故意与槽不一致：注入必须取槽
	}
	sess, meta := sel.CodexIdentity()
	st := slot.state.Load()
	require.Equal(t, st.ThreadID, sess.SessionID)
	require.Equal(t, st.ThreadID, sess.ThreadID)
	require.Equal(t, st.WindowID(), sess.WindowID)
	require.Equal(t, "slot-inst", meta.InstallationID, "installation 取槽 state，非 ext")
	require.Equal(t, sess.SessionID, meta.SessionID)
	require.Equal(t, sess.ThreadID, meta.ThreadID)
	require.Equal(t, sess.WindowID, meta.WindowID)

	// 无槽（非 codex / 池缺席）→ 零值（不注入）。
	empty := &Selection{Ext: codexExt("ext-inst")}
	s2, m2 := empty.CodexIdentity()
	require.Zero(t, s2)
	require.Zero(t, m2)
	var nilSel *Selection
	s3, m3 := nilSel.CodexIdentity()
	require.Zero(t, s3)
	require.Zero(t, m3)
}

// TestResizeUpdatesInstallationOnMigration resize（容量/安装 ID 变化）迁移按位次
// 复用旧槽 thread/window，但 installation 更新为本次生效值——不再留旧值（P1-3）。
func TestResizeUpdatesInstallationOnMigration(t *testing.T) {
	old := newTestPool(2, testPolicy()) // installation "inst"
	np := resizeIdentityPool(old, 2, "inst-new", testPolicy())
	require.Len(t, np.slots, 2)
	for i, s := range np.slots {
		require.Equal(t, "inst-new", s.state.Load().InstallationID, "迁移槽 installation 取新值")
		require.Equal(t, old.slots[i].state.Load().ThreadID, s.state.Load().ThreadID, "thread 沿用（身份演化不丢）")
	}
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

// TestInvalidateGroupMaintainsIdentityPool 组级定向重载按新 byID 维护槽位池，
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

// TestReleaseFreesSlotBeforeConcurrencyDecrement 释放窗口内 claim 恒成功（P0-4）：
// 旧序（先减并发计数、后归还槽）下新预留可放行而 K 槽全忙 → claim()==nil → 兜底
// 临时身份（不落池）。并发 Select/Release 断言任一成功预留都拿到**池内**槽。
func TestReleaseFreesSlotBeforeConcurrencyDecrement(t *testing.T) {
	const k = 2
	a := codexAcc(1, domain.FormatOpenAIResponses, "m", k, "inst-1")
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {a}}))
	route := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})

	pool := s.View().static.identityPools.pools[1]
	require.NotNil(t, pool)
	inPool := make(map[*identitySlot]bool, k)
	for _, sl := range pool.slots {
		inPool[sl] = true
	}

	var ephemeral, successes int64
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				sel, err := s.Select(10, domain.FormatOpenAIResponses, "m")
				if err != nil {
					continue // 门禁满（预期 ErrNoAvailable）
				}
				atomic.AddInt64(&successes, 1)
				if sel.identitySlot == nil || !inPool[sel.identitySlot] {
					atomic.AddInt64(&ephemeral, 1)
				}
				sel.Release()
			}
		}()
	}
	wg.Wait()
	require.Positive(t, atomic.LoadInt64(&successes), "并发至少成功预留一次（否则用例空转）")
	require.Zero(t, atomic.LoadInt64(&ephemeral), "释放窗口内 claim 不得落池外临时身份")
}

// TestIdentityPoolClaimReleaseNoCrossSlot 认领 K 槽互不相同（不串槽）、全忙
// claim 返回 nil、归还可再认领。
func TestIdentityPoolClaimReleaseNoCrossSlot(t *testing.T) {
	pool := newTestPool(3, testPolicy())
	seen := map[*identitySlot]bool{}
	threads := map[string]bool{}
	for i := 0; i < 3; i++ {
		s := pool.claim()
		require.NotNil(t, s)
		require.False(t, seen[s], "槽不得重复认领")
		seen[s] = true
		require.True(t, s.busy.Load())
		threads[s.state.Load().ThreadID] = true
	}
	require.Len(t, threads, 3, "K 槽身份互不相同（不串槽）")
	require.Nil(t, pool.claim(), "全忙 → nil（兜底在外层）")

	pool.slots[1].release()
	require.False(t, pool.slots[1].busy.Load())
	require.Same(t, pool.slots[1], pool.claim(), "唯一空闲槽被认领")
}

// TestIdentityPoolReuseKeepsIdentity 复用（占用）不改身份：同一槽释放后再认领，
// thread/window 不变。
func TestIdentityPoolReuseKeepsIdentity(t *testing.T) {
	pool := newTestPool(2, testPolicy())
	slot := pool.claim()
	require.NotNil(t, slot)
	thread := slot.state.Load().ThreadID
	slot.release()
	// K=2，轮转一圈内必再得同一槽对象。
	var again *identitySlot
	for i := 0; i < len(pool.slots); i++ {
		c := pool.claim()
		if c == slot {
			again = c
			break
		}
		c.release()
	}
	require.Same(t, slot, again)
	require.Equal(t, thread, again.state.Load().ThreadID, "复用不换身份")
	again.release()
}

// TestAdvanceIdentityOnlyStepsAtThreshold 水位未跨 θ_w 不推进；跨过且 WMax 未达
// 不换线程（WindowN++）；达 WMax 退休换新线程。
func TestAdvanceIdentityOnlyStepsAtThreshold(t *testing.T) {
	slug := "unknown-slug" // 目录外 → fallback θ_w
	theta := codexsdk.AutoCompactTokens(slug)
	require.Positive(t, theta)

	// 无退休（WMax 0）：θ 下不推进，θ 上升沿 WindowN++，持续高位不重复计数。
	noRetire := codexsdk.RotatePolicy{Scope: codexsdk.ScopeTotal}
	slot := newIdentitySlot("inst", noRetire)
	sel := &Selection{identitySlot: slot, Model: slug}
	thread := slot.state.Load().ThreadID

	sel.AdvanceIdentity(theta - 1)
	require.Equal(t, uint64(0), slot.state.Load().WindowN, "低于 θ_w 不推进")
	sel.AdvanceIdentity(theta)
	require.Equal(t, uint64(1), slot.state.Load().WindowN, "跨 θ_w 上升沿 WindowN++")
	sel.AdvanceIdentity(theta)
	require.Equal(t, uint64(1), slot.state.Load().WindowN, "持续高位不重复计数")
	sel.AdvanceIdentity(theta - 1)
	sel.AdvanceIdentity(theta)
	require.Equal(t, uint64(2), slot.state.Load().WindowN, "回落再跨 → 新上升沿")
	require.Equal(t, thread, slot.state.Load().ThreadID, "无退休不换线程")

	// WMax=1：WindowN 达上限 → 退休换新线程。
	retire := codexsdk.RotatePolicy{WMaxLo: 1, WMaxHi: 1, Scope: codexsdk.ScopeTotal}
	rslot := newIdentitySlot("inst", retire)
	rsel := &Selection{identitySlot: rslot, Model: slug}
	oldThread := rslot.state.Load().ThreadID
	rsel.AdvanceIdentity(theta)
	after := rslot.state.Load()
	require.Equal(t, uint64(0), after.WindowN, "退休换新线程后 WindowN 归零")
	require.NotEqual(t, oldThread, after.ThreadID, "WindowN ≥ WMax → 换 thread")
}

// TestIdentityPoolResizeMigratesMin 容量变化按位次迁移旧槽身份至 min(old,new)；
// 扩容新位次开新线程；busy 旧槽不迁移（避免与在途请求共享身份）。
func TestIdentityPoolResizeMigratesMin(t *testing.T) {
	old := newTestPool(4, testPolicy())
	oldThreads := make([]string, 4)
	for i, s := range old.slots {
		oldThreads[i] = s.state.Load().ThreadID
	}

	shrunk := resizeIdentityPool(old, 2, "inst", testPolicy())
	require.Len(t, shrunk.slots, 2)
	require.Equal(t, oldThreads[0], shrunk.slots[0].state.Load().ThreadID)
	require.Equal(t, oldThreads[1], shrunk.slots[1].state.Load().ThreadID)

	grown := resizeIdentityPool(shrunk, 4, "inst", testPolicy())
	require.Len(t, grown.slots, 4)
	require.Equal(t, oldThreads[0], grown.slots[0].state.Load().ThreadID)
	require.Equal(t, oldThreads[1], grown.slots[1].state.Load().ThreadID)
	require.NotEmpty(t, grown.slots[2].state.Load().ThreadID)
	require.NotEmpty(t, grown.slots[3].state.Load().ThreadID)

	// busy 槽不迁移：认领 slots[0] 后同容量重建 → 新 pools[0] 身份不等同旧 busy 槽。
	busyPool := newTestPool(2, testPolicy())
	busy := busyPool.claim()
	var got *identitySlot
	for i := 0; i < 2; i++ {
		if busyPool.slots[i] == busy {
			got = busyPool.slots[i]
		}
	}
	require.NotNil(t, got)
	rebuilt := resizeIdentityPool(busyPool, 2, "inst", testPolicy())
	require.NotEqual(t, busy.state.Load().ThreadID, rebuilt.slots[0].state.Load().ThreadID)
}

// TestIdentityPoolForReusesWhenUnchanged 容量/安装 ID/策略均未变 → 原样复用
// （保留在途 busy 与槽身份）；任一变化才重建。
func TestIdentityPoolForReusesWhenUnchanged(t *testing.T) {
	p := testPolicy()
	pool := newTestPool(3, p)
	require.Same(t, pool, identityPoolFor(pool, 3, "inst", p), "未变复用")
	require.NotSame(t, pool, identityPoolFor(pool, 4, "inst", p), "容量变化重建")
	require.NotSame(t, pool, identityPoolFor(pool, 3, "other", p), "安装 ID 变化重建")
	require.NotSame(t, pool, identityPoolFor(pool, 3, "inst", codexsdk.RotatePolicy{Scope: codexsdk.ScopeBodyAfterPrefix}), "策略变化重建")
}

// TestIdentityPoolConcurrentClaimUnique 并发认领唯一性：G 个 goroutine 同时
// claim（WaitGroup 屏障对齐起跑），成功者互不相同、成功数恰为 K、全忙后 nil。
func TestIdentityPoolConcurrentClaimUnique(t *testing.T) {
	const k = 8
	pool := newTestPool(k, testPolicy())
	const g = 64

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	var mu sync.Mutex
	claimed := map[*identitySlot]int{}
	success := 0
	for i := 0; i < g; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // 屏障：全部就绪后同时认领，最大化 CAS 争用
			s := pool.claim()
			if s == nil {
				return
			}
			mu.Lock()
			claimed[s]++
			success++
			mu.Unlock()
		}()
	}
	start.Done()
	done.Wait()

	require.Len(t, claimed, success, "并发认领的成功者互不相同（不串槽）")
	require.Equal(t, k, success, "K 个空槽恰好被全部认领一次")
	for s, n := range claimed {
		require.Equal(t, 1, n, "同一槽不得被重复认领")
		require.True(t, s.busy.Load())
	}
	require.Nil(t, pool.claim(), "全忙 → nil（兜底在外层）")
}

// TestIdentitySlotConcurrentAdvanceNoLostUpdate 同槽并发推进 CAS 面：G 个
// goroutine 并发对同一槽 AdvanceIdentity(θ)，ScopeTotal 上升沿恰计一次
// （CAS 读改写不丢不重）；回落再跨界继续恰计一次（证明 arm 更新也被 CAS 应用）。
func TestIdentitySlotConcurrentAdvanceNoLostUpdate(t *testing.T) {
	slug := "unknown-slug" // 目录外 → fallback θ_w
	theta := codexsdk.AutoCompactTokens(slug)
	require.Positive(t, theta)
	pol := codexsdk.RotatePolicy{Scope: codexsdk.ScopeTotal} // WMaxHi=0 不退休
	slot := newIdentitySlot("inst", pol)
	sel := &Selection{identitySlot: slot, Model: slug}

	const g = 16
	const m = 100
	advanceAll := func() {
		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		for i := 0; i < g; i++ {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				for j := 0; j < m; j++ {
					sel.AdvanceIdentity(theta) // 每步都是「跨 θ_w」
				}
			}()
		}
		start.Done()
		done.Wait()
	}

	advanceAll()
	require.Equal(t, uint64(1), slot.state.Load().WindowN, "并发跨 θ_w 恰计一次上升沿（CAS 不丢不重）")

	sel.AdvanceIdentity(theta - 1) // 回落 → 重新武装
	advanceAll()
	require.Equal(t, uint64(2), slot.state.Load().WindowN, "回落再跨 → 继续恰计一次（arm 更新未被丢）")
}

// TestClaimIdentitySlotOnlyCodexAndFallback 仅 codex 凭据认领；全忙兜底临时身份
// 不落池；非 codex / 无池 → nil。
func TestClaimIdentitySlotOnlyCodexAndFallback(t *testing.T) {
	pool := newTestPool(2, testPolicy())
	s := &Scheduler{}
	reg := &identityRegistry{pools: map[int64]*identityPool{7: pool}}

	// 非 codex → nil
	require.Nil(t, s.claimIdentitySlot(reg, 7, credential.TypeAPIKey))
	// 无池 → nil
	require.Nil(t, s.claimIdentitySlot(reg, 99, credential.TypeCodexOAuth))

	// 认领入池槽
	slot := s.claimIdentitySlot(reg, 7, credential.TypeCodexOAuth)
	require.NotNil(t, slot)
	require.True(t, slot.busy.Load())

	// 全忙兜底：临时身份不落池、不计 busy。
	s.claimIdentitySlot(reg, 7, credential.TypeCodexPAT) // 占满第二槽
	ephem := s.claimIdentitySlot(reg, 7, credential.TypeCodexPAT)
	require.NotNil(t, ephem)
	require.False(t, ephem.busy.Load(), "兜底临时身份不落池（不计 busy）")
	for _, sl := range pool.slots {
		require.NotSame(t, ephem, sl)
	}
	require.NotEmpty(t, ephem.state.Load().ThreadID)
}
