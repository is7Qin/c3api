// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttemptPlan_maxAttemptsBoundExhaustsDistinctly(t *testing.T) {
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{MaxAttempts: 2}, &RouteDecision{
		Primary: ccPrimary(1, 2, 3, 4, 5),
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

func TestAttemptPlan_maxAttemptsNormalization(t *testing.T) {
	full := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{
		Primary: ccPrimary(1, 2, 3, 4, 5, 6, 7, 8, 9, 10),
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

	clamped := mustNewAttemptPlan(t, AttemptPlanIdentity{MaxAttempts: 200}, &RouteDecision{
		Primary: ccPrimary(1, 2, 3, 4, 5, 6, 7, 8, 9),
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

func TestAttemptPlan_abandonedReservationRefundsBudget(t *testing.T) {
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{RequestID: "req", MaxAttempts: 1}, &RouteDecision{
		Primary: ccPrimary(1, 2),
	})
	p.AbandonLastAttempt()
	a1, err := p.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	require.Equal(t, int64(1), a1.AccountID)
	require.Equal(t, uint8(1), a1.Ordinal)
	require.Equal(t, "req:1", a1.AttemptID)
	p.AbandonLastAttempt()
	a2, err := p.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	require.Equal(t, int64(2), a2.AccountID, "scan advances past the abandoned candidate")
	require.Equal(t, uint8(1), a2.Ordinal, "the refunded slot is reused, not a second attempt")
	require.Equal(t, "req:1", a2.AttemptID)
	_, err = p.Reserve(func(int64) bool { return true })
	require.ErrorIs(t, err, ErrAttemptsExhausted, "the bound dispatch still consumes the single slot")
}

func TestAttemptPlan_previousLinkageSurvivesReserveAndAbandon(t *testing.T) {
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{RequestID: "req"}, &RouteDecision{
		Primary: ccPrimary(1, 2, 3),
	})
	first, err := p.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	second, err := p.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	require.Equal(t, first.AttemptID, *second.PreviousAttemptID)
	require.Equal(t, first.AccountID, *second.PreviousAccountID)

	p.AbandonLastAttempt()
	third, err := p.Reserve(func(int64) bool { return true })
	require.NoError(t, err)
	require.Equal(t, first.AttemptID, *second.PreviousAttemptID)
	require.Equal(t, first.AttemptID, *third.PreviousAttemptID)
	require.Equal(t, first.AccountID, *third.PreviousAccountID)
}

func TestAttemptPlan_rejectsDoNotConsumeAttemptBudget(t *testing.T) {
	primary := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{MaxAttempts: 2}, &RouteDecision{Primary: ccPrimary(primary...)})
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
