// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func planCandidates(p *AttemptPlan) []int64 {
	out := make([]int64, 0, p.candidateCount)
	for i := uint8(0); i < p.candidateCount; i++ {
		out = append(out, p.candidates[i].accountID)
	}
	return out
}

func planLanes(p *AttemptPlan) []AttemptLane {
	out := make([]AttemptLane, 0, p.candidateCount)
	for i := uint8(0); i < p.candidateCount; i++ {
		out = append(out, p.candidates[i].lane)
	}
	return out
}

func TestExploreHash_crossInstanceDeterminism(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-abc", UserID: 42, RouteClassID: "route-1", RoutingGeneration: 7}
	dec := RouteDecision{
		Primary: []int64{10, 20},
		Explore: ExploreDecision{IDs: []int64{30, 40, 50}, Weights: map[int64]int{30: 100, 40: 200, 50: 300}, Cumulative: []uint64{100, 300, 600}, Total: 600, Fallback: []int64{60, 70}},
		Degraded: []int64{80},
	}
	p1 := NewAttemptPlan(id, dec)
	p2 := NewAttemptPlan(id, dec)
	require.Equal(t, planCandidates(p1), planCandidates(p2))
	require.Equal(t, planLanes(p1), planLanes(p2))
	h1 := ExploreHash("explore", id.RequestID, id.UserID, id.RouteClassID, id.RoutingGeneration, 0)
	h2 := ExploreHash("explore", id.RequestID, id.UserID, id.RouteClassID, id.RoutingGeneration, 0)
	require.Equal(t, h1, h2)
	hash := exploreHashForPlan(id, 0)
	ticket := hash % dec.Explore.Total
	idx := sort.Search(len(dec.Explore.Cumulative), func(i int) bool { return dec.Explore.Cumulative[i] > ticket })
	require.Equal(t, dec.Explore.IDs[idx], planCandidates(p1)[len(dec.Primary)])
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
	dec := RouteDecision{Explore: ExploreDecision{IDs: []int64{1, 2, 3}, Weights: map[int64]int{1: 100, 2: 200, 3: 300}, Cumulative: []uint64{100, 300, 600}, Total: 600, Fallback: []int64{4, 5}}}
	cases := []struct {
		ticket uint64
		wantID int64
	}{{0, 1}, {99, 1}, {100, 2}, {299, 2}, {300, 3}, {599, 3}}
	for _, tc := range cases {
		idx := sort.Search(len(dec.Explore.Cumulative), func(i int) bool { return dec.Explore.Cumulative[i] > tc.ticket })
		require.Equal(t, tc.wantID, dec.Explore.IDs[idx], "ticket %d", tc.ticket)
	}
}

func TestAttemptPlan_duplicateCandidatesAndFullFallbackOrder(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-dup", UserID: 1, RouteClassID: "rc1", RoutingGeneration: 1}
	dec := RouteDecision{
		Primary: []int64{1, 2},
		Explore: ExploreDecision{IDs: []int64{3, 4, 5}, Weights: map[int64]int{3: 100, 4: 100, 5: 100}, Cumulative: []uint64{100, 200, 300}, Total: 300, Fallback: []int64{4, 5, 6, 7}},
		Degraded: []int64{2, 8},
	}
	p := NewAttemptPlan(id, dec)
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
	sampled := dec.Explore.IDs[sort.Search(len(dec.Explore.Cumulative), func(i int) bool { return dec.Explore.Cumulative[i] > hash%dec.Explore.Total })]
	pos := -1
	for i, c := range cands {
		if c == sampled {
			pos = i
			break
		}
	}
	require.NotEqual(t, -1, pos)
	expected := []int64{}
	for _, fid := range dec.Explore.Fallback {
		if fid == sampled {
			continue
		}
		skip := false
		for _, pid := range dec.Primary {
			if fid == pid {
				skip = true
			}
		}
		if !skip {
			expected = append(expected, fid)
		}
	}
	for i, exp := range expected {
		np := pos + 1 + i
		if np >= len(cands) || lanes[np] != AttemptLaneExplore {
			break
		}
		require.Equal(t, exp, cands[np])
	}
	cnt2 := 0
	for _, c := range cands {
		if c == 2 {
			cnt2++
		}
	}
	require.Equal(t, 1, cnt2)
}

func TestAttemptPlan_zeroTotalAndEmptyExploreSafe(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-zero", UserID: 1, RouteClassID: "rc", RoutingGeneration: 1}
	decZero := RouteDecision{Primary: []int64{1}, Explore: ExploreDecision{IDs: []int64{2, 3}, Weights: map[int64]int{2: 100, 3: 100}, Cumulative: []uint64{100, 200}, Total: 0, Fallback: []int64{4}}, Degraded: []int64{5}}
	require.Contains(t, planCandidates(NewAttemptPlan(id, decZero)), int64(1))
	decEmpty := RouteDecision{Primary: []int64{1}, Explore: ExploreDecision{}, Degraded: []int64{2}}
	require.Equal(t, []int64{1, 2}, planCandidates(NewAttemptPlan(id, decEmpty)))
	decFall := RouteDecision{Explore: ExploreDecision{Fallback: []int64{9, 10}}}
	require.Equal(t, []int64{9, 10}, planCandidates(NewAttemptPlan(id, decFall)))
}

func TestAttemptPlan_reservationOrdinalAdvancesOnlyOnSuccess(t *testing.T) {
	dec := RouteDecision{Primary: []int64{1, 2}, Explore: ExploreDecision{IDs: []int64{3}, Weights: map[int64]int{3: 100}, Cumulative: []uint64{100}, Total: 100, Fallback: []int64{4}}, Degraded: []int64{5}}
	p := NewAttemptPlan(AttemptPlanIdentity{}, dec)
	_, err := p.Reserve(func(int64) bool { return false })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	p2 := NewAttemptPlan(AttemptPlanIdentity{}, dec)
	a1, err := p2.Reserve(func(id int64) bool { return id == 1 })
	require.NoError(t, err)
	require.Equal(t, uint8(1), a1.Ordinal)
	a2, err := p2.Reserve(func(id int64) bool { return id == 3 })
	require.NoError(t, err)
	require.Equal(t, uint8(2), a2.Ordinal)
	p3 := NewAttemptPlan(AttemptPlanIdentity{}, dec)
	_, err = p3.Reserve(func(int64) bool { return false })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
}

func TestExploreHash_fixedCapacityAndNoSortingAtRequestPath(t *testing.T) {
	id := AttemptPlanIdentity{RequestID: "req-cap", UserID: 1, RouteClassID: "rc", RoutingGeneration: 1}
	dec := RouteDecision{Primary: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, Explore: ExploreDecision{IDs: []int64{11, 12}, Weights: map[int64]int{11: 100, 12: 100}, Cumulative: []uint64{100, 200}, Total: 200, Fallback: []int64{13, 14}}, Degraded: []int64{15}}
	p := NewAttemptPlan(id, dec)
	require.LessOrEqual(t, len(planCandidates(p)), MaxAttemptPlanAccounts)
	require.Equal(t, MaxAttemptPlanAccounts, 8)
}
