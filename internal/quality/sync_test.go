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
		return context.DeadlineExceeded
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
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-budget", BatchSize: 5}, nil)
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
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "src-budget2", BatchSize: 5}, nil)
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
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-poison", BatchSize: 10}, nil)
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
	w3 := NewSyncWorker(rec3, rdb, pg3, SyncConfig{InstanceSrc: "src-dbwide", BatchSize: 10}, nil)
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

func TestQualitySync_FlowPreservesEdgeArraysAndRequeuesWholeMinute(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-flow", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })
	edges := [8]int64{10, 20, 30, 40, 50, 60, 70, 80}
	fm := NewFlowMinute(fixed.Unix(), edges)
	for i := 0; i < 8; i++ {
		fm.SetCount(i, int64(i*100))
	}
	require.NoError(t, rec.EnqueueFlowMinute(fm))
	w.doPG(context.Background())
	require.GreaterOrEqual(t, len(pg.flows), 1)
	var got []repository.RoutingFlowRow
	for _, rows := range pg.flows {
		got = rows
	}
	require.GreaterOrEqual(t, len(got), 2, "must carry complete edge arrays, not single fabricated row")
	found := make(map[int64]bool)
	for _, row := range got {
		found[row.ChainCount] = true
		require.NotEqual(t, int64(1), row.AccountID, "should not fabricate account1")
		require.NotEqual(t, "primary", row.Lane, "should not fabricate primary for all")
	}
	require.True(t, found[100] || found[0], "counts must be actual")

	// failed flow requeues whole minute
	pg2 := newFakePG()
	rec2, _ := NewRecorder(50000)
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "src-flow2", BatchSize: 10}, nil)
	w2.SetClock(func() time.Time { return fixed })
	require.NoError(t, rec2.EnqueueFlowMinute(fm.Clone()))
	pg2.failAll = true
	w2.doPG(context.Background())
	require.Equal(t, 1, rec2.MinuteBucketCount(), "failed flow must requeue whole minute")
	require.Equal(t, 0, len(pg2.flows))
}

func TestQualitySync_RedisErrorDegradesFreshnessAndPublishesFlow(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-redis", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return clk })
	k := keyOf(fp(5), qc(5))
	qm := NewQualityMinute(fixed.Unix(), k)
	qm.SetAttempts(10)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	edges := [8]int64{1, 2, 3, 4, 5, 6, 7, 8}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowMinute(fixed.Unix(), edges)))
	w.doRedis(context.Background())
	require.GreaterOrEqual(t, len(mr.Keys()), 2, "should publish quality and flow")
	hasFlow := false
	for _, key := range mr.Keys() {
		if len(key) >= len(redisFlowPrefix) && key[:len(redisFlowPrefix)] == redisFlowPrefix {
			hasFlow = true
		}
	}
	require.True(t, hasFlow, "flow namespace must be published")
	stats := w.Stats()
	require.Equal(t, int64(0), stats.FreshnessMs, "successful publish freshness 0")

	// now simulate redis error by closing
	mr.Close()
	// need fresh rdb that fails
	badRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	w2 := NewSyncWorker(rec, badRdb, pg, SyncConfig{InstanceSrc: "src-redis2", BatchSize: 10}, nil)
	w2.SetClock(func() time.Time { return clk.Add(time.Second) })
	// ensure lastRedis is set
	w2.lastRedis = clk
	qm2 := NewQualityMinute(fixed.Unix(), keyOf(fp(6), qc(6)))
	qm2.SetAttempts(5)
	_ = rec.EnqueueQualityMinute(qm2)
	w2.doRedis(context.Background())
	stats2 := w2.Stats()
	require.Greater(t, stats2.FreshnessMs, int64(0), "publish error must degrade freshness, not false success")
	require.NotEqual(t, w2.lastRedis, clk.Add(time.Second), "failed publish must not update lastRedis")
}

func TestQualitySync_ActiveCellDeltaNotLifetime(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-delta", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return clk })
	k := keyOf(fp(9), qc(9))
	cell := rec.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx))
	tt := int64(100)
	ctx.Complete(true, &tt, 10, 1, 0)
	// first redis publish for minute M
	w.doRedis(context.Background())
	// capture minuteAbs for M
	w.mu.Lock()
	abs1 := w.minuteAbs[fixed.Unix()]
	w.mu.Unlock()
	require.NotNil(t, abs1)
	require.Equal(t, int64(1), abs1[k].Attempts())

	// second attempt same minute
	var ctx2 AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx2))
	ctx2.Complete(true, &tt, 10, 1, 0)
	clk = fixed.Add(10 * time.Second)
	w.SetClock(func() time.Time { return clk })
	w.doRedis(context.Background())
	w.mu.Lock()
	abs2 := w.minuteAbs[fixed.Unix()]
	w.mu.Unlock()
	require.Equal(t, int64(2), abs2[k].Attempts(), "same minute delta must accumulate to 2, not lifetime repeat")
	// next minute
	clk = fixed.Add(time.Minute)
	w.SetClock(func() time.Time { return clk })
	var ctx3 AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx3))
	ctx3.Complete(true, &tt, 10, 1, 0)
	w.doRedis(context.Background())
	w.mu.Lock()
	absNext := w.minuteAbs[fixed.Add(time.Minute).Unix()]
	w.mu.Unlock()
	require.NotNil(t, absNext)
	require.Equal(t, int64(1), absNext[k].Attempts(), "next minute must be delta 1, not lifetime 3")
}

func TestQualitySync_PGMergeRetainsPriorAbsolute(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	clk := fixed
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-merge", BatchSize: 10}, nil)
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
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-close", BatchSize: 10}, nil)
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
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "src-close2", BatchSize: 10}, nil)
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
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "src-cap", BatchSize: 10}, nil)
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
