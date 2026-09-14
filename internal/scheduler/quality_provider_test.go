// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Phase 3 RED: windowed quality provider core — settled PG (Q1+Q2) merged
// with live rows, M-keyed PG cache, hot gating, fail-closed conversions.

var providerFixed = time.Date(2026, time.August, 29, 12, 0, 30, 0, time.UTC)

type fakeSettledSource struct {
	cur     []WindowSettledCurrent
	base    map[CandidateQualityKey]WindowSettledBaseline
	q1Calls int
	q2Calls int
	q1Err   error
	q2Err   error
	version int16
	lastHot []WindowSettledHotKey
	lastM   time.Time
}

func (f *fakeSettledSource) QueryCurrentWindowStats(_ context.Context, v int16, m time.Time) ([]WindowSettledCurrent, error) {
	f.q1Calls++
	f.version = v
	f.lastM = m
	if f.q1Err != nil {
		return nil, f.q1Err
	}
	return f.cur, nil
}

func (f *fakeSettledSource) QueryBaselineTruncated(_ context.Context, _ int16, _ time.Time, hot []WindowSettledHotKey) ([]WindowSettledBaseline, error) {
	f.q2Calls++
	f.lastHot = append([]WindowSettledHotKey(nil), hot...)
	if f.q2Err != nil {
		return nil, f.q2Err
	}
	out := make([]WindowSettledBaseline, 0, len(hot))
	for _, h := range hot {
		if b, ok := f.base[CandidateQualityKey{RouteClassID: h.RouteClassID, Fingerprint: h.Fingerprint}]; ok {
			out = append(out, b)
		}
	}
	return out, nil
}

type fakeLiveSource struct {
	rows  []WindowLiveRow
	calls int
}

func (f *fakeLiveSource) UnflushedMinutes(_ time.Time) []WindowLiveRow {
	f.calls++
	return f.rows
}

func providerKey(route, fp byte) CandidateQualityKey {
	var rc domain.RouteClassIDVal
	var f domain.CandidateFingerprintVal
	rc[0] = route
	f[0] = fp
	return CandidateQualityKey{RouteClassID: rc, Fingerprint: f}
}

func q32(f float64) int64 { return int64(f * float64(int64(1)<<32)) }

func TestWindowedQualityProvider(t *testing.T) {
	m := providerFixed.UTC().Truncate(time.Minute)
	k := providerKey(1, 7)
	logged := math.Log(100)

	settled := &fakeSettledSource{
		cur: []WindowSettledCurrent{{
			Key:             k,
			Attempts:         30,
			Successes:        21,
			TTFTN:            20,
			SumLogQ32:        q32(logged) * 20,
			SumSqQ32:         q32(logged*logged) * 20,
			InputTokens:      300,
			OutputTokens:     150,
			CacheReadTokens:  10,
		}},
		base: map[CandidateQualityKey]WindowSettledBaseline{
			k: {Key: k, Attempts: 60, Successes: 55},
		},
	}
	live := &fakeLiveSource{rows: []WindowLiveRow{{
		Minute:          m.Add(-time.Minute).Unix(),
		IdentityVersion: int16(domain.RoutingIdentityVersion),
		RouteClassID:    k.RouteClassID,
		Fingerprint:     k.Fingerprint,
		Attempts:        5,
		Successes:       4,
		TTFTCount:       4,
		SumLogQ32:       q32(logged) * 4,
		SumSqQ32:        q32(logged*logged) * 4,
		InputTokens:     50,
		OutputTokens:    25,
	}, {
		// Stale live minute: outside [M-5m, M), excluded from current.
		Minute:          m.Add(-10 * time.Minute).Unix(),
		IdentityVersion: int16(domain.RoutingIdentityVersion),
		RouteClassID:    k.RouteClassID,
		Fingerprint:     k.Fingerprint,
		Attempts:        1000,
		Successes:       1000,
	}, {
		// Identity-version mismatch: ignored.
		Minute:          m.Add(-time.Minute).Unix(),
		IdentityVersion: 2,
		RouteClassID:    k.RouteClassID,
		Fingerprint:     k.Fingerprint,
		Attempts:        1000,
		Successes:       1000,
	}}}

	provider := NewWindowedQualitySource(settled, live)
	wq := provider(providerFixed)

	// SettledBoundary is the minute M.
	require.True(t, wq.SettledBoundary.Equal(m), "boundary is M, computed once per fire")

	// Current tiling equals the AccumulateCurrent oracle: settled bucket in
	// [M-5m, M) plus the live delta (stale + version-mismatched excluded).
	oracle, err := AccumulateCurrent([]MinuteBucket{{
		Start:  m.Add(-2 * time.Minute),
		Counts: Counts{Attempts: 30, Successes: 21, TTFTCount: 20, SumLog: logged * 20, SumSq: logged * logged * 20},
	}}, Counts{Attempts: 5, Successes: 4, TTFTCount: 4, SumLog: logged * 4, SumSq: logged * logged * 4}, providerFixed)
	require.NoError(t, err)
	got := wq.Current[k]
	require.Equal(t, oracle.Attempts, got.Counts.Attempts)
	require.Equal(t, oracle.Successes, got.Counts.Successes)
	require.Equal(t, oracle.TTFTCount, got.Counts.TTFTCount)
	require.InDelta(t, oracle.SumLog, got.Counts.SumLog, 1e-6)
	require.InDelta(t, oracle.SumSq, got.Counts.SumSq, 1e-6)
	require.Equal(t, int64(350), got.InputTokens)
	require.Equal(t, int64(175), got.OutputTokens)
	require.Equal(t, int64(10), got.CacheReadTokens)

	// Baseline from PG only; hot key derived from merged current (30+5 ≥ 30).
	require.Equal(t, Counts{Attempts: 60, Successes: 55}, wq.Baseline[k])
	require.Len(t, settled.lastHot, 1)
	require.Equal(t, k.RouteClassID, settled.lastHot[0].RouteClassID)
	require.Equal(t, k.Fingerprint, settled.lastHot[0].Fingerprint)
	require.Equal(t, int16(domain.RoutingIdentityVersion), settled.version)

	// M-keyed cache: same-M refires skip PG; live stays fresh per call.
	wq2 := provider(providerFixed.Add(10 * time.Second))
	require.Equal(t, 1, settled.q1Calls)
	require.Equal(t, 1, settled.q2Calls)
	require.Equal(t, 2, live.calls)
	require.Equal(t, wq.Current, wq2.Current, "deterministic within M")
	require.Equal(t, wq.Baseline, wq2.Baseline)

	// M advance refetches PG once.
	provider(providerFixed.Add(time.Minute))
	require.Equal(t, 2, settled.q1Calls)
	require.Equal(t, 2, settled.q2Calls)
}

func TestWindowedQualityProviderHotGating(t *testing.T) {
	kHot := providerKey(2, 1)
	kCold := providerKey(2, 2)
	settled := &fakeSettledSource{
		cur: []WindowSettledCurrent{
			{Key: kHot, Attempts: 25, Successes: 20},
			{Key: kCold, Attempts: 25, Successes: 20},
		},
		base: map[CandidateQualityKey]WindowSettledBaseline{
			kHot:  {Key: kHot, Attempts: 40, Successes: 35},
			kCold: {Key: kCold, Attempts: 40, Successes: 35},
		},
	}
	m := providerFixed.UTC().Truncate(time.Minute)
	live := &fakeLiveSource{rows: []WindowLiveRow{{
		// kHot crosses 30 via live; kCold stays below.
		Minute: m.Add(-time.Minute).Unix(), IdentityVersion: int16(domain.RoutingIdentityVersion),
		RouteClassID: kHot.RouteClassID, Fingerprint: kHot.Fingerprint, Attempts: 10, Successes: 9,
	}}}
	wq := NewWindowedQualitySource(settled, live)(providerFixed)
	require.Len(t, settled.lastHot, 1)
	require.Equal(t, kHot.Fingerprint, settled.lastHot[0].Fingerprint)
	require.Contains(t, wq.Baseline, kHot, "hot baseline present")
	require.NotContains(t, wq.Baseline, kCold, "cold candidate has no baseline until next M")
	require.Contains(t, wq.Current, kCold, "lanes still serve current-only candidates")
}

func TestWindowedQualityProviderPGErrorLiveOnly(t *testing.T) {
	k := providerKey(3, 3)
	settled := &fakeSettledSource{q1Err: context.DeadlineExceeded}
	m := providerFixed.UTC().Truncate(time.Minute)
	live := &fakeLiveSource{rows: []WindowLiveRow{{
		Minute: m.Add(-time.Minute).Unix(), IdentityVersion: int16(domain.RoutingIdentityVersion),
		RouteClassID: k.RouteClassID, Fingerprint: k.Fingerprint, Attempts: 8, Successes: 6,
	}}}
	provider := NewWindowedQualitySource(settled, live)
	wq := provider(providerFixed)
	require.Contains(t, wq.Current, k, "live survives PG failure")
	require.Empty(t, wq.Baseline, "no stale baseline on PG failure")
	require.True(t, wq.SettledBoundary.Equal(m))
	// Same-M refire does not hammer the failing PG.
	provider(providerFixed.Add(5 * time.Second))
	require.Equal(t, 1, settled.q1Calls)
	require.Equal(t, 0, settled.q2Calls)
	// Next M retries.
	provider(providerFixed.Add(time.Minute))
	require.Equal(t, 2, settled.q1Calls)
}

func TestWindowedQualityProviderNilSources(t *testing.T) {
	m := providerFixed.UTC().Truncate(time.Minute)
	wq := NewWindowedQualitySource(nil, nil)(providerFixed)
	require.Empty(t, wq.Current)
	require.Empty(t, wq.Baseline)
	require.True(t, wq.SettledBoundary.Equal(m))
}
