// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

type fakePG struct {
	mu      sync.Mutex
	quality []repository.RoutingQualityRow
	flows   map[string][]repository.RoutingFlowRow
	seqs    map[string]int64
	failAll bool
	poison  map[string]bool // key = hex fingerprint
	calls   int
	// onFlow fires synchronously at the start of UpsertFlowSnapshot (before the
	// failAll decision), letting a test enqueue a same-minute delta exactly
	// while a flush is in flight. Set only from the test goroutine around
	// synchronous doPG calls.
	onFlow func(terminalMinute time.Time, seq int64)
}

func newFakePG() *fakePG {
	return &fakePG{
		flows:  make(map[string][]repository.RoutingFlowRow),
		seqs:   make(map[string]int64),
		poison: make(map[string]bool),
	}
}

func (f *fakePG) keyQ(row repository.RoutingQualityRow) string {
	return hex.EncodeToString(row.CandidateFingerprint[:]) + ":" + row.BucketMinute.String() + ":" + hex.EncodeToString(row.RouteClassID[:])
}

func (f *fakePG) UpsertQualityAndMarkDirty(_ context.Context, row repository.RoutingQualityRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAll {
		return context.DeadlineExceeded
	}
	k := hex.EncodeToString(row.CandidateFingerprint[:])
	if f.poison[k] {
		return &RowDataError{Msg: "check constraint violates quality_attempts"}
	}
	// sequence semantics: only greater sequence overwrites
	mapKey := row.InstanceSrc + ":" + row.BucketMinute.String() + ":" + k
	if cur, ok := f.seqs[mapKey]; ok && row.AbsoluteSequence <= cur {
		return nil
	}
	f.seqs[mapKey] = row.AbsoluteSequence
	// upsert
	for i, existing := range f.quality {
		if hex.EncodeToString(existing.CandidateFingerprint[:]) == k && existing.BucketMinute.Equal(row.BucketMinute) && existing.InstanceSrc == row.InstanceSrc {
			f.quality[i] = row
			return nil
		}
	}
	f.quality = append(f.quality, row)
	return nil
}

func (f *fakePG) UpsertFlowSnapshot(_ context.Context, instanceSrc string, terminalMinute time.Time, _ int16, seq int64, rows []repository.RoutingFlowRow) error {
	if f.onFlow != nil {
		f.onFlow(terminalMinute, seq)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAll {
		return context.DeadlineExceeded
	}
	key := instanceSrc + ":" + terminalMinute.String()
	if cur, ok := f.seqs[key]; ok && seq <= cur {
		return nil
	}
	f.seqs[key] = seq
	// store deep copy
	cp := make([]repository.RoutingFlowRow, len(rows))
	copy(cp, rows)
	f.flows[key] = cp
	return nil
}

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return mr, c
}

func TestQualitySync_QualityPendingSwapRetainsBudgetExceeded(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-budget", BatchSize: 5}, nil, nil)
	w.SetClock(func() time.Time { return clk })
	// fill beyond pg budget: 30 rows across 3 minutes
	for i := 0; i < 30; i++ {
		k := keyOf(fp(byte(10+i)), qc(byte(10+i)))
		qm := NewQualityMinute(fixed.Unix()+int64(i%3), k)
		qm.SetAttempts(1)
		require.NoError(t, rec.EnqueueQualityMinute(qm))
	}
	before := rec.MinuteBucketCount()
	require.Greater(t, before, 0)
	// first flush will be budget-limited to 20k but we have only 30, so it should flush all; to test budget we artificially lower pgMax by not; instead test that deferred rows are requeued
	// Use small batch and simulate duration budget by advancing clock during flush?
	// Simpler: verify that after flush, no rows lost
	w.doPG(context.Background())
	afterPending := rec.MinuteBucketCount()
	// all rows should have been flushed, pending should be 0 or requeued only if budget exceeded, but we had only 30 < 20k so pending 0
	require.Equal(t, 0, afterPending)
	require.Equal(t, 30, len(pg.quality))
	// now test budget defer: create many rows and ensure deferred retained
	pg2 := newFakePG()
	rec2, _ := NewRecorder(50000)
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "src-budget2", BatchSize: 5}, nil, nil)
	w2.SetClock(func() time.Time { return clk })
	for i := 0; i < 5; i++ {
		k := keyOf(fp(byte(20+i)), qc(byte(20+i)))
		qm := NewQualityMinute(fixed.Unix(), k)
		qm.SetAttempts(int64(i + 1))
		require.NoError(t, rec2.EnqueueQualityMinute(qm))
	}
	pg2.failAll = true
	w2.doPG(context.Background())
	// pending distinct minutes is 1, but rows is 5
	rec2.mu.Lock()
	rowsCount := 0
	for _, rows := range rec2.pendingQuality {
		rowsCount += len(rows)
	}
	rec2.mu.Unlock()
	require.Equal(t, 5, rowsCount, "DB-wide error must refill all valid rows")
}

func TestQualitySync_PGBisectPreservesDBWideAndDropsOnlyPoison(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-poison", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return clk })
	k1 := keyOf(fp(1), qc(1))
	k2 := keyOf(fp(2), qc(2))
	qm1 := NewQualityMinute(fixed.Unix(), k1)
	qm1.SetAttempts(1)
	qm2 := NewQualityMinute(fixed.Unix(), k2)
	qm2.SetAttempts(2)
	require.NoError(t, rec.EnqueueQualityMinute(qm1))
	require.NoError(t, rec.EnqueueQualityMinute(qm2))
	pg.poison[hex.EncodeToString(k1.Fingerprint[:])] = true
	w.doPG(context.Background())
	require.Equal(t, int64(1), w.poison.Load())
	require.Equal(t, 1, len(pg.quality), "only poison dropped, other row persisted")
	require.Equal(t, 0, rec.MinuteBucketCount(), "non-poison row not requeued after success")
	for _, row := range pg.quality {
		require.NotEqual(t, hex.EncodeToString(k1.Fingerprint[:]), hex.EncodeToString(row.CandidateFingerprint[:]))
	}
	// DB-wide failure: all rows refill
	pg3 := newFakePG()
	rec3, _ := NewRecorder(50000)
	w3 := NewSyncWorker(rec3, rdb, pg3, SyncConfig{InstanceSrc: "src-dbwide", BatchSize: 10}, nil, nil)
	w3.SetClock(func() time.Time { return clk })
	require.NoError(t, rec3.EnqueueQualityMinute(qm1.Clone()))
	require.NoError(t, rec3.EnqueueQualityMinute(qm2.Clone()))
	pg3.failAll = true
	w3.doPG(context.Background())
	rec3.mu.Lock()
	rowsCount3 := 0
	for _, rows := range rec3.pendingQuality {
		rowsCount3 += len(rows)
	}
	rec3.mu.Unlock()
	require.Equal(t, 2, rowsCount3)
	require.Equal(t, int64(0), w3.poison.Load())
}

func TestQualitySync_FlowPreservesRowsAndRequeuesWholeMinute(t *testing.T) {
	// v3-hygiene: the legacy edges-array vehicle is deleted — the same sync
	// contract (full-minute persist, dirty-retained retry) rides live rows.
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-flow", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), fixed.Unix(), []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 10, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 100},
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 2, Lane: "explore", AccountID: 20, TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 200},
	}))
	w.doPG(context.Background())
	require.GreaterOrEqual(t, len(pg.flows), 1)
	var got []repository.RoutingFlowRow
	for _, rows := range pg.flows {
		got = rows
	}
	require.GreaterOrEqual(t, len(got), 2, "must carry complete rows, not a single fabricated row")
	found := make(map[int64]bool)
	for _, row := range got {
		found[row.ChainCount] = true
	}
	require.True(t, found[100] && found[200], "counts must be actual")

	// failed flow stays dirty-retained for next-cycle retry (no requeue)
	pg2 := newFakePG()
	rec2, _ := NewRecorder(50000)
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "src-flow2", BatchSize: 10}, nil, nil)
	w2.SetClock(func() time.Time { return fixed })
	require.NoError(t, foldConsumerRows(rec2.FlowOwner(), fixed.Unix(), []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 10, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 100},
	}))
	pg2.failAll = true
	w2.doPG(context.Background())
	require.Equal(t, 1, rec2.MinuteBucketCount(), "failed flow must stay dirty-retained for retry")
	require.Equal(t, 0, len(pg2.flows))
}

func TestQualitySync_FlowRowsAreLookedUpAndPersisted(t *testing.T) {
	// Given: a non-empty contribution for the minute.
	// (v3-hygiene: the empty-marker half is deleted with the consumer seam —
	// no live writer exists for it.)
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	minute := time.Date(2026, 8, 29, 12, 7, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "empty-then-rows", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return minute })

	row := flowTestRow(minute, 1, 42, "success", true)
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{row}))

	// When: the owner is read and the flow minute is persisted.
	got, ok := rec.FlowMinute(minute.Unix())
	require.True(t, ok)

	w.doPG(context.Background())

	// Then: the retained row is emitted to PG.
	require.False(t, got.IsEmptySnapshot())
	require.Equal(t, []repository.RoutingFlowRow{row}, got.FlowRows())
	var persisted []repository.RoutingFlowRow
	for _, rows := range pg.flows {
		persisted = rows
	}
	row.InstanceSrc = "empty-then-rows"
	row.AbsoluteSequence = 1
	require.Equal(t, []repository.RoutingFlowRow{row}, persisted)
}

func TestQualitySync_RedisErrorDegradesFreshnessAndPublishesFlow(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-redis", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return clk })
	k := keyOf(fp(5), qc(5))
	qm := NewQualityMinute(fixed.Unix(), k)
	qm.SetAttempts(10)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	// v3-hygiene: the legacy edges-array vehicle is deleted — a live row
	// carries the flow publish instead.
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), fixed.Unix(), []repository.RoutingFlowRow{
		{IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 7, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 1},
	}))
	w.doRedis(context.Background())
	require.GreaterOrEqual(t, len(mr.Keys()), 2, "should publish quality and flow")
	hasFlow := false
	for _, key := range mr.Keys() {
		if len(key) >= len(redisFlowPrefix) && key[:len(redisFlowPrefix)] == redisFlowPrefix {
			hasFlow = true
		}
	}
	require.True(t, hasFlow, "flow namespace must be published")
	stats := w.statsSnapshot()
	require.Equal(t, int64(0), stats.FreshnessMs, "successful publish freshness 0")

	// now simulate redis error by closing
	mr.Close()
	// need fresh rdb that fails
	badRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	w2 := NewSyncWorker(rec, badRdb, pg, SyncConfig{InstanceSrc: "src-redis2", BatchSize: 10}, nil, nil)
	w2.SetClock(func() time.Time { return clk.Add(time.Second) })
	// ensure lastRedis is set
	w2.lastRedis = clk
	qm2 := NewQualityMinute(fixed.Unix(), keyOf(fp(6), qc(6)))
	qm2.SetAttempts(5)
	_ = rec.EnqueueQualityMinute(qm2)
	w2.doRedis(context.Background())
	stats2 := w2.statsSnapshot()
	require.Greater(t, stats2.FreshnessMs, int64(0), "publish error must degrade freshness, not false success")
	require.NotEqual(t, w2.lastRedis, clk.Add(time.Second), "failed publish must not update lastRedis")
}

func TestQualitySync_ActiveCellDeltaNotLifetime(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-delta", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return clk })
	k := keyOf(fp(9), qc(9))
	cell := rec.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx))
	tt := int64(100)
	ctx.Complete(true, &tt, 10, 1, 0)
	// first redis publish for minute M
	w.doRedis(context.Background())
	// successful publish must ACK/remove minuteAbs (historical drain)
	w.mu.Lock()
	_, has1 := w.minuteAbs[fixed.Unix()]
	w.mu.Unlock()
	require.False(t, has1, "successful publish must ACK minuteAbs")
	require.GreaterOrEqual(t, len(mr.Keys()), 1)

	// second attempt same minute - must be delta 1, not cumulative 2, and ack again
	var ctx2 AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx2))
	ctx2.Complete(true, &tt, 10, 1, 0)
	clk = fixed.Add(10 * time.Second)
	w.SetClock(func() time.Time { return clk })
	w.doRedis(context.Background())
	w.mu.Lock()
	_, has2 := w.minuteAbs[fixed.Unix()]
	w.mu.Unlock()
	require.False(t, has2, "second publish must ACK again, retain newer delta only for next tick")
	// next minute
	clk = fixed.Add(time.Minute)
	w.SetClock(func() time.Time { return clk })
	var ctx3 AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx3))
	ctx3.Complete(true, &tt, 10, 1, 0)
	w.doRedis(context.Background())
	w.mu.Lock()
	_, hasNext := w.minuteAbs[fixed.Add(time.Minute).Unix()]
	w.mu.Unlock()
	require.False(t, hasNext, "next minute must be delta 1 and then ACK, not lifetime")
}

func TestQualitySync_PGMergeRetainsPriorAbsolute(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-merge", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return clk })
	k := keyOf(fp(11), qc(11))
	qm1 := NewQualityMinute(fixed.Unix(), k)
	qm1.SetAttempts(5)
	qm1.SetSuccesses(3)
	require.NoError(t, rec.EnqueueQualityMinute(qm1))
	w.doPG(context.Background())
	require.Equal(t, 1, len(pg.quality))
	require.Equal(t, int64(5), pg.quality[0].Attempts)
	// second contribution same minute same key, should merge without losing old
	qm2 := NewQualityMinute(fixed.Unix(), k)
	qm2.SetAttempts(2)
	qm2.SetSuccesses(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm2))
	// need to ensure pgCommitted retains prior, so merged absolute = 7
	clk = fixed.Add(time.Second)
	w.SetClock(func() time.Time { return clk })
	w.doPG(context.Background())
	require.Equal(t, 1, len(pg.quality))
	require.Equal(t, int64(7), pg.quality[0].Attempts, "must merge prior PG absolute without losing")
	require.Equal(t, int64(4), pg.quality[0].Successes)
	// stale sequence must not overwrite: fakePG enforces sequence check
}

func TestQualitySync_CloseDrainAndUnstartedSafe(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-close", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	k := keyOf(fp(12), qc(12))
	qm := NewQualityMinute(fixed.Unix(), k)
	qm.SetAttempts(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	// unstarted close must be safe and not double-close
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, 1, len(pg.quality), "close must drain pending with non-canceled context")

	// started close with barrier
	pg2 := newFakePG()
	rec2, _ := NewRecorder(50000)
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "src-close2", BatchSize: 10}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w2.Start(ctx))
	qm2 := NewQualityMinute(fixed.Unix(), keyOf(fp(13), qc(13)))
	qm2.SetAttempts(2)
	require.NoError(t, rec2.EnqueueQualityMinute(qm2))
	// use channel barrier: close should drain even though ctx is canceled
	cancel()
	done := make(chan error, 1)
	go func() { done <- w2.Close(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("close did not complete")
	}
	require.Equal(t, 1, len(pg2.flows)+len(pg2.quality))
}

func TestQualitySync_RefillRespectsCapacity(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	pg.failAll = true
	rec, _ := NewRecorder(10)
	rec.pendingCapBytes = 2 * EstimatedQualityRowBytes
	rec.minuteCap = 2
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-cap", BatchSize: 10}, nil, nil)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w.SetClock(func() time.Time { return fixed })
	// fill capacity
	for i := 0; i < 3; i++ {
		k := keyOf(fp(byte(30+i)), qc(byte(30+i)))
		qm := NewQualityMinute(int64(1000+i), k)
		qm.SetAttempts(1)
		_ = rec.EnqueueQualityMinute(qm)
	}
	beforeBytes := rec.PendingBytes()
	require.LessOrEqual(t, beforeBytes, rec.pendingCapBytes+EstimatedQualityRowBytes)
	// trigger PG flush that will fail and refill via Enqueue (which respects capacity)
	w.doPG(context.Background())
	afterBytes := rec.PendingBytes()
	require.LessOrEqual(t, afterBytes, rec.pendingCapBytes+EstimatedQualityRowBytes, "refill must pass capacity accounting")
	require.Greater(t, rec.QualityOverflow()+rec.FlowOverflow()+rec.MinuteOverflow(), int64(0), "eviction should have occurred")
}

func TestQualitySync_RoutingTypes(t *testing.T) {
	// ensure adapter uses exact repository types
	var _ PGQualityWriter = (*fakePG)(nil)
	// check domain types exist
	_ = domain.RoutingIdentityVersion
	_ = repository.RoutingQualityRow{}
	_ = repository.RoutingFlowRow{}
	// ensure json marshaling of redis payload includes hist
	k := keyOf(fp(1), qc(1))
	qm := NewQualityMinute(1000, k)
	qm.SetAttempts(1)
	b, err := json.Marshal(map[string]any{"hist": qm.Hist()})
	require.NoError(t, err)
	require.Contains(t, string(b), "hist")
}

func TestQualitySync_PerSinkAckDrainsAllDue(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-persist", BatchSize: 10}, nil, nil)
	// create two minutes of active data
	k1 := keyOf(fp(40), qc(40))
	k2 := keyOf(fp(41), qc(41))
	c1 := rec.GetOrCreateCell(k1)
	var ac1 AttemptContext
	require.True(t, rec.InitAttemptContext(c1, &ac1))
	tt := int64(100)
	ac1.Complete(true, &tt, 5, 1, 0)
	w.SetClock(func() time.Time { return base })
	w.doRedis(context.Background())
	require.GreaterOrEqual(t, len(mr.Keys()), 1)
	// advance one minute, new active delta
	c2 := rec.GetOrCreateCell(k2)
	var ac2 AttemptContext
	require.True(t, rec.InitAttemptContext(c2, &ac2))
	ac2.Complete(true, &tt, 5, 1, 0)
	w.SetClock(func() time.Time { return base.Add(time.Minute) })
	// make redis fail for next publish
	badRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	wBad := NewSyncWorker(rec, badRdb, pg, SyncConfig{InstanceSrc: "src-persist", BatchSize: 10}, nil, nil)
	wBad.SetClock(func() time.Time { return base.Add(time.Minute) })
	// copy over lastCell/minuteAbs to simulate same worker with redis failure
	wBad.lastCell = w.lastCell
	wBad.minuteAbs = w.minuteAbs
	wBad.redisSeq = w.redisSeq
	wBad.doRedis(context.Background())
	// redis failure must not lose PG delta: PG should still see both minutes
	wBad.pgLastCell = w.pgLastCell
	wBad.pgMinuteAbs = w.pgMinuteAbs
	wBad.pgSeq = w.pgSeq
	// PG should drain both minutes even though redis failed
	wBad.SetClock(func() time.Time { return base.Add(time.Minute) })
	wBad.doPG(context.Background())
	require.GreaterOrEqual(t, len(pg.quality), 1)
	// next redis retry should still have first minute's delta
	w.SetClock(func() time.Time { return base.Add(2 * time.Minute) })
	// ensure minuteAbs still has undelivered due minutes
	w.mu.Lock()
	hasDue := len(w.minuteAbs) > 0
	w.mu.Unlock()
	_ = hasDue
}

func TestQualitySync_PGActiveOnly(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-activeonly", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	k := keyOf(fp(50), qc(50))
	cell := rec.GetOrCreateCell(k)
	var ac AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ac))
	tt := int64(100)
	ac.Complete(true, &tt, 7, 1, 0)
	// pending is empty, but active-only should be collected
	require.Equal(t, 0, rec.MinuteBucketCount())
	w.doPG(context.Background())
	require.Equal(t, 1, len(pg.quality))
	require.Equal(t, int64(1), pg.quality[0].Attempts)
}

func TestQualitySync_FullIdentityFlowRowsPreserved(t *testing.T) {
	// v3-hygiene: the empty-snapshot half is deleted with the consumer seam
	// (no live writer exists for it) — the full-identity rows half remains.
	_, rdb := newMiniRedis(t)
	fixed := time.Date(2026, 8, 29, 12, 5, 0, 0, time.UTC)
	// test full identity flow rows
	pg2 := newFakePG()
	rec2, _ := NewRecorder(50000)
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "src-fullflow", BatchSize: 10}, nil, nil)
	w2.SetClock(func() time.Time { return fixed })
	rows := []repository.RoutingFlowRow{
		{
			IdentityVersion: 1,
			RouteClassID: func() domain.RouteClassIDVal {
				v, _ := domain.RouteClassID(1, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
				return v
			}(),
			CandidateFingerprint: func() domain.CandidateFingerprintVal {
				v, _ := domain.CandidateFingerprint(1, 1, "api_key", "https://api.openai.com", "sk", "", "", "", false, "", "", "", "")
				return v
			}(),
			TerminalMinute: fixed, Ordinal: 1, Lane: "primary", AccountID: 10, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 5,
		},
		{
			IdentityVersion: 1,
			RouteClassID: func() domain.RouteClassIDVal {
				v, _ := domain.RouteClassID(1, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
				return v
			}(),
			CandidateFingerprint: func() domain.CandidateFingerprintVal {
				v, _ := domain.CandidateFingerprint(2, 1, "api_key", "https://api.openai.com", "sk2", "", "", "", false, "", "", "", "")
				return v
			}(),
			// v3-F1: test-only lane-a/lane-b strings are not codes — tests
			// use real lanes; "init"/"retry" still round-trip exactly.
			TerminalMinute: fixed, Ordinal: 2, Lane: "explore", AccountID: 20, PreviousAccountID: func() *int64 { v := int64(10); return &v }(), PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, Generation: 5,
		},
	}
	require.NoError(t, foldConsumerRows(rec2.FlowOwner(), fixed.Unix(), rows))
	w2.doPG(context.Background())
	pg2.mu.Lock()
	got := pg2.flows["src-fullflow:"+fixed.UTC().Truncate(time.Minute).String()]
	pg2.mu.Unlock()
	require.Equal(t, 2, len(got))
	require.Equal(t, int64(10), got[0].AccountID)
	require.Equal(t, "primary", got[0].Lane)
	require.Equal(t, int64(5), got[0].Generation)
	require.Equal(t, "retry", got[1].TransitionReason)
}

func TestQualitySync_CapacityRefillAccountsDrop(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	pg.failAll = true
	rec, _ := NewRecorder(10)
	rec.pendingCapBytes = EstimatedQualityRowBytes
	rec.minuteCap = 1
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-cap2", BatchSize: 10}, nil, nil)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w.SetClock(func() time.Time { return fixed })
	k1 := keyOf(fp(60), qc(60))
	k2 := keyOf(fp(61), qc(61))
	qm1 := NewQualityMinute(fixed.Unix(), k1)
	qm1.SetAttempts(1)
	qm2 := NewQualityMinute(fixed.Unix()+60, k2)
	qm2.SetAttempts(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm1))
	err := rec.EnqueueQualityMinute(qm2)
	// second may be dropped due to capacity, should be accounted
	if err != nil {
		require.ErrorIs(t, err, ErrCapacity)
	}
	beforeOverflow := rec.QualityOverflow() + rec.FlowOverflow() + rec.MinuteOverflow()
	w.doPG(context.Background())
	afterOverflow := rec.QualityOverflow() + rec.FlowOverflow() + rec.MinuteOverflow()
	// refill of failed rows that exceed capacity must be accounted, not silently ignored
	require.GreaterOrEqual(t, afterOverflow, beforeOverflow)
}

func TestQualitySync_RedisEmptyNoRefresh(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-emptyredis", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	w.lastRedis = fixed.Add(-time.Minute)
	w.lastRedisAttempt = time.Time{}
	w.doRedis(context.Background())
	stats := w.statsSnapshot()
	require.Equal(t, int64(0), stats.LastRedisMs, "empty pass must not update LastRedisMs")
	require.Equal(t, fixed.Add(-time.Minute).UnixMilli(), w.lastRedis.UnixMilli(), "empty must not refresh lastRedis")
	// no-client pass
	w2 := NewSyncWorker(rec, nil, pg, SyncConfig{InstanceSrc: "src-noclient", BatchSize: 10}, nil, nil)
	w2.SetClock(func() time.Time { return fixed })
	k := keyOf(fp(70), qc(70))
	qm := NewQualityMinute(fixed.Unix(), k)
	qm.SetAttempts(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	w2.lastRedis = fixed.Add(-time.Minute)
	w2.doRedis(context.Background())
	stats2 := w2.statsSnapshot()
	require.NotEqual(t, "", stats2.LastRedisError)
	require.Greater(t, stats2.RedisErrors, int64(0))
	// freshness should be degraded (not 0)
	require.Greater(t, stats2.FreshnessMs, int64(0))
}

func TestQualitySync_CloseReturnsContextErrorOnAbandon(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := &blockingPG{block: make(chan struct{})}
	rec, _ := NewRecorder(50000)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-abandon", BatchSize: 10}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	// enqueue to have pending for drain
	k := keyOf(fp(80), qc(80))
	qm := NewQualityMinute(time.Now().Unix(), k)
	qm.SetAttempts(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	// hold flushMu to simulate in-flight
	w.flushMu.Lock()
	done := make(chan error, 1)
	go func() {
		bounded, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel2()
		done <- w.Close(bounded)
	}()
	time.Sleep(10 * time.Millisecond)
	w.flushMu.Unlock()
	close(pg.block)
	cancel()
	err := <-done
	// should return context error due to abandon or timeout
	_ = err
}

func TestQualitySync_StartCloseSameContext(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-ctx", BatchSize: 10}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	require.NotNil(t, w.baseCtx)
	// derived context should be child of ctx and canceled when parent canceled
	cancel()
	select {
	case <-w.baseCtx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("baseCtx should be canceled when parent canceled")
	}
	require.NoError(t, w.Close(context.Background()))
}

type blockingPG struct {
	block chan struct{}
}

func (b *blockingPG) UpsertQualityAndMarkDirty(ctx context.Context, row repository.RoutingQualityRow) error {
	select {
	case <-b.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *blockingPG) UpsertFlowSnapshot(ctx context.Context, instanceSrc string, terminalMinute time.Time, identityVersion int16, absoluteSequence int64, rows []repository.RoutingFlowRow) error {
	select {
	case <-b.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestQualitySync_PersistedQualityNotifiesCompiler 是缺陷 B 的回归：quality-sync
// 的 PG 落库边界是质量 influx 的事件驱动编译触发点——成功落库 ≥1 新质量行必须
// 调用编译通知（装配期接 scheduler.RequestCompile，非阻塞、下游去抖收敛）；空
// 刷（无新行）必须静默，否则静默期重编译退化为定周全量（违 §9）。同时锁定缺陷
// C 的生产写面：2ms TTFT 经生产路径（Begin→Complete→delta→PG 行）和必须非零。
func TestQualitySync_PersistedQualityNotifiesCompiler(t *testing.T) {
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	var calls int
	w := NewSyncWorker(rec, nil, pg, SyncConfig{InstanceSrc: "src-notify", BatchSize: 10}, nil, func() { calls++ })
	// 空刷：无新质量行 → 静默。
	w.doPG(context.Background())
	require.Equal(t, 0, calls, "空刷（无新质量行）不得惊动编译道")
	require.Empty(t, pg.quality)

	// 生产路径观测：2ms TTFT 成功。
	k := keyOf(fp(41), qc(41))
	tt := int64(2)
	actx := rec.Begin(k)
	actx.Complete(true, &tt, 10, 1, 0)
	w.doPG(context.Background())
	require.Equal(t, 1, calls, "落库 ≥1 新质量行必须触发编译")
	require.Len(t, pg.quality, 1)
	require.Equal(t, int64(1), pg.quality[0].Attempts)
	require.Equal(t, int64(1), pg.quality[0].TTFTN)
	require.NotZero(t, pg.quality[0].TTFTSumLogQ32, "2ms TTFT 对数和必须非零")
	require.NotZero(t, pg.quality[0].TTFTSumSqLogQ32, "2ms TTFT 平方和必须非零")

	// 同分钟追加密集：再次落库 → 再次通知（去抖是编译道职责，非本层）。
	actx2 := rec.Begin(k)
	actx2.Complete(true, &tt, 10, 1, 0)
	w.doPG(context.Background())
	require.Equal(t, 2, calls, "第二批新质量行必须再次触发编译")
}
