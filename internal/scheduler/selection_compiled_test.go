// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Select executes the compiled DecisionView plan when the route is compiled:
// lane order comes from the plan.
func TestSelect_executesCompiledPlanWhenRoutePublished(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(2, 1)})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID)
	sel.Release()
}

// Once a route is compiled, reservation failure is the plan's verdict: the
// scheduler surfaces it unchanged, never substituting another scan.
func TestSelect_compiledRouteReservationFailureReturnsPlanError(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	snap, ok := s.View().Account(1)
	require.True(t, ok)
	av := snap.static.Load()
	fp, err := candidateFingerprint(&av.acc)
	require.NoError(t, err)
	s.latch.TryAcquire(1, fp, 1)

	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.NotErrorIs(t, err, ErrNoAvailable)
}

// Unknown models fall back to the compiled default bucket (model "").
func TestSelect_unknownModelUsesCompiledDefaultBucket(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, nil) // full-model template
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})
	def := RouteRefFor(10, string(domain.FormatOpenAIChat), "")
	publishAttemptDecision(s, def, &RouteDecision{Primary: ccPrimary(1)})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "unknown-model-xyz")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}

// After the bootstrap compile the published plan serves Select with no
// extra publish step.
func TestSelect_compiledPlanServesAfterBootstrapCompile(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}

// NewAttemptPlan resolves an exact-route miss to the compiled default bucket.
func TestNewAttemptPlan_defaultBucketFallback(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, nil)
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})
	def := RouteRefFor(10, string(domain.FormatOpenAIChat), "")
	publishAttemptDecision(s, def, &RouteDecision{Primary: ccPrimary(1)})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-def"}, RouteRefFor(10, string(domain.FormatOpenAIChat), "no-such-model"))
	require.NoError(t, err)
	// the query key stays normalized; RouteClassID is borrowed from
	// the interned default-bucket decision.
	defDec, ok := s.View().DecisionView().Route(10, string(domain.FormatOpenAIChat), "")
	require.True(t, ok)
	require.NotEmpty(t, defDec.RouteClassID)
	require.Equal(t, defDec.RouteClassID, plan.Identity().RouteClassID)
	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)
	require.Equal(t, defDec.RouteClassID, attempt.RouteClassID)
	sel.Release()
}

func TestReserveAttempt_projectsConcreteModelThroughDefaultBucket(t *testing.T) {
	// Given
	tplx := tpl(1, domain.FormatOpenAIChat, nil)
	tplx.ModelMapping = domain.ModelMapping{
		"requested-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeExplicit},
	}
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})

	// When
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-default-model", ApplyModelMapping: true}, RouteRefFor(10, string(domain.FormatOpenAIChat), "requested-model"))
	require.NoError(t, err)
	sel, attempt, err := s.ReserveAttempt(&plan)

	// Then
	require.NoError(t, err)
	require.Equal(t, "requested-model", attempt.RequestedModel)
	require.Equal(t, "upstream-model", attempt.MappedModel)
	require.Equal(t, qualityClassHexForWithOp(domain.FormatOpenAIChat, "upstream-model", domain.OpChatCompletions), attempt.QualityClassID)
	require.Equal(t, "upstream-model", sel.Model)
	require.Equal(t, domain.ModelMappingModeExplicit, sel.ModelMappingMode)
	sel.Release()
}

// A static replacement (credential/revision change) fences the plan's captured
// leaf: the stale candidate is rejected, never leased from the old identity.
func TestReserveAttempt_fencesStaleLeafAfterStaticReplacement(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-fence"}, route)
	require.NoError(t, err)

	// Replace account 1's static leaf (credential rotation rebuilds the leaf).
	m := s.Loader().(*memLoader)
	m.mu.Lock()
	rotated := acc(1, tplx, 4)
	rotated.UpstreamKey = "k1-rotated"
	m.byGroup[10] = []*domain.Account{rotated, acc(2, tplx, 4)}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	// Atomic publication: the staged replacement pairs on the next compile,
	// fencing the plan's captured leaf.
	s.compileOnce()

	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(2), attempt.AccountID, "stale leaf must be fenced out")
	require.Equal(t, int64(2), sel.AccountID)
	sel.Release()
}

// A decision-generation mismatch is tolerated only before a plan has ever
// successfully reserved: the initial reservation after a decision-only
// republish succeeds against the current leaves.
func TestReserveAttempt_firstReserveSucceedsAfterDecisionRepublish(t *testing.T) {
	// Given: a plan bound before a decision-only republish lands.
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-first"}, route)
	require.NoError(t, err)
	oldGeneration := plan.Identity().RoutingGeneration

	// When: a decision-only republish lands before the first reservation.
	s.PublishDecisionForTest(route, &RouteDecision{Primary: ccPrimary(1)})
	require.Greater(t, s.View().Generation(), oldGeneration)
	sel, attempt, err := s.ReserveAttempt(&plan)

	// Then: the initial reservation succeeds — the plan never executed.
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.Equal(t, int64(1), attempt.AccountID)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}

// Once a reservation has succeeded the plan is execution-started: a later
// decision-only republish fences the next reservation, even after the lease
// was released.
func TestReserveAttempt_rejectsPostReleaseReserveAfterDecisionRepublish(t *testing.T) {
	// Given: an execution-started plan (first reservation succeeded).
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-started"}, route)
	require.NoError(t, err)
	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)
	sel.Release()
	require.True(t, plan.reservationStarted, "successful reservation marks execution start")

	// When: a decision-only republish lands after execution started.
	s.PublishDecisionForTest(route, &RouteDecision{Primary: ccPrimary(1, 2)})
	sel2, _, err := s.ReserveAttempt(&plan)

	// Then: the next reservation is rejected (a second candidate exists, so
	// only the generation fence can reject here).
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.Nil(t, sel2)
	account, ok := s.View().Account(2)
	require.True(t, ok)
	require.Zero(t, account.runtime.concurrency.Load(), "rejected reserve leases nothing")
}

// AbandonLastAttempt rewinds bookkeeping but never un-starts execution: a
// decision-only republish after reserve+abandon still fences the next reserve.
func TestReserveAttempt_rejectsPostAbandonReserveAfterDecisionRepublish(t *testing.T) {
	// Given: a plan reserved once, then abandoned without dispatch.
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-abandon"}, route)
	require.NoError(t, err)
	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)
	sel.Release()
	plan.AbandonLastAttempt()
	require.True(t, plan.reservationStarted, "abandon must not clear execution start")

	// When: a decision-only republish lands after the abandoned reservation.
	s.PublishDecisionForTest(route, &RouteDecision{Primary: ccPrimary(1, 2)})
	sel2, _, err := s.ReserveAttempt(&plan)

	// Then: the next reservation is rejected.
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.Nil(t, sel2)
}

// Health/latch fencing on the plan path uses the candidate's own quality
// class and lifecycle revision.
func TestReserveAttempt_fencesHealthByQualityClassAndRevision(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	resolved := "m"
	qc := qualityClassHexForWithOp(domain.FormatOpenAIChat, resolved, domain.OpChatCompletions)
	s.health = &RuntimeHealth{}
	// 身份分量取账号 1 的候选指纹：预留路径按 c.Fingerprint 读取，注入的记录
	// 必须带同一指纹才会被查询到（否则本用例会因"记录查不到"而假通过）。
	fpAv := s.view.Load().static.byID[1].static.Load()
	fp1, fpErr := candidateFingerprint(&fpAv.acc)
	require.NoError(t, fpErr)
	s.health.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		{AccountID: 1, Quality: qc, Identity: fp1, IdentityRevision: 1}: {State: StateOPEN},
	}})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-h"}, route)
	require.NoError(t, err)
	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(2), attempt.AccountID)
	sel.Release()
}
