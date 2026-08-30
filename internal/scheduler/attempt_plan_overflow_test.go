// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// reserveAll drains the plan collecting every accountID the reserve predicate
// was consulted with, until a terminal error. Returns (consulted IDs, error).
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

// The compiled plan keeps the COMPLETE unique overflow tail: candidates past
// the 8-slot hot prefix stay reservable (late eligible overflow), never
// truncated into a false exhaustion.
func TestAttemptPlan_fullUniqueOverflowTailLateEligible(t *testing.T) {
	primary := make([]int64, 0, 12)
	for id := int64(1); id <= 12; id++ {
		primary = append(primary, id)
	}
	p := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{
		Primary:  primary,
		Degraded: []int64{13, 14},
	})
	// Hot prefix holds 8; account 12 lives only in the lazy overflow tail.
	a, err := p.Reserve(func(id int64) bool { return id == 12 })
	require.NoError(t, err)
	require.Equal(t, int64(12), a.AccountID)
	require.Equal(t, uint8(1), a.Ordinal)
	require.Equal(t, AttemptLanePrimary, a.Lane)
}

// All rejects must consult the full unique tail before exhausting.
func TestAttemptPlan_overflowTailFullyScannedBeforeExhaustion(t *testing.T) {
	primary := make([]int64, 0, 12)
	for id := int64(1); id <= 12; id++ {
		primary = append(primary, id)
	}
	p := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{
		Primary:  primary,
		Degraded: []int64{13, 14},
	})
	got, err := reserveAll(t, p, func(int64) bool { return false })
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	require.NotErrorIs(t, err, ErrNoAvailable)
	want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	require.Equal(t, want, got)
}

// Overflow candidates never repeat an account already consulted in the hot
// prefix or already attempted.
func TestAttemptPlan_overflowDedupesAgainstPrefixAndAttempted(t *testing.T) {
	p := NewAttemptPlan(AttemptPlanIdentity{}, RouteDecision{
		Primary:  []int64{1, 2, 3, 4, 5, 6, 7, 8},
		Degraded: []int64{1, 2, 9},
	})
	var consulted []int64
	a, err := p.Reserve(func(id int64) bool {
		consulted = append(consulted, id)
		return id == 1
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), a.AccountID)
	// Next reserve: 2..8 pass through the prefix cursor rejecting; overflow
	// must skip 1/2 (prefix + attempted) and land on 9.
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
