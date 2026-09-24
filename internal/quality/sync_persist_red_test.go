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

type typedFakePG struct {
	mu      sync.Mutex
	quality []repository.RoutingQualityRow
	failAll error
	rowErr  map[string]error
	calls   int
}

func newTypedFakePG() *typedFakePG {
	return &typedFakePG{rowErr: make(map[string]error)}
}

func (f *typedFakePG) UpsertQualityRow(_ context.Context, row repository.RoutingQualityRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAll != nil {
		return f.failAll
	}
	k := string(row.CandidateFingerprint[:])
	if err, ok := f.rowErr[k]; ok && err != nil {
		return err
	}
	for i, existing := range f.quality {
		if existing.CandidateFingerprint == row.CandidateFingerprint && existing.BucketMinute.Equal(row.BucketMinute) && existing.InstanceSrc == row.InstanceSrc {
			f.quality[i] = row
			return nil
		}
	}
	f.quality = append(f.quality, row)
	return nil
}

func (f *typedFakePG) UpsertFlowSnapshot(_ context.Context, instanceSrc string, terminalMinute time.Time, _ int16, seq int64, rows []repository.RoutingFlowRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAll != nil {
		return f.failAll
	}
	return nil
}

func fpByte(b byte) [32]byte {
	var out [32]byte
	out[0] = b
	out[1] = b ^ 0x55
	return out
}

func qcByte(b byte) [32]byte {
	var out [32]byte
	out[0] = b
	out[31] = b
	return out
}

func TestQualitySync_SingletonTransientMustRefill(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newTypedFakePG()
	pg.failAll = context.DeadlineExceeded
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-singleton-transient", BatchSize: 1}, nil, nil)
	w.SetClock(func() time.Time { return fixed })

	barrier := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-barrier
		w.doPG(context.Background())
		close(done)
	}()
	k := keyOf(fpByte(201), qcByte(201))
	qm := NewQualityMinute(fixed.Unix(), k)
	qm.SetAttempts(3)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	close(barrier)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("doPG did not complete")
	}
	// singleton transient must not be dropped as poison
	require.Equal(t, int64(0), w.poison.Load())
	rec.mu.Lock()
	rowsCount := 0
	for _, rows := range rec.pendingQuality {
		rowsCount += len(rows)
	}
	rec.mu.Unlock()
	require.Equal(t, 1, rowsCount, "singleton DB-wide/transient failure must refill")
	_ = rdb
}

func TestQualitySync_SingletonRowDataErrorIsPoison(t *testing.T) {
	_, rdb := newMiniRedis(t)
	pg := newTypedFakePG()
	pg.failAll = &RowDataError{Msg: "check constraint violates quality_attempts"}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "red-singleton-poison", BatchSize: 1}, nil, nil)
	w.SetClock(func() time.Time { return fixed })

	barrier := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-barrier
		w.doPG(context.Background())
		close(done)
	}()
	k := keyOf(fpByte(202), qcByte(202))
	qm := NewQualityMinute(fixed.Unix(), k)
	qm.SetAttempts(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	close(barrier)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("doPG did not complete")
	}
	require.Equal(t, int64(1), w.poison.Load(), "only proven row-specific data error may be dropped and counted poison")
	require.Equal(t, int64(1), w.statsSnapshot().PoisonDropped)
	rec.mu.Lock()
	rowsCount := 0
	for _, rows := range rec.pendingQuality {
		rowsCount += len(rows)
	}
	rec.mu.Unlock()
	require.Equal(t, 0, rowsCount, "poison row must be dropped, not refilled")
}

func TestQualitySync_MixedActivePendingRefillExactlyOnce(t *testing.T) {
	_, rdb := newMiniRedis(t)
	// barrier to prove concurrent Enqueue vs doPG uses independent snapshots
	blockPG := &typedFakePG{failAll: context.DeadlineExceeded}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	w := NewSyncWorker(rec, rdb, blockPG, SyncConfig{InstanceSrc: "red-mixed", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })

	// pending component
	kPending := keyOf(fpByte(203), qcByte(203))
	qmPending := NewQualityMinute(fixed.Unix(), kPending)
	qmPending.SetAttempts(5)
	qmPending.SetSuccesses(2)
	require.NoError(t, rec.EnqueueQualityMinute(qmPending))

	// active component via independent cursor
	kActive := keyOf(fpByte(204), qcByte(204))
	cell := rec.GetOrCreateCell(kActive)
	var ac AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ac))
	tt := int64(80)
	ac.Complete(true, &tt, 7, 1, 0)

	// snapshot barriers: ensure active delta collected via pgLastCell independent of pending swap
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		w.doPG(context.Background())
		close(finished)
	}()
	<-started
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("mixed doPG did not finish")
	}
	// both components must be restored exactly once after PG failure
	rec.mu.Lock()
	totalRows := 0
	totalAttempts := int64(0)
	for _, rows := range rec.pendingQuality {
		for _, qm := range rows {
			totalRows++
			totalAttempts += qm.Attempts()
		}
	}
	rec.mu.Unlock()
	require.Equal(t, 2, totalRows, "mixed active+pending PG failure must restore both components exactly once")
	// pending 5 + active 1 = 6 attempts, not duplicated
	require.Equal(t, int64(6), totalAttempts, "must not lose/duplicating active contribution; use independent cursors/snapshots")
	require.Equal(t, int64(0), w.poison.Load(), "DB-wide transient must not be counted poison")
	// verify independent cursors: redis cursor must be separate from pg cursor
	w.mu.Lock()
	_, hasPgLast := w.pgLastCell[kActive]
	_, hasRedisLast := w.lastCell[kActive]
	w.mu.Unlock()
	require.True(t, hasPgLast, "PG cursor must be independent")
	_ = hasRedisLast

	// second attempt with success should drain without duplication
	pg2 := newTypedFakePG()
	w.pg = pg2
	w.SetClock(func() time.Time { return fixed.Add(time.Second) })
	w.doPG(context.Background())
	require.Equal(t, 2, len(pg2.quality), "retry must persist both restored rows")
	var attemptsSum int64
	for _, row := range pg2.quality {
		attemptsSum += row.Attempts
	}
	require.Equal(t, int64(6), attemptsSum, "persisted sum must equal original pending+active without duplication")
}
