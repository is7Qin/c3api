// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWilson95Exact(t *testing.T) {
	interval := Wilson95(80, 100)
	require.InDelta(t, 0.711, interval.LCB, 0.02)
	require.InDelta(t, 0.867, interval.UCB, 0.02)
	require.True(t, interval.LCB <= 0.8 && 0.8 <= interval.UCB)
	zero := Wilson95(0, 30)
	require.GreaterOrEqual(t, zero.LCB, 0.0)
	require.LessOrEqual(t, zero.UCB, 0.12)
	full := Wilson95(30, 30)
	require.Greater(t, full.LCB, 0.88)
	require.Equal(t, 1.0, full.UCB)
}

func TestWilson95ZConstant(t *testing.T) {
	require.Equal(t, 1.959963984540054, WilsonZ)
	require.InDelta(t, 0, Wilson95(50, 100).LCB, 1)
}

func TestWilson95Boundaries(t *testing.T) {
	require.Equal(t, Interval{}, Wilson95(0, 0))
	require.Equal(t, Wilson95(5, 10), Wilson95(5, 10))
	require.Equal(t, Wilson95(0, 10), Wilson95(-5, 10))
	require.Equal(t, Wilson95(10, 10), Wilson95(15, 10))
}

func TestLogTTFTInterval(t *testing.T) {
	samples := make([]int64, 30)
	for index := range samples {
		samples[index] = 100
	}
	interval, ok := LogTTFTFromSamples(samples)
	require.True(t, ok)
	require.InDelta(t, 100, interval.LCB, 1)
	require.InDelta(t, 100, interval.UCB, 1)

	mixed := make([]int64, 30)
	for index := range mixed {
		if index < 15 {
			mixed[index] = 50
		} else {
			mixed[index] = 200
		}
	}
	interval, ok = LogTTFTFromSamples(mixed)
	require.True(t, ok)
	require.Less(t, interval.LCB, interval.UCB)
	require.Greater(t, interval.LCB, 50.0)
	require.Less(t, interval.UCB, 300.0)

	interval, ok = LogTTFTFromSamples(make([]int64, 29))
	require.False(t, ok)
	require.Equal(t, Interval{}, interval)
	interval, ok = LogTTFTFromSamples(make([]int64, 30))
	require.True(t, ok)
	require.InDelta(t, 1, interval.LCB, 0.5)
}

func TestLogTTFTMath(t *testing.T) {
	logged := math.Log(100)
	interval, ok := LogTTFTInterval(logged*30, logged*logged*30, 30)
	require.True(t, ok)
	require.InDelta(t, 100, interval.LCB, 1e-4)
	require.InDelta(t, 100, interval.UCB, 1e-4)
	_, ok = LogTTFTInterval(0, 0, 29)
	require.False(t, ok)
}

func TestQualityWindowBoundaries(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 34, 56, 123456789, time.UTC)
	settledStart, settledEnd, liveStart, liveEnd := CurrentWindow(now)
	minute := time.Date(2026, 8, 28, 12, 34, 0, 0, time.UTC)
	require.Equal(t, minute.Add(-5*time.Minute), settledStart)
	require.Equal(t, minute, settledEnd)
	require.Equal(t, minute, liveStart)
	require.Equal(t, now.UTC(), liveEnd)
	baselineStart, baselineEnd := BaselineWindow(now)
	require.Equal(t, minute.Add(-24*time.Hour), baselineStart)
	require.Equal(t, minute.Add(-5*time.Minute), baselineEnd)
	require.Equal(t, baselineEnd, settledStart)
}

func TestQualityAccumulation(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 10, 0, 0, time.UTC)
	buckets := []MinuteBucket{
		{Start: now.Add(-6 * time.Minute), Counts: Counts{Attempts: 100}},
		{Start: now.Add(-4 * time.Minute), Counts: Counts{Attempts: 10, Successes: 9}},
		{Start: now.Add(-2 * time.Minute), Counts: Counts{Attempts: 20, Successes: 18}},
		{Start: now.Add(-1 * time.Minute), Counts: Counts{Attempts: 30, Successes: 27}},
	}
	current, err := AccumulateCurrent(buckets, Counts{Attempts: 5, Successes: 5}, now)
	require.NoError(t, err)
	require.Equal(t, 65, current.Attempts)

	baseline, err := AccumulateBaseline([]MinuteBucket{
		{Start: now.Add(-10 * time.Minute), Counts: Counts{Attempts: 20}},
		{Start: now.Add(-20 * time.Minute), Counts: Counts{Attempts: 20}},
		{Start: now.Add(-30 * time.Minute), Counts: Counts{Attempts: 20}},
		{Start: now.Add(-6 * time.Minute), Counts: Counts{Attempts: 20}},
	}, now)
	require.NoError(t, err)
	require.Equal(t, 40, baseline.Attempts)
	empty, err := AccumulateBaseline(nil, now)
	require.NoError(t, err)
	require.Equal(t, Counts{}, empty)
}
