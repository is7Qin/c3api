// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"slices"
)

type QualityCandidate struct {
	AccountID int64
	Successes int
	Attempts  int
	SumLog    float64
	SumSq     float64
	TTFTCount int
	Cost      int64
}

type classifiedCandidate struct {
	candidate         QualityCandidate
	success           Interval
	ttft              Interval
	successEquivalent bool
}

type QualityWorkspace struct {
	candidates []classifiedCandidate
}

func NewQualityWorkspace(capacity int) QualityWorkspace {
	return QualityWorkspace{candidates: make([]classifiedCandidate, 0, capacity)}
}

type QualityClassification struct {
	Primary  []QualityCandidate
	Degraded []QualityCandidate
	Explore  []QualityCandidate
}

func NewQualityClassification(capacity int) QualityClassification {
	return QualityClassification{
		Primary:  make([]QualityCandidate, 0, capacity),
		Degraded: make([]QualityCandidate, 0, capacity),
		Explore:  make([]QualityCandidate, 0, capacity),
	}
}

func ClassifyQuality(candidates []QualityCandidate, workspace *QualityWorkspace, out *QualityClassification) error {
	if workspace == nil || out == nil || cap(workspace.candidates) < len(candidates) ||
		cap(out.Primary) < len(candidates) || cap(out.Degraded) < len(candidates) || cap(out.Explore) < len(candidates) {
		return ErrInsufficientQualityCapacity
	}
	workspace.candidates = workspace.candidates[:0]
	out.Primary = out.Primary[:0]
	out.Degraded = out.Degraded[:0]
	out.Explore = out.Explore[:0]

	for _, candidate := range candidates {
		if !validQualityCandidate(candidate) {
			return ErrInvalidQualityStats
		}
		if candidate.Attempts < 30 || candidate.TTFTCount < 30 {
			out.Explore = append(out.Explore, candidate)
			continue
		}
		ttft, ok := LogTTFTInterval(candidate.SumLog, candidate.SumSq, candidate.TTFTCount)
		if !ok {
			return ErrInvalidQualityStats
		}
		workspace.candidates = append(workspace.candidates, classifiedCandidate{
			candidate: candidate,
			success:   Wilson95(candidate.Successes, candidate.Attempts),
			ttft:      ttft,
		})
	}
	if len(workspace.candidates) == 0 {
		sortQualityByAccountID(out.Explore)
		return nil
	}

	bestLCB := workspace.candidates[0].success.LCB
	for _, candidate := range workspace.candidates[1:] {
		if candidate.success.LCB > bestLCB {
			bestLCB = candidate.success.LCB
		}
	}
	fastestUCB := math.Inf(1)
	for index := range workspace.candidates {
		candidate := &workspace.candidates[index]
		candidate.successEquivalent = candidate.success.UCB >= bestLCB
		if candidate.successEquivalent && candidate.ttft.UCB < fastestUCB {
			fastestUCB = candidate.ttft.UCB
		}
	}
	for _, candidate := range workspace.candidates {
		if candidate.successEquivalent && candidate.ttft.LCB <= fastestUCB {
			out.Primary = append(out.Primary, candidate.candidate)
		} else {
			out.Degraded = append(out.Degraded, candidate.candidate)
		}
	}

	slices.SortFunc(out.Primary, comparePrimary)
	slices.SortFunc(out.Degraded, func(left, right QualityCandidate) int {
		return compareDegraded(left, right, bestLCB, fastestUCB)
	})
	sortQualityByAccountID(out.Explore)
	return nil
}

func validQualityCandidate(candidate QualityCandidate) bool {
	return candidate.Attempts >= 0 && candidate.Successes >= 0 && candidate.Successes <= candidate.Attempts &&
		candidate.TTFTCount >= 0 &&
		finiteNonnegative(candidate.SumLog) && finiteNonnegative(candidate.SumSq)
}

func comparePrimary(left, right QualityCandidate) int {
	if left.Cost != right.Cost {
		if left.Cost < right.Cost {
			return -1
		}
		return 1
	}
	leftSuccess, rightSuccess := Wilson95(left.Successes, left.Attempts), Wilson95(right.Successes, right.Attempts)
	if leftSuccess.LCB != rightSuccess.LCB {
		if leftSuccess.LCB > rightSuccess.LCB {
			return -1
		}
		return 1
	}
	leftTTFT, _ := LogTTFTInterval(left.SumLog, left.SumSq, left.TTFTCount)
	rightTTFT, _ := LogTTFTInterval(right.SumLog, right.SumSq, right.TTFTCount)
	if leftTTFT.UCB != rightTTFT.UCB {
		if leftTTFT.UCB < rightTTFT.UCB {
			return -1
		}
		return 1
	}
	return compareInt64(left.AccountID, right.AccountID)
}

func compareDegraded(left, right QualityCandidate, bestLCB, fastestUCB float64) int {
	leftSuccess, rightSuccess := Wilson95(left.Successes, left.Attempts), Wilson95(right.Successes, right.Attempts)
	leftGap, rightGap := math.Max(0, bestLCB-leftSuccess.UCB), math.Max(0, bestLCB-rightSuccess.UCB)
	if leftGap != rightGap {
		if leftGap < rightGap {
			return -1
		}
		return 1
	}
	leftTTFT, _ := LogTTFTInterval(left.SumLog, left.SumSq, left.TTFTCount)
	rightTTFT, _ := LogTTFTInterval(right.SumLog, right.SumSq, right.TTFTCount)
	leftGap, rightGap = math.Max(0, leftTTFT.LCB-fastestUCB), math.Max(0, rightTTFT.LCB-fastestUCB)
	if leftGap != rightGap {
		if leftGap < rightGap {
			return -1
		}
		return 1
	}
	if left.Cost != right.Cost {
		if left.Cost < right.Cost {
			return -1
		}
		return 1
	}
	return compareInt64(left.AccountID, right.AccountID)
}

func sortQualityByAccountID(candidates []QualityCandidate) {
	slices.SortFunc(candidates, func(left, right QualityCandidate) int {
		return compareInt64(left.AccountID, right.AccountID)
	})
}
