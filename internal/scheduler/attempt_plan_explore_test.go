// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func planCandidates(p *AttemptPlan) []int64 {
	cp := *p
	cp.walkSeg, cp.walkPos = 0, 0
	cp.affinPhase = 0
	if cp.sampleValid {
		// sampleIdx already set; keep
	}
	out := []int64{}
	for {
		c, ok := cp.next()
		if !ok {
			break
		}
		out = append(out, c.AccountID)
		if len(out) > 64 {
			break
		}
		if len(out) >= MaxAttemptPlanAccounts {
			break
		}
	}
	return out
}

func planLanes(p *AttemptPlan) []AttemptLane {
	cp := *p
	cp.walkSeg, cp.walkPos = 0, 0
	cp.affinPhase = 0
	out := []AttemptLane{}
	for {
		c, ok := cp.next()
		if !ok {
			break
		}
		out = append(out, c.Lane)
		if len(out) >= MaxAttemptPlanAccounts {
			break
		}
	}
	return out
}

func TestExploreHash_crossInstanceDeterminism(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-abc", UserID: 42, RouteClassID: "route-1", RoutingGeneration: 7}
	dec := RouteDecision{
		Primary:  ccPrimary(10, 20),
		Explore:  ExploreDecision{Ordered: ccExplore(30, 40, 50), Weights: map[int64]int{30: 100, 40: 200, 50: 300}, Cumulative: []uint64{100, 300, 600}, Total: 600, Fallback: fallbackIndexes(0, 1, 2)},
		Degraded: ccDegraded(80),
	}
	p1 := mustNewAttemptPlan(t, id, &dec)
	p2 := mustNewAttemptPlan(t, id, &dec)
	require.Equal(t, planCandidates(p1), planCandidates(p2))
	require.Equal(t, planLanes(p1), planLanes(p2))
	h1 := ExploreHash("explore", id.RequestID, id.UserID, id.RouteClassID, id.RoutingGeneration, 0)
	h2 := ExploreHash("explore", id.RequestID, id.UserID, id.RouteClassID, id.RoutingGeneration, 0)
	require.Equal(t, h1, h2)
	hash := exploreHashForPlan(id, 0)
	ticket := hash % dec.Explore.Total
	idx := sort.Search(len(dec.Explore.Cumulative), func(i int) bool { return dec.Explore.Cumulative[i] > ticket })
	require.Equal(t, dec.Explore.Ordered[idx].AccountID, planCandidates(p1)[len(dec.Primary)])
}

func TestExploreHash_eachIdentityFieldChangesHash(t *testing.T) {
	base := ExploreHash("explore", "req-1", 100, "rc-1", 5, 1)
	require.NotEqual(t, base, ExploreHash("other", "req-1", 100, "rc-1", 5, 1))
	require.NotEqual(t, base, ExploreHash("explore", "req-2", 100, "rc-1", 5, 1))
	require.NotEqual(t, base, ExploreHash("explore", "req-1", 101, "rc-1", 5, 1))
	require.NotEqual(t, base, ExploreHash("explore", "req-1", 100, "rc-2", 5, 1))
	require.NotEqual(t, base, ExploreHash("explore", "req-1", 100, "rc-1", 6, 1))
	require.NotEqual(t, base, ExploreHash("explore", "req-1", 100, "rc-1", 5, 2))
	require.NotEqual(t, ExploreHash("a", "bc", 100, "rc-1", 5, 1), ExploreHash("ab", "c", 100, "rc-1", 5, 1))
	require.NotEqual(t, ExploreHash("explore", "a\x00b", 100, "rc-1", 5, 1), ExploreHash("explore", "a", 100, "rc-1", 5, 1))
}

func TestExplore_ticketModuloAndCumulativeBoundaries(t *testing.T) {
	dec := RouteDecision{Explore: ExploreDecision{Ordered: ccExplore(1, 2, 3), Weights: map[int64]int{1: 100, 2: 200, 3: 300}, Cumulative: []uint64{100, 300, 600}, Total: 600, Fallback: fallbackIndexes(0, 1, 2)}}
	cases := []struct {
		ticket uint64
		wantID int64
	}{{0, 1}, {99, 1}, {100, 2}, {299, 2}, {300, 3}, {599, 3}}
	for _, tc := range cases {
		idx := sort.Search(len(dec.Explore.Cumulative), func(i int) bool { return dec.Explore.Cumulative[i] > tc.ticket })
		require.Equal(t, tc.wantID, dec.Explore.Ordered[idx].AccountID, "ticket %d", tc.ticket)
	}
}

func TestAttemptPlan_duplicateCandidatesAndFullFallbackOrder(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-dup", UserID: 1, RouteClassID: "rc1", RoutingGeneration: 1}
	dec := RouteDecision{
		Primary:  ccPrimary(1, 2),
		Explore:  ExploreDecision{Ordered: ccExplore(3, 4, 5, 6, 7), Weights: map[int64]int{3: 100, 4: 100, 5: 100, 6: 100, 7: 100}, Cumulative: []uint64{100, 200, 300, 400, 500}, Total: 500, Fallback: fallbackIndexes(1, 2, 3, 4)},
		Degraded: ccDegraded(8),
	}
	p := mustNewAttemptPlan(t, id, &dec)
	cands := planCandidates(p)
	lanes := planLanes(p)
	require.LessOrEqual(t, len(cands), MaxAttemptPlanAccounts)
	seen := map[int64]int{}
	for _, v := range cands {
		seen[v]++
	}
	for k, cnt := range seen {
		require.Equal(t, 1, cnt, "duplicate %d", k)
	}
	firstExplore, lastExplore, firstDeg := -1, -1, -1
	for i, l := range lanes {
		if l == AttemptLaneExplore && firstExplore == -1 {
			firstExplore = i
		}
		if l == AttemptLaneExplore {
			lastExplore = i
		}
		if l == AttemptLaneDegraded && firstDeg == -1 {
			firstDeg = i
		}
	}
	require.Greater(t, firstExplore, 1)
	if firstDeg != -1 {
		require.Greater(t, firstDeg, lastExplore)
	}
	hash := exploreHashForPlan(id, 0)
	sampled := dec.Explore.Ordered[sort.Search(len(dec.Explore.Cumulative), func(i int) bool { return dec.Explore.Cumulative[i] > hash%dec.Explore.Total })].AccountID
	pos := -1
	for i, c := range cands {
		if c == sampled {
			pos = i
			break
		}
	}
	require.NotEqual(t, -1, pos)
}

func TestAttemptPlan_fallbackIndexResolvesCanonicalCandidate(t *testing.T) {
	dec := RouteDecision{Explore: ExploreDecision{
		Ordered: ccExplore(11, 22, 33),
		Weights: map[int64]int{11: 1, 22: 1, 33: 1}, Cumulative: []uint64{1, 2, 3}, Total: 3,
		Fallback: fallbackIndexes(2, 0),
	}}
	for _, idx := range dec.Explore.Fallback {
		require.Less(t, int(idx), len(dec.Explore.Ordered))
	}
	require.Equal(t, []int64{33, 11}, fallbackIDs(dec.Explore))
	require.Equal(t, []int64{33, 11}, planCandidates(mustNewAttemptPlan(t, AttemptPlanIdentity{}, &dec)))
}

func TestExploreDecision_hasOneCompiledCandidateTable(t *testing.T) {
	typ := reflect.TypeOf(ExploreDecision{})
	ordered, ok := typ.FieldByName("Ordered")
	require.True(t, ok)
	fallback, ok := typ.FieldByName("Fallback")
	require.True(t, ok)
	require.Equal(t, reflect.TypeOf([]CompiledCandidate{}), ordered.Type)
	require.Equal(t, reflect.TypeOf([]uint16{}), fallback.Type)
	dec := ExploreDecision{Ordered: ccExplore(1, 2, 3), Weights: map[int64]int{1: 1, 2: 1, 3: 1}, Cumulative: []uint64{1, 2, 3}, Total: 3, Fallback: fallbackIndexes(2, 1, 0)}
	require.Equal(t, []int64{3, 2, 1}, fallbackIDs(dec))
	for _, idx := range dec.Fallback {
		require.Less(t, int(idx), len(dec.Ordered))
	}
}

func TestAttemptPlan_zeroTotalAndEmptyExploreSafe(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-zero", UserID: 1, RouteClassID: "rc", RoutingGeneration: 1}
	decZero := RouteDecision{Primary: ccPrimary(1), Explore: ExploreDecision{}, Degraded: ccDegraded(5)}
	require.Contains(t, planCandidates(mustNewAttemptPlan(t, id, &decZero)), int64(1))
	decEmpty := RouteDecision{Primary: ccPrimary(1), Explore: ExploreDecision{}, Degraded: ccDegraded(2)}
	require.Equal(t, []int64{1, 2}, planCandidates(mustNewAttemptPlan(t, id, &decEmpty)))
	decFall := RouteDecision{Explore: ExploreDecision{Ordered: ccExplore(9, 10), Weights: map[int64]int{9: 1, 10: 1}, Cumulative: []uint64{1, 2}, Total: 2, Fallback: fallbackIndexes(0, 1)}}
	require.Len(t, planCandidates(mustNewAttemptPlan(t, id, &decFall)), 2)
}

func TestAttemptPlan_reservationOrdinalAdvancesOnlyOnSuccess(t *testing.T) {
	dec := RouteDecision{Primary: ccPrimary(1, 2), Explore: ExploreDecision{Ordered: ccExplore(3, 4), Weights: map[int64]int{3: 100, 4: 100}, Cumulative: []uint64{100, 200}, Total: 200, Fallback: fallbackIndexes(1)}, Degraded: ccDegraded(5)}
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &dec)
	_, err := p.Reserve(func(int64) bool { return false })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	p2 := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &dec)
	a1, err := p2.Reserve(func(id int64) bool { return id == 1 })
	require.NoError(t, err)
	require.Equal(t, uint8(1), a1.Ordinal)
	a2, err := p2.Reserve(func(id int64) bool { return id >= 3 && id <= 5 })
	require.NoError(t, err)
	require.Contains(t, []int64{3, 4, 5}, a2.AccountID)
	require.Equal(t, uint8(2), a2.Ordinal)
	p3 := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &dec)
	_, err = p3.Reserve(func(int64) bool { return false })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
}

func TestExploreHash_fixedCapacityAndNoSortingAtRequestPath(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-cap", UserID: 1, RouteClassID: "rc", RoutingGeneration: 1}
	dec := RouteDecision{Primary: ccPrimary(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), Explore: ExploreDecision{Ordered: ccExplore(11, 12, 13, 14), Weights: map[int64]int{11: 100, 12: 100, 13: 100, 14: 100}, Cumulative: []uint64{100, 200, 300, 400}, Total: 400, Fallback: fallbackIndexes(2, 3)}, Degraded: ccDegraded(15)}
	p := mustNewAttemptPlan(t, id, &dec)
	require.LessOrEqual(t, len(planCandidates(p)), MaxAttemptPlanAccounts)
	require.Equal(t, MaxAttemptPlanAccounts, 8)
	consulted := 0
	for {
		_, err := p.Reserve(func(int64) bool {
			consulted++
			return false
		})
		if err != nil {
			require.ErrorIs(t, err, ErrAttemptsExhausted)
			break
		}
	}
	require.Equal(t, 14, consulted, "complete unique tail must be scanned (10 primary + explore sample + 2 fallback + 1 degraded)")
}
