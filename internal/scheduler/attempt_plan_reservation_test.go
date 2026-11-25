// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/domain"
)

func publishAttemptDecision(s *Scheduler, route RouteRef, decision *RouteDecision) {
	decision = enrichDecision(s, route, decision)
	base := s.View()
	s.publisher.publishWithBase(base.Generation(), base.StaticView(), func(cur *RoutingView) *DecisionView {
		return &DecisionView{routes: map[RouteRef]*RouteDecision{route: decision}}
	})
}

func TestSchedulerNewAttemptPlanReservesExactCompiledRoute(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 1), acc(2, tplx, 1)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{
		Primary:  ccPrimary(1),
		Explore:  ExploreDecision{Ordered: ccExplore(2), Weights: map[int64]int{2: 1}, Cumulative: []uint64{1}, Total: 1},
		Degraded: ccDegraded(3),
	})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-1", UserID: 7}, route)
	require.NoError(t, err)
	// the query key stays normalized; RouteClassID is borrowed from
	// the interned published decision.
	dec, ok := s.View().DecisionView().Route(10, string(domain.FormatOpenAIChat), "m")
	require.True(t, ok)
	require.NotEmpty(t, dec.RouteClassID)
	require.Equal(t, dec.RouteClassID, plan.Identity().RouteClassID)
	require.Equal(t, s.View().Generation(), plan.Identity().RoutingGeneration)

	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)
	require.Equal(t, AttemptLanePrimary, attempt.Lane)
	require.Equal(t, uint8(1), attempt.Ordinal)
	require.Equal(t, int64(1), sel.AccountID)
	snap, ok := s.View().Account(1)
	require.True(t, ok)
	av := snap.static.Load()
	fp, err := candidateFingerprint(&av.acc)
	require.NoError(t, err)
	require.Equal(t, fp, sel.CandidateFingerprint)
	require.Equal(t, uint8(1), plan.attemptedCnt)
	sel.Handle.Release()
	sel.Handle.Release()
}

func TestSchedulerReserveAttemptUsesDynamicCandidateGates(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 1), acc(2, tplx, 1), acc(3, tplx, 1), acc(4, tplx, 1)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2, 3, 4)})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-2"}, route)
	require.NoError(t, err)

	acc1, ok := s.View().Account(1)
	require.True(t, ok)
	acc1.runtime.concurrency.Store(1)
	acc2, ok := s.View().Account(2)
	require.True(t, ok)
	av2 := acc2.static.Load()
	fp2, err := candidateFingerprint(&av2.acc)
	require.NoError(t, err)
	s.latch.TryAcquire(2, fp2, 1)
	s.health = &RuntimeHealth{}
	s.health.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		{AccountID: 1, Quality: "*", IdentityRevision: 1}: {State: StateOPEN},
	}})
	acc3, ok := s.View().Account(3)
	require.True(t, ok)
	st3 := *acc3.statePtr()
	st3.status = domain.StatusDisabled
	acc3.runtime.state.Store(&st3)

	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(4), attempt.AccountID)
	require.Equal(t, int64(4), sel.AccountID)
	require.Equal(t, uint8(1), attempt.Ordinal)
	sel.Handle.Release()
}

func TestSchedulerReserveAttemptUsesClusterBorrowSnapshotOnce(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)})
	s.SetInstancesProvider(fixedN(2))
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-3"}, route)
	require.NoError(t, err)
	s.concView.Store(&clusterView{accounts: map[int64]concSnap{
		1: {total: 2, selfLast: 2, at: time.Now()},
	}})
	acc1, ok := s.View().Account(1)
	require.True(t, ok)
	acc1.runtime.concurrency.Store(1)

	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Handle.Release()
}

func TestSchedulerNewAttemptPlanRejectsMissingExactRoute(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 1)})
	_, err := s.NewAttemptPlan(AttemptPlanIdentity{}, RouteRefFor(10, string(domain.FormatOpenAIChat), "missing"))
	require.ErrorIs(t, err, api.ErrFormatUnavailable)
}

// Compile-lag sentinel: static faces warm but no decision published yet →
// ErrPlanNotReady (503 at the proxy); genuinely unroutable shapes keep their
// 404 sentinels. Uses the static-only constructor (compile lane never ran).
func TestSchedulerNewAttemptPlanPlanNotReady(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestSchedulerStatic(t, []*domain.Account{acc(1, tplx, 1)})
	_, err := s.NewAttemptPlan(AttemptPlanIdentity{}, RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))
	require.ErrorIs(t, err, api.ErrPlanNotReady)
	require.NotErrorIs(t, err, api.ErrFormatUnavailable)
	_, err = s.NewAttemptPlan(AttemptPlanIdentity{}, RouteRefFor(99, string(domain.FormatOpenAIChat), "m"))
	require.ErrorIs(t, err, api.ErrGroupNotFound)

	// After the first paired publish the same route binds normally, and an
	// unroutable model keeps ErrFormatUnavailable (404).
	wireSources(s, nil, nil)
	s.compileOnce()
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-ready"}, RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))
	require.NoError(t, err)
	sel, _, rerr := s.ReserveAttempt(&plan)
	require.NoError(t, rerr)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Handle.Release()
	_, err = s.NewAttemptPlan(AttemptPlanIdentity{}, RouteRefFor(10, string(domain.FormatOpenAIChat), "missing"))
	require.ErrorIs(t, err, api.ErrFormatUnavailable)
}

// TestReserveAttemptPreservesConcurrentFailAccount 回归：两处写点互不覆盖。
// ReserveAttempt 在写「使用时刻」，FailAccount 在写 disabled——两条写路径落在
// 两个**独立**的原子位点（sharedRuntime.lastUsedNano / accState 的 CAS 链），
// 最终态同时携带 disabled + 使用时刻。本例把两个时钟分开注入，因此不是只证
// 「有个时刻」，而是证明预留的那次写没有被 FailAccount 的写吞掉。
func TestReserveAttemptPreservesConcurrentFailAccount(t *testing.T) {
	tpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl, 10)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-race", UserID: 1}, route)
	require.NoError(t, err)
	failAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reserveAt := failAt.Add(time.Minute)
	clock := failAt
	s.timeNow = func() time.Time { return clock }
	// Barrier between the concurrency CAS and the last-used write
	barrier := make(chan struct{})
	unblock := make(chan struct{})
	reserveHook := func() {
		close(barrier)
		<-unblock
	}
	s.reserveHook = reserveHook
	defer func() { s.reserveHook = nil }()
	done := make(chan *Selection, 1)
	go func() {
		sel, _, e := s.ReserveAttempt(&plan)
		require.NoError(t, e)
		done <- sel
	}()
	<-barrier
	s.FailAccount(1) // 并发失效摘除（disabled 终态 CAS），此刻时钟 = failAt
	// 屏障两侧有 happens-before（close(barrier) / close(unblock)），换钟无需再加锁。
	clock = reserveAt
	close(unblock)
	var sel *Selection
	select {
	case sel = <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "ReserveAttempt blocked")
	}
	require.NotNil(t, sel)
	defer sel.Handle.Release()
	accSnap, ok := s.View().Account(1)
	require.True(t, ok)
	st := accSnap.statePtr()
	require.Equal(t, domain.StatusDisabled, st.status, "concurrent FailAccount must be retained after reservation")
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "展示面与 statePtr 同源")
	require.NotNil(t, ri.LastUsedAt, "reservation lastUsed write retained")
	require.Equal(t, reserveAt.UnixNano(), ri.LastUsedAt.UnixNano(),
		"预留的写必须落在 FailAccount 的写之后：两处写各自独立，不得互相覆盖")
}

// TestLastUsedWriteIsAllocationFree 钉住写点的**分配形状**：markUsed 传值进、
// 单次 Store 出 ⇒ 0 alloc/写。
//
// 历史形状（accState.lastUsedAt *time.Time + copy-modify-CAS）每次写 2 次分配
// （&now 装箱 + &next 逃逸）；只把字段换成结构体内 int64 仍留 1 次（&next 逃逸），
// 所以这条断言必须落在「写点这一句」的形态上——「结构体里不用指针」不足以证明
// 零分配。
func TestLastUsedWriteIsAllocationFree(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 10)})
	snap, ok := s.View().Account(1)
	require.True(t, ok)
	rt := snap.runtime
	at := time.Date(2026, 11, 22, 8, 30, 0, 42, time.UTC)
	allocs := testing.AllocsPerRun(1000, func() { rt.markUsed(at) })
	require.Zero(t, allocs,
		"写点必须 0 分配：装箱（&t 逃逸到原子指针）或 copy-modify-CAS（&next 逃逸）都会在这里留下 1 次")
}

// TestRuntimeLastUsedAtProjection 展示面用例：管理端读到的必须是正确时刻。
//
// 三件事分开钉：
//  1. 从未使用 → nil（不是 1970，也不是零时刻——那两者都是真实可记录的时刻，
//     拿来当代值就是把「没发生过」说成「很久以前发生过」）；
//  2. 使用过 → 与注入时钟逐纳秒相等的 time.Time；
//  3. 写使用时刻**不得**替换 accState（替换 = 值拷贝 + &next 逃逸 = 1 alloc/写，
//     与上面的分配断言互为表里：一条管分配次数，一条管形态）。
func TestRuntimeLastUsedAtProjection(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 10), acc(2, tplx, 10)})

	// 1) 从未使用 → nil。
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Nil(t, ri.LastUsedAt, "从未使用必须投影成 nil，不得是 1970 / 零时刻")

	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	usedAt := time.Date(2026, 11, 22, 9, 15, 0, 123456789, time.UTC)
	s.timeNow = func() time.Time { return usedAt }
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-last-used", UserID: 1}, route)
	require.NoError(t, err)
	accSnap, ok := s.View().Account(1)
	require.True(t, ok)
	stateBefore := accSnap.statePtr()
	require.NotNil(t, stateBefore)
	sel, _, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	defer sel.Handle.Release()
	require.Same(t, stateBefore, accSnap.statePtr(),
		"写使用时刻不得替换 accState：替换就是值拷贝 + &next 逃逸（1 alloc/写）")

	// 2) 使用过 → 正确时刻；同一次写不得波及其它账号。
	ri, ok = s.Runtime(1)
	require.True(t, ok)
	require.NotNil(t, ri.LastUsedAt, "使用过的账号必须有值")
	require.Equal(t, usedAt.UnixNano(), ri.LastUsedAt.UnixNano(), "展示面必须逐纳秒还原注入时刻")
	require.True(t, usedAt.Equal(*ri.LastUsedAt), "instants must match: got %v want %v", ri.LastUsedAt, usedAt)
	require.False(t, ri.LastUsedAt.Equal(time.Unix(0, 0)), "展示面不得把有效值渲染成 1970")
	require.False(t, ri.LastUsedAt.IsZero(), "展示面不得把有效值渲染成零时刻")
	untouched, ok := s.Runtime(2)
	require.True(t, ok)
	require.Nil(t, untouched.LastUsedAt, "写点只写本次放行的账号")

	// 3) FailAccount 的触碰写同样经展示面可见；disabled 幂等早退不刷时间。
	failAt := usedAt.Add(time.Hour)
	s.timeNow = func() time.Time { return failAt }
	s.FailAccount(2)
	ri2, ok := s.Runtime(2)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri2.Status)
	require.NotNil(t, ri2.LastUsedAt, "FailAccount 的触碰写 retained")
	require.Equal(t, failAt.UnixNano(), ri2.LastUsedAt.UnixNano())
	s.timeNow = func() time.Time { return failAt.Add(time.Hour) }
	s.FailAccount(2)
	ri2, ok = s.Runtime(2)
	require.True(t, ok)
	require.Equal(t, failAt.UnixNano(), ri2.LastUsedAt.UnixNano(),
		"已 disabled 的幂等早退不得刷新触碰时刻")
}
