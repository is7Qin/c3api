// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func TestRed_FlowStaleRequeueSequenceAware(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-stale", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	owner := rec.FlowOwner()

	// Old rows A retained for minute M.
	m := fixed.Unix()
	require.NoError(t, foldConsumerRows(owner, m, []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 1, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 1},
	}))

	// Acquire lease L1 for the minute.
	_, tok1, ok := owner.snapshotForPG(m)
	require.True(t, ok, "dirty minute must offer a lease")

	// Same-minute merge B during the lease: folds into the live accumulator,
	// bumps the version, stays dirty, exceeds the captured watermark.
	require.NoError(t, foldConsumerRows(owner, m, []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 2, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 2, ChainCount: 99},
	}))

	// L1 released on the failure path without ack.
	require.True(t, owner.releasePG(tok1), "release settles the live lease")

	// The L1 token is now stale: ack and release are no-ops that change no
	// watermark, flag, lease, dirty state, or counter.
	require.False(t, owner.ackPG(tok1), "stale lease token must not ack after release")
	require.False(t, owner.releasePG(tok1), "stale lease token must not release twice")

	// Retry acquires a fresh lease identity; the stale L1 ack stays a no-op
	// even while the newer lease is active.
	_, tok2, ok := owner.snapshotForPG(m)
	require.True(t, ok, "dirty minute must offer a retry lease")
	require.NotEqual(t, tok1.leaseID, tok2.leaseID, "retry carries a fresh lease identity at the same version")
	require.False(t, owner.ackPG(tok1), "stale ack must not settle beside a newer lease")

	// Owner still holds both contributions exactly once, old-first.
	fmAfter, ok := rec.FlowMinute(m)
	require.True(t, ok)
	rows := fmAfter.FlowRows()
	require.Len(t, rows, 2, "post-snapshot merge must be covered by the retry, never duplicated")
	require.Equal(t, int64(1), rows[0].AccountID)
	require.Equal(t, int64(2), rows[1].AccountID)
	require.Equal(t, int64(1), rows[0].ChainCount)
	require.Equal(t, int64(99), rows[1].ChainCount)

	// L2 ack succeeds; the minute is clean with no next candidate.
	require.True(t, owner.ackPG(tok2))
	require.Empty(t, owner.pgCandidateMinutes(), "clean minute offers no next candidate")

	// Sync level: a clean minute persists nothing further; later deltas
	// persist the full cumulative snapshot with advancing sequence.
	w.doPG(context.Background())
	require.Empty(t, pg.flows, "clean minute must not re-persist")
	rowC := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "explore", AccountID: 3, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 3, ChainCount: 7}
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowC}))
	w.doPG(context.Background())
	rowD := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "explore", AccountID: 4, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 4, ChainCount: 11}
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowD}))
	w.doPG(context.Background())
	key := "red-flow-stale:" + fixed.UTC().Truncate(time.Minute).String()
	pg.mu.Lock()
	got := pg.flows[key]
	seq := pg.seqs[key]
	pg.mu.Unlock()
	require.Len(t, got, 4, "retry persists the full cumulative snapshot, not a delta")
	require.Equal(t, int64(2), seq, "durable sequence advances per successful snapshot")
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
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowA}))
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
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowB}))
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
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowA}))
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
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowA}))
	w.doPG(context.Background())
	failed, retained := rec.FlowMinute(m)
	require.True(t, retained, "failed delta must stay dirty-retained for next-cycle retry")
	require.Equal(t, int64(3), failed.FlowRows()[0].ChainCount, "failed cycle must not duplicate the delta")

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
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowB}))
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

// TestRed_FlowRefillMergesConcurrentSameMinuteDelta locks the post-snapshot
// merge race: when D1's upsert fails AND a fresh same-minute D2 lands while
// that very call is in flight, D2 folds into the live leased accumulator
// (version bumps, stays dirty) instead of being lost. There is no refill:
// the failed lease releases, the state stays dirty, and the next successful
// cycle persists A+B exactly once.
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
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowA}))

	// barrier: D2 merges into the live accumulator exactly while D1's upsert
	// is failing (lease active, snapshot already materialized).
	pg.onFlow = func(time.Time, int64) {
		_ = foldConsumerRows(rec.FlowOwner(), m, []repository.RoutingFlowRow{rowB})
	}
	w.doPG(context.Background())
	pg.onFlow = nil

	// the failed cycle leaves A+B merged dirty-retained, each counted once.
	merged, ok := rec.FlowMinute(m)
	require.True(t, ok, "failed cycle must leave the merged state dirty-retained")
	survived := make(map[int64]int64)
	for _, r := range merged.FlowRows() {
		survived[r.AccountID] += r.ChainCount
	}
	require.Equal(t, int64(3), survived[11], "D1 must survive the failed flush")
	require.Equal(t, int64(5), survived[22], "D2 must fold into the leased minute exactly once")

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
	require.Len(t, got, 2, "retry must persist the merged D1+D2, each exactly once")
	require.Equal(t, int64(3), counts[11], "D1 must survive the failed flush")
	require.Equal(t, int64(5), counts[22], "D2 must be conserved exactly once")
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
		require.NoError(t, foldConsumerRows(rec.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
			{IdentityVersion: 1, TerminalMinute: minute, Ordinal: 1, Lane: "primary", AccountID: int64(i + 100), TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 1},
		}))
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
	// Without pruning, these would be ~20 entries (one per minute).
	// With pruning, old minutes (< cutoff and not unsettled) must be removed,
	// so len should be bounded < 20. The flow accumulator itself is retained
	// under the owner (no detach); only unsettled minutes pin sequences.
	// pgSeq is deliberately retained (never pruned) so later same-minute
	// mutations continue the sequence instead of restarting into the
	// repository greater-sequence fence; it pins nothing and ⊆ live minutes
	// plus history, so only its presence (not its size) is asserted here.
	require.Less(t, seqLen, 20, "seq map must be pruned after successful publication without losing pending")
	require.Less(t, redisSeqLen, 20, "redisSeq must be pruned")
	require.GreaterOrEqual(t, pgSeqLen, 20, "pgSeq must be retained for sequence fencing")
	require.Less(t, committedLen, 20, "committed must be pruned")
	require.Equal(t, 0, minuteAbsLen, "minuteAbs must be drained after success")

	// Verify pending data not lost: create new minute after prune and ensure it still publishes
	newMinute := future
	w.SetClock(func() time.Time { return newMinute })
	kNew := keyOf(fpByte(99), qcByte(99))
	qmNew := NewQualityMinute(newMinute.Unix(), kNew)
	qmNew.SetAttempts(42)
	require.NoError(t, rec.EnqueueQualityMinute(qmNew))
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), newMinute.Unix(), []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: newMinute, Ordinal: 1, Lane: "primary", AccountID: 999, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 1},
	}))
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
