// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func reserveAll(t *testing.T, p *AttemptPlan, accept func(int64) bool) ([]int64, error) {
	t.Helper()
	var got []int64
	for {
		a, err := p.Reserve(func(id int64) bool {
			got = append(got, id)
			return accept(id)
		})
		if err != nil {
			if errors.Is(err, ErrAttemptsExhausted) || errors.Is(err, ErrNoAvailable) {
				return got, err
			}
			require.NoError(t, err)
		}
		_ = a
		if len(got) > 64 {
			t.Fatal("reserve loop unbounded")
		}
	}
}

func TestAttemptPlan_fullUniqueOverflowTailLateEligible(t *testing.T) {
	primary := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{
		Primary:  ccPrimary(primary...),
		Degraded: ccDegraded(13, 14),
	})
	a, err := p.Reserve(func(id int64) bool { return id == 12 })
	require.NoError(t, err)
	require.Equal(t, int64(12), a.AccountID)
	require.Equal(t, uint8(1), a.Ordinal)
	require.Equal(t, AttemptLanePrimary, a.Lane)
}

func TestAttemptPlan_overflowTailFullyScannedBeforeExhaustion(t *testing.T) {
	primary := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{
		Primary:  ccPrimary(primary...),
		Degraded: ccDegraded(13, 14),
	})
	got, err := reserveAll(t, p, func(int64) bool { return false })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.NotErrorIs(t, err, ErrNoAvailable)
	want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	require.Equal(t, want, got)
}

func TestAttemptPlan_overflowDedupesAgainstPrefixAndAttempted(t *testing.T) {
	p := mustNewAttemptPlan(t, AttemptPlanIdentity{}, &RouteDecision{
		Primary:  ccPrimary(1, 2, 3, 4, 5, 6, 7, 8),
		Degraded: ccDegraded(9),
	})
	var consulted []int64
	a, err := p.Reserve(func(id int64) bool {
		consulted = append(consulted, id)
		return id == 1
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), a.AccountID)
	a2, err := p.Reserve(func(id int64) bool {
		consulted = append(consulted, id)
		return id == 9
	})
	require.NoError(t, err)
	require.Equal(t, int64(9), a2.AccountID)
	seen := map[int64]int{}
	for _, id := range consulted {
		seen[id]++
	}
	for id, cnt := range seen {
		require.Equal(t, 1, cnt, "account %d consulted twice", id)
	}
}
