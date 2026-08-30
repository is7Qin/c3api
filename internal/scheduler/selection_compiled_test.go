// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Select executes the compiled DecisionView plan when the route is compiled:
// lane order comes from the plan, not from the legacy weighted sequence.
func TestSelect_executesCompiledPlanWhenRoutePublished(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: []int64{2, 1}})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID)
	sel.Release()
}

// Once a route is compiled, reservation failure is the plan's verdict: the
// scheduler must not silently fall through to the legacy weighted scan.
func TestSelect_compiledRouteReservationFailureReturnsPlanError(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: []int64{1}})
	av := s.View().ByID()[1].static.Load()
	fp, err := candidateFingerprint(&av.acc)
	require.NoError(t, err)
	s.latch.TryAcquire(1, fp, 1)

	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.NotErrorIs(t, err, ErrNoAvailable)
}

// Unknown models fall back to the compiled default bucket (model "") before
// the legacy path is consulted.
func TestSelect_unknownModelUsesCompiledDefaultBucket(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, nil) // full-model template
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4)})
	def := RouteRefFor(10, string(domain.FormatOpenAIChat), "")
	publishAttemptDecision(s, def, &RouteDecision{Primary: []int64{1}})

	sel, err := s.Select(10, domain.FormatOpenAIChat, "unknown-model-xyz")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}

// Without any published decision the intermediate legacy scan path keeps
// serving (removed at the Task27 cutover, not here).
func TestSelect_noDecisionKeepsIntermediateLegacyPath(t *testing.T) {
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
	publishAttemptDecision(s, def, &RouteDecision{Primary: []int64{1}})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-def"}, RouteRefFor(10, string(domain.FormatOpenAIChat), "no-such-model"))
	require.NoError(t, err)
	require.Equal(t, def.RouteClassID, plan.Identity().RouteClassID)
	sel, attempt, err := s.ReserveAttempt(plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), attempt.AccountID)
	require.Equal(t, def.RouteClassID, attempt.RouteClassID)
	sel.Release()
}

// A static replacement (credential/revision change) fences the plan's captured
// leaf: the stale candidate is rejected, never leased from the old identity.
func TestReserveAttempt_fencesStaleLeafAfterStaticReplacement(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: []int64{1, 2}})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-fence"}, route)
	require.NoError(t, err)

	// Replace account 1's static leaf (weight action rebuilds the leaf).
	w := 50
	s.apply(1, nil, nil, &w, "credential rotated")

	sel, attempt, err := s.ReserveAttempt(plan)
	require.NoError(t, err)
	require.Equal(t, int64(2), attempt.AccountID, "stale leaf must be fenced out")
	require.Equal(t, int64(2), sel.AccountID)
	sel.Release()
}

// Health/latch fencing on the plan path uses the candidate's own quality
// class and lifecycle revision (not the wildcard-only legacy check).
func TestReserveAttempt_fencesHealthByQualityClassAndRevision(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tplx, 4), acc(2, tplx, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: []int64{1, 2}})
	resolved := "m"
	qc := qualityClassHexForWithOp(domain.FormatOpenAIChat, resolved, domain.OpChatCompletions)
	s.health = &RuntimeHealth{}
	s.health.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		{AccountID: 1, Quality: qc, Revision: 1}: {State: StateOPEN},
	}})

	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-h"}, route)
	require.NoError(t, err)
	sel, attempt, err := s.ReserveAttempt(plan)
	require.NoError(t, err)
	require.Equal(t, int64(2), attempt.AccountID)
	sel.Release()
}
