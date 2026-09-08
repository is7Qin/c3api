// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	require.Equal(t, route.RouteClassID, plan.Identity().RouteClassID)
	require.Equal(t, s.View().Generation(), plan.Identity().RoutingGeneration)

	sel, attempt, err := s.ReserveAttempt(plan)
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
	sel.Release()
	sel.Release()
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
		{AccountID: 1, Quality: "*", Revision: 1}: {State: StateOPEN},
	}})
	acc3, ok := s.View().Account(3)
	require.True(t, ok)
	st3 := *acc3.statePtr()
	st3.status = domain.StatusDisabled
	acc3.runtime.state.Store(&st3)

	sel, attempt, err := s.ReserveAttempt(plan)
	require.NoError(t, err)
	require.Equal(t, int64(4), attempt.AccountID)
	require.Equal(t, int64(4), sel.AccountID)
	require.Equal(t, uint8(1), attempt.Ordinal)
	sel.Release()
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

	sel, attempt, err := s.ReserveAttempt(plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}

func TestSchedulerNewAttemptPlanRejectsMissingExactRoute(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 1)})
	_, err := s.NewAttemptPlan(AttemptPlanIdentity{}, RouteRefFor(10, string(domain.FormatOpenAIChat), "missing"))
	require.ErrorIs(t, err, ErrFormatUnavailable)
}

// TestReserveAttemptPreservesConcurrentFailAccount 回归：并发 CAS 屏障下，
// ReserveAttempt 的 lastUsedAt 写与 FailAccount 的 disabled 写互不覆盖——
// 两者各经独立 CAS，最终态同时携带 disabled + lastUsedAt。
func TestReserveAttemptPreservesConcurrentFailAccount(t *testing.T) {
	tpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl, 10)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-race", UserID: 1}, route)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	s.timeNow = func() time.Time { return fixed }
	// Barrier between concurrency CAS and state CAS
	barrier := make(chan struct{})
	unblock := make(chan struct{})
	reserveHook = func() {
		close(barrier)
		<-unblock
	}
	defer func() { reserveHook = nil }()
	done := make(chan *Selection, 1)
	go func() {
		sel, _, e := s.ReserveAttempt(plan)
		require.NoError(t, e)
		done <- sel
	}()
	<-barrier
	s.FailAccount(1) // 并发失效摘除（disabled 终态 CAS）
	close(unblock)
	var sel *Selection
	select {
	case sel = <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "ReserveAttempt blocked")
	}
	require.NotNil(t, sel)
	defer sel.Release()
	accSnap, ok := s.View().Account(1)
	require.True(t, ok)
	st := accSnap.statePtr()
	require.Equal(t, domain.StatusDisabled, st.status, "concurrent FailAccount must be retained after reservation")
	require.NotNil(t, st.lastUsedAt, "reservation lastUsedAt write retained")
}
