// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The dispatch bound is 1..8 attempts: once maxAttempts successful reserves
// happened, the next Reserve returns ErrAttemptsExhausted (distinct from
// ErrNoAvailable) even though candidates remain.
func TestAttemptPlan_maxAttemptsBoundExhaustsDistinctly(t *testing.T) {
	p := NewAttemptPlan(AttemptPlanIdentity{MaxAttempts: 2}, RouteDecision{
		Primary: []int64{1, 2, 3, 4, 5},
	})
	a1, err := p.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	require.Equal(t, uint8(1), a1.Ordinal)
	a2, err := p.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	require.Equal(t, uint8(2), a2.Ordinal)
	_, err = p.Reserve(func(int64) bool { return true })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.NotErrorIs(t, err, ErrNoAvailable)
}

// Unset max attempts falls back to the array bound (8); out-of-range values
// clamp into 1..8.
func TestAttemptPlan_maxAttemptsNormalization(t *testing.T) {
	full := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{
		Primary: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	})
	n := 0
	for {
		_, err := full.Reserve(func(int64) bool { return true })
		if err != nil {
			require.ErrorIs(t, err, ErrAttemptsExhausted)
			break
		}
		n++
		require.LessOrEqual(t, n, MaxAttemptPlanAccounts)
	}
	require.Equal(t, MaxAttemptPlanAccounts, n)

	clamped := NewAttemptPlan(AttemptPlanIdentity{MaxAttempts: 200}, RouteDecision{
		Primary: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9},
	})
	c := 0
	for {
		_, err := clamped.Reserve(func(int64) bool { return true })
		if err != nil {
			break
		}
		c++
	}
	require.Equal(t, MaxAttemptPlanAccounts, c)
}

// Reservation rejects never consume the attempt budget: ten rejects followed by
// one accept still yields ordinal 1 under a max-attempts bound of 2.
func TestAttemptPlan_rejectsDoNotConsumeAttemptBudget(t *testing.T) {
	primary := make([]int64, 0, 12)
	for id := int64(1); id <= 12; id++ {
		primary = append(primary, id)
	}
	p := NewAttemptPlan(AttemptPlanIdentity{MaxAttempts: 2}, RouteDecision{Primary: primary})
	a, err := p.Reserve(func(id int64) bool { return id == 11 })
	require.NoError(t, err)
	require.Equal(t, int64(11), a.AccountID)
	require.Equal(t, uint8(1), a.Ordinal)
	a2, err := p.Reserve(func(id int64) bool { return id == 12 })
	require.NoError(t, err)
	require.Equal(t, uint8(2), a2.Ordinal)
	_, err = p.Reserve(func(int64) bool { return true })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
}
