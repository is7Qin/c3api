// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Todo 1 behavioral red set for the identity-indexed flow accumulator
// (docs/superpowers/specs/identity-indexed-flow-accumulator.md section 11).
// Every test compiles against current symbols only: the synchronous
// enqueue(NewFlowSnapshot(...)) seam, existing doPGLocked/owner paths, the
// current SyncWorker plus current Close, current submit/drain seams, and the
// existing FlowOwner.SnapshotStats. No snapshot/ack/lease/seal skeleton name
// is referenced. Each test fails today on its named behavioral assertion.

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/worker"
)

// redFlowRow builds a distinct flow edge identity per account: fingerprint is
// derived from the account so preloaded retained identities stay distinct.
func redFlowRow(minute time.Time, account int64) repository.RoutingFlowRow {
	row := flowTestRow(minute, 1, account, "success", true)
	return row
}

// redFlowBlockingPG blocks inside UpsertFlowSnapshot until release is closed,
// letting a test hold a PG flush in flight behind a barrier. entered fires
// exactly once when the first flow upsert arrives.
type redFlowBlockingPG struct {
	inner   *fakePG
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (f *redFlowBlockingPG) UpsertQualityAndMarkDirty(context.Context, repository.RoutingQualityRow) error {
	return nil
}

func (f *redFlowBlockingPG) UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, v int16, seq int64, rows []repository.RoutingFlowRow) error {
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
	case <-ctx.Done():
	}
	return f.inner.UpsertFlowSnapshot(ctx, instanceSrc, terminalMinute, v, seq, rows)
}

func redFlowWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// TestRed_FlowAccumulatorDuplicateMergeBytesScaleWithIncomingOnly pins the
// scaling law: folding one fixed at-most-8-row duplicate chain must cost
// bytes proportional to the incoming chain only, never to the retained
// distinct identity count. Preload happens outside the measured closures;
// Submit plus lookup is never measured. AllocsPerOp is secondary evidence.
func TestRed_FlowAccumulatorDuplicateMergeBytesScaleWithIncomingOnly(t *testing.T) {
	fixed := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	minute := fixed.Unix()
	measure := func(retained int) (bytesPerOp, allocsPerOp float64) {
		rec, err := NewRecorder(50000)
		require.NoError(t, err)
		for i := 0; i < retained; i++ {
			require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(minute, []repository.RoutingFlowRow{redFlowRow(fixed, int64(i))})))
		}
		dup := make([]repository.RoutingFlowRow, 0, 8)
		for i := 0; i < 8; i++ {
			dup = append(dup, redFlowRow(fixed, int64(i)))
		}
		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = rec.EnqueueFlowMinute(NewFlowSnapshot(minute, dup))
			}
		})
		return float64(res.AllocedBytesPerOp()), float64(res.AllocsPerOp())
	}
	smallBop, smallAllocs := measure(10)
	largeBop, largeAllocs := measure(2000)
	t.Logf("duplicate-merge bytes/op: retained-10=%.1f retained-2000=%.1f (allocs/op secondary: %.1f vs %.1f)",
		smallBop, largeBop, smallAllocs, largeAllocs)
	require.LessOrEqual(t, largeBop, smallBop*2, "merge bytes must scale with incoming chain only, not retained identities")
	require.LessOrEqual(t, largeBop-smallBop, 4096.0, "retained-count bytes delta must stay within one minute charge")
}

// TestRed_FlowOwnerRetainsCumulativeAfterPGSuccess: after a current doPG
// success the owner lookup must still retain the cumulative minute. Fails
// today because the old detach path removes owner state.
func TestRed_FlowOwnerRetainsCumulativeAfterPGSuccess(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-retain", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })

	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	w.doPG(context.Background())
	fm, ok := rec.FlowOwner().lookup(m)
	require.True(t, ok, "owner must retain the cumulative minute after PG success")
	require.Len(t, fm.FlowRows(), 1)
	require.Equal(t, int64(11), fm.FlowRows()[0].AccountID)
}

// TestRed_FlowInFlightPGStateCannotBeDisplaced: while a fake PG blocks on a
// barrier with one minute submitted, owner.lookup(minute) must remain true
// under minute-cap pressure and a new minute cannot be admitted. Fails today
// at the lookup because the old detach removed the state while PG blocked.
func TestRed_FlowInFlightPGStateCannotBeDisplaced(t *testing.T) {
	_, rdb := newMiniRedis(t)
	release := make(chan struct{})
	pg := &redFlowBlockingPG{inner: newFakePG(), entered: make(chan struct{}), release: release}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	rec.minuteCap = 1
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-inflight", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })

	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.doPG(context.Background())
	}()
	redFlowWait(t, pg.entered, "in-flight PG upsert")
	_, ok := rec.FlowOwner().lookup(m)
	require.True(t, ok, "in-flight PG minute must remain owner-visible under cap pressure")
	require.ErrorIs(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m+60, []repository.RoutingFlowRow{ownerTestRow(22)})),
		ErrCapacity, "a new minute cannot be admitted while the capped minute is retained")
	close(release)
	redFlowWait(t, done, "blocked doPG")
}

// TestRed_FlowSyncCloseSealsLaterSubmissionResidual: after the current
// SyncWorker.Close, a later submission drained through the owner must land
// residual (residual_rows/residual_submissions), never PG-open accepted.
// Compiles today on current Close/submit/drain seams plus SnapshotStats;
// fails behaviorally because current code keeps accepted-class handling.
func TestRed_FlowSyncCloseSealsLaterSubmissionResidual(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-seal", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	require.NoError(t, w.Close(context.Background()))

	before := rec.FlowOwner().SnapshotStats()
	m := fixed.Truncate(time.Minute).Unix()
	require.Equal(t, SubmitAccepted, rec.FlowOwner().Submit(m, []repository.RoutingFlowRow{ownerTestRow(11)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	after := rec.FlowOwner().SnapshotStats()
	require.GreaterOrEqual(t, after.ResidualSubmissions, before.ResidualSubmissions+1,
		"post-Close submission must be residual-classified")
	require.GreaterOrEqual(t, after.ResidualRows, before.ResidualRows+1,
		"post-Close submission rows must be residual-classified")
	require.Equal(t, before.EdgeRowsAccepted, after.EdgeRowsAccepted,
		"post-Close submission must not be PG-open accepted")
	require.Equal(t, before.Processed, after.Processed,
		"post-Close submission must not count as processed")
}

// redFlowHangingRedis returns a client whose server accepts connections and
// never replies, so the sync loop blocks inside its first Redis publish.
// entered fires on the first accepted connection. Cleanup unblocks and closes
// everything; the loop then observes the cancelled context and exits.
func redFlowHangingRedis(t *testing.T) (*redis.Client, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	hold := make(chan struct{})
	t.Cleanup(func() { ln.Close() })
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	entered := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			once.Do(func() { close(entered) })
			go func(conn net.Conn) {
				<-hold
				conn.Close()
			}(c)
		}
	}()
	c := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), ReadTimeout: time.Minute, WriteTimeout: 10 * time.Second})
	t.Cleanup(func() { _ = c.Close() })
	return c, entered
}

func redFlowExpiredCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("test context did not expire")
	}
	return ctx, cancel
}

// TestRed_FlowSyncCloseLoopDoneTimeoutSealsLaterSubmissionResidual forces the
// existing loopDone early-return branch: a started worker whose loop never
// finishes (blocked in its first Redis publish against a hanging server),
// then Close with an already-expired context so the select on loopDone takes
// the case <-ctx.Done() path returning ctx.Err(). The later submission must
// be residual. Fails today on the residual assertions.
func TestRed_FlowSyncCloseLoopDoneTimeoutSealsLaterSubmissionResidual(t *testing.T) {
	c, entered := redFlowHangingRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, c, pg, SyncConfig{InstanceSrc: "red-flow-loopdone", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	liveCtx, stopLoop := context.WithCancel(context.Background())
	defer stopLoop()
	require.NoError(t, w.Start(liveCtx))
	redFlowWait(t, entered, "sync loop Redis publish")

	ctx, cancel := redFlowExpiredCtx(t)
	defer cancel()
	require.Error(t, w.Close(ctx), "expired Close against a never-finishing loop must take the ctx.Done branch")

	before := rec.FlowOwner().SnapshotStats()
	require.Equal(t, SubmitAccepted, rec.FlowOwner().Submit(m, []repository.RoutingFlowRow{ownerTestRow(12)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	after := rec.FlowOwner().SnapshotStats()
	require.GreaterOrEqual(t, after.ResidualSubmissions, before.ResidualSubmissions+1,
		"post-Close submission must be residual-classified")
	require.GreaterOrEqual(t, after.ResidualRows, before.ResidualRows+1,
		"post-Close submission rows must be residual-classified")
	require.Equal(t, before.EdgeRowsAccepted, after.EdgeRowsAccepted,
		"post-Close submission must not be PG-open accepted")
}

// TestRed_FlowSyncCloseFlushDoneTimeoutSealsLaterSubmissionResidual forces
// the existing flushDone early-return branch: the loop is already done (the
// worker was never started), an in-flight flush is held open in the test, so
// the select on flushDone takes the case <-ctx.Done() drain-incomplete path.
// The later submission must be residual. Fails today on the residual
// assertions. Recorded separately from the loopDone red above.
func TestRed_FlowSyncCloseFlushDoneTimeoutSealsLaterSubmissionResidual(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-flushdone", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })

	w.flushMu.Lock()
	require.True(t, w.beginFlush(), "test-held in-flight flush must open flushDone")
	ctx, cancel := redFlowExpiredCtx(t)
	defer cancel()
	require.Error(t, w.Close(ctx), "expired Close against a held flush must report drain-incomplete")
	w.endFlush()

	before := rec.FlowOwner().SnapshotStats()
	m := fixed.Truncate(time.Minute).Unix()
	require.Equal(t, SubmitAccepted, rec.FlowOwner().Submit(m, []repository.RoutingFlowRow{ownerTestRow(11)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	after := rec.FlowOwner().SnapshotStats()
	require.GreaterOrEqual(t, after.ResidualSubmissions, before.ResidualSubmissions+1,
		"post-Close submission must be residual-classified")
	require.GreaterOrEqual(t, after.ResidualRows, before.ResidualRows+1,
		"post-Close submission rows must be residual-classified")
	require.Equal(t, before.EdgeRowsAccepted, after.EdgeRowsAccepted,
		"post-Close submission must not be PG-open accepted")
}

// TestRed_FlowOldAbsentMinuteRejected: with an injected fixed owner clock, a
// consumer-path minute for an absent minute older than the 600-second cutoff
// is dropped with no accumulator created. (Default construction admits
// totally; the request Submit path always carries the just-assigned terminal
// minute.) It fails today because current code accepts it on every path.
func TestRed_FlowOldAbsentMinuteRejected(t *testing.T) {
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec.FlowOwner().clock = func() time.Time { return fixed }
	oldMinute := fixed.Add(-11 * time.Minute).Truncate(time.Minute).Unix()

	before := rec.FlowOwner().SnapshotStats()
	require.ErrorIs(t, rec.EnqueueFlowMinute(NewFlowSnapshot(oldMinute, []repository.RoutingFlowRow{ownerTestRow(11)})),
		ErrCapacity, "old absent minute must be rejected")
	_, ok := rec.FlowMinute(oldMinute)
	require.False(t, ok, "old absent minute must be rejected with no accumulator created")
	after := rec.FlowOwner().SnapshotStats()
	require.Equal(t, int64(1), after.EdgeRowsDropped-before.EdgeRowsDropped,
		"old-absent rejection must count exactly the offered rows as dropped")
	require.Equal(t, before.EdgeRowsAccepted, after.EdgeRowsAccepted,
		"old-absent rejection must not accept rows")
}

// Lease identity focused tests: snapshot/ack/lease/incarnation semantics
// once the production API exists.

func redFlowLeaseOwner(t *testing.T, recNow time.Time) (*Recorder, *FlowOwner) {
	t.Helper()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	rec.now = func() time.Time { return recNow }
	return rec, rec.FlowOwner()
}

// TestRed_FlowSnapshotAckClean: success with no concurrent merge leaves the
// minute clean with no next candidate, retained under the owner.
func TestRed_FlowSnapshotAckClean(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	snap, tok, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	require.Len(t, snap.FlowRows(), 1)
	require.True(t, owner.ackPG(tok))
	require.Empty(t, owner.pgCandidateMinutes(), "clean minute offers no next candidate")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok, "clean minute stays retained under the owner")
	require.Len(t, fm.FlowRows(), 1)
	require.False(t, owner.ackPG(tok), "settled lease cannot ack twice")
	require.False(t, owner.releasePG(tok), "settled lease cannot release after ack")
}

// TestRed_FlowSnapshotRaceStaysDirty: success with a post-snapshot merge
// leaves the minute dirty and the next snapshot includes both contributions
// exactly once.
func TestRed_FlowSnapshotRaceStaysDirty(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	_, tok1, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(22)})))
	require.True(t, owner.ackPG(tok1), "ack settles the lease even when newer merges exist")
	require.NotEmpty(t, owner.pgCandidateMinutes(), "post-snapshot merge keeps the minute dirty")
	snap2, tok2, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	counts := make(map[int64]int64)
	for _, r := range snap2.FlowRows() {
		counts[r.AccountID] += r.ChainCount
	}
	require.Equal(t, int64(1), counts[11])
	require.Equal(t, int64(1), counts[22], "next snapshot covers both contributions exactly once")
	require.True(t, owner.ackPG(tok2))
	require.Empty(t, owner.pgCandidateMinutes())
}

// TestRed_FlowNoAckRetryIdentical: failure or no ack yields an identical
// retry payload with no duplicate counting.
func TestRed_FlowNoAckRetryIdentical(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11), ownerTestRow(12)})))
	snap1, tok1, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	before := owner.SnapshotStats()
	require.True(t, owner.releasePG(tok1), "release on failure keeps the minute dirty")
	snap2, tok2, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	require.Equal(t, snap1.FlowRows(), snap2.FlowRows(), "retry payload is identical, nothing duplicated")
	after := owner.SnapshotStats()
	require.Equal(t, before.EdgeRowsAccepted, after.EdgeRowsAccepted)
	require.Equal(t, before.EdgeRowsDropped, after.EdgeRowsDropped)
	require.True(t, owner.ackPG(tok2))
}

// TestRed_FlowRepoMutationLeavesOwnerUnchanged: the repo mutating its payload
// leaves the owner and the retry payload unchanged.
func TestRed_FlowRepoMutationLeavesOwnerUnchanged(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	m := fixed.Truncate(time.Minute).Unix()
	prev := int64(7)
	row := ownerTestRow(11)
	row.PreviousAccountID = &prev
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{row})))
	snap, tok, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	mut := snap.FlowRows()
	mut[0].AccountID = 999
	mut[0].ChainCount = 999
	*mut[0].PreviousAccountID = 999
	require.True(t, owner.releasePG(tok))
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok, "repo-side mutation must not reach the owner")
	got := fm.FlowRows()
	require.Equal(t, int64(11), got[0].AccountID)
	require.Equal(t, int64(1), got[0].ChainCount)
	require.Equal(t, int64(7), *got[0].PreviousAccountID)
	snap2, tok2, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	got2 := snap2.FlowRows()
	require.Equal(t, int64(11), got2[0].AccountID, "retry payload is unaffected by repo mutation")
	require.Equal(t, int64(7), *got2[0].PreviousAccountID)
	require.True(t, owner.ackPG(tok2))
}

// TestRed_FlowEmptySnapshotSequenceAck: a dirty empty-only minute still calls
// UpsertFlowSnapshot with zero rows and advances durable sequence before ack.
func TestRed_FlowEmptySnapshotSequenceAck(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-empty-ack", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewEmptyFlowSnapshot(m)))
	w.doPG(context.Background())
	key := "red-flow-empty-ack:" + fixed.UTC().Truncate(time.Minute).String()
	pg.mu.Lock()
	rows, has := pg.flows[key]
	seq := pg.seqs[key]
	pg.mu.Unlock()
	require.True(t, has, "empty-only minute must still call UpsertFlowSnapshot")
	require.Len(t, rows, 0)
	require.Equal(t, int64(1), seq, "empty snapshot advances durable sequence")
	require.Empty(t, rec.FlowOwner().pgCandidateMinutes(), "acked empty minute is clean")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok, "acked empty minute stays retained")
	require.True(t, fm.IsEmptySnapshot())
}

// TestRed_FlowCandidatesOldestFirst: candidate IDs are deterministic
// oldest-first with no row work.
func TestRed_FlowCandidatesOldestFirst(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	base := fixed.Truncate(time.Minute).Unix()
	for _, m := range []int64{base + 120, base, base + 60} {
		require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(m)})))
	}
	require.Equal(t, []int64{base, base + 60, base + 120}, owner.pgCandidateMinutes())
}

// TestRed_FlowLeaseEvictionPressure: a leased minute survives eviction
// pressure while unleased victims are chosen; after release the minute is
// evictable again with exact bucket accounting.
func TestRed_FlowLeaseEvictionPressure(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	rec.minuteCap = 1
	m1 := fixed.Truncate(time.Minute).Unix()
	m2 := m1 + 60
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m1, []repository.RoutingFlowRow{ownerTestRow(11)})))
	_, tok1, ok := owner.snapshotForPG(m1)
	require.True(t, ok)
	before := owner.SnapshotStats()
	require.ErrorIs(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m2, []repository.RoutingFlowRow{ownerTestRow(22)})),
		ErrCapacity, "leased minute is never evictable")
	_, ok = rec.FlowMinute(m1)
	require.True(t, ok, "leased minute survives pressure")
	_, ok = rec.FlowMinute(m2)
	require.False(t, ok, "new minute cannot displace the leased minute")
	mid := owner.SnapshotStats()
	require.Equal(t, before.EdgeRowsAccepted, mid.EdgeRowsAccepted)
	require.Equal(t, before.EdgeRowsDropped+1, mid.EdgeRowsDropped)
	require.True(t, owner.releasePG(tok1))
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m2, []repository.RoutingFlowRow{ownerTestRow(22)})))
	_, ok = rec.FlowMinute(m1)
	require.False(t, ok, "released never-persisted minute is evictable under pressure")
	_, ok = rec.FlowMinute(m2)
	require.True(t, ok)
	after := owner.SnapshotStats()
	require.Equal(t, mid.EdgeRowsAccepted, after.EdgeRowsAccepted, "eviction of never-persisted consumer credits moves no accepted bucket")
	require.Equal(t, mid.EdgeRowsDropped, after.EdgeRowsDropped)
	require.Greater(t, rec.FlowOverflow(), int64(0), "eviction bumps the lane overflow counter")
	require.Greater(t, rec.MinuteOverflow(), int64(0), "eviction bumps the minute overflow counter")
}

// TestRed_FlowStaleLeaseIDNoop: a stale token from any prior lease is a no-op
// for ack and release even when the version is unchanged; a recreated minute
// gets a fresh incarnation so the stale token stays a no-op.
func TestRed_FlowStaleLeaseIDNoop(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	_, tok1, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	require.True(t, owner.releasePG(tok1), "release without mutation retires the lease identity")
	require.False(t, owner.ackPG(tok1), "stale token is a no-op for ack at the same version")
	require.False(t, owner.releasePG(tok1), "stale token is a no-op for release at the same version")
	_, tok2, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	require.NotEqual(t, tok1.leaseID, tok2.leaseID, "independent leaseID never repeats")
	require.True(t, owner.releasePG(tok2))

	// Recreate the minute under pressure: fresh incarnation, stale token dead.
	rec.minuteCap = 1
	m2 := m + 60
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m2, []repository.RoutingFlowRow{ownerTestRow(22)})))
	_, ok = rec.FlowMinute(m)
	require.False(t, ok, "unleased never-persisted minute evicted under pressure")
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(33)})))
	require.False(t, owner.ackPG(tok2), "stale token cannot ack a recreated incarnation")
	require.False(t, owner.releasePG(tok2), "stale token cannot release a recreated incarnation")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok)
	require.Equal(t, int64(33), fm.FlowRows()[0].AccountID, "recreated minute holds only new state")
}

// TestRed_FlowAckReleaseExactOnce: every path settles the lease exactly once —
// either ack or release, never both, never zero — including context expiry.
func TestRed_FlowAckReleaseExactOnce(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	_, tok1, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	require.True(t, owner.ackPG(tok1))
	require.False(t, owner.ackPG(tok1), "no double ack")
	require.False(t, owner.releasePG(tok1), "no release after ack")

	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(12)})))
	_, tok2, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	require.True(t, owner.releasePG(tok2))
	require.False(t, owner.releasePG(tok2), "no double release")
	require.False(t, owner.ackPG(tok2), "no ack after release")

	// Context expiry through the sync path releases exactly once: the minute
	// stays dirty and offers the next candidate.
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	pg.failAll = true
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-exactonce", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	w.doPG(context.Background())
	require.NotEmpty(t, owner.pgCandidateMinutes(), "released minute retries next cycle")
	pg.failAll = false
	w.doPG(context.Background())
	require.Empty(t, owner.pgCandidateMinutes(), "retry ack leaves the minute clean")
}

// Seal focused tests: post-sync-Close residual, clean retained minutes,
// lease revocation, section 9 equations, queuedPG reconciliation.

func redFlowSealOwner(t *testing.T) (*Recorder, *FlowOwner, int64) {
	t.Helper()
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	rec.now = func() time.Time { return fixed }
	return rec, rec.FlowOwner(), fixed.Truncate(time.Minute).Unix()
}

// TestRed_FlowCleanRetainedNotBlockingClose: clean retained minutes neither
// block Close nor inflate pgWorkTotal; Close seals and returns.
func TestRed_FlowCleanRetainedNotBlockingClose(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, owner, m := redFlowSealOwner(t)
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(11)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-cleanclose", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	w.doPG(context.Background())
	require.Equal(t, 0, owner.pgWorkTotal(), "clean retained state contributes no Close work")
	require.NoError(t, w.Close(context.Background()), "clean retained minutes must not block Close")
	_, ok = rec.FlowMinute(m)
	require.True(t, ok, "sealed minute stays retained for diagnostics")
	require.Equal(t, 0, owner.pgWorkTotal())
}

// TestRed_FlowSealRevokesLeaseLateAckReleaseNoop: seal revokes every active
// lease with all unconfirmed accepted credits moved to residual at seal, and
// every late ack/release is a stale-token no-op with no owner mutation after
// seal.
func TestRed_FlowSealRevokesLeaseLateAckReleaseNoop(t *testing.T) {
	rec, owner, m := redFlowSealOwner(t)
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(11)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	_, tok, ok := owner.snapshotForPG(m)
	require.True(t, ok, "dirty minute offers a lease before seal")
	sealed := owner.SnapshotStats()
	owner.sealPG()
	require.False(t, owner.ackPG(tok), "late ack after seal is a stale-token no-op")
	require.False(t, owner.releasePG(tok), "late release after seal is a stale-token no-op")
	after := owner.SnapshotStats()
	require.Equal(t, int64(0), after.EdgeRowsAccepted, "seal moves all unconfirmed accepted credits to residual")
	require.Equal(t, sealed.EdgeRowsAccepted, after.ResidualRows-sealed.ResidualRows)
	require.False(t, owner.ackPG(tok))
	require.False(t, owner.releasePG(tok))
	require.Equal(t, after, owner.SnapshotStats(), "late callbacks mutate nothing after seal")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok, "sealed minute stays retained for diagnostics")
	require.Len(t, fm.FlowRows(), 1)
	require.Equal(t, int64(11), fm.FlowRows()[0].AccountID)
	require.Equal(t, sealed.Accepted, after.Processed+after.ResidualSubmissions,
		"processed + residual_submissions == accepted")
	require.Equal(t, int64(1), after.EdgeRowsAccepted+after.EdgeRowsDropped+after.ResidualRows,
		"edge_rows_accepted + edge_rows_dropped + residual_rows == offered edge rows")
}

// TestRed_FlowCloseResidualEquation: both section 9 equations hold at drained
// points across pre-seal success and post-seal residual submissions.
func TestRed_FlowCloseResidualEquation(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, owner, m := redFlowSealOwner(t)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-equation", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })

	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(11), ownerTestRow(12), ownerTestRow(13)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	w.doPG(context.Background())
	mid := owner.SnapshotStats()
	require.Equal(t, int64(1), mid.Accepted)
	require.Equal(t, int64(1), mid.Processed)
	require.Equal(t, int64(0), mid.ResidualSubmissions)
	require.Equal(t, int64(3), mid.EdgeRowsAccepted+mid.EdgeRowsDropped+mid.ResidualRows)

	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(21), ownerTestRow(22)}))
	_, ok = rec.FlowMinute(m)
	require.True(t, ok)
	after := owner.SnapshotStats()
	require.Equal(t, after.Accepted, after.Processed+after.ResidualSubmissions,
		"processed + residual_submissions == accepted")
	require.Equal(t, int64(5), after.EdgeRowsAccepted+after.EdgeRowsDropped+after.ResidualRows,
		"row equation holds over all offered Submit rows")
	require.Equal(t, int64(3), after.EdgeRowsAccepted, "persisted history is never rewritten")
	require.Equal(t, int64(0), after.EdgeRowsDropped)
	require.Equal(t, int64(2), after.ResidualRows)
}

// TestRed_FlowQueuedPGReconcilesAroundSeal: pre-seal queued submissions
// increment queuedPG, seal-time and post-seal residual tagging never does,
// and full drain returns it to zero.
func TestRed_FlowQueuedPGReconcilesAroundSeal(t *testing.T) {
	rec, owner, m := redFlowSealOwner(t)
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(11)}))
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(12)}))
	require.Equal(t, 2, owner.pgWorkTotal(), "pre-seal queued submissions increment queuedPG")
	owner.sealPG()
	require.Equal(t, 0, owner.pgWorkTotal(), "seal drain returns queuedPG to zero")
	after := owner.SnapshotStats()
	require.Equal(t, int64(2), after.Processed, "seal drains pre-seal submissions as accepted merges")
	require.Equal(t, int64(0), after.ResidualSubmissions)
	require.Equal(t, int64(0), after.EdgeRowsAccepted, "seal sweep moves all unconfirmed accepted credits to residual")
	require.Equal(t, int64(2), after.ResidualRows)
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(13)}))
	require.Equal(t, 0, owner.pgWorkTotal(), "post-seal residual tagging never increments queuedPG")
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	require.Equal(t, 0, owner.pgWorkTotal(), "full drain returns queuedPG to zero")
	final := owner.SnapshotStats()
	require.Equal(t, int64(2), final.Processed)
	require.Equal(t, int64(1), final.ResidualSubmissions)
	require.Equal(t, int64(3), final.ResidualRows)
	require.Equal(t, final.Accepted, final.Processed+final.ResidualSubmissions)
}

// TestRed_FlowSubmissionCountersAroundSeal: a pre-seal normal owner merge
// increments processed; seal-sweeping rows out of already processed
// submissions changes only row classes, never submission counters.
func TestRed_FlowSubmissionCountersAroundSeal(t *testing.T) {
	rec, owner, m := redFlowSealOwner(t)
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(11)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	before := owner.SnapshotStats()
	require.Equal(t, int64(1), before.Processed)
	require.Equal(t, int64(0), before.ResidualSubmissions)
	owner.sealPG()
	after := owner.SnapshotStats()
	require.Equal(t, before.Accepted, after.Accepted, "seal changes row classes, never submission counters")
	require.Equal(t, before.Processed, after.Processed)
	require.Equal(t, before.ResidualSubmissions, after.ResidualSubmissions)
	require.Equal(t, int64(0), after.EdgeRowsAccepted)
	require.Equal(t, int64(1), after.ResidualRows)
}

// Cutoff and eviction focused tests: persisted-baseline protection and exact
// reclassification. Admission stays total; the horizon gates eviction
// eligibility only.

func redFlowPersistedMinute(t *testing.T, now time.Time, rows []repository.RoutingFlowRow) (*Recorder, *FlowOwner, *SyncWorker, *fakePG, int64) {
	t.Helper()
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	rec.now = func() time.Time { return now }
	owner := rec.FlowOwner()
	m := now.Truncate(time.Minute).Unix()
	for _, row := range rows {
		require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{row}))
	}
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-cutoff", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return now })
	w.doPG(context.Background())
	require.Empty(t, owner.pgCandidateMinutes(), "setup minute must persist clean")
	return rec, owner, w, pg, m
}

// TestRed_FlowEvictionReclassifiesOnlyUnpersisted: eviction of a persisted
// minute after the cutoff reclassifies only totalAccepted -
// persistedWatermark via the exact section 9 bucket transitions; persisted
// accepted history and counters remain unchanged.
func TestRed_FlowEvictionReclassifiesOnlyUnpersisted(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner, _, _, m := redFlowPersistedMinute(t, fixed, []repository.RoutingFlowRow{ownerTestRow(11), ownerTestRow(12), ownerTestRow(13)})
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(14), ownerTestRow(15)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	before := owner.SnapshotStats()
	require.Equal(t, int64(5), before.EdgeRowsAccepted)

	// Past the cutoff the persisted minute becomes evictable; only the two
	// unpersisted credits move accepted->dropped.
	rec.now = func() time.Time { return fixed.Add(11 * time.Minute) }
	rec.minuteCap = 1
	m2 := fixed.Add(11 * time.Minute).Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m2, []repository.RoutingFlowRow{ownerTestRow(21)})))
	_, ok = rec.FlowMinute(m)
	require.False(t, ok, "post-cutoff persisted minute evicted under pressure")
	_, ok = rec.FlowMinute(m2)
	require.True(t, ok)
	after := owner.SnapshotStats()
	require.Equal(t, int64(3), after.EdgeRowsAccepted, "persisted accepted history is never rewritten")
	require.Equal(t, int64(2), after.EdgeRowsDropped-before.EdgeRowsDropped, "only unpersisted credits reclassify")
	require.Greater(t, rec.FlowOverflow(), int64(0))
	require.Greater(t, rec.MinuteOverflow(), int64(0))

	// Never-persisted queue-path credits reclassify fully accepted->dropped.
	rec.minuteCap = 1
	m3 := m2 + 60
	require.Equal(t, SubmitAccepted, owner.Submit(m3, []repository.RoutingFlowRow{ownerTestRow(31)}))
	_, ok = rec.FlowMinute(m3)
	require.True(t, ok)
	preEvict := owner.SnapshotStats()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m3+60, []repository.RoutingFlowRow{ownerTestRow(41)})))
	_, ok = rec.FlowMinute(m3)
	require.False(t, ok, "never-persisted minute evicted under pressure")
	postEvict := owner.SnapshotStats()
	require.Equal(t, preEvict.EdgeRowsAccepted-1, postEvict.EdgeRowsAccepted)
	require.Equal(t, preEvict.EdgeRowsDropped+1, postEvict.EdgeRowsDropped)
	require.GreaterOrEqual(t, postEvict.EdgeRowsAccepted, int64(0))
	require.GreaterOrEqual(t, postEvict.EdgeRowsDropped, int64(0))
}

// TestRed_FlowPersistedSurvivesEviction: a clean persisted minute before the
// cutoff survives pressure; the newcomer is rejected with exact counters.
func TestRed_FlowPersistedSurvivesEviction(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner, _, _, m := redFlowPersistedMinute(t, fixed, []repository.RoutingFlowRow{ownerTestRow(11)})
	rec.minuteCap = 1
	m2 := fixed.Add(time.Minute).Truncate(time.Minute).Unix()
	before := owner.SnapshotStats()
	require.ErrorIs(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m2, []repository.RoutingFlowRow{ownerTestRow(22)})),
		ErrCapacity, "persisted baseline is eviction-ineligible before the cutoff")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok, "clean persisted minute survives pressure")
	require.Len(t, fm.FlowRows(), 1)
	_, ok = rec.FlowMinute(m2)
	require.False(t, ok)
	after := owner.SnapshotStats()
	require.Equal(t, before.EdgeRowsAccepted, after.EdgeRowsAccepted)
	require.Equal(t, before.EdgeRowsDropped+1, after.EdgeRowsDropped)
}

// TestRed_FlowDirtyPersistedProtectedBeforeCutoff: a dirty everPersisted
// minute survives pressure before the cutoff.
func TestRed_FlowDirtyPersistedProtectedBeforeCutoff(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner, _, _, m := redFlowPersistedMinute(t, fixed, []repository.RoutingFlowRow{ownerTestRow(11), ownerTestRow(12)})
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(13)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	rec.minuteCap = 1
	m2 := fixed.Add(time.Minute).Truncate(time.Minute).Unix()
	require.ErrorIs(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m2, []repository.RoutingFlowRow{ownerTestRow(22)})),
		ErrCapacity, "dirty everPersisted minute survives pressure before the cutoff")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 3, "dirty persisted accumulation conserved")
	after := owner.SnapshotStats()
	require.Equal(t, int64(3), after.EdgeRowsAccepted)
	require.Equal(t, int64(1), after.EdgeRowsDropped)
}

// TestRed_FlowEmptyPersistedProtectedBeforeCutoff: an empty-snapshot-persisted
// minute survives pressure before the cutoff.
func TestRed_FlowEmptyPersistedProtectedBeforeCutoff(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	rec.now = func() time.Time { return fixed }
	owner := rec.FlowOwner()
	m := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewEmptyFlowSnapshot(m)))
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-emptyprot", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	w.doPG(context.Background())
	require.Empty(t, owner.pgCandidateMinutes())
	rec.minuteCap = 1
	m2 := fixed.Add(time.Minute).Truncate(time.Minute).Unix()
	require.ErrorIs(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m2, []repository.RoutingFlowRow{ownerTestRow(22)})),
		ErrCapacity, "empty-snapshot-persisted minute survives pressure before the cutoff")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok)
	require.True(t, fm.IsEmptySnapshot())
}

// TestRed_FlowPersistedHistoryProtected: a clean persisted minute survives
// repeated pressure with history intact and exact newcomer drop counts.
func TestRed_FlowPersistedHistoryProtected(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner, _, _, m := redFlowPersistedMinute(t, fixed, []repository.RoutingFlowRow{ownerTestRow(11), ownerTestRow(12)})
	rec.minuteCap = 1
	for i := int64(1); i <= 3; i++ {
		mn := fixed.Add(time.Duration(i) * time.Minute).Truncate(time.Minute).Unix()
		require.ErrorIs(t, rec.EnqueueFlowMinute(NewFlowSnapshot(mn, []repository.RoutingFlowRow{ownerTestRow(100 + i)})),
			ErrCapacity)
	}
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok, "clean persisted history survives repeated pressure")
	require.Len(t, fm.FlowRows(), 2)
	after := owner.SnapshotStats()
	require.Equal(t, int64(2), after.EdgeRowsAccepted, "persisted history never rewritten")
	require.Equal(t, int64(3), after.EdgeRowsDropped, "each rejected newcomer counted exactly once")
	require.GreaterOrEqual(t, after.EdgeRowsAccepted, int64(0))
	require.GreaterOrEqual(t, after.EdgeRowsDropped, int64(0))
}

// Redis focused tests: one-minute-at-a-time read-only publish with at most
// one live O(R) payload globally under flushMu.

// TestRed_FlowRedisCandidatesOldestFirst: Redis candidate IDs are
// deterministic oldest-first with no row work.
func TestRed_FlowRedisCandidatesOldestFirst(t *testing.T) {
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec, owner := redFlowLeaseOwner(t, fixed)
	base := fixed.Truncate(time.Minute).Unix()
	cur := base + 120
	for _, m := range []int64{cur, base, base + 60} {
		require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(m)})))
	}
	require.Equal(t, []int64{base, base + 60, cur}, owner.redisCandidateMinutes(cur))
	require.Equal(t, []int64{base, base + 60}, owner.redisCandidateMinutes(base+60), "future minutes stay unpublished")
}

// TestRed_FlowRedisOneLivePayload: sequential one-minute publish leaves the
// owner unchanged (read-only, no lease, no ack) with every retained minute
// published exactly once.
func TestRed_FlowRedisOneLivePayload(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	owner := rec.FlowOwner()
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-redispayload", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	base := fixed.Truncate(time.Minute).Unix()
	for _, m := range []int64{base - 120, base - 60, base} {
		require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(m), ownerTestRow(m + 1)})))
	}
	beforeRows := make(map[int64][]repository.RoutingFlowRow)
	for _, m := range []int64{base - 120, base - 60, base} {
		fm, ok := rec.FlowMinute(m)
		require.True(t, ok)
		beforeRows[m] = fm.FlowRows()
	}
	beforeStats := owner.SnapshotStats()
	w.doRedis(context.Background())

	flowKeys := 0
	for _, key := range mr.Keys() {
		if len(key) >= len(redisFlowPrefix) && key[:len(redisFlowPrefix)] == redisFlowPrefix {
			flowKeys++
		}
	}
	require.Equal(t, 3, flowKeys, "every retained minute published exactly once")
	for _, m := range []int64{base - 120, base - 60, base} {
		fm, ok := rec.FlowMinute(m)
		require.True(t, ok, "read-only publish retains every minute")
		require.Equal(t, beforeRows[m], fm.FlowRows(), "read-only publish mutates no retained row")
	}
	afterStats := owner.SnapshotStats()
	require.Equal(t, beforeStats.EdgeRowsAccepted, afterStats.EdgeRowsAccepted)
	require.Equal(t, beforeStats.EdgeRowsDropped, afterStats.EdgeRowsDropped)
	require.Equal(t, beforeStats.ResidualRows, afterStats.ResidualRows)
	require.Len(t, owner.pgCandidateMinutes(), 3, "read-only publish takes no lease and acks nothing")
}

// redFlowRetainingPG retains each received rows slice to prove the caller
// never materializes the next payload until the call returned and never
// reuses a shared buffer across minutes.
type redFlowRetainingPG struct {
	inner   *fakePG
	mu      sync.Mutex
	held    [][]repository.RoutingFlowRow
	minutes []int64
}

func (f *redFlowRetainingPG) UpsertQualityAndMarkDirty(context.Context, repository.RoutingQualityRow) error {
	return nil
}

func (f *redFlowRetainingPG) UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, v int16, seq int64, rows []repository.RoutingFlowRow) error {
	f.mu.Lock()
	f.held = append(f.held, rows)
	f.minutes = append(f.minutes, terminalMinute.Unix())
	f.mu.Unlock()
	return f.inner.UpsertFlowSnapshot(ctx, instanceSrc, terminalMinute, v, seq, rows)
}

// TestRed_FlowPayloadLifetimeSingleLive: per-candidate materialize, consume
// synchronously in one repo call, then drop all references before the next
// snapshot. The retaining fake proves no shared-buffer reuse: every retained
// slice still carries exactly its own minute after the flush.
func TestRed_FlowPayloadLifetimeSingleLive(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := &redFlowRetainingPG{inner: newFakePG()}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	owner := rec.FlowOwner()
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-lifetime", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	base := fixed.Truncate(time.Minute).Unix()
	minutes := []int64{base, base + 60, base + 120}
	for _, m := range minutes {
		require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(1000 + m)})))
	}
	w.doPG(context.Background())
	pg.mu.Lock()
	defer pg.mu.Unlock()
	require.Len(t, pg.held, 3, "one repo call per candidate minute")
	for i, m := range minutes {
		require.Equal(t, m, pg.minutes[i], "calls run oldest-first, one minute at a time")
		for _, r := range pg.held[i] {
			require.Equal(t, m, r.TerminalMinute.Unix(), "retained slice %d still carries only its own minute: no shared-buffer reuse", i)
			require.Equal(t, 1000+m, r.AccountID)
		}
	}
	for _, m := range minutes {
		_, ok := rec.FlowMinute(m)
		require.True(t, ok, "owner retains every minute after the flush")
	}
	_ = owner
}

// TestRed_FlowPreviousAccountOwnershipBoundaries: deep PreviousAccountID
// ownership holds at every remaining boundary — submission copy, accumulator
// fold clone, snapshot payload clone, repo payload — with no aliasing.
func TestRed_FlowPreviousAccountOwnershipBoundaries(t *testing.T) {
	_, rdb := newMiniRedis(t)
	inner := newFakePG()
	pg := &redFlowMutatingPG{inner: inner}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	owner := rec.FlowOwner()
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-prevacct", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	m := fixed.Truncate(time.Minute).Unix()

	previous := int64(7)
	row := ownerTestRow(11)
	row.PreviousAccountID = &previous
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{row}))
	previous = 99
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok)
	require.Equal(t, int64(7), *fm.FlowRows()[0].PreviousAccountID, "submission copy owns the pointee")

	snap, tok, ok := owner.snapshotForPG(m)
	require.True(t, ok)
	*fm.FlowRows()[0].PreviousAccountID = 42
	*snap.FlowRows()[0].PreviousAccountID = 43
	fm2, ok := rec.FlowMinute(m)
	require.True(t, ok)
	require.Equal(t, int64(7), *fm2.FlowRows()[0].PreviousAccountID, "snapshot payload clones fresh pointees")
	require.True(t, owner.ackPG(tok))

	// Repo mutating its payload (accounts and pointees) must not reach the
	// owner or the retry payload.
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(12)})))
	w.doPG(context.Background())
	fm3, ok := rec.FlowMinute(m)
	require.True(t, ok, "repo mutation leaves the owner unchanged")
	byAccount := make(map[int64]*int64)
	for _, r := range fm3.FlowRows() {
		byAccount[r.AccountID] = r.PreviousAccountID
	}
	require.NotNil(t, byAccount[11])
	require.Equal(t, int64(7), *byAccount[11])
	require.Nil(t, byAccount[12])
}

// redFlowMutatingPG rewrites every received row in place, proving the caller
// handed over an isolated copy rather than owner-aliased memory.
type redFlowMutatingPG struct {
	inner *fakePG
}

func (f *redFlowMutatingPG) UpsertQualityAndMarkDirty(context.Context, repository.RoutingQualityRow) error {
	return nil
}

func (f *redFlowMutatingPG) UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, v int16, seq int64, rows []repository.RoutingFlowRow) error {
	for i := range rows {
		rows[i].AccountID = -rows[i].AccountID
		rows[i].ChainCount = 999999
		if rows[i].PreviousAccountID != nil {
			*rows[i].PreviousAccountID = -777
		} else {
			rows[i].PreviousAccountID = new(int64)
		}
	}
	return f.inner.UpsertFlowSnapshot(ctx, instanceSrc, terminalMinute, v, seq, rows)
}

// TestWorkerManager_ReverseShutdownInflightFlowPGSealedNoop pins the
// shutdown-revocation contract at manager level: block a flow PG call, let
// SyncWorker.Close hit its deadline and seal/revoke, let the Manager proceed
// to FlowOwner.Close, release PG, then assert the late ack/release is a no-op
// with stats and final snapshot unchanged and both section 9 equations
// holding.
func TestWorkerManager_ReverseShutdownInflightFlowPGSealedNoop(t *testing.T) {
	_, rdb := newMiniRedis(t)
	release := make(chan struct{})
	pg := &redFlowBlockingPG{inner: newFakePG(), entered: make(chan struct{}), release: release}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	owner := rec.FlowOwner()
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-mgrseal", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	m := fixed.Truncate(time.Minute).Unix()

	// One accepted submission drained before the in-flight flush.
	require.Equal(t, SubmitAccepted, owner.Submit(m, []repository.RoutingFlowRow{ownerTestRow(11)}))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)

	mgr := worker.New(nil)
	require.NoError(t, mgr.Register(owner, w), "reverse shutdown closes quality-sync first and the owner second")
	require.NoError(t, mgr.StartAll(context.Background()))
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		w.doPG(context.Background())
	}()
	redFlowWait(t, pg.entered, "in-flight flow PG upsert")

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		shutdownDone <- mgr.Shutdown(ctx)
	}()
	var shutdownErr error
	select {
	case shutdownErr = <-shutdownDone:
	case <-time.After(10 * time.Second):
		t.Fatal("manager shutdown did not return past the sealed worker")
	}
	require.Error(t, shutdownErr, "drain-incomplete Close surfaces through the manager")
	select {
	case <-owner.loopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("owner loop did not join after manager shutdown")
	}
	sealedStats := owner.SnapshotStats()
	sealedRows := mustFlowRows(t, rec, m)

	// Release PG after both closes: the late ack/release is a no-op.
	close(release)
	redFlowWait(t, flushDone, "released in-flight doPG")
	afterStats := owner.SnapshotStats()
	require.Equal(t, sealedStats, afterStats, "late ack/release after seal mutates nothing")
	require.Equal(t, sealedRows, mustFlowRows(t, rec, m), "final snapshot unchanged by the late callback")
	require.Equal(t, sealedStats.Accepted, sealedStats.Processed+sealedStats.ResidualSubmissions,
		"processed + residual_submissions == accepted")
	require.Equal(t, int64(1), sealedStats.EdgeRowsAccepted+sealedStats.EdgeRowsDropped+sealedStats.ResidualRows,
		"row equation holds over the offered Submit row")
	require.Equal(t, int64(0), sealedStats.EdgeRowsAccepted, "seal moved the unconfirmed credit to residual")
	require.Equal(t, int64(1), sealedStats.ResidualRows)
}

func mustFlowRows(t *testing.T, rec *Recorder, minute int64) []repository.RoutingFlowRow {
	t.Helper()
	fm, ok := rec.FlowMinute(minute)
	require.True(t, ok)
	return fm.FlowRows()
}

// TestRed_FlowPostSealEnqueueStaysResidual: after SyncWorker.Close seals, a
// synchronous/legacy EnqueueFlowMinute is residual-classified consistently
// with Submit — it can never create PG-open dirty accepted state, and the
// residual row equations hold.
func TestRed_FlowPostSealEnqueueStaysResidual(t *testing.T) {
	rec, owner, m := redFlowSealOwner(t)
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(11)})))
	_, ok := rec.FlowMinute(m)
	require.True(t, ok)
	before := owner.SnapshotStats()
	owner.sealPG()

	// Post-seal rows fold residual: retained cumulatively, never dirty.
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{ownerTestRow(12)})))
	// Post-seal empty marker takes effect without dirtying.
	require.NoError(t, rec.EnqueueFlowMinute(NewEmptyFlowSnapshot(m+60)))
	// Post-seal legacy edges are preserved without dirtying.
	legacyEdges := [8]int64{9, 0, 0, 0, 0, 0, 0, 0}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowMinute(m+120, legacyEdges)))

	after := owner.SnapshotStats()
	require.Empty(t, owner.pgCandidateMinutes(), "post-seal enqueue must create no PG-open dirty state")
	fm, ok := rec.FlowMinute(m)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 2, "post-seal rows retained cumulatively for diagnostics")
	fme, ok := rec.FlowMinute(m + 120)
	require.True(t, ok)
	require.Equal(t, legacyEdges, fme.Edges(), "post-seal legacy edges preserved")
	require.Equal(t, before.EdgeRowsAccepted, after.EdgeRowsAccepted, "no accepted-class state after seal")
	require.Equal(t, before.EdgeRowsDropped, after.EdgeRowsDropped)
	require.Equal(t, before.ResidualRows+1, after.ResidualRows, "exactly the offered row reclassified residual")
	require.Equal(t, before.Processed, after.Processed, "consumer path touches no submission counters")
	require.Equal(t, before.ResidualSubmissions, after.ResidualSubmissions)
	require.Empty(t, owner.redisCandidateMinutes(m+3600), "no post-seal publish candidacy")
}

// TestRed_FlowPruneRetainsSequenceFencing: persisting a minute, advancing the
// clock beyond cutoff/prune, mutating the same minute, and flushing must
// continue the PG sequence — never restart into the repository
// greater-sequence fence (which would no-op while ack clears dirty, losing
// the contribution).
func TestRed_FlowPruneRetainsSequenceFencing(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-flow-seqfence", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return clk })
	m := fixed.Truncate(time.Minute).Unix()
	key := "red-flow-seqfence:" + fixed.UTC().Truncate(time.Minute).String()

	// Cycle 1: persist A at durable sequence 1.
	rowA := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "primary", AccountID: 11, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 3}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowA})))
	w.doPG(context.Background())
	pg.mu.Lock()
	require.Equal(t, int64(1), pg.seqs[key])
	pg.mu.Unlock()

	// Advance beyond cutoff and prune via a young-minute cycle.
	clk = fixed.Add(30 * time.Minute)
	mYoung := clk.Truncate(time.Minute).Unix()
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(mYoung, []repository.RoutingFlowRow{ownerTestRow(99)})))
	w.doPG(context.Background())

	// Mutate the old minute and flush: the repository must receive a greater
	// sequence carrying the full cumulative replacement.
	rowC := repository.RoutingFlowRow{IdentityVersion: 1, Ordinal: 1, Lane: "explore", AccountID: 22, TransitionReason: "init", Outcome: "error", IsTerminal: true, Generation: 2, ChainCount: 5}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(m, []repository.RoutingFlowRow{rowC})))
	w.doPG(context.Background())
	pg.mu.Lock()
	defer pg.mu.Unlock()
	require.Equal(t, int64(2), pg.seqs[key], "sequence must continue past prune, never restart at 1")
	got := pg.flows[key]
	require.Len(t, got, 2, "persisted replacement contains the new contribution")
	counts := make(map[int64]int64, len(got))
	for _, r := range got {
		counts[r.AccountID] += r.ChainCount
	}
	require.Equal(t, int64(3), counts[11])
	require.Equal(t, int64(5), counts[22])
	_, ok := rec.FlowMinute(m)
	require.True(t, ok, "owner retains the minute")
}
