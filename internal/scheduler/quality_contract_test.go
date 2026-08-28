// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func classifyContract(t *testing.T, cands []QualityCandidate) QualityClassification {
	t.Helper()
	workspace := NewQualityWorkspace(len(cands))
	out := NewQualityClassification(len(cands))
	require.NoError(t, ClassifyQuality(cands, &workspace, &out))
	return out
}

func TestQualityClassifyExploresInsufficientTTFTSamples(t *testing.T) {
	y := math.Log(100)
	cands := []QualityCandidate{
		{AccountID: 2, Successes: 29, Attempts: 30, SumLog: y * 29, SumSq: y * y * 29, TTFTCount: 29},
		{AccountID: 1, Successes: 29, Attempts: 29, SumLog: y * 30, SumSq: y * y * 30, TTFTCount: 30},
	}

	out := classifyContract(t, cands)

	require.Empty(t, out.Primary)
	require.Empty(t, out.Degraded)
	require.Equal(t, []QualityCandidate{cands[1], cands[0]}, out.Explore)
}

func TestQualityClassifyDegradesEveryNonPrimaryCandidate(t *testing.T) {
	fast := math.Log(100)
	slow := math.Log(1000)
	cands := []QualityCandidate{
		{AccountID: 1, Successes: 30, Attempts: 30, SumLog: fast * 30, SumSq: fast * fast * 30, TTFTCount: 30},
		{AccountID: 2, Successes: 0, Attempts: 30, SumLog: fast * 30, SumSq: fast * fast * 30, TTFTCount: 30},
		{AccountID: 3, Successes: 30, Attempts: 30, SumLog: slow * 30, SumSq: slow * slow * 30, TTFTCount: 30},
	}

	out := classifyContract(t, cands)

	require.Equal(t, []QualityCandidate{cands[0]}, out.Primary)
	require.Equal(t, []QualityCandidate{cands[2], cands[1]}, out.Degraded)
	require.Empty(t, out.Explore)
}

func TestQualityClassifyRejectsInvalidSufficientStats(t *testing.T) {
	valid := QualityCandidate{AccountID: 1, Successes: 30, Attempts: 30, SumLog: 1, SumSq: 1, TTFTCount: 30}
	tests := []QualityCandidate{
		{AccountID: 1, Successes: -1, Attempts: 30, TTFTCount: 30},
		{AccountID: 1, Successes: 31, Attempts: 30, TTFTCount: 30},
		{AccountID: 1, Successes: 30, Attempts: 30, TTFTCount: -1},
		{AccountID: 1, Successes: 30, Attempts: 30, SumLog: math.NaN(), SumSq: 1, TTFTCount: 30},
		{AccountID: 1, Successes: 30, Attempts: 30, SumLog: 1, SumSq: math.Inf(1), TTFTCount: 30},
	}
	workspace := NewQualityWorkspace(1)
	out := NewQualityClassification(1)
	for _, candidate := range tests {
		require.ErrorIs(t, ClassifyQuality([]QualityCandidate{candidate}, &workspace, &out), ErrInvalidQualityStats)
	}
	require.NoError(t, ClassifyQuality([]QualityCandidate{valid}, &workspace, &out))
}

func TestLogTTFTIntervalRejectsNonFiniteStats(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, ok := LogTTFTInterval(value, 1, 30)
		require.False(t, ok)
		_, ok = LogTTFTInterval(1, value, 30)
		require.False(t, ok)
	}
}

func TestCountsAddRejectsOverflowAndNonFiniteStats(t *testing.T) {
	_, err := (Counts{Attempts: math.MaxInt}).Add(Counts{Attempts: 1})
	require.ErrorIs(t, err, ErrQualityOverflow)
	_, err = (Counts{SumLog: math.MaxFloat64}).Add(Counts{SumLog: math.MaxFloat64})
	require.ErrorIs(t, err, ErrQualityOverflow)
	_, err = (Counts{SumSq: math.NaN()}).Add(Counts{})
	require.ErrorIs(t, err, ErrInvalidQualityStats)
}

func TestExplorationWeightHandlesMaxIntExactly(t *testing.T) {
	weight, err := ExploreWeight(math.MaxInt, math.MaxInt)
	require.NoError(t, err)
	require.Equal(t, 10000, weight)
	_, err = ExploreWeight(-1, 1)
	require.ErrorIs(t, err, ErrInvalidQualityStats)
}

func TestExploreTableSelectsExactCumulativeBoundaries(t *testing.T) {
	table := NewExploreTable(3)
	cands := []ExploreCandidate{{AccountID: 3, Weight: 1}, {AccountID: 1, Weight: 2}, {AccountID: 2, Weight: 3}}
	require.NoError(t, table.Build(cands))
	require.Equal(t, []uint64{2, 5, 6}, table.Cumulative())
	require.Equal(t, uint64(6), table.Total())
	for hash, accountID := range []int64{1, 1, 2, 2, 2, 3, 1} {
		selected, ok := table.Select(uint64(hash))
		require.True(t, ok)
		require.Equal(t, accountID, selected.AccountID)
	}
}

func TestExploreTableRejectsCumulativeOverflow(t *testing.T) {
	table := NewExploreTable(3)
	err := table.Build([]ExploreCandidate{
		{AccountID: 1, Weight: math.MaxInt},
		{AccountID: 2, Weight: math.MaxInt},
		{AccountID: 3, Weight: math.MaxInt},
	})
	require.ErrorIs(t, err, ErrQualityOverflow)
}

func TestExplorationFallbackTailIsCompleteAndUnique(t *testing.T) {
	cands := []FallbackCandidate{
		{AccountID: 3, Successes: 9, Attempts: 10},
		{AccountID: 1, Successes: 9, Attempts: 10},
		{AccountID: 2, Successes: 5, Attempts: 10},
		{AccountID: 4, Successes: 9, Attempts: 9},
	}
	tail, err := FallbackTail(cands, 4, make([]FallbackCandidate, 0, len(cands)))
	require.NoError(t, err)
	require.Equal(t, []FallbackCandidate{cands[1], cands[0], cands[2]}, tail)
	require.Len(t, tail, len(cands)-1)
}

func TestExplorationFallbackUsesExactPosteriorAtMaxInt(t *testing.T) {
	cands := []FallbackCandidate{
		{AccountID: 1, Successes: math.MaxInt/2 - 1, Attempts: math.MaxInt},
		{AccountID: 2, Successes: math.MaxInt / 2, Attempts: math.MaxInt},
	}
	ordered, err := FallbackTail(cands, 0, make([]FallbackCandidate, 0, len(cands)))
	require.NoError(t, err)
	require.Equal(t, int64(2), ordered[0].AccountID)
}

func TestQualityClassifyAllocatesNothingAfterSetup(t *testing.T) {
	y := math.Log(100)
	cands := []QualityCandidate{
		{AccountID: 1, Successes: 28, Attempts: 30, SumLog: y * 30, SumSq: y * y * 30, TTFTCount: 30, Cost: 10},
		{AccountID: 2, Successes: 25, Attempts: 30, SumLog: y * 30, SumSq: y * y * 30, TTFTCount: 30, Cost: 20},
		{AccountID: 3, Successes: 5, Attempts: 10, Cost: 5},
	}
	workspace := NewQualityWorkspace(len(cands))
	out := NewQualityClassification(len(cands))

	var err error
	allocs := testing.AllocsPerRun(100, func() {
		err = ClassifyQuality(cands, &workspace, &out)
	})

	require.NoError(t, err)
	require.Zero(t, allocs)
}
