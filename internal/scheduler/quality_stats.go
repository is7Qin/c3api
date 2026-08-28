// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"math/bits"
	"sort"
	"time"
)

const WilsonZ = 1.959963984540054
const ttftZ = 1.96

type Interval struct{ LCB, UCB float64 }

func Wilson95(successes, attempts int) Interval {
	if attempts <= 0 {
		return Interval{}
	}
	if successes < 0 {
		successes = 0
	}
	if successes > attempts {
		successes = attempts
	}
	n := float64(attempts)
	p := float64(successes) / n
	z2 := WilsonZ * WilsonZ
	denom := 1 + z2/n
	center := p + z2/(2*n)
	variance := p*(1-p)/n + z2/(4*n*n)
	if variance < 0 {
		variance = 0
	}
	margin := WilsonZ * math.Sqrt(variance)
	lcb := (center - margin) / denom
	ucb := (center + margin) / denom
	if lcb < 0 {
		lcb = 0
	}
	if ucb > 1 {
		ucb = 1
	}
	return Interval{lcb, ucb}
}

func LogTTFTInterval(sumLog, sumSq float64, n int) (Interval, bool) {
	if n < 30 || !finiteNonnegative(sumLog) || !finiteNonnegative(sumSq) {
		return Interval{}, false
	}
	mean := sumLog / float64(n)
	variance := (sumSq - float64(n)*mean*mean) / float64(n-1)
	if !isFinite(variance) {
		return Interval{}, false
	}
	if variance < 0 {
		variance = 0
	}
	half := ttftZ * math.Sqrt(variance) / math.Sqrt(float64(n))
	interval := Interval{math.Exp(mean - half), math.Exp(mean + half)}
	if !isFinite(interval.LCB) || !isFinite(interval.UCB) {
		return Interval{}, false
	}
	return interval, true
}

func LogTTFTFromSamples(ttftMs []int64) (Interval, bool) {
	if len(ttftMs) < 30 {
		return Interval{}, false
	}
	var sum, sumSq float64
	for _, value := range ttftMs {
		if value < 1 {
			value = 1
		}
		logged := math.Log(float64(value))
		sum += logged
		sumSq += logged * logged
	}
	return LogTTFTInterval(sum, sumSq, len(ttftMs))
}

func CurrentWindow(now time.Time) (time.Time, time.Time, time.Time, time.Time) {
	minute := now.UTC().Truncate(time.Minute)
	return minute.Add(-5 * time.Minute), minute, minute, now.UTC()
}

func BaselineWindow(now time.Time) (time.Time, time.Time) {
	minute := now.UTC().Truncate(time.Minute)
	return minute.Add(-24 * time.Hour), minute.Add(-5 * time.Minute)
}

type Counts struct {
	Attempts  int
	Successes int
	SumLog    float64
	SumSq     float64
	TTFTCount int
}

func (c Counts) Add(other Counts) (Counts, error) {
	if !validCounts(c) || !validCounts(other) {
		return Counts{}, ErrInvalidQualityStats
	}
	attempts, ok := addCount(c.Attempts, other.Attempts)
	if !ok {
		return Counts{}, ErrQualityOverflow
	}
	successes, ok := addCount(c.Successes, other.Successes)
	if !ok {
		return Counts{}, ErrQualityOverflow
	}
	ttftCount, ok := addCount(c.TTFTCount, other.TTFTCount)
	if !ok {
		return Counts{}, ErrQualityOverflow
	}
	sumLog, sumSq := c.SumLog+other.SumLog, c.SumSq+other.SumSq
	if !isFinite(sumLog) || !isFinite(sumSq) {
		return Counts{}, ErrQualityOverflow
	}
	return Counts{Attempts: attempts, Successes: successes, SumLog: sumLog, SumSq: sumSq, TTFTCount: ttftCount}, nil
}

type MinuteBucket struct {
	Start  time.Time
	Counts Counts
}

func AccumulateCurrent(buckets []MinuteBucket, live Counts, now time.Time) (Counts, error) {
	minute := now.UTC().Truncate(time.Minute)
	cut := minute.Add(-5 * time.Minute)
	var out Counts
	var err error
	for _, bucket := range buckets {
		if !bucket.Start.Before(cut) && bucket.Start.Before(minute) {
			out, err = out.Add(bucket.Counts)
			if err != nil {
				return Counts{}, err
			}
		}
	}
	return out.Add(live)
}

func AccumulateBaseline(buckets []MinuteBucket, now time.Time) (Counts, error) {
	minute := now.UTC().Truncate(time.Minute)
	start, end := minute.Add(-24*time.Hour), minute.Add(-5*time.Minute)
	filtered := make([]MinuteBucket, 0, len(buckets))
	for _, bucket := range buckets {
		if !bucket.Start.Before(start) && bucket.Start.Before(end) {
			filtered = append(filtered, bucket)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Start.After(filtered[j].Start) })
	var out Counts
	var err error
	for _, bucket := range filtered {
		out, err = out.Add(bucket.Counts)
		if err != nil {
			return Counts{}, err
		}
		if out.Attempts >= 30 {
			break
		}
	}
	return out, nil
}

func IsExplore(attempts int) bool { return attempts < 30 }

func validCounts(counts Counts) bool {
	return counts.Attempts >= 0 && counts.Successes >= 0 && counts.Successes <= counts.Attempts &&
		counts.TTFTCount >= 0 &&
		finiteNonnegative(counts.SumLog) && finiteNonnegative(counts.SumSq)
}

func addCount(left, right int) (int, bool) {
	sum, carry := bits.Add64(uint64(left), uint64(right), 0)
	if carry != 0 || sum > uint64(^uint(0)>>1) {
		return 0, false
	}
	return int(sum), true
}

func finiteNonnegative(value float64) bool { return value >= 0 && isFinite(value) }

func isFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
