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

	plan := NewAttemptPlan(identity, RouteDecision{Primary: []int64{1}})

	require.Equal(t, identity, plan.Identity())
	field, ok := reflect.TypeOf(*plan).FieldByName("attempted")
	require.True(t, ok)
	require.Equal(t, reflect.Array, field.Type.Kind())
	require.Equal(t, MaxAttemptPlanAccounts, field.Type.Len())
	require.NotContains(t, reflect.TypeOf(*plan).String(), "map[")
}

func TestAttemptPlan_reservesInLaneOrderAndAdvancesOrdinalOnlyOnSuccess(t *testing.T) {
	plan := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{
		Primary:  []int64{1, 2},
		Degraded: []int64{7},
		Explore:  ExploreDecision{IDs: []int64{3, 4}, Fallback: []int64{4, 5}},
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

	second, err := plan.Reserve(func(accountID int64) bool { return accountID == 3 })

	require.NoError(t, err)
	require.Equal(t, int64(3), second.AccountID)
	require.Equal(t, AttemptLaneExplore, second.Lane)
	require.Equal(t, uint8(2), second.Ordinal)
}

func TestAttemptPlan_skipsDuplicatesAndCapsAtEightAccounts(t *testing.T) {
	plan := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{
		Primary:  []int64{1, 1, 2, 2, 3, 4, 5, 6, 7},
		Degraded: []int64{8, 9},
		Explore:  ExploreDecision{IDs: []int64{3}, Fallback: []int64{4, 8, 10}},
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

	require.Equal(t, []int64{1, 2, 3, 4, 5, 6, 7, 8}, got)
}

func TestAttemptPlan_distinguishesNoAvailableFromAttemptsExhausted(t *testing.T) {
	empty := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{})
	_, err := empty.Reserve(func(int64) bool { return true })

	require.ErrorIs(t, err, ErrNoAvailable)
	require.NotErrorIs(t, err, ErrAttemptsExhausted)

	full := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{Primary: []int64{1}})
	_, err = full.Reserve(func(int64) bool { return false })

	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.NotErrorIs(t, err, ErrNoAvailable)
}
