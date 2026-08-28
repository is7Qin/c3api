// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"math/bits"
	"slices"
	"sort"
)

func ExploreBP(eligible, unknown, primaryCount int) int {
	if eligible <= 0 {
		return 0
	}
	if primaryCount == 0 {
		return 10000
	}
	if unknown <= 0 {
		return 100
	}
	value := math.Ceil(400 * float64(unknown) / float64(eligible))
	if value > 400 {
		value = 400
	}
	return 100 + int(value)
}

func ExploreWeight(successes, attempts int) (int, error) {
	if successes < 0 || attempts < 0 || successes > attempts {
		return 0, ErrInvalidQualityStats
	}
	high, low := bits.Mul64(uint64(successes)+1, 10000)
	denominator := uint64(attempts) + 2
	weight, remainder := bits.Div64(high, low, denominator)
	if remainder >= (denominator+1)/2 {
		weight++
	}
	if weight < 100 {
		weight = 100
	}
	return int(weight), nil
}

type ExploreCandidate struct {
	AccountID int64
	Weight    int
}

type ExploreTable struct {
	candidates []ExploreCandidate
	cumulative []uint64
	total      uint64
}

func NewExploreTable(capacity int) ExploreTable {
	return ExploreTable{
		candidates: make([]ExploreCandidate, 0, capacity),
		cumulative: make([]uint64, 0, capacity),
	}
}

func (table *ExploreTable) Build(candidates []ExploreCandidate) error {
	if table == nil || cap(table.candidates) < len(candidates) || cap(table.cumulative) < len(candidates) {
		return ErrInsufficientQualityCapacity
	}
	table.candidates = append(table.candidates[:0], candidates...)
	table.cumulative = table.cumulative[:len(candidates)]
	table.total = 0
	slices.SortFunc(table.candidates, func(left, right ExploreCandidate) int {
		return compareInt64(left.AccountID, right.AccountID)
	})
	for index, candidate := range table.candidates {
		if candidate.Weight < 0 {
			return ErrInvalidQualityStats
		}
		total, carry := bits.Add64(table.total, uint64(candidate.Weight), 0)
		if carry != 0 {
			return ErrQualityOverflow
		}
		table.total = total
		table.cumulative[index] = total
	}
	return nil
}

func (table ExploreTable) Select(hash uint64) (ExploreCandidate, bool) {
	if table.total == 0 || len(table.candidates) != len(table.cumulative) {
		return ExploreCandidate{}, false
	}
	ticket := hash % table.total
	index := sort.Search(len(table.cumulative), func(index int) bool {
		return table.cumulative[index] > ticket
	})
	if index == len(table.candidates) {
		return ExploreCandidate{}, false
	}
	return table.candidates[index], true
}

func (table ExploreTable) Cumulative() []uint64 { return table.cumulative }

func (table ExploreTable) Total() uint64 { return table.total }

type FallbackCandidate struct {
	AccountID int64
	Successes int
	Attempts  int
}

func FallbackTail(candidates []FallbackCandidate, selectedAccountID int64, out []FallbackCandidate) ([]FallbackCandidate, error) {
	if cap(out) < len(candidates) {
		return nil, ErrInsufficientQualityCapacity
	}
	out = append(out[:0], candidates...)
	for _, candidate := range out {
		if candidate.Successes < 0 || candidate.Attempts < 0 || candidate.Successes > candidate.Attempts {
			return nil, ErrInvalidQualityStats
		}
	}
	slices.SortFunc(out, func(left, right FallbackCandidate) int {
		return compareInt64(left.AccountID, right.AccountID)
	})
	write := 0
	for index, candidate := range out {
		if index > 0 && candidate.AccountID == out[index-1].AccountID {
			return nil, ErrDuplicateQualityCandidate
		}
		if candidate.AccountID != selectedAccountID {
			out[write] = candidate
			write++
		}
	}
	out = out[:write]
	slices.SortFunc(out, compareFallback)
	return out, nil
}

func compareFallback(left, right FallbackCandidate) int {
	leftHigh, leftLow := bits.Mul64(uint64(left.Successes)+1, uint64(right.Attempts)+2)
	rightHigh, rightLow := bits.Mul64(uint64(right.Successes)+1, uint64(left.Attempts)+2)
	if compared := compareUint128(leftHigh, leftLow, rightHigh, rightLow); compared != 0 {
		return -compared
	}
	if left.Attempts != right.Attempts {
		if left.Attempts < right.Attempts {
			return -1
		}
		return 1
	}
	return compareInt64(left.AccountID, right.AccountID)
}

func compareUint128(leftHigh, leftLow, rightHigh, rightLow uint64) int {
	if leftHigh != rightHigh {
		if leftHigh < rightHigh {
			return -1
		}
		return 1
	}
	if leftLow < rightLow {
		return -1
	}
	if leftLow > rightLow {
		return 1
	}
	return 0
}

func compareInt64(left, right int64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
