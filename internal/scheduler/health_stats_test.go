// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRuntimeHealthStats pins the ops-face projection: per-state counts over
// the live view, generation, and freshness/error counters (0/false = never
// ticked).
func TestRuntimeHealthStats(t *testing.T) {
	h := NewRuntimeHealth(nil, "self", nil, nil, nil)
	st := h.Stats().(RuntimeHealthStats)
	require.Zero(t, st.Records)
	require.False(t, st.LastTickOK, "no tick yet")
	require.Zero(t, st.LastSyncOKUnixMs)

	hkOpen := HealthKey{AccountID: 1, Quality: "*", Revision: 1}
	hkProbe := HealthKey{AccountID: 2, Quality: "*", Revision: 1}
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		hkOpen:  {Key: hkOpen, State: StateOPEN},
		hkProbe: {Key: hkProbe, State: StateProbing},
	}})
	h.curGen.Store(7)
	h.lastSyncOkMs.Store(1234)
	h.syncErrors.Add(2)
	h.lastTickOk.Store(true)

	st = h.Stats().(RuntimeHealthStats)
	require.Equal(t, 2, st.Records)
	require.Equal(t, 1, st.Open)
	require.Equal(t, 1, st.Probing)
	require.Zero(t, st.RetryAfter)
	require.Zero(t, st.Ready)
	require.Equal(t, int64(7), st.Generation)
	require.Equal(t, int64(1234), st.LastSyncOKUnixMs)
	require.Equal(t, int64(2), st.SyncErrors)
	require.True(t, st.LastTickOK)
}

// TestRuntimeHealthCloseJoinsProbeLoop pins the orderly-shutdown contract:
// Close must not return while a probe is in flight (both loops joined).
// Barriers + one bounded watchdog (quality-sync precedent), no sleeps-as-sync.
func TestRuntimeHealthCloseJoinsProbeLoop(t *testing.T) {
	h := NewRuntimeHealth(nil, "self", nil, nil, nil)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	h.SetProbeFn(func(context.Context, HealthKey) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New("probe released")
	})
	hk := HealthKey{AccountID: 1, Quality: "*", Revision: 1}
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateOPEN}}})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.Start(ctx))
	<-entered // probe in flight; the loop cannot exit until released
	cancel()

	closeDone := make(chan error, 1)
	go func() { closeDone <- h.Close(context.Background()) }()
	select {
	case err := <-closeDone:
		close(release)
		t.Fatalf("Close returned while a probe was in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-closeDone)
	select {
	case <-h.probeDone:
	default:
		t.Fatal("probe loop still running after Close returned")
	}
	select {
	case <-h.syncDone:
	default:
		t.Fatal("sync loop still running after Close returned")
	}
}

// TestRuntimeHealthCloseUnstartedSafe pins the worker contract: Close before
// Start is a no-op (cancel absent), never blocks.
func TestRuntimeHealthCloseUnstartedSafe(t *testing.T) {
	h := NewRuntimeHealth(nil, "self", nil, nil, nil)
	require.NoError(t, h.Close(context.Background()))
	require.NoError(t, h.Close(context.Background()), "idempotent")
}
