// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package main

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/scheduler"
)

// TestCompilerQualitySource pins the recorder→compiler quality adapter:
// aggregation across quality classes sharing (RouteClassID, Fingerprint),
// Q32→float conversion, token sums, and nil on an empty recorder.
func TestCompilerQualitySource(t *testing.T) {
	rec, err := quality.NewRecorder(50000)
	require.NoError(t, err)
	route := [32]byte{1}
	fp := [32]byte{7}
	k1 := quality.CanonicalKey(route, [32]byte{2}, fp)
	k2 := quality.CanonicalKey(route, [32]byte{3}, fp) // same compiler key, other quality class
	k3 := quality.CanonicalKey([32]byte{9}, [32]byte{4}, [32]byte{8})

	ttft100, ttft400 := int64(100), int64(400)
	rec.Begin(k1).CompleteObservation(quality.Observation{Success: true, TTFTMs: &ttft100, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40})
	rec.Begin(k2).CompleteObservation(quality.Observation{Success: true, TTFTMs: &ttft400, InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4})
	rec.Begin(k3).CompleteObservation(quality.Observation{Success: false, InputTokens: 5})

	src := compilerQualitySource(rec)
	q := src()
	require.Len(t, q, 2, "one entry per (route, fingerprint)")

	merged := q[scheduler.CandidateQualityKey{RouteClassID: domain.RouteClassIDVal(route), Fingerprint: domain.CandidateFingerprintVal(fp)}]
	require.Equal(t, 2, merged.Counts.Attempts)
	require.Equal(t, 2, merged.Counts.Successes)
	require.Equal(t, 2, merged.Counts.TTFTCount)
	require.InDelta(t, math.Log(100)+math.Log(400), merged.Counts.SumLog, 1e-6)
	require.InDelta(t, math.Log(100)*math.Log(100)+math.Log(400)*math.Log(400), merged.Counts.SumSq, 1e-6)
	require.Equal(t, int64(11), merged.InputTokens)
	require.Equal(t, int64(22), merged.OutputTokens)
	require.Equal(t, int64(33), merged.CacheReadTokens)
	require.Equal(t, int64(44), merged.CacheCreateTokens)

	other := q[scheduler.CandidateQualityKey{RouteClassID: domain.RouteClassIDVal([32]byte{9}), Fingerprint: domain.CandidateFingerprintVal([32]byte{8})}]
	require.Equal(t, 1, other.Counts.Attempts)
	require.Equal(t, 0, other.Counts.Successes)
	require.Zero(t, other.Counts.TTFTCount)

	// Empty recorder → nil (compile lane treats as "no quality", all-explore).
	empty, err := quality.NewRecorder(50000)
	require.NoError(t, err)
	require.Nil(t, compilerQualitySource(empty)())
}
