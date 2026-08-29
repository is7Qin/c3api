// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func publishAttemptDecision(s *Scheduler, route RouteRef, decision *RouteDecision) {
	s.publisher.publishWithBase(s.View().Generation(), func(cur *RoutingView) *DecisionView {
		return &DecisionView{routes: map[RouteRef]*RouteDecision{route: decision}}
	})
}

func TestSchedulerNewAttemptPlanReservesExactCompiledRoute(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 1), acc(2, tplx, 1)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{
		Primary:  []int64{1},
		Explore:  ExploreDecision{IDs: []int64{2}},
		Degraded: []int64{1},
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
	av := s.View().ByID()[1].static.Load()
	fp, err := candidateFingerprint(&av.acc)
	require.NoError(t, err)
	require.Equal(t, fp, sel.CandidateFingerprint)
	require.Equal(t, uint8(1), plan.attemptedCount)
	sel.Release()
	sel.Release()
}

func TestSchedulerReserveAttemptUsesDynamicCandidateGates(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 1), acc(2, tplx, 1), acc(3, tplx, 1), acc(4, tplx, 1)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: []int64{1, 2, 3, 4}})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-2"}, route)
	require.NoError(t, err)

	s.View().ByID()[1].runtime.concurrency.Store(1)
	av2 := s.View().ByID()[2].static.Load()
	fp2, err := candidateFingerprint(&av2.acc)
	require.NoError(t, err)
	s.latch.TryAcquire(2, fp2, 1)
	s.health = &RuntimeHealth{}
	s.health.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		{AccountID: 1, Quality: "*", Revision: 1}: {State: StateOPEN},
	}})
	st := s.View().ByID()[3].statePtr()
	cooldown := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	st2 := *st
	st2.cooldownUntil = &cooldown
	s.View().ByID()[3].runtime.state.Store(&st2)
	s.timeNow = func() time.Time { return cooldown }

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
	publishAttemptDecision(s, route, &RouteDecision{Primary: []int64{1}})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-3"}, route)
	require.NoError(t, err)
	s.concView.Store(&clusterView{accounts: map[int64]concSnap{
		1: {total: 2, selfLast: 2, at: time.Now()},
	}})
	s.View().ByID()[1].runtime.concurrency.Store(1)

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
