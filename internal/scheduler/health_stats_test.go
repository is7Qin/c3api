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
	h := NewRuntimeHealth(nil, "self", nil, nil)
	st := h.Stats().(RuntimeHealthStats)
	require.Zero(t, st.Records)
	require.False(t, st.LastTickOK, "no tick yet")
	require.Zero(t, st.LastSyncOKUnixMs)

	hkOpen := HealthKey{AccountID: 1, Quality: "*", IdentityRevision: 1}
	hkProbe := HealthKey{AccountID: 2, Quality: "*", IdentityRevision: 1}
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
	h := NewRuntimeHealth(nil, "self", nil, nil)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	probe := func(context.Context, HealthKey) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New("probe released")
	}
	hk := HealthKey{AccountID: 1, Quality: "*", IdentityRevision: 1}
	// 探针只服务 PROBING（窗口 honored）——以 PROBING 构造在飞探测夹具。
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateProbing}}})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.Start(ctx, probe))
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
	h := NewRuntimeHealth(nil, "self", nil, nil)
	require.NoError(t, h.Close(context.Background()))
	require.NoError(t, h.Close(context.Background()), "idempotent")
}

// blockingProbeHealth builds a started RuntimeHealth whose probe is parked
// in-flight (entered closed once, released only when the test closes release).
func blockingProbeHealth(t *testing.T) (*RuntimeHealth, chan struct{}, chan struct{}) {
	t.Helper()
	h := NewRuntimeHealth(nil, "self", nil, nil)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	probe := func(context.Context, HealthKey) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New("probe released")
	}
	hk := HealthKey{AccountID: 1, Quality: "*", IdentityRevision: 1}
	// 探针只服务 PROBING（窗口 honored）——以 PROBING 构造在飞探测夹具。
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{hk: {Key: hk, State: StateProbing}}})
	require.NoError(t, h.Start(context.Background(), probe))
	<-entered
	return h, entered, release
}

// TestRuntimeHealthCloseTimeoutAllowsLaterJoin pins the timeout semantics: a
// deadline-limited Close reports the incomplete join as an error AND does not
// permanently suppress a later Close—retry blocks until both loops exit, then
// returns nil; a further Close after success stays an immediate no-op.
func TestRuntimeHealthCloseTimeoutAllowsLaterJoin(t *testing.T) {
	h, _, release := blockingProbeHealth(t)

	deadCtx, dcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer dcancel()
	err := h.Close(deadCtx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "deadline-limited Close must report incomplete join")

	closeDone := make(chan error, 1)
	go func() { closeDone <- h.Close(context.Background()) }()
	select {
	case got := <-closeDone:
		close(release)
		t.Fatalf("retry Close returned before loops exited: %v", got)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-closeDone, "retry Close must join to completion")
	select {
	case <-h.probeDone:
	default:
		t.Fatal("probe loop still running after successful Close")
	}
	select {
	case <-h.syncDone:
	default:
		t.Fatal("sync loop still running after successful Close")
	}
	require.NoError(t, h.Close(context.Background()), "Close after completed join stays idempotent")
}

// TestRuntimeHealthConcurrentCloseWaitsJoin pins that no concurrent Close
// caller may report success before both loops exit: every caller independently
// joins the done signals (bounded watchdog precedent, no sleep-as-sync).
func TestRuntimeHealthConcurrentCloseWaitsJoin(t *testing.T) {
	h, _, release := blockingProbeHealth(t)

	const n = 4
	errs := make([]error, n)
	allDone := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = h.Close(context.Background())
		}(i)
	}
	go func() { wg.Wait(); close(allDone) }()
	select {
	case <-allDone:
		close(release)
		t.Fatal("concurrent Close returned while probe was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-allDone
	for i, err := range errs {
		require.NoError(t, err, "Close caller %d", i)
	}
	select {
	case <-h.probeDone:
	default:
		t.Fatal("probe loop still running after all Close returned")
	}
}
