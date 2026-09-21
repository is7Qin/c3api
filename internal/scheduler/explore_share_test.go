// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// walkAll reserves every attempt of a fresh session with an accept-all gate,
// returning the (account, lane) sequence. Exercises the real Reserve path.
func walkAll(t *testing.T, identity AttemptPlanIdentity, decision *RouteDecision) ([]int64, []AttemptLane) {
	t.Helper()
	plan, err := NewAttemptPlan(identity, decision)
	require.NoError(t, err)
	var ids []int64
	var lanes []AttemptLane
	for {
		a, err := plan.Reserve(func(int64) bool { return true })
		if err != nil {
			require.ErrorIs(t, err, ErrAttemptsExhausted)
			break
		}
		ids = append(ids, a.AccountID)
		lanes = append(lanes, a.Lane)
		if len(ids) > MaxAttemptPlanAccounts {
			t.Fatal("walk exceeded plan bound")
		}
	}
	return ids, lanes
}

func shareDecision() *RouteDecision {
	return &RouteDecision{
		Primary: ccPrimary(1),
		Explore: ExploreDecision{
			Ordered: ccExplore(2, 3), Weights: map[int64]int{2: 100, 3: 100},
			Cumulative: []uint64{100, 200}, Total: 200, Fallback: fallbackIndexes(0, 1),
			ExploreBP: 500,
		},
		Degraded: ccDegraded(4),
	}
}

// The compiler stores the charter share on the immutable decision:
// cold start (no Primary) → 10000bp.
func TestExploreShare_compilerColdStartBP(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
	dec := s.View().DecisionView().routes[normRouteRef(RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))]
	require.NotNil(t, dec)
	require.Empty(t, dec.Primary)
	require.NotEmpty(t, dec.Explore.Ordered)
	require.Equal(t, 10000, dec.Explore.ExploreBP)
}

// One qualified primary + one unknown of two eligible → 100+ceil(400*1/2)=300bp.
func TestExploreShare_compilerSteadyStateBP(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000), OutputPerM: pricePtr(2000)}}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 500, OutputTokens: 500},
	}, accs)
	wireSources(s, q, prices)
	s.compileOnce()
	dec := s.View().DecisionView().routes[normRouteRef(RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))]
	require.NotNil(t, dec)
	require.Equal(t, []int64{1}, compiledAccountIDs(dec.Primary))
	require.Equal(t, 300, dec.Explore.ExploreBP)
}

// All qualified, nothing unknown, Primary serves → floor 100bp (1%).
func TestExploreShare_compilerNoUnknownBP(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000), OutputPerM: pricePtr(2000)}}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
	}, accs)
	wireSources(s, q, prices)
	s.compileOnce()
	dec := s.View().DecisionView().routes[normRouteRef(RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))]
	require.NotNil(t, dec)
	require.Len(t, dec.Primary, 2)
	require.Empty(t, dec.Explore.Ordered)
	require.Equal(t, 100, dec.Explore.ExploreBP)
}

// Share follows the window formula: 1 primary + 10 unknown of 11 eligible →
// 100+ceil(400*10/11)=464bp. (The 500bp cap only binds for unknown>eligible,
// which production lanes cannot produce; the clamp itself is unit-covered.)
func TestExploreShare_compilerProportionalBP(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := make([]*domain.Account, 0, 11)
	for id := int64(1); id <= 11; id++ {
		accs = append(accs, accWithEnabled(id, tpl, true, 10000))
	}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000), OutputPerM: pricePtr(2000)}}
	raw := map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
	}
	for id := int64(2); id <= 11; id++ {
		raw[id] = CandidateQualityInput{Counts: Counts{Attempts: 5, Successes: 4}, InputTokens: 400, OutputTokens: 400}
	}
	wireSources(s, buildQuality(10, domain.FormatOpenAIChat, "m", raw, accs), prices)
	s.compileOnce()
	dec := s.View().DecisionView().routes[normRouteRef(RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))]
	require.NotNil(t, dec)
	require.Equal(t, []int64{1}, compiledAccountIDs(dec.Primary))
	require.Len(t, dec.Explore.Ordered, 10)
	require.Equal(t, 464, dec.Explore.ExploreBP)
}

// Full Compile and the scoped single-route seam store the same share
// (scoped output must equal the full-recompile oracle, bp included).
func TestExploreShare_scopedMatchesFullBP(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	accs := []*domain.Account{accWithEnabled(1, tpl, true, 10000), accWithEnabled(2, tpl, true, 10000)}
	m := newMemLoader(map[int64][]*domain.Account{10: accs})
	s := newSched(t, m)
	logged := math.Log(100)
	prices := map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000), OutputPerM: pricePtr(2000)}}
	q := buildQuality(10, domain.FormatOpenAIChat, "m", map[int64]CandidateQualityInput{
		1: {Counts: Counts{Attempts: 30, Successes: 29, TTFTCount: 30, SumLog: logged * 30, SumSq: logged * logged * 30}, InputTokens: 2900, OutputTokens: 2900},
		2: {Counts: Counts{Attempts: 10, Successes: 5}, InputTokens: 500, OutputTokens: 500},
	}, accs)
	c := NewRoutingCompiler()
	full, err := c.Compile(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices})
	require.NoError(t, err)
	rk := routeKey{format: domain.FormatOpenAIChat, model: "m"}
	single, err := c.compileSingleRoute(CompilerInputs{Static: s.View().StaticView(), Quality: q, Prices: prices}, 10, rk, domain.OpChatCompletions)
	require.NoError(t, err)
	key := normRouteRef(RouteRefFor(10, string(domain.FormatOpenAIChat), "m"))
	require.Equal(t, full.routes[key].Explore.ExploreBP, single.Explore.ExploreBP)
	require.Equal(t, 300, single.Explore.ExploreBP)
}

// Lane distribution: bp=500 over 10000 canonical identities yields ~5%
// explore-first within a tight binomial band (mean 500, sd≈21.8; ±150 ≈ ±7sd).
// First pick tells the lane: explore-first → sample, primary-first → primary[0].
func TestExploreShare_laneDistribution(t *testing.T) {
	dec := shareDecision()
	const n = 10000
	exploreFirst := 0
	for i := 0; i < n; i++ {
		id := AttemptPlanIdentity{RequestID: fmt.Sprintf("dist-%d", i), UserID: int64(i % 97)}
		plan, err := NewAttemptPlan(id, dec)
		require.NoError(t, err)
		a, err := plan.Reserve(func(int64) bool { return true })
		require.NoError(t, err)
		if a.Lane == AttemptLaneExplore && (a.AccountID == 2 || a.AccountID == 3) {
			exploreFirst++
		} else {
			require.Equal(t, int64(1), a.AccountID, "primary-first must lead with primary[0]")
		}
	}
	t.Logf("explore-first=%d/%d (bp=500, expect ~500)", exploreFirst, n)
	require.GreaterOrEqual(t, exploreFirst, 350)
	require.LessOrEqual(t, exploreFirst, 650)
	// Same identity decides identically (cross-instance determinism).
	id := AttemptPlanIdentity{RequestID: "dist-7", UserID: 7}
	a1, err := mustReserveFirst(t, id, dec)
	require.NoError(t, err)
	a2, err := mustReserveFirst(t, id, dec)
	require.NoError(t, err)
	require.Equal(t, a1.AccountID, a2.AccountID)
	require.Equal(t, a1.Lane, a2.Lane)
}

func mustReserveFirst(t *testing.T, id AttemptPlanIdentity, dec *RouteDecision) (Attempt, error) {
	t.Helper()
	plan, err := NewAttemptPlan(id, dec)
	require.NoError(t, err)
	return plan.Reserve(func(int64) bool { return true })
}

// Cold start (no Primary, bp=10000): every request leads with the explore
// sample; the walk is sample → fallback → degraded, exactly the old order.
func TestExploreShare_coldStartExploreFirst(t *testing.T) {
	dec := &RouteDecision{
		Explore: ExploreDecision{
			Ordered: ccExplore(2, 3), Weights: map[int64]int{2: 100, 3: 300},
			Cumulative: []uint64{100, 400}, Total: 400, Fallback: fallbackIndexes(0, 1),
			ExploreBP: 10000,
		},
		Degraded: ccDegraded(4),
	}
	for i := 0; i < 200; i++ {
		ids, lanes := walkAll(t, AttemptPlanIdentity{RequestID: fmt.Sprintf("cold-%d", i)}, dec)
		require.Equal(t, AttemptLaneExplore, lanes[0], "cold start must lead with explore")
		require.Equal(t, AttemptLaneDegraded, lanes[len(lanes)-1])
		seen := map[int64]bool{}
		for _, id := range ids {
			require.False(t, seen[id], "duplicate %d", id)
			seen[id] = true
		}
		// Complete unique tail: sample + the other explore candidate via
		// fallback (sample skipped inside fallback) + degraded.
		require.Len(t, ids, 3)
	}
}

// Unset share (bp=0, hand-built/legacy decisions): primary-first, always.
// This locks the no-weakening contract — every pre-existing walk-order test
// uses bp=0 and keeps its assertions verbatim.
func TestExploreShare_unsetBPStaysPrimaryFirst(t *testing.T) {
	dec := &RouteDecision{
		Primary: ccPrimary(1),
		Explore: ExploreDecision{
			Ordered: ccExplore(2, 3), Weights: map[int64]int{2: 100, 3: 100},
			Cumulative: []uint64{100, 200}, Total: 200, Fallback: fallbackIndexes(0, 1),
		},
		Degraded: ccDegraded(4),
	}
	for i := 0; i < 200; i++ {
		plan, err := NewAttemptPlan(AttemptPlanIdentity{RequestID: fmt.Sprintf("legacy-%d", i)}, dec)
		require.NoError(t, err)
		require.False(t, plan.exploreFirst)
		a, err := plan.Reserve(func(int64) bool { return true })
		require.NoError(t, err)
		require.Equal(t, int64(1), a.AccountID)
		require.Equal(t, AttemptLanePrimary, a.Lane)
	}
}

// Explore-first serves the sample exactly once even though the sample also
// sits in the fallback index range; the complete unique tail is preserved.
func TestExploreShare_sampleServedExactlyOnceExploreFirst(t *testing.T) {
	dec := shareDecision()
	var id AttemptPlanIdentity
	found := false
	for i := 0; i < 1000 && !found; i++ {
		candidate := AttemptPlanIdentity{RequestID: fmt.Sprintf("scan-%d", i)}
		plan, err := NewAttemptPlan(candidate, dec)
		require.NoError(t, err)
		if plan.exploreFirst {
			id, found = candidate, true
		}
	}
	require.True(t, found, "bp=500 must elect explore-first within 1000 draws")
	ids, _ := walkAll(t, id, dec)
	sampled := dec.Explore.Ordered[planSampleIdx(t, id, dec)].AccountID
	require.Equal(t, sampled, ids[0], "explore-first must lead with the sample")
	count := 0
	seen := map[int64]bool{}
	for _, v := range ids {
		if v == sampled {
			count++
		}
		require.False(t, seen[v], "duplicate %d", v)
		seen[v] = true
	}
	require.Equal(t, 1, count)
	require.Len(t, ids, 4, "primary + sample + 1 fallback + degraded")
}

func planSampleIdx(t *testing.T, id AttemptPlanIdentity, dec *RouteDecision) int {
	t.Helper()
	plan, err := NewAttemptPlan(id, dec)
	require.NoError(t, err)
	require.True(t, plan.sampleValid)
	return plan.sampleIdx
}

// Affinity overlay under explore-first: each phase (preferred, spill) keeps
// single-domain order with lane order inside; every candidate served once.
func TestExploreShare_affinityPreservesLanes(t *testing.T) {
	ring, err := buildCacheDomainRing([]string{"dx.example", "dy.example"})
	require.NoError(t, err)
	accounts := []CacheDomainAccount{
		{AccountID: 1, Domain: "dx.example"},
		{AccountID: 2, Domain: "dx.example"},
		{AccountID: 3, Domain: "dy.example"},
	}
	dec := &RouteDecision{
		Primary:             ccPrimary(1, 2),
		Explore:             ExploreDecision{Ordered: ccExplore(3), Weights: map[int64]int{3: 100}, Cumulative: []uint64{100}, Total: 100, Fallback: fallbackIndexes(0), ExploreBP: 10000},
		Degraded:            ccDegraded(4),
		CacheDomainRing:     ring,
		CacheDomainAccounts: accounts,
	}
	domOf := map[int64]string{1: "dx.example", 2: "dx.example", 3: "dy.example", 4: "private:4"}
	const hash uint64 = 12345
	preferred, ok := ring.Lookup(hash)
	require.True(t, ok)
	plan, err := NewAttemptPlan(AttemptPlanIdentity{RequestID: "aff-1"}, dec)
	require.NoError(t, err)
	require.True(t, plan.ApplyCacheAffinity(hash))
	var seq []int64
	for {
		a, err := plan.Reserve(func(int64) bool { return true })
		if err != nil {
			require.ErrorIs(t, err, ErrAttemptsExhausted)
			break
		}
		seq = append(seq, a.AccountID)
	}
	require.Len(t, seq, 4, "affinity must still consult every candidate exactly once")
	seen := map[int64]bool{}
	for _, v := range seq {
		require.False(t, seen[v], "duplicate %d", v)
		seen[v] = true
	}
	// Affinity invariant under explore-first: preferred-domain members form
	// a prefix (preferred sweep completes before the spill sweep); the spill
	// itself keeps lane order across its mixed domains.
	require.Equal(t, preferred, domOf[seq[0]], "first pick must be in the preferred domain")
	seenNonPreferred := false
	for _, v := range seq {
		if domOf[v] != preferred {
			seenNonPreferred = true
		} else {
			require.False(t, seenNonPreferred, "preferred-domain pick %d after spill started: %v", v, seq)
		}
	}
}

// Out-of-range shares are rejected alongside the existing decision invariants.
func TestExploreShare_validationRejectsBPOutOfRange(t *testing.T) {
	for _, bp := range []int{-1, 10001} {
		dec := &RouteDecision{
			Primary: ccPrimary(1),
			Explore: ExploreDecision{
				Ordered: ccExplore(2), Weights: map[int64]int{2: 100},
				Cumulative: []uint64{100}, Total: 100, Fallback: fallbackIndexes(0),
				ExploreBP: bp,
			},
		}
		_, err := NewAttemptPlan(AttemptPlanIdentity{RequestID: "bp-bad"}, dec)
		require.ErrorIs(t, err, ErrInvalidRouteDecision, "bp=%d", bp)
	}
}

// Serialized decisions distinguish shares: same lanes with different bp
// publish as different bytes (publish guard cannot conflate them).
func TestExploreShare_bytesDistinguishBP(t *testing.T) {
	a := shareDecision()
	b := shareDecision()
	b.Explore.ExploreBP = 100
	require.NotEqual(t, decisionViewBytes(&DecisionView{routes: map[RouteRef]*RouteDecision{RouteRefFor(10, "openai-chat", "m"): a}}),
		decisionViewBytes(&DecisionView{routes: map[RouteRef]*RouteDecision{RouteRefFor(10, "openai-chat", "m"): b}}))
}
