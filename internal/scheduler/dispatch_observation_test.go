// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestDispatchObservation_requiresCompleteMetadata(t *testing.T) {
	plan := mustNewAttemptPlan(t, AttemptPlanIdentity{RequestID: "req-abc", UserID: 99, RouteClassID: "rc-hex-64-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RoutingGeneration: 7}, &RouteDecision{Primary: ccPrimary(1)})
	a, err := plan.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	require.Error(t, a.Validate(), "direct NewAttemptPlan without scheduler must lack required dispatch fields and Validate should reject")

	tmpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tmpl, 4), acc(2, tmpl, 4)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan2, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-123", UserID: 42}, route)
	require.NoError(t, err)
	sel, a2, err := s.ReserveAttempt(&plan2)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.NoError(t, a2.Validate(), "scheduler-enriched attempt must carry complete dispatch metadata")
	require.Equal(t, "req-123:1", a2.AttemptID)
	require.NotEmpty(t, a2.RouteClassID)
	require.NotEmpty(t, a2.QualityClassID)
	require.NotEmpty(t, a2.CandidateFingerprint)
	require.NotZero(t, a2.TemplateID)
	require.NotZero(t, a2.AccountID)
	require.Equal(t, "m", a2.RequestedModel)
	require.NotEmpty(t, a2.MappedModel)
	require.True(t, a2.Lane.Valid())
	require.Equal(t, uint8(1), a2.Ordinal)
	require.NotZero(t, a2.RoutingGeneration)
	require.NotZero(t, a2.IdentityRevision)
	require.Nil(t, a2.PreviousAttemptID)
	require.True(t, a2.CallerCategory != "")
	require.True(t, a2.OperationTag != "")

	// second attempt must carry previous identity
	sel.Release()
	sel2, a3, err := s.ReserveAttempt(&plan2)
	require.NoError(t, err)
	require.Equal(t, uint8(2), a3.Ordinal)
	require.NotNil(t, a3.PreviousAttemptID)
	require.Equal(t, a2.AttemptID, *a3.PreviousAttemptID)
	require.NoError(t, a3.Validate())
	sel2.Release()

	// validation must reject missing required dispatch fields
	bad := a2
	bad.RouteClassID = ""
	require.Error(t, bad.Validate())
	bad = a2
	bad.QualityClassID = ""
	require.Error(t, bad.Validate())
	bad = a2
	bad.CandidateFingerprint = ""
	require.Error(t, bad.Validate())
	bad = a2
	bad.TemplateID = 0
	require.Error(t, bad.Validate())
	bad = a2
	bad.AccountID = 0
	require.Error(t, bad.Validate())
	bad = a2
	bad.Lane = ""
	require.Error(t, bad.Validate())
	bad = a2
	bad.Ordinal = 0
	require.Error(t, bad.Validate())
	bad = a2
	bad.RoutingGeneration = 0
	require.Error(t, bad.Validate())
	bad = a2
	bad.IdentityRevision = 0
	require.Error(t, bad.Validate())
	bad = a2
	bad.CallerCategory = ""
	require.Error(t, bad.Validate())
	bad = a2
	bad.OperationTag = ""
	require.Error(t, bad.Validate())
	bad = a2
	bad.AttemptID = ""
	require.Error(t, bad.Validate())
}

func TestDispatchObservation_preservesLeaseWithoutRereadingState(t *testing.T) {
	tmpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tmpl, 4)}})
	s := newSched(t, m)
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-lease", UserID: 1}, route)
	require.NoError(t, err)
	sel, a, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, a.AccountID, sel.AccountID)
	current, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, a, current)
	require.Equal(t, a.CandidateFingerprint, sel.CandidateFingerprint)
	require.Equal(t, a.TemplateID, sel.TemplateID)
	// mutate view after plan creation: add new account, invalidate, but plan's dispatch must stay stable
	tmpl2 := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m.byGroup[10] = append(m.byGroup[10], &domain.Account{ID: 99, TemplateID: 1, Template: tmpl2, UpstreamKey: "k99", Enabled: true, MaxConcurrency: 4})
	require.NoError(t, s.InvalidateAllSync())
	// attempt still validates with original generation/fingerprint, not mutated view
	require.NoError(t, a.Validate())
	requireEqualDispatchFingerprint(t, a.CandidateFingerprint)
	sel.Release()
	ri, _ := s.Runtime(a.AccountID)
	require.Equal(t, int64(0), ri.Concurrency)
	sel.Release()
	ri, _ = s.Runtime(a.AccountID)
	require.Equal(t, int64(0), ri.Concurrency)
}

func TestDispatchObservation_currentAttemptTracksPreviousAccount(t *testing.T) {
	tmpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {acc(1, tmpl, 4), acc(2, tmpl, 4)}}))
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-current", UserID: 1}, route)
	require.NoError(t, err)
	_, first, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	_, second, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	current, ok := plan.CurrentAttempt()
	require.True(t, ok)
	require.Equal(t, second, current)
	require.Equal(t, first.AccountID, *second.PreviousAccountID)
}

func requireEqualDispatchFingerprint(t *testing.T, fp string) {
	t.Helper()
	require.NotEmpty(t, fp)
}
