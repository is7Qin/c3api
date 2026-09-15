// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func TestRed_CursorGenerationFencingRedis(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-gen-redis", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return base })
	k := keyOf(fp(200), qc(200))
	cell1 := rec.GetOrCreateCell(k)
	var ac1 AttemptContext
	require.True(t, rec.InitAttemptContext(cell1, &ac1))
	tt := int64(100)
	ac1.Complete(true, &tt, 10, 1, 0)
	barrier := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-barrier
		w.doRedis(context.Background())
		close(done)
	}()
	close(barrier)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first doRedis did not complete")
	}
	w.mu.Lock()
	cur1, hasCur1 := w.lastCell[k]
	w.mu.Unlock()
	require.True(t, hasCur1)
	require.Equal(t, cell1.gen, cur1.gen)
	rec.Retire(k)
	cell2 := rec.GetOrCreateCell(k)
	require.NotSame(t, cell1, cell2)
	require.NotEqual(t, cell1.gen, cell2.gen)
	var ac2 AttemptContext
	require.True(t, rec.InitAttemptContext(cell2, &ac2))
	ac2.Complete(true, &tt, 10, 1, 0)
	barrier2 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		<-barrier2
		w.doRedis(context.Background())
		close(done2)
	}()
	close(barrier2)
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("second doRedis did not complete")
	}
	w.mu.Lock()
	cur2, hasCur2 := w.lastCell[k]
	w.mu.Unlock()
	require.True(t, hasCur2)
	require.Equal(t, cell2.gen, cur2.gen)
	require.Equal(t, int64(1), cur2.snap.attempts)
}

func TestRed_CursorGenerationFencingPG(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-gen-pg", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return base })
	k := keyOf(fp(201), qc(201))
	cell1 := rec.GetOrCreateCell(k)
	var ac1 AttemptContext
	require.True(t, rec.InitAttemptContext(cell1, &ac1))
	tt := int64(80)
	ac1.Complete(true, &tt, 5, 1, 0)
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
		t.Fatal("first doPG did not complete")
	}
	require.Equal(t, 1, len(pg.quality))
	require.Equal(t, int64(1), pg.quality[0].Attempts)
	w.mu.Lock()
	pgCur1, ok1 := w.pgLastCell[k]
	w.mu.Unlock()
	require.True(t, ok1)
	require.Equal(t, cell1.gen, pgCur1.gen)
	rec.Retire(k)
	cell2 := rec.GetOrCreateCell(k)
	require.NotSame(t, cell1, cell2)
	var ac2 AttemptContext
	require.True(t, rec.InitAttemptContext(cell2, &ac2))
	ac2.Complete(true, &tt, 5, 1, 0)
	barrier2 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		<-barrier2
		w.doPG(context.Background())
		close(done2)
	}()
	close(barrier2)
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("second doPG did not complete")
	}
	t.Logf("w clock base=%v nowClock=%v", base, w.clock())
	var totalAttempts int64
	for i, row := range pg.quality {
		t.Logf("pg row %d: attempts=%d seq=%d minute=%v", i, row.Attempts, row.AbsoluteSequence, row.BucketMinute)
		totalAttempts += row.Attempts
	}
	t.Logf("totalAttempts=%d len=%d", totalAttempts, len(pg.quality))
	// generation fencing: second generation must not be subtracted, so total should be 2 or 3 depending on minute bucketing
	// The key point is that second publish's attempts is not 0 and not negative
	require.GreaterOrEqual(t, totalAttempts, int64(2))
	require.LessOrEqual(t, len(pg.quality), 2)
	w.mu.Lock()
	pgCur2, ok2 := w.pgLastCell[k]
	w.mu.Unlock()
	require.True(t, ok2)
	require.Equal(t, cell2.gen, pgCur2.gen)
	require.Equal(t, int64(1), pgCur2.snap.attempts)
}

func TestRed_CloseBoundedDrainContinuesUntilEmpty(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := &retryFakePG{failFirst: 1}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-close-drain", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	for i := 0; i < 2; i++ {
		k := keyOf(fp(byte(210+i)), qc(byte(210+i)))
		qm := NewQualityMinute(fixed.Unix(), k)
		qm.SetAttempts(int64(i + 1))
		require.NoError(t, rec.EnqueueQualityMinute(qm))
	}
	barrier := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-barrier
		done <- w.Close(context.Background())
	}()
	close(barrier)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	rec.mu.Lock()
	qRem := 0
	for _, rows := range rec.pendingQuality {
		qRem += len(rows)
	}
	rec.mu.Unlock()
	require.Equal(t, 0, qRem)
	require.GreaterOrEqual(t, len(pg.quality), 2)
}

func TestRed_CloseReturnsRemainingWorkError(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := &blockingPG{block: make(chan struct{})}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-close-remaining", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	k := keyOf(fp(220), qc(220))
	qm := NewQualityMinute(fixed.Unix(), k)
	qm.SetAttempts(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	require.NoError(t, w.Start(context.Background()))
	w.flushMu.Lock()
	barrier := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-barrier
		bounded, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		done <- w.Close(bounded)
	}()
	close(barrier)
	select {
	case <-time.After(20 * time.Millisecond):
	}
	w.flushMu.Unlock()
	select {
	case err := <-done:
		require.Error(t, err)
		require.Contains(t, err.Error(), "remaining")
	case <-time.After(3 * time.Second):
		t.Fatal("Close with bounded context did not return")
	}
	close(pg.block)
}

func TestRed_BisectDropsOnlyTypedAndRefillsTransient(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newTypedFakePG()
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-bisect-typed", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	pg.failAll = context.DeadlineExceeded
	kT := keyOf(fpByte(231), qcByte(231))
	qmT := NewQualityMinute(fixed.Unix(), kT)
	qmT.SetAttempts(3)
	require.NoError(t, rec.EnqueueQualityMinute(qmT))
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
		t.Fatal("doPG transient singleton did not complete")
	}
	require.Equal(t, int64(0), w.poison.Load())
	rec.mu.Lock()
	remT := 0
	for _, rows := range rec.pendingQuality {
		remT += len(rows)
	}
	rec.mu.Unlock()
	require.Equal(t, 1, remT)
	require.Equal(t, 0, len(pg.quality))
	pg2 := newTypedFakePG()
	pg2.failAll = &RowDataError{Msg: "check constraint violates"}
	rec2, _ := NewRecorder(50000)
	w2 := NewSyncWorker(rec2, rdb, pg2, SyncConfig{InstanceSrc: "red-bisect-poison", BatchSize: 10}, nil, nil)
	w2.SetClock(func() time.Time { return fixed })
	kP := keyOf(fpByte(232), qcByte(232))
	qmP := NewQualityMinute(fixed.Unix(), kP)
	qmP.SetAttempts(3)
	require.NoError(t, rec2.EnqueueQualityMinute(qmP))
	barrier2 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		<-barrier2
		w2.doPG(context.Background())
		close(done2)
	}()
	close(barrier2)
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("doPG poison singleton did not complete")
	}
	require.Equal(t, int64(1), w2.poison.Load())
	rec2.mu.Lock()
	remP := 0
	for _, rows := range rec2.pendingQuality {
		remP += len(rows)
	}
	rec2.mu.Unlock()
	require.Equal(t, 0, remP)
	require.Equal(t, 0, len(pg2.quality))
}

func TestRed_BisectTransientInChunkMustRefill(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newTypedFakePG()
	k1 := keyOf(fpByte(233), qcByte(233))
	k2 := keyOf(fpByte(234), qcByte(234))
	pg.rowErr[string(k1.Fingerprint[:])] = context.DeadlineExceeded
	rec, _ := NewRecorder(50000)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-bisect-chunk-transient", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	qm1 := NewQualityMinute(fixed.Unix(), k1)
	qm1.SetAttempts(1)
	qm2 := NewQualityMinute(fixed.Unix(), k2)
	qm2.SetAttempts(2)
	require.NoError(t, rec.EnqueueQualityMinute(qm1))
	require.NoError(t, rec.EnqueueQualityMinute(qm2))
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
		t.Fatal("doPG chunk transient did not complete")
	}
	require.Equal(t, int64(0), w.poison.Load())
	rec.mu.Lock()
	rem := 0
	for _, rows := range rec.pendingQuality {
		rem += len(rows)
	}
	rec.mu.Unlock()
	require.GreaterOrEqual(t, rem, 1)
	require.Equal(t, 1, len(pg.quality))
}

type retryFakePG struct {
	quality   []repository.RoutingQualityRow
	failFirst int
	calls     int
}

func (f *retryFakePG) UpsertQualityAndMarkDirty(_ context.Context, row repository.RoutingQualityRow) error {
	f.calls++
	if f.calls <= f.failFirst {
		return context.DeadlineExceeded
	}
	f.quality = append(f.quality, row)
	return nil
}

func (f *retryFakePG) UpsertFlowSnapshot(_ context.Context, _ string, _ time.Time, _ int16, _ int64, _ []repository.RoutingFlowRow) error {
	return nil
}
