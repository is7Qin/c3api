// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQualityExploreThreshold(t *testing.T) {
	require.True(t, IsExplore(0))
	require.True(t, IsExplore(29))
	require.False(t, IsExplore(30))
	require.False(t, IsExplore(1000))
}

func TestExplorationBP(t *testing.T) {
	require.Equal(t, 10000, ExploreBP(10, 5, 0))
	require.Equal(t, 100, ExploreBP(10, 0, 1))
	require.Equal(t, 0, ExploreBP(0, 0, 1))
	require.Equal(t, 500, ExploreBP(10, 10, 1))
	require.Equal(t, 300, ExploreBP(10, 5, 1))
	require.Equal(t, 500, ExploreBP(10, 20, 1))
}

func TestExplorationWeight(t *testing.T) {
	tests := []struct {
		successes int
		attempts  int
		want      int
	}{
		{successes: 0, attempts: 0, want: 5000},
		{successes: 0, attempts: 100000, want: 100},
		{successes: 100, attempts: 100, want: 9902},
	}
	for _, test := range tests {
		weight, err := ExploreWeight(test.successes, test.attempts)
		require.NoError(t, err)
		require.Equal(t, test.want, weight)
	}
}

func TestExplorationTableSortedByAccountID(t *testing.T) {
	table := NewExploreTable(3)
	err := table.Build([]ExploreCandidate{{AccountID: 3, Weight: 100}, {AccountID: 1, Weight: 200}, {AccountID: 2, Weight: 300}})
	require.NoError(t, err)
	require.Equal(t, uint64(600), table.Total())
	require.Equal(t, []uint64{200, 500, 600}, table.Cumulative())
	selected, ok := table.Select(500)
	require.True(t, ok)
	require.Equal(t, int64(3), selected.AccountID)
}

func TestExplorationTableRejectsNegativeWeight(t *testing.T) {
	table := NewExploreTable(1)
	require.ErrorIs(t, table.Build([]ExploreCandidate{{AccountID: 1, Weight: -1}}), ErrInvalidQualityStats)
	empty := NewExploreTable(0)
	require.NoError(t, empty.Build(nil))
	_, ok := empty.Select(0)
	require.False(t, ok)
}

func TestExplorationFallbackOrder(t *testing.T) {
	candidates := []FallbackCandidate{
		{AccountID: 3, Successes: 9, Attempts: 10},
		{AccountID: 1, Successes: 9, Attempts: 10},
		{AccountID: 2, Successes: 5, Attempts: 10},
		{AccountID: 4, Successes: 9, Attempts: 9},
	}
	ordered, err := FallbackTail(candidates, 0, make([]FallbackCandidate, 0, len(candidates)))
	require.NoError(t, err)
	require.Equal(t, []int64{4, 1, 3, 2}, []int64{
		ordered[0].AccountID,
		ordered[1].AccountID,
		ordered[2].AccountID,
		ordered[3].AccountID,
	})
}

func TestExplorationFallbackRejectsDuplicateAccount(t *testing.T) {
	candidates := []FallbackCandidate{{AccountID: 1}, {AccountID: 1}}
	_, err := FallbackTail(candidates, 0, make([]FallbackCandidate, 0, len(candidates)))
	require.ErrorIs(t, err, ErrDuplicateQualityCandidate)
}
