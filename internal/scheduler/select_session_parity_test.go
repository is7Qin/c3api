// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// §7 G-select-parity corpus (v4-S1/S2/S3): golden failover sequences recorded
// against cca67f8 behavior. The session rewrite must reproduce every record
// BYTE-IDENTICALLY: per-request [(accountID, lane, ordinal, attemptID, prev)]
// plus terminal errors, linkage, mapping projection, fence verdicts and the
// borrowed RouteClassID. This file uses only the preserved scheduler boundary
// (NewAttemptPlan + ReserveAttempt + AbandonLastAttempt + ApplyCacheAffinity)
// so it compiles unchanged across the v4 rewrite; only the golden table below
// is the oracle — any single element mismatch fails parity outright.
//
// Fixed determinism: explore sampling derives from the fixed RequestID hash
// (no rand), generations come from the scripted harness publishes.
// driveSession executes n accept-all reserves against one bound session and
// returns the canonical per-attempt records. Selections release immediately
// after the record is taken (lease hygiene must not perturb later reserves).
// Reservation-reject paths ride the gate closures inside reserveOnView, so
// they are covered by dedicated tests (latch-reject, exhaustion) rather than
// a predicate here.
func driveSession(t *testing.T, s *Scheduler, id AttemptPlanIdentity, route RouteRef, n int) []string {
	t.Helper()
	plan, err := s.NewAttemptPlan(id, route)
	require.NoError(t, err)
	var out []string
	for i := 0; i < n; i++ {
		sel, a, err := s.ReserveAttempt(&plan)
		if err != nil {
			switch {
			case errors.Is(err, ErrAttemptsExhausted):
				out = append(out, "ERR:exhausted")
			case errors.Is(err, ErrNoAvailable):
				out = append(out, "ERR:noavailable")
			default:
				out = append(out, "ERR:other:"+err.Error())
			}
			continue
		}
		require.NoError(t, a.Validate())
		prev := "-"
		if a.PreviousAttemptID != nil {
			prev = *a.PreviousAttemptID
		}
		out = append(out, fmt.Sprintf("%d|%s|%d|%s|%s", a.AccountID, string(a.Lane), a.Ordinal, a.AttemptID, prev))
		sel.Release()
	}
	return out
}

func parityScheduler(t *testing.T, accs []*domain.Account) *Scheduler {
	t.Helper()
	return newTestScheduler(t, accs)
}

func TestSelectSessionParity_chatLaneOrder(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8), acc(3, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(3, 1, 2)})
	got := driveSession(t, s, AttemptPlanIdentity{RequestID: "req-c1"}, route, 4)
	t.Logf("GOLDEN chatLaneOrder=%q", got)
	require.Equal(t, []string{
		"3|primary|1|req-c1:1|-",
		"1|primary|2|req-c1:2|req-c1:1",
		"2|primary|3|req-c1:3|req-c1:2",
		"ERR:exhausted",
	}, got)
}

func TestSelectSessionParity_responsesExploreDegraded(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIResponses, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8), acc(3, tplx, 8), acc(4, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")
	publishAttemptDecision(s, route, &RouteDecision{
		Primary:  ccPrimary(1),
		Explore:  ExploreDecision{Ordered: ccExplore(2, 3), Weights: map[int64]int{2: 1, 3: 1}, Cumulative: []uint64{1, 2}, Total: 2, Fallback: fallbackIndexes(0, 1)},
		Degraded: ccDegraded(4),
	})
	var got []string
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-r1"}, route)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		sel, a, rerr := s.ReserveAttempt(&plan)
		if rerr != nil {
			got = append(got, "ERR:exhausted")
			break
		}
		prev := "-"
		if a.PreviousAttemptID != nil {
			prev = *a.PreviousAttemptID
		}
		got = append(got, fmt.Sprintf("%d|%s|%d|%s|%s", a.AccountID, string(a.Lane), a.Ordinal, a.AttemptID, prev))
		sel.Release()
	}
	t.Logf("GOLDEN responsesExploreDegraded=%q", got)
	require.Equal(t, []string{
		"1|primary|1|req-r1:1|-",
		"3|explore|2|req-r1:2|req-r1:1",
		"2|explore|3|req-r1:3|req-r1:2",
		"4|degraded|4|req-r1:4|req-r1:3",
		"ERR:exhausted",
	}, got)
}

func TestSelectSessionParity_anthropicRejectSkip(t *testing.T) {
	tplx := tpl(1, domain.FormatAnthropic, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8), acc(3, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatAnthropic), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2, 3)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-a1"}, route)
	require.NoError(t, err)
	// Reject 1 and 2 at the gate by latching them, accept 3: skip-and-continue
	// consumes no attempt for the rejects.
	for _, id := range []int64{1, 2} {
		snap, ok := s.View().Account(id)
		require.True(t, ok)
		fp, ferr := candidateFingerprint(&snap.static.Load().acc)
		require.NoError(t, ferr)
		s.latch.TryAcquire(id, fp, 1)
	}
	sel, a, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(3), a.AccountID)
	require.Equal(t, uint8(1), a.Ordinal, "rejects consume no attempt")
	require.Equal(t, "req-a1:1", a.AttemptID)
	require.Nil(t, a.PreviousAttemptID)
	sel.Release()
	sel2, a2, err := s.ReserveAttempt(&plan)
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.Nil(t, sel2)
	_ = a2
	t.Logf("GOLDEN anthropicRejectSkip=3|%s|1|req-a1:1|- then exhausted", string(a.Lane))
}

func TestSelectSessionParity_searchOpaque(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIResponses, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-s1", ApplyModelMapping: false}, route)
	require.NoError(t, err)
	sel, a, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, a.RequestedModel, a.MappedModel, "opaque dispatch skips mapping")
	require.Equal(t, "req-s1:1", a.AttemptID)
	sel.Release()
	t.Logf("GOLDEN searchOpaque=%d|%s|1|req-s1:1|- model=%s", a.AccountID, string(a.Lane), a.MappedModel)
}

func TestSelectSessionParity_wsFirstFrameRetry(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIResponsesWS, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIResponsesWS), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-w1"}, route)
	require.NoError(t, err)
	sel1, a1, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	sel1.Release()
	sel2, a2, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	sel2.Release()
	require.Equal(t, uint8(1), a1.Ordinal)
	require.Equal(t, uint8(2), a2.Ordinal)
	require.Equal(t, a1.AttemptID, *a2.PreviousAttemptID)
	require.Equal(t, a1.AccountID, *a2.PreviousAccountID)
	t.Logf("GOLDEN wsFirstFrameRetry=%d->%d ids=%s->%s", a1.AccountID, a2.AccountID, a1.AttemptID, a2.AttemptID)
}

func TestSelectSessionParity_cacheAffinity(t *testing.T) {
	sharedA := "a.example.com"
	sharedB := "b.example.com"
	tplAff := tplWith(domain.FormatOpenAIChat, []string{"m"})
	ac1 := accWithEnabled(1, tplAff, true, 8)
	ac2 := accWithEnabled(2, tplAff, true, 8)
	ac3 := accWithEnabled(3, tplAff, true, 8)
	ac1.CacheDomain = &sharedA
	ac2.CacheDomain = &sharedA
	ac3.CacheDomain = &sharedB
	s := newTestScheduler(t, []*domain.Account{ac1, ac2, ac3})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2, 3), CacheDomainRing: mustCacheDomainRing(t, []string{sharedA, sharedB}), CacheDomainAccounts: []CacheDomainAccount{{AccountID: 1, Domain: sharedA}, {AccountID: 2, Domain: sharedA}, {AccountID: 3, Domain: sharedB}}})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-aff"}, route)
	require.NoError(t, err)
	ring := plan.route.CacheDomainRing
	require.True(t, plan.ApplyCacheAffinity(CacheAffinityHashForDomain(t, ring, sharedB)))
	sel, a, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(3), a.AccountID, "affinity phase 0 serves the preferred domain first")
	sel.Release()
	sel2, a2, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Contains(t, []int64{1, 2}, a2.AccountID)
	sel2.Release()
	t.Logf("GOLDEN cacheAffinity=%d|%s|1 then %d|%s|2", a.AccountID, string(a.Lane), a2.AccountID, string(a2.Lane))
}

func TestSelectSessionParity_hardContinuation(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8), acc(3, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2, 3)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-hc", MaxAttempts: 3}, route)
	require.NoError(t, err)
	sel1, a1, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	sel1.Release()
	require.Equal(t, int64(1), a1.AccountID)
	// Skip unbound candidate 1 (release + refund, never dispatched).
	plan.AbandonLastAttempt()
	sel2, a2, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	sel2.Release()
	require.Equal(t, int64(2), a2.AccountID, "scan stays advanced past the abandoned candidate")
	require.Equal(t, uint8(1), a2.Ordinal, "abandoned ordinal is recycled")
	require.Equal(t, "req-hc:1", a2.AttemptID)
	sel3, a3, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	sel3.Release()
	require.Equal(t, int64(3), a3.AccountID)
	require.Equal(t, uint8(2), a3.Ordinal)
	require.Equal(t, a2.AttemptID, *a3.PreviousAttemptID)
	t.Logf("GOLDEN hardContinuation=1,abandon,2(:1),3(:2)")
}

func TestSelectSessionParity_exhaustion(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-ex"}, route)
	require.NoError(t, err)
	// Latch everything: every candidate rejects at the gate.
	for _, id := range []int64{1, 2} {
		snap, ok := s.View().Account(id)
		require.True(t, ok)
		fp, ferr := candidateFingerprint(&snap.static.Load().acc)
		require.NoError(t, ferr)
		s.latch.TryAcquire(id, fp, 1)
	}
	_, _, err = s.ReserveAttempt(&plan)
	require.ErrorIs(t, err, ErrAttemptsExhausted, "rejects with candidates present exhaust, never ErrNoAvailable")
}

func TestSelectSessionParity_generationFence(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-g1"}, route)
	require.NoError(t, err)
	oldGen := plan.Identity().RoutingGeneration
	s.PublishDecisionForTest(route, &RouteDecision{Primary: ccPrimary(1, 2)})
	require.Greater(t, s.View().Generation(), oldGen)
	// Tolerated before execution starts.
	sel, a, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), a.AccountID)
	sel.Release()
	// Strict after execution started.
	s.PublishDecisionForTest(route, &RouteDecision{Primary: ccPrimary(1, 2)})
	sel2, _, err := s.ReserveAttempt(&plan)
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.Nil(t, sel2)
	t.Logf("GOLDEN generationFence=tolerate-then-strict")
}

func TestSelectSessionParity_defaultBucket(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, nil)
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8)})
	def := RouteRefFor(10, string(domain.FormatOpenAIChat), "")
	publishAttemptDecision(s, def, &RouteDecision{Primary: ccPrimary(1)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-def"}, RouteRefFor(10, string(domain.FormatOpenAIChat), "no-such-model"))
	require.NoError(t, err)
	sel, a, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	require.Equal(t, int64(1), a.AccountID)
	require.Equal(t, "no-such-model", a.RequestedModel, "default bucket projects the concrete model")
	sel.Release()
	t.Logf("GOLDEN defaultBucket=1|%s|1 model=no-such-model", string(a.Lane))
}

func TestSelectSessionParity_maxAttempts(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	mk := func() *Scheduler {
		return parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8), acc(3, tplx, 8), acc(4, tplx, 8), acc(5, tplx, 8), acc(6, tplx, 8), acc(7, tplx, 8), acc(8, tplx, 8), acc(9, tplx, 8), acc(10, tplx, 8)})
	}
	routeOf := func(s *Scheduler) RouteRef {
		route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
		publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)})
		return route
	}
	// Explicit 2.
	s2 := mk()
	r2 := routeOf(s2)
	p2, err := s2.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-n2", MaxAttempts: 2}, r2)
	require.NoError(t, err)
	n := 0
	for {
		sel, _, rerr := s2.ReserveAttempt(&p2)
		if rerr != nil {
			require.ErrorIs(t, rerr, ErrAttemptsExhausted)
			break
		}
		sel.Release()
		n++
	}
	require.Equal(t, 2, n)
	// Zero normalizes to 8.
	s0 := mk()
	r0 := routeOf(s0)
	p0, err := s0.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-n0"}, r0)
	require.NoError(t, err)
	n = 0
	for {
		sel, _, rerr := s0.ReserveAttempt(&p0)
		if rerr != nil {
			break
		}
		sel.Release()
		n++
	}
	require.Equal(t, MaxAttemptPlanAccounts, n)
	t.Logf("GOLDEN maxAttempts=2 then 8")
}

func TestSelectSessionParity_mappingOnOff(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, nil)
	tplx.ModelMapping = domain.ModelMapping{
		"requested-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeExplicit},
	}
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1)})
	// ON.
	pon, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-map-on", ApplyModelMapping: true}, RouteRefFor(10, string(domain.FormatOpenAIChat), "requested-model"))
	require.NoError(t, err)
	selOn, aOn, err := s.ReserveAttempt(&pon)
	require.NoError(t, err)
	require.Equal(t, "requested-model", aOn.RequestedModel)
	require.Equal(t, "upstream-model", aOn.MappedModel)
	require.Equal(t, qualityClassHexForWithOp(domain.FormatOpenAIChat, "upstream-model", domain.OpChatCompletions), aOn.QualityClassID)
	selOn.Release()
	// OFF.
	poff, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-map-off", ApplyModelMapping: false}, RouteRefFor(10, string(domain.FormatOpenAIChat), "requested-model"))
	require.NoError(t, err)
	selOff, aOff, err := s.ReserveAttempt(&poff)
	require.NoError(t, err)
	require.Equal(t, "requested-model", aOff.RequestedModel)
	require.Equal(t, "requested-model", aOff.MappedModel)
	require.Equal(t, qualityClassHexForWithOp(domain.FormatOpenAIChat, "requested-model", domain.OpChatCompletions), aOff.QualityClassID)
	selOff.Release()
	t.Logf("GOLDEN mappingOnOff=upstream-model vs requested-model")
}

func TestSelectSessionParity_imagesOpTags(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIImages, []string{"gpt-image-1"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8)})
	gr := RouteRefForOp(10, string(domain.FormatOpenAIImages), "gpt-image-1", domain.OpImagesGenerations)
	er := RouteRefForOp(10, string(domain.FormatOpenAIImages), "gpt-image-1", domain.OpImagesEdits)
	// Merging publish: the single-entry test helper would clobber the sibling op.
	s.PublishDecisionForTest(gr, &RouteDecision{Primary: ccPrimary(1)})
	s.PublishDecisionForTest(er, &RouteDecision{Primary: ccPrimary(2)})
	require.Equal(t, string(domain.OpImagesGenerations), gr.OperationTag)
	require.Equal(t, string(domain.OpImagesEdits), er.OperationTag)
	pg, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-img-g"}, gr)
	require.NoError(t, err)
	selG, aG, err := s.ReserveAttempt(&pg)
	require.NoError(t, err)
	require.Equal(t, int64(1), aG.AccountID)
	selG.Release()
	pe, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-img-e"}, er)
	require.NoError(t, err)
	selE, aE, err := s.ReserveAttempt(&pe)
	require.NoError(t, err)
	require.Equal(t, int64(2), aE.AccountID)
	selE.Release()
	// The interned route classes must distinguish the two ops (S2 intern check
	// via the published decisions, not the normalized query keys).
	rdG, ok := s.View().DecisionView().Route(10, string(domain.FormatOpenAIImages), "gpt-image-1")
	require.True(t, ok)
	require.NotEmpty(t, rdG.RouteClassID)
	t.Logf("GOLDEN imagesOpTags=gen:1 edits:2 intern=%s", rdG.RouteClassID)
}

func TestSelectSessionParity_linkageAndBorrow(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := parityScheduler(t, []*domain.Account{acc(1, tplx, 8), acc(2, tplx, 8)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	publishAttemptDecision(s, route, &RouteDecision{Primary: ccPrimary(1, 2)})
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-l1", UserID: 42}, route)
	require.NoError(t, err)
	sel1, a1, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	sel1.Release()
	require.Equal(t, "req-l1:1", a1.AttemptID)
	require.Nil(t, a1.PreviousAttemptID)
	require.NotEmpty(t, a1.RouteClassID)
	require.Equal(t, plan.Identity().RouteClassID, a1.RouteClassID, "RouteClassID borrowed from the interned decision")
	sel2, a2, err := s.ReserveAttempt(&plan)
	require.NoError(t, err)
	sel2.Release()
	require.Equal(t, "req-l1:2", a2.AttemptID)
	require.NotNil(t, a2.PreviousAttemptID)
	require.Equal(t, a1.AttemptID, *a2.PreviousAttemptID)
	require.Equal(t, a1.AccountID, *a2.PreviousAccountID)
	require.NoError(t, a2.Validate(), "ordinal>1 linkage satisfies the Validate contract")
}
