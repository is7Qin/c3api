// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func FuzzWilson95(f *testing.F) {
	f.Add(10, 20)
	f.Add(0, 0)
	f.Add(math.MaxInt, math.MaxInt)
	f.Fuzz(func(t *testing.T, successes, attempts int) {
		interval := Wilson95(successes, attempts)
		require.GreaterOrEqual(t, interval.LCB, 0.0)
		require.LessOrEqual(t, interval.UCB, 1.0)
		require.LessOrEqual(t, interval.LCB, interval.UCB)
	})
}

func FuzzExploreWeight(f *testing.F) {
	f.Add(5, 10)
	f.Add(math.MaxInt, math.MaxInt)
	f.Add(-1, 1)
	f.Fuzz(func(t *testing.T, successes, attempts int) {
		weight, err := ExploreWeight(successes, attempts)
		if successes < 0 || attempts < 0 || successes > attempts {
			require.ErrorIs(t, err, ErrInvalidQualityStats)
			return
		}
		require.NoError(t, err)
		require.GreaterOrEqual(t, weight, 100)
		require.LessOrEqual(t, weight, 10000)
	})
}

func FuzzExploreTable(f *testing.F) {
	f.Add(1, 2, 3, uint64(0))
	f.Add(math.MaxInt, math.MaxInt, math.MaxInt, uint64(math.MaxUint64))
	f.Fuzz(func(t *testing.T, first, second, third int, hash uint64) {
		table := NewExploreTable(3)
		err := table.Build([]ExploreCandidate{
			{AccountID: 1, Weight: first},
			{AccountID: 2, Weight: second},
			{AccountID: 3, Weight: third},
		})
		if first < 0 || second < 0 || third < 0 {
			require.ErrorIs(t, err, ErrInvalidQualityStats)
			return
		}
		if err != nil {
			require.ErrorIs(t, err, ErrQualityOverflow)
			return
		}
		selected, ok := table.Select(hash)
		if first == 0 && second == 0 && third == 0 {
			require.False(t, ok)
			return
		}
		require.True(t, ok)
		require.Contains(t, []int64{1, 2, 3}, selected.AccountID)
	})
}
