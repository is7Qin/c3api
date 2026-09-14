// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// fakeWindowBackend is a PG-free windowSettledBackend: records call counts and
// serves canned rollup rows for the adapter-conversion tests.
type fakeWindowBackend struct {
	cur     []repository.WindowCurrentStat
	base    []repository.WindowBaselineStat
	q1Calls int
	q2Calls int
	version int16
	lastM   time.Time
	lastHot []repository.WindowHotKey
}

func (f *fakeWindowBackend) QueryCurrentWindowStats(_ context.Context, v int16, m time.Time) ([]repository.WindowCurrentStat, error) {
	f.q1Calls++
	f.version = v
	f.lastM = m
	return f.cur, nil
}

func (f *fakeWindowBackend) QueryBaselineTruncated(_ context.Context, _ int16, _ time.Time, hot []repository.WindowHotKey) ([]repository.WindowBaselineStat, error) {
	f.q2Calls++
	f.lastHot = append([]repository.WindowHotKey(nil), hot...)
	return f.base, nil
}

// TestNewWindowedQualityProvider pins the recorder→provider + PG→provider
// adapters: aggregation across quality classes sharing (RouteClassID,
// Fingerprint), Q32→float conversion, token sums, M-cached PG reads, and the
// identity-version contract on both halves.
func TestNewWindowedQualityProvider(t *testing.T) {
	rec, err := quality.NewRecorder(50000)
	require.NoError(t, err)
	var route domain.RouteClassIDVal
	route[0] = 1
	var fp domain.CandidateFingerprintVal
	fp[0] = 7
	k1 := quality.CanonicalKey(route, [32]byte{2}, fp)
	k2 := quality.CanonicalKey(route, [32]byte{3}, fp) // same compiler key, other quality class
	k3 := quality.CanonicalKey([32]byte{9}, [32]byte{4}, [32]byte{8})

	ttft100, ttft400 := int64(100), int64(400)
	rec.Begin(k1).CompleteObservation(quality.Observation{Success: true, TTFTMs: &ttft100, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40})
	rec.Begin(k2).CompleteObservation(quality.Observation{Success: true, TTFTMs: &ttft400, InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4})
	rec.Begin(k3).CompleteObservation(quality.Observation{Success: false, InputTokens: 5})

	fixed := time.Date(2026, time.August, 29, 12, 0, 30, 0, time.UTC)
	m := fixed.UTC().Truncate(time.Minute)
	backend := &fakeWindowBackend{
		cur: []repository.WindowCurrentStat{{
			RouteClassID: route, Fingerprint: fp,
			Attempts: 30, Successes: 21, TTFTN: 20,
			TTFTSumLogQ32: int64(math.Log(100)*float64(int64(1)<<32)) * 20,
			InputTokens:   300, OutputTokens: 150,
		}},
		base: []repository.WindowBaselineStat{{
			RouteClassID: route, Fingerprint: fp, Attempts: 60, Successes: 55,
		}},
	}

	provider := NewWindowedQualityProvider(rec, backend)
	wq := provider(fixed)

	require.True(t, wq.SettledBoundary.Equal(m))
	require.Equal(t, int16(domain.RoutingIdentityVersion), backend.version)
	require.True(t, backend.lastM.Equal(m))

	// Settled (30/21) + live (2/2 across quality classes) tile per key.
	ck := scheduler.CandidateQualityKey{RouteClassID: route, Fingerprint: fp}
	merged := wq.Current[ck]
	require.Equal(t, 32, merged.Counts.Attempts)
	require.Equal(t, 23, merged.Counts.Successes)
	require.Equal(t, 22, merged.Counts.TTFTCount)
	require.InDelta(t, math.Log(100)*21+math.Log(400), merged.Counts.SumLog, 1e-6)
	require.Equal(t, int64(311), merged.InputTokens)
	require.Equal(t, int64(172), merged.OutputTokens)
	require.Equal(t, int64(33), merged.CacheReadTokens)
	require.Equal(t, int64(44), merged.CacheCreateTokens)

	other := wq.Current[scheduler.CandidateQualityKey{
		RouteClassID: domain.RouteClassIDVal([32]byte{9}),
		Fingerprint:  domain.CandidateFingerprintVal([32]byte{8}),
	}]
	require.Equal(t, 1, other.Counts.Attempts)
	require.Equal(t, 0, other.Counts.Successes)

	require.Equal(t, scheduler.Counts{Attempts: 60, Successes: 55}, wq.Baseline[ck])

	// M-keyed cache: same-M refire serves PG from cache, live stays fresh.
	provider(fixed.Add(10 * time.Second))
	require.Equal(t, 1, backend.q1Calls)
	require.Equal(t, 1, backend.q2Calls)
}
