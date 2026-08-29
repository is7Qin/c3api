// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func TestRed_FlowStaleRequeueSequenceAware(t *testing.T) {
	_, rdb := newMiniRedis(t)
	failingPG := &typedFakePG{failAll: context.DeadlineExceeded}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, failingPG, SyncConfig{InstanceSrc: "red-flow-stale", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })

	// Prepare initial flow snapshot for minute M with old edges
	m := fixed.Unix()
	fmOld := NewFlowSnapshot(m, []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 1, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 1},
	})
	require.NoError(t, rec.EnqueueFlowMinute(fmOld))

	// barrier: doPG will attempt and fail, invoking refill
	barrier := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-barrier
		w.doPG(context.Background())
		close(done)
	}()
	close(barrier)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("doPG did not finish")
	}
	// After failure, old should be refilled (pending still has it)
	rec.mu.Lock()
	_, hasOldAfterFail := rec.pendingFlow[m]
	rec.mu.Unlock()
	require.True(t, hasOldAfterFail, "failed flow must be refilled for retry")

	// Now enqueue newer snapshot for same minute while pg still failing
	fmNew := NewFlowSnapshot(m, []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 2, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 2, ChainCount: 99},
	})
	// Advance pgSeq to simulate newer sequence already assigned via prior success path
	// To emulate sequence-aware, we will manually bump pgSeq as doPG would have done
	w.mu.Lock()
	prevSeq := w.pgSeq[m]
	w.pgSeq[m] = prevSeq + 5
	curSeq := w.pgSeq[m]
	w.mu.Unlock()
	require.NoError(t, rec.EnqueueFlowMinute(fmNew))

	// Attempt refill of stale old entry with old seq should NOT overwrite newer
	staleEntry := flowEntry{minute: m, fm: fmOld, seq: prevSeq + 1}
	// Barrier to ensure concurrent refill does not overwrite
	refillDone := make(chan struct{})
	go func() {
		w.refillFlow([]flowEntry{staleEntry})
		close(refillDone)
	}()
	select {
	case <-refillDone:
	case <-time.After(2 * time.Second):
		t.Fatal("refill did not finish")
	}
	rec.mu.Lock()
	fmAfter, ok := rec.pendingFlow[m]
	rec.mu.Unlock()
	require.True(t, ok)
	rows := fmAfter.FlowRows()
	require.Equal(t, int64(2), rows[0].AccountID, "stale failed requeue must not overwrite newer snapshot (sequence-aware)")

	// Also verify that stale seq < curSeq is dropped
	require.Greater(t, curSeq, staleEntry.seq, "test setup ensures stale seq < cur seq")
	var wg sync.WaitGroup
	wg.Add(2)
	// concurrent newer enqueue vs stale refill race
	go func() {
		defer wg.Done()
		w.refillFlow([]flowEntry{{minute: m, fm: fmOld, seq: staleEntry.seq}})
	}()
	go func() {
		defer wg.Done()
		_ = rec.EnqueueFlowMinute(fmNew)
	}()
	wg.Wait()
	rec.mu.Lock()
	fmFinal, _ := rec.pendingFlow[m]
	rec.mu.Unlock()
	require.Equal(t, int64(2), fmFinal.FlowRows()[0].AccountID, "concurrent stale refill must not win over newer")
}

func TestRed_BoundPruneLongLivedMaps(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newTypedFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-prune", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return base })

	// Simulate many distinct minutes of successful publication to grow maps
	for i := 0; i < 20; i++ {
		minute := base.Add(time.Duration(i) * time.Minute)
		w.SetClock(func() time.Time { return minute })
		k := keyOf(fpByte(byte(i)), qcByte(byte(i)))
		qm := NewQualityMinute(minute.Unix(), k)
		qm.SetAttempts(int64(i + 1))
		require.NoError(t, rec.EnqueueQualityMinute(qm))
		fm := NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
			{IdentityVersion: 1, TerminalMinute: minute, Ordinal: 1, Lane: "primary", AccountID: int64(i + 100), TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 1},
		})
		require.NoError(t, rec.EnqueueFlowMinute(fm))
		// Use barrier for doPG to ensure sequential
		done := make(chan struct{})
		go func() {
			w.doPG(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("doPG hang")
		}
		// also do redis
		w.doRedis(context.Background())
	}
	// Advance clock beyond prune cutoff (10 min) to allow pruning of old minutes
	future := base.Add(30 * time.Minute)
	w.SetClock(func() time.Time { return future })
	// trigger prune via successful empty publish (will prune)
	w.doRedis(context.Background())
	w.doPG(context.Background())

	w.mu.Lock()
	seqLen := len(w.seq)
	redisSeqLen := len(w.redisSeq)
	pgSeqLen := len(w.pgSeq)
	committedLen := len(w.committed)
	minuteAbsLen := len(w.minuteAbs)
	w.mu.Unlock()
	// Without pruning, these would be ~20 entries (one per minute)
	// With pruning, old minutes (< cutoff and not pending) must be removed, so len should be bounded < 20
	require.Less(t, seqLen, 20, "seq map must be pruned after successful publication without losing pending")
	require.Less(t, redisSeqLen, 20, "redisSeq must be pruned")
	require.Less(t, pgSeqLen, 20, "pgSeq must be pruned")
	require.Less(t, committedLen, 20, "committed must be pruned")
	require.Equal(t, 0, minuteAbsLen, "minuteAbs must be drained after success")

	// Verify pending data not lost: create new minute after prune and ensure it still publishes
	newMinute := future
	w.SetClock(func() time.Time { return newMinute })
	kNew := keyOf(fpByte(99), qcByte(99))
	qmNew := NewQualityMinute(newMinute.Unix(), kNew)
	qmNew.SetAttempts(42)
	require.NoError(t, rec.EnqueueQualityMinute(qmNew))
	fmNewCheck := NewFlowSnapshot(newMinute.Unix(), []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: newMinute, Ordinal: 1, Lane: "primary", AccountID: 999, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 1},
	})
	require.NoError(t, rec.EnqueueFlowMinute(fmNewCheck))
	done2 := make(chan struct{})
	go func() {
		w.doPG(context.Background())
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("doPG hang after prune")
	}
	require.GreaterOrEqual(t, len(pg.quality), 1, "pending data after prune must still be publishable without loss")
	// Ensure at least one of the new rows present
	found := false
	for _, row := range pg.quality {
		if row.Attempts == 42 {
			found = true
		}
	}
	require.True(t, found, "new quality row must be persisted after prune, proving no pending loss")
}
