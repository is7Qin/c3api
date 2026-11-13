// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 轮换策略的**不可变构造参数**契约（spec §3.3b-3）：resize 只迁移会话指针，不写
// 已发布会话；策略真的变了就开新会话并回收旧线程。三条用例分别固定：迁移、重建、
// 以及「resize 期间策略只读」的并发判别（`-race` 下旧实现会在写 os.policy 处现红）。

import (
	"runtime"
	"sync"
	"testing"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
)

var (
	policyA = codexsdk.RotatePolicy{WMaxLo: 1, WMaxHi: 3}
	policyB = codexsdk.RotatePolicy{WMaxLo: 4, WMaxHi: 9}
)

// TestResizeSamePolicyMigratesSessionPointers 容量变化（策略不变）只迁移会话指针：
// 线程/窗口计数全部随对象走，池不复制任何会话状态。
func TestResizeSamePolicyMigratesSessionPointers(t *testing.T) {
	old := newTestPool(2, policyA)
	old.slots[0].st.Turns = 5
	threads := []string{old.slots[0].st.ThreadID, old.slots[1].st.ThreadID}

	grown := resizeForTest(old, poolSpec{accountID: 7, k: 4, installationID: "inst", policy: policyA})
	require.Len(t, grown.slots, 4)
	for i := range 2 {
		require.Same(t, old.slots[i], grown.slots[i], "同一策略：按位次迁移会话指针")
		require.Equal(t, threads[i], grown.slots[i].st.ThreadID)
		require.Equal(t, policyA, grown.slots[i].policy)
	}
	require.Equal(t, uint64(5), grown.slots[0].st.Turns, "会话状态随对象迁移")
	for i := 2; i < 4; i++ {
		require.NotSame(t, old.slots[0], grown.slots[i], "扩容位次是新会话")
		require.Equal(t, policyA, grown.slots[i].policy)
	}

	shrunk := resizeForTest(grown, poolSpec{accountID: 7, k: 1, installationID: "inst", policy: policyA})
	require.Same(t, grown.slots[0], shrunk.slots[0], "缩容保留的位次仍迁移同一指针")
}

// TestResizePolicyChangeRebuildsSessions 策略变化 = 换会话：旧会话的线程不再属于
// 新配置（窗口/退休口径全变），必须退役旧线程并开新会话。旧池/旧会话的策略字段
// 保持原值——持旧视图的请求读到的仍是它建会话时的策略。
func TestResizePolicyChangeRebuildsSessions(t *testing.T) {
	spy := &retireSpy{}
	old := newTestPoolWith(2, policyA, spy.fn)

	next := resizeForTest(old, poolSpec{accountID: 7, k: 2, installationID: "inst", policy: policyB, retire: spy.fn})
	require.Len(t, next.slots, 2)
	for i := range 2 {
		require.NotSame(t, old.slots[i], next.slots[i], "策略变化 ⇒ 新会话")
		require.Equal(t, policyB, next.slots[i].policy, "新会话按新策略构造")
		require.Equal(t, policyA, old.slots[i].policy, "已发布池/会话的策略不被 resize 改写")
		require.Equal(t, policyA, old.rotatePolicy())
	}
	require.Equal(t, policyB, next.rotatePolicy())
	require.Len(t, spy.snapshot(), 2, "旧线程在锁外退役（每会话一条）")
}

// TestResizePolicyReadDuringResize resize 期间策略只读（§3.3b 第 4 条的并发判别
// 用例）：多个读者在租借之外反复读旧池策略，同时执行策略变更的 resize。策略只在
// 构造期写入 ⇒ `-race` 干净；旧实现（resize 写 os.policy）在这里现红。
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
				if got := old.rotatePolicy(); got != policyA {
					t.Errorf("已发布池的策略被 resize 改写：%+v", got)
					return
				}
				runtime.Gosched()
			}
		}()
	}

	for range 32 {
		next := resizeForTest(old, poolSpec{accountID: 7, k: k, installationID: "inst", policy: policyB})
		require.Equal(t, policyB, next.rotatePolicy())
		require.Equal(t, policyA, old.rotatePolicy(), "旧池策略恒定")
		for _, sl := range old.slots {
			require.Equal(t, policyA, sl.policy, "旧会话策略恒定（resize 不写共享会话）")
		}
	}
	close(stop)
	wg.Wait()
}

// TestFallbackSessionUsesPoolPolicy 全忙兜底的临时身份只读池的不可变字段——不读
// slot 0 的 policy，也不需要任何租借。
func TestFallbackSessionUsesPoolPolicy(t *testing.T) {
	pool := newTestPool(1, policyB)
	s := &Scheduler{}
	reg := &identityRegistry{pools: map[int64]*identityPool{7: pool}}

	busy, owner := s.claimCodexSession(reg, 7, credential.TypeCodexOAuth)
	require.NotNil(t, busy)
	require.Same(t, pool, owner)

	ephem, ephPool := s.claimCodexSession(reg, 7, credential.TypeCodexOAuth)
	require.NotNil(t, ephem)
	require.Nil(t, ephPool, "兜底身份无池")
	require.Equal(t, policyB, ephem.policy, "兜底身份的策略取自池字段")
	require.Equal(t, "inst", ephem.st.InstallationID, "installation 同源")
}
