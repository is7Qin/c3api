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
	_, hasOldAfterFail := rec.FlowMinute(m)
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
	fmAfter, ok := rec.FlowMinute(m)
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
	fmFinal, _ := rec.FlowMinute(m)
	require.Equal(t, int64(2), fmFinal.FlowRows()[0].AccountID, "concurrent stale refill must not win over newer")
}

// TestRed_FlowCumulativeSnapshotAcrossCycles locks the cross-cycle contract:
// UpsertFlowSnapshot replaces the whole (minute, instance, identity) edge set,
// so every successful cycle must persist previous committed snapshot + newly
// drained delta, not just the delta.
func TestRed_FlowCumulativeSnapshotAcrossCycles(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-cum", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return clk })

	m := fixed.Unix()
	rowA := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "primary", AccountID: 11, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 3}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowA})))
	w.doPG(context.Background())
	key := "red-flow-cum:" + fixed.UTC().Truncate(time.Minute).String()
	pg.mu.Lock()
	first := pg.flows[key]
	pg.mu.Unlock()
	require.Len(t, first, 1, "cycle 1 persists delta A")
	require.Equal(t, int64(3), first[0].ChainCount)

	// cycle 2: distinct edge B for the same minute must land on top of A
	rowB := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "explore", AccountID: 22, TransitionReason: "init", Outcome: "error", IsTerminal: true, Generation: 2, ChainCount: 5}
	clk = fixed.Add(time.Second)
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowB})))
	w.doPG(context.Background())
	pg.mu.Lock()
	second := pg.flows[key]
	pg.mu.Unlock()
	require.Len(t, second, 2, "cycle 2 payload must be cumulative A+B, not delta-only B")
	counts := make(map[int64]int64, len(second))
	for _, r := range second {
		counts[r.AccountID] += r.ChainCount
	}
	require.Equal(t, int64(3), counts[11], "chain count of committed A conserved")
	require.Equal(t, int64(5), counts[22], "chain count of new delta B present")

	// cycle 3: same edge identity as A folds into it (sum), never duplicates
	clk = fixed.Add(2 * time.Second)
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowA})))
	w.doPG(context.Background())
	pg.mu.Lock()
	third := pg.flows[key]
	pg.mu.Unlock()
	require.Len(t, third, 2, "identical edge identity must merge, not append")
	counts = make(map[int64]int64, len(third))
	for _, r := range third {
		counts[r.AccountID] += r.ChainCount
	}
	require.Equal(t, int64(6), counts[11], "repeat of A sums chain counts")
	require.Equal(t, int64(5), counts[22])
}

// TestRed_FlowFailedRequeueNoDuplicate locks that committed state advances only
// after a successful upsert: a failed cycle requeues the delta and the retry
// must persist it exactly once on top of the prior committed snapshot.
func TestRed_FlowFailedRequeueNoDuplicate(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	pg.failAll = true
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-retry", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return clk })

	m := fixed.Unix()
	rowA := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "primary", AccountID: 11, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 3}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowA})))
	w.doPG(context.Background())
	_, requeued := rec.FlowMinute(m)
	require.True(t, requeued, "failed delta must be requeued")

	// retry succeeds: A persisted exactly once (chain count 3, not 6)
	pg.failAll = false
	clk = fixed.Add(time.Second)
	w.doPG(context.Background())
	key := "red-flow-retry:" + fixed.UTC().Truncate(time.Minute).String()
	pg.mu.Lock()
	got := pg.flows[key]
	pg.mu.Unlock()
	require.Len(t, got, 1)
	require.Equal(t, int64(3), got[0].ChainCount, "retry after failure must not duplicate the requeued delta")

	// failure after a committed success: B fails, retry must carry A+B once
	rowB := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "explore", AccountID: 22, TransitionReason: "init", Outcome: "error", IsTerminal: true, Generation: 2, ChainCount: 5}
	pg.failAll = true
	clk = fixed.Add(2 * time.Second)
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowB})))
	w.doPG(context.Background())
	pg.failAll = false
	clk = fixed.Add(3 * time.Second)
	w.doPG(context.Background())
	pg.mu.Lock()
	got = pg.flows[key]
	pg.mu.Unlock()
	require.Len(t, got, 2, "successful retry after committed state must persist cumulative A+B")
	counts := make(map[int64]int64, len(got))
	for _, r := range got {
		counts[r.AccountID] += r.ChainCount
	}
	require.Equal(t, int64(3), counts[11], "committed A must not be duplicated by B's retry")
	require.Equal(t, int64(5), counts[22])
}

// TestRed_FlowRefillMergesConcurrentSameMinuteDelta locks the refill race: when
// D1's upsert fails AND a fresh same-minute D2 lands in pendingFlow during that
// very call, the refill must merge D1 back in (EnqueueFlowMinute sums distinct
// edges) instead of skipping on "pending already exists" — skipping drops D1
// permanently. The next successful cycle must persist A+B exactly once.
func TestRed_FlowRefillMergesConcurrentSameMinuteDelta(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	pg.failAll = true
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-refill-race", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return clk })

	m := fixed.Unix()
	rowA := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "primary", AccountID: 11, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 3}
	rowB := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "explore", AccountID: 22, TransitionReason: "init", Outcome: "error", IsTerminal: true, Generation: 2, ChainCount: 5}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowA})))

	// barrier: D2 enters pendingFlow exactly while D1's upsert is failing
	pg.onFlow = func(time.Time, int64) {
		_ = rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowB}))
	}
	w.doPG(context.Background())
	pg.onFlow = nil

	// retry succeeds: must carry the merged D1+D2, each counted once
	pg.failAll = false
	clk = fixed.Add(time.Second)
	w.doPG(context.Background())
	key := "red-flow-refill-race:" + fixed.UTC().Truncate(time.Minute).String()
	pg.mu.Lock()
	got := pg.flows[key]
	pg.mu.Unlock()
	counts := make(map[int64]int64, len(got))
	for _, r := range got {
		counts[r.AccountID] += r.ChainCount
	}
	require.Len(t, got, 2, "refill must merge D1 into concurrent pending D2, not drop it")
	require.Equal(t, int64(3), counts[11], "D1 must survive the failed flush")
	require.Equal(t, int64(5), counts[22], "D2 must be conserved exactly once")
}

// TestRed_FlowEmptyMarkerSemantics locks both empty-snapshot sides: the marker
// is authoritative for a never-populated minute (empty payload, sequence
// advances) and must never erase a committed non-empty accumulation.
func TestRed_FlowEmptyMarkerSemantics(t *testing.T) {
	_, rdb := newMiniRedis(t)
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	// never-populated: empty marker flushes an authoritative empty snapshot
	pg := newFakePG()
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-empty-first", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return clk })
	m := fixed.Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewEmptyFlowSnapshot(m)))
	w.doPG(context.Background())
	key := "red-flow-empty-first:" + fixed.UTC().Truncate(time.Minute).String()
	pg.mu.Lock()
	rows, has := pg.flows[key]
	seq := pg.seqs[key]
	pg.mu.Unlock()
	require.True(t, has, "empty marker for never-populated minute must still be published")
	require.Len(t, rows, 0)
	require.GreaterOrEqual(t, seq, int64(1), "empty snapshot must advance sequence")

	// populated: empty marker after committed rows must not erase them
	pg2 := newFakePG()
	rec2, err := NewRecorder(50000)
	require.NoError(t, err)
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "red-flow-empty-after", BatchSize: 10}, nil)
	w2.SetClock(func() time.Time { return clk })
	rowA := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "primary", AccountID: 11, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 3}
	require.NoError(t, rec2.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowA})))
	w2.doPG(context.Background())
	require.NoError(t, rec2.EnqueueFlowMinute(NewEmptyFlowSnapshot(m)))
	clk = fixed.Add(time.Second)
	w2.doPG(context.Background())
	key2 := "red-flow-empty-after:" + fixed.UTC().Truncate(time.Minute).String()
	pg2.mu.Lock()
	got := pg2.flows[key2]
	pg2.mu.Unlock()
	require.Len(t, got, 1, "empty marker must not erase accumulated committed rows")
	require.Equal(t, int64(3), got[0].ChainCount)
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
	committedFlowLen := len(w.committedFlow)
	minuteAbsLen := len(w.minuteAbs)
	w.mu.Unlock()
	// Without pruning, these would be ~20 entries (one per minute)
	// With pruning, old minutes (< cutoff and not pending) must be removed, so len should be bounded < 20
	require.Less(t, seqLen, 20, "seq map must be pruned after successful publication without losing pending")
	require.Less(t, redisSeqLen, 20, "redisSeq must be pruned")
	require.Less(t, pgSeqLen, 20, "pgSeq must be pruned")
	require.Less(t, committedLen, 20, "committed must be pruned")
	require.Less(t, committedFlowLen, 20, "committedFlow accumulator must be pruned under the same policy")
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
