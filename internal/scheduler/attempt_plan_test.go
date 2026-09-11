// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttemptPlan_preservesRequestIdentityAndFixedStorage(t *testing.T) {
	identity := AttemptPlanIdentity{RequestID: "req-1", UserID: 7, RouteClassID: "route-1", RoutingGeneration: 12}

	plan := mustNewAttemptPlan(t, identity, &RouteDecision{Primary: ccPrimary(1)})

	require.Equal(t, identity, plan.Identity())
	field, ok := reflect.TypeOf(plan).FieldByName("attempted")
	require.True(t, ok)
	require.Equal(t, reflect.Array, field.Type.Kind())
	require.Equal(t, MaxAttemptPlanAccounts, field.Type.Len())
	require.NotContains(t, reflect.TypeOf(plan).String(), "map[")
}

func TestNewAttemptPlan_initializesGenerationFromIdentity(t *testing.T) {
	// Given
	identity := AttemptPlanIdentity{RequestID: "req-generation", RouteClassID: "route-1", RoutingGeneration: 12}
	plan := mustNewAttemptPlan(t, identity, &RouteDecision{Primary: ccPrimary(1)})

	// When
	attempt, err := plan.Reserve(func(int64) bool { return true })

	// Then
	require.NoError(t, err)
	require.Equal(t, identity.RoutingGeneration, attempt.RoutingGeneration)
}

func TestAttemptPlan_reservesInLaneOrderAndAdvancesOrdinalOnlyOnSuccess(t *testing.T) {
	plan := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{
		Primary:  ccPrimary(1, 2),
		Degraded: ccDegraded(7),
		Explore:  ExploreDecision{Ordered: ccExplore(3, 4, 5), Weights: map[int64]int{3: 1, 4: 1, 5: 1}, Cumulative: []uint64{1, 2, 3}, Total: 3, Fallback: fallbackIndexes(1, 2)},
	})
	var reserved []int64
	reserve := func(accountID int64) bool {
		reserved = append(reserved, accountID)
		return accountID != 1
	}

	first, err := plan.Reserve(reserve)

	require.NoError(t, err)
	require.Equal(t, int64(2), first.AccountID)
	require.Equal(t, AttemptLanePrimary, first.Lane)
	require.Equal(t, uint8(1), first.Ordinal)
	require.Equal(t, []int64{1, 2}, reserved)

	second, err := plan.Reserve(func(accountID int64) bool { return accountID >= 3 && accountID <= 5 })

	require.NoError(t, err)
	require.Contains(t, []int64{3, 4, 5}, second.AccountID)
	require.Equal(t, AttemptLaneExplore, second.Lane)
	require.Equal(t, uint8(2), second.Ordinal)
}

func TestAttemptPlan_reserve_returnsAcceptedCandidate(t *testing.T) {
	// Given
	candidate := testCC(AttemptLanePrimary, 7)[0]
	plan := mustNewAttemptPlan(t, AttemptPlanIdentity{RouteClassID: "route-1", RoutingGeneration: 1}, &RouteDecision{
		RouteClassID: "route-1",
		Primary:      []CompiledCandidate{candidate},
	})

	// When
	attempt, accepted, err := plan.reserve(func(got CompiledCandidate) bool {
		return got.AccountID == candidate.AccountID
	})

	// Then
	require.NoError(t, err)
	require.Equal(t, candidate, accepted)
	require.Equal(t, candidate.AccountID, attempt.AccountID)
}

// The plan keeps the complete unique overflow tail: every unique lane
// account is consulted before exhaustion (truncation would fake an early
// AttemptsExhausted). Uniqueness is a compiler guarantee; the session walks
// the published order verbatim.
func TestAttemptPlan_keepsCompleteUniqueTail(t *testing.T) {
	plan := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{
		Primary:  ccPrimary(1, 2, 3, 4, 5, 6, 7),
		Degraded: ccDegraded(11),
		Explore:  ExploreDecision{Ordered: ccExplore(10, 8, 9), Weights: map[int64]int{10: 1, 8: 1, 9: 1}, Cumulative: []uint64{1, 2, 3}, Total: 3, Fallback: fallbackIndexes(1, 2)},
	})
	var got []int64

	for {
		attempt, err := plan.Reserve(func(accountID int64) bool {
			got = append(got, accountID)
			return false
		})
		if errors.Is(err, ErrAttemptsExhausted) {
			break
		}
		require.NoError(t, err)
		require.Zero(t, attempt)
	}

	hash := exploreHashForPlan(AttemptPlanIdentity{}, 0)
	sampled := []int64{10, 8, 9}[hash%3]
	want := []int64{1, 2, 3, 4, 5, 6, 7, sampled}
	for _, id := range []int64{8, 9} {
		if id != sampled {
			want = append(want, id)
		}
	}
	want = append(want, 11)
	require.Equal(t, want, got)
}

func TestAttemptPlan_distinguishesNoAvailableFromAttemptsExhausted(t *testing.T) {
	empty := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{})
	_, err := empty.Reserve(func(int64) bool { return true })

	require.ErrorIs(t, err, ErrNoAvailable)
	require.NotErrorIs(t, err, ErrAttemptsExhausted)

	full := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{Primary: ccPrimary(1)})
	_, err = full.Reserve(func(int64) bool { return false })

	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.NotErrorIs(t, err, ErrNoAvailable)
}
