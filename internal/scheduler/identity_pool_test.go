// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 本文件只剩**读调度器/视图**的那 5 个用例（P5b 修正 6：池语义的 16 个用例已搬到
// `internal/identitypool`）。判据不是"搬一半"，而是用例名集合的机械对照：
//
//	搬走 16 名（各出现一次，在 identitypool 包）
//	留 5 名（本文件，且仍绿）：
//	  TestClaimCodexSessionOnlyCodexAndFallback
//	  TestReleaseFreesSessionBeforeConcurrencyDecrement
//	  TestInvalidateGroupMaintainsIdentityPool
//	  TestIdentityPoolCapacityClamped
//	  TestReloadPublishesPoolWithView
//
// 留下的共同点：它们断言的都不是池内部语义，而是**调度器的装配**——哪个账号建池、
// K 与门禁是否同发布点、认领/兜底/释放的时序、凭据通道谓词。

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/identitypool"
)

// sessionThreadKey 会话当前线程键（不解析：只用于"是否换了线程"的比较）。
func sessionThreadKey(s *identitypool.Session) string { return s.Key() }

// testPolicy 无退休（Hi 0）——池机制测试用（身份演化另测）。零值语义是契约：
// "不退休"由零值脊线承担，而不是由一个 Default 常量承担。
func testPolicy() identitypool.Policy {
	return identitypool.Policy{}
}

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
func newTestPool(k int, p identitypool.Policy) *identitypool.Pool {
	return newTestPoolWith(k, p, nil)
}

func newTestPoolWith(k int, p identitypool.Policy, retire func(int64, string)) *identitypool.Pool {
	return identitypool.Resize(nil, identitypool.Spec{
		AccountID: 7, K: k, InstallationID: "inst", Policy: p, Retire: retire,
	}, nil)
}

// TestClaimCodexSessionOnlyCodexAndFallback 仅 codex **通道**认领（入参是预留装配点
// 算好的 credential.Channel，不再是凭据类型谓词）；全忙兜底临时身份不落池；
// 非 codex 通道 / 无池 → nil。
func TestClaimCodexSessionOnlyCodexAndFallback(t *testing.T) {
	pool := newTestPool(2, testPolicy())
	s := &Scheduler{}
	reg := &identitypool.Registry{Pools: map[int64]*identitypool.Pool{7: pool}}

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
	require.True(t, session.Busy())

	// 全忙兜底：临时身份不落池、不计 busy、无池。
	s.claimCodexSession(reg, 7, credential.ChannelCodex) // 占满第二会话
	ephem, ephPool := s.claimCodexSession(reg, 7, credential.ChannelCodex)
	require.NotNil(t, ephem)
	require.False(t, ephem.Busy(), "兜底临时身份不落池（不计 busy）")
	require.Nil(t, ephPool, "兜底身份无池（无代际/退役面）")
	require.Equal(t, pool.RotatePolicy(), ephem.Policy(), "兜底身份的策略取自池的不可变字段")
	for i := range pool.Len() {
		require.NotSame(t, ephem, pool.Slot(i))
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

	pool := s.View().static.identityPools.Pools[1]
	require.NotNil(t, pool)
	inPool := make(map[*identitypool.Session]bool, k)
	for i := range pool.Len() {
		inPool[pool.Slot(i)] = true
	}

	var ephemeral, successes int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 2000 {
				sel, err := s.Select(10, domain.FormatOpenAIResponses, "m")
				if err != nil {
					continue // 门禁满（预期 api.ErrNoAvailable）
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

	require.Equal(t, 2, s.View().static.identityPools.Pools[1].Len(), "初始池 K = max_concurrency=2")

	// 管理面调高 max_concurrency（账号变更生产路由 = InvalidateGroup）→ 池同源扩容。
	m.mu.Lock()
	m.byGroup[10][0].MaxConcurrency = 5
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()

	v := s.View()
	require.Equal(t, 5, v.static.byID[1].static.Load().acc.MaxConcurrency, "门禁上限更新")
	require.Equal(t, 5, v.static.identityPools.Pools[1].Len(), "池 K 与门禁同发布点更新")

	// 新增 codex 账号（组内）→ 立即有池（此前缺失 → 不注入身份）。
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], codexAcc(2, domain.FormatOpenAIResponses, "gpt-5", 3, "inst-2"))
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()
	require.Equal(t, 3, s.View().static.identityPools.Pools[2].Len(), "新增账号经 InvalidateGroup 即建池")

	// 删除账号 → 池随注册表替换 GC（有界泄漏修复）。
	m.mu.Lock()
	m.byGroup[10] = m.byGroup[10][:1]
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()
	require.NotContains(t, s.View().static.identityPools.Pools, int64(2), "删除账号池被回收")
}

// TestIdentityPoolCapacityClamped codex 账号的 max_concurrency 上界在装载期钳制：
// 池 K 与门禁上限同源取钳后值，reload 锁内 UUID 分配有界；非 codex 账号不受
// 上界影响（不建池、门禁照旧）。
func TestIdentityPoolCapacityClamped(t *testing.T) {
	huge := identitypool.MaxSlots + 5000
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
	require.Equal(t, identitypool.MaxSlots, v.static.identityPools.Pools[1].Len(), "codex 池 K 钳到上界")
	require.Equal(t, identitypool.MaxSlots, v.static.byID[1].static.Load().acc.MaxConcurrency, "门禁上限同源钳制")
	require.Nil(t, v.static.identityPools.Pools[2], "非 codex 账号不建池")
	require.Equal(t, huge, v.static.byID[2].static.Load().acc.MaxConcurrency, "非 codex 账号门禁不受上界影响")
	require.Nil(t, v.static.identityPools.Pools[3], "BPS 不建 codex 会话池")
	require.Equal(t, huge, v.static.byID[3].static.Load().acc.MaxConcurrency,
		"BPS 的 max_concurrency 不得被钳到 identitypool.MaxSlots（它没有会话池，钳它等于凭空砍并发）")
}

// TestReloadPublishesPoolWithView 全量 reload 的池挂在与门禁同读的静态根上：
// 同一 RoutingView.static 既给门禁 limit（byID 叶）又给池 K（identityPools）。
func TestReloadPublishesPoolWithView(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {codexAcc(1, domain.FormatOpenAIResponses, "gpt-5", 4, "inst-1")}})
	s := newSched(t, m)
	v := s.View()
	require.NotNil(t, v.static.identityPools, "池随静态根发布（非独立发布点）")
	require.Equal(t, 4, v.static.identityPools.Pools[1].Len())
	require.Same(t, v.static, s.View().static, "同一静态根")
}
