// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQualityClassification(t *testing.T) {
	first := math.Log(100)
	second := math.Log(200)
	candidates := []QualityCandidate{
		{AccountID: 1, Successes: 28, Attempts: 30, SumLog: first * 30, SumSq: first * first * 30, TTFTCount: 30, Cost: 10},
		{AccountID: 2, Successes: 15, Attempts: 30, SumLog: second * 30, SumSq: second * second * 30, TTFTCount: 30, Cost: 5},
		{AccountID: 3, Successes: 5, Attempts: 10},
	}

	out := classifyContract(t, candidates)

	require.Equal(t, []QualityCandidate{candidates[2]}, out.Explore)
	require.Len(t, out.Primary, 1)
	require.Len(t, out.Degraded, 1)
	require.Equal(t, int64(1), out.Primary[0].AccountID)
	require.Equal(t, int64(2), out.Degraded[0].AccountID)
}

func TestQualityClassificationDeterministic(t *testing.T) {
	logged := math.Log(100)
	candidates := []QualityCandidate{
		{AccountID: 2, Successes: 25, Attempts: 30, SumLog: logged * 30, SumSq: logged * logged * 30, TTFTCount: 30, Cost: 20},
		{AccountID: 1, Successes: 25, Attempts: 30, SumLog: logged * 30, SumSq: logged * logged * 30, TTFTCount: 30, Cost: 20},
	}

	first := classifyContract(t, candidates)
	second := classifyContract(t, candidates)

	require.Equal(t, first, second)
	require.Equal(t, int64(1), first.Primary[0].AccountID)
}

func TestQualityClassificationSortsPrimaryByCostThenQuality(t *testing.T) {
	logged := math.Log(100)
	candidates := []QualityCandidate{
		{AccountID: 3, Successes: 29, Attempts: 30, SumLog: logged * 30, SumSq: logged * logged * 30, TTFTCount: 30, Cost: 30},
		{AccountID: 2, Successes: 29, Attempts: 30, SumLog: logged * 30, SumSq: logged * logged * 30, TTFTCount: 30, Cost: 10},
		{AccountID: 1, Successes: 29, Attempts: 30, SumLog: logged * 30, SumSq: logged * logged * 30, TTFTCount: 30, Cost: 10},
	}

	out := classifyContract(t, candidates)

	require.Equal(t, []int64{1, 2, 3}, []int64{
		out.Primary[0].AccountID,
		out.Primary[1].AccountID,
		out.Primary[2].AccountID,
	})
}

func TestQualityClassificationExploresAllLowSamples(t *testing.T) {
	candidates := []QualityCandidate{
		{AccountID: 2, Successes: 1, Attempts: 10},
		{AccountID: 1, Successes: 0, Attempts: 10},
	}

	out := classifyContract(t, candidates)

	require.Empty(t, out.Primary)
	require.Empty(t, out.Degraded)
	require.Equal(t, []int64{1, 2}, []int64{out.Explore[0].AccountID, out.Explore[1].AccountID})
}

func TestQualityClassificationRejectsSmallWorkspace(t *testing.T) {
	workspace := NewQualityWorkspace(0)
	out := NewQualityClassification(0)
	err := ClassifyQuality([]QualityCandidate{{AccountID: 1}}, &workspace, &out)
	require.ErrorIs(t, err, ErrInsufficientQualityCapacity)
}
