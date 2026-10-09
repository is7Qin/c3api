// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// TestRendezvousOwnerStandardFNV pins probe-owner election to the standard
// FNV-1a 64-bit basis. The historical bug used the truncated 19-digit
// 1469598103934665603, which skewed the weight distribution; the expected
// owners below are computed with the standard basis, so a regression to the
// buggy constant fails this test.
func TestRendezvousOwnerStandardFNV(t *testing.T) {
	require.Equal(t, uint64(14695981039346656037), uint64(fnvOffset64), "FNV basis must be the standard 64-bit offset")
	require.Equal(t, uint64(1099511628211), uint64(fnvPrime64), "FNV prime must be the standard 64-bit prime")

	members := []string{"self-a", "self-b", "self-c"}
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"account:1:q:1", "self-b"},
		{"assign:2:*:1", "self-c"},
		{"", "self-a"},
	} {
		require.Equalf(t, tc.want, rendezvousOwner(tc.key, members), "key=%q", tc.key)
	}
	require.Empty(t, rendezvousOwner("any", nil), "empty member set never elects an owner")

	// All FNV sites share the single basis/prime: an independent FNV-1a over a
	// key (literal basis) must equal CacheAffinityHash.
	for _, key := range []string{"opaque-request-key", "domain.example"} {
		var want uint64 = 14695981039346656037
		for i := 0; i < len(key); i++ {
			want ^= uint64(key[i])
			want *= 1099511628211
		}
		require.Equal(t, want, CacheAffinityHash(key), "CacheAffinityHash must use the shared basis/prime")
	}
}

// TestHealthProbeCountsApplyFailures pins D4: the probe path no longer swallows
// the MarkReady/reopen-Throttle write errors — each failure is counted in
// probeErrors. A nil client makes both writes fail deterministically.
func TestHealthProbeCountsApplyFailures(t *testing.T) {
	h := NewRuntimeHealth(nil, "self", nil, nil)
	key := healthKeyFor(1, "q1", 1)
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{key: {Key: key, State: StateProbing}}})

	// Two probe successes reach MarkReady, whose write fails (no redis client).
	h.probeFn = func(context.Context, HealthKey) error { return nil }
	h.probeTick(context.Background())
	h.probeTick(context.Background())
	require.Equal(t, int64(1), h.probeErrors.Load(), "MarkReady failure must be counted")

	// A failed probe reaches the reopen Throttle, whose write fails.
	h.probeErrors.Store(0)
	h.probeFn = func(context.Context, HealthKey) error { return errors.New("probe boom") }
	h.probeTick(context.Background())
	require.Equal(t, int64(1), h.probeErrors.Load(), "reopen Throttle failure must be counted")
}

// TestMarkResultSkipsOnFingerprintError pins D4: MarkResult no longer swallows a
// fingerprint error and delivers an identity-less event; it skips and counts.
func TestMarkResultSkipsOnFingerprintError(t *testing.T) {
	tpl := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tpl, 4)})

	// Force the fingerprint to fail: candidateFingerprint requires a non-nil
	// template, so strip it in the published static leaf. (Test-only mutation of
	// a leaf the scheduler would never mutate itself.)
	snap, ok := s.View().Account(1)
	require.True(t, ok)
	av := snap.static.Load()
	accNoTpl := av.acc
	accNoTpl.Template = nil
	snap.static.Store(newSnapshotStatic(accNoTpl, av.tpl, av.groupIDs))

	require.Zero(t, s.fingerprintSkips.Load())
	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom", "m")
	require.Equal(t, uint64(1), s.fingerprintSkips.Load(), "fingerprint error must be counted and the event skipped")

	// failureEvent takes the same skip path (returns an identity-less event).
	ev := s.failureEvent(1, rule.Kind5xx, "boom")
	require.Empty(t, ev.CandidateFingerprint)
	require.Equal(t, uint64(2), s.fingerprintSkips.Load(), "failureEvent fingerprint error must be counted too")
}
