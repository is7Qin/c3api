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

// TestQualitySync_ShutdownDrainFailureRetainsPending pins the behavior behind
// the corrected shutdown lifecycle: the sync worker drains while the recorder
// is still open, so a persistently failing PG leaves pending quality/flow
// either retained in the recorder or reported via an explicit incomplete-drain
// error — never silently dropped. Under the reversed lifecycle (recorder
// finalized before worker drain) the same failure refill hits a closed
// recorder, gets rejected as capacity, and the pending data vanishes with
// Close returning nil.
func TestQualitySync_ShutdownDrainFailureRetainsPending(t *testing.T) {
	// Given: open recorder with one pending quality row and one pending flow
	// minute; PG persistently failing with a transient (non-poison) error;
	// Redis healthy so the Redis face is not the failure source.
	_, rdb := newMiniRedis(t)
	pg := newTypedFakePG()
	pg.failAll = context.DeadlineExceeded
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "shutdown-drain-fail", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })

	minute := fixed.Unix()
	k := keyOf(fpByte(210), qcByte(210))
	qm := NewQualityMinute(minute, k)
	qm.SetAttempts(3)
	qm.SetSuccesses(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	edges := [8]int64{7, 0, 0, 0, 0, 0, 0, 0}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowMinute(minute, edges)))

	// When: worker Close runs under a bounded budget (exactly what the worker
	// manager drain does during graceful shutdown), before the recorder is
	// finalized.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	closeErr := w.Close(ctx)

	// Then: Close reports the incomplete drain explicitly instead of
	// pretending success.
	require.ErrorIs(t, closeErr, context.DeadlineExceeded)
	require.ErrorContains(t, closeErr, "drain incomplete")

	// And: nothing was silently dropped — pending rows are back in the
	// recorder for retry, all drop counters stay zero.
	rec.mu.Lock()
	qualityRows := 0
	for _, rows := range rec.pendingQuality {
		qualityRows += len(rows)
	}
	flowMinutes := len(rec.pendingFlow)
	rec.mu.Unlock()
	require.Equal(t, 1, qualityRows, "pending quality must be retained for retry after failed drain")
	require.Equal(t, 1, flowMinutes, "pending flow must be retained for retry after failed drain")
	require.Equal(t, int64(0), rec.QualityOverflow())
	require.Equal(t, int64(0), rec.FlowOverflow())
	require.Equal(t, int64(0), rec.MinuteOverflow())
	require.Equal(t, int64(0), w.poison.Load(), "transient failure must not be poison-dropped")

	// And: recorder finalization after the drained worker captures the
	// retained rows in the final snapshot — the corrected lifecycle loses
	// nothing end to end.
	require.NoError(t, rec.CloseWithContext(context.Background()))
	snap := rec.Snapshot()
	require.NotNil(t, snap)
	retained, ok := snap.Quality[minute][k]
	require.True(t, ok, "final snapshot must retain the pending quality row")
	require.Equal(t, int64(3), retained.Attempts())
	require.Equal(t, int64(1), retained.Successes())
	fm, ok := snap.Flow[minute]
	require.True(t, ok, "final snapshot must retain the pending flow minute")
	require.Equal(t, edges, fm.Edges())
}

// gatedFailingPG signals entry on the first quality upsert, blocks there until
// released, then fails every call with a transient (non-poison) error so the
// owning flush refills the recorder on its way out.
type gatedFailingPG struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedFailingPG) UpsertQualityAndMarkDirty(ctx context.Context, row repository.RoutingQualityRow) error {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return context.DeadlineExceeded
}

func (g *gatedFailingPG) UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, identityVersion int16, absoluteSequence int64, rows []repository.RoutingFlowRow) error {
	return context.DeadlineExceeded
}

// TestQualitySync_CloseWaitsForInflightFlushNoLateRefill pins the Close
// lifecycle barrier (review blocker 2026-08-31): Close must not return while an
// in-flight flush still holds flushMu, because that flush's failure-refill
// lands in the recorder — and the shutdown tail finalizes the recorder right
// after Close returns, turning a late refill into a silent drop. The old
// abandon path returned at drainCtx+grace while the flush was still stuck;
// the fixed path blocks until the flush releases, keeps the explicit
// incomplete-drain error, and the refill is visible in the final snapshot.
// Barriers only (entered/release channels + one watchdog bounding the old
// abandon time); no sleep-race masking.
func TestQualitySync_CloseWaitsForInflightFlushNoLateRefill(t *testing.T) {
	// Given: open recorder with one pending quality row and a flush stuck
	// inside the PG writer holding flushMu (simulates the loop's in-flight doPG).
	_, rdb := newMiniRedis(t)
	pg := &gatedFailingPG{entered: make(chan struct{}), release: make(chan struct{})}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "close-barrier", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	// Old abandon path returned at drainCtx(50ms)+grace(10ms)=~60ms; the
	// watchdog below fires at 300ms, so any early return is the bug itself.
	w.inflightAbandonGrace = 10 * time.Millisecond

	minute := fixed.Unix()
	k := keyOf(fpByte(240), qcByte(240))
	qm := NewQualityMinute(minute, k)
	qm.SetAttempts(5)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	require.NoError(t, w.Start(context.Background()))

	flushDone := make(chan struct{})
	go func() {
		w.doPG(context.Background())
		close(flushDone)
	}()
	<-pg.entered

	// When: Close runs under a budget far shorter than the stuck flush.
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		closeDone <- w.Close(ctx)
	}()

	// Then: Close must not return while the flush is in flight.
	select {
	case cerr := <-closeDone:
		close(pg.release)
		<-flushDone
		t.Fatalf("Close returned while an in-flight flush could still refill the recorder: %v", cerr)
	case <-time.After(300 * time.Millisecond):
	}
	close(pg.release)

	// Explicit incomplete-drain semantics are preserved despite the wait.
	cerr := <-closeDone
	require.ErrorIs(t, cerr, context.DeadlineExceeded)
	require.ErrorContains(t, cerr, "drain incomplete")
	<-flushDone

	// And: the failed flush's refill landed before Close returned, so the
	// recorder finalization that follows Close retains the row instead of
	// rejecting a late refill into a closed recorder.
	require.NoError(t, rec.CloseWithContext(context.Background()))
	snap := rec.Snapshot()
	retained, ok := snap.Quality[minute][k]
	require.True(t, ok, "refill must land before Close returns, not after recorder finalization")
	require.Equal(t, int64(5), retained.Attempts())
}
