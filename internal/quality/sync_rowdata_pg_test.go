// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

type pgRowFake struct {
	calls   int
	quality []repository.RoutingQualityRow
	rowErr  map[string]error
	failAll error
}

func newPgRowFake() *pgRowFake { return &pgRowFake{rowErr: make(map[string]error)} }

func (f *pgRowFake) UpsertQualityRow(_ context.Context, row repository.RoutingQualityRow) error {
	f.calls++
	if f.failAll != nil {
		return f.failAll
	}
	k := string(row.CandidateFingerprint[:])
	if err, ok := f.rowErr[k]; ok {
		return err
	}
	f.quality = append(f.quality, row)
	return nil
}

func (f *pgRowFake) UpsertFlowSnapshot(_ context.Context, _ string, _ time.Time, _ int16, _ int64, _ []repository.RoutingFlowRow) error {
	return nil
}

func TestRegression_PGConstraintIsPoisonAndTransientRefills(t *testing.T) {
	_, rdb := newMiniRedis(t)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	pgConstraint := newPgRowFake()
	pgConstraint.failAll = &pgconn.PgError{Code: "23514", Message: "check constraint violates: octet_length"}
	rec1, err := NewRecorder(50000)
	require.NoError(t, err)
	w1 := NewSyncWorker(rec1, rdb, pgConstraint, SyncConfig{InstanceSrc: "regress-constraint-poison", BatchSize: 10}, nil, nil)
	w1.SetClock(func() time.Time { return fixed })
	k1 := keyOf(fpByte(91), qcByte(91))
	qm1 := NewQualityMinute(fixed.Unix(), k1)
	qm1.SetAttempts(1)
	require.NoError(t, rec1.EnqueueQualityMinute(qm1))
	barrier1 := make(chan struct{})
	done1 := make(chan struct{})
	go func() {
		<-barrier1
		w1.doPG(context.Background())
		close(done1)
	}()
	close(barrier1)
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("doPG constraint did not complete")
	}
	require.Equal(t, int64(1), w1.poison.Load())
	require.Equal(t, int64(1), w1.statsSnapshot().PoisonDropped)
	var pgTarget *pgconn.PgError
	require.True(t, errors.As(pgConstraint.failAll, &pgTarget))
	pgErr := &pgconn.PgError{Code: "23514", Message: "check"}
	require.True(t, isRowDataError(pgErr))
	require.True(t, isRowDataError(&RowDataError{Msg: "x"}))
	var poison *RowDataError
	_ = poison
	rec1.mu.Lock()
	rem1 := 0
	for _, rows := range rec1.pendingQuality {
		rem1 += len(rows)
	}
	rec1.mu.Unlock()
	require.Equal(t, 0, rem1, "constraint/data error must be poison dropped, not refilled")
	require.Equal(t, 0, len(pgConstraint.quality))
	_ = poison
	_ = errors.As

	pgTransient := newPgRowFake()
	pgTransient.failAll = &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	rec2, err := NewRecorder(50000)
	require.NoError(t, err)
	w2 := NewSyncWorker(rec2, rdb, pgTransient, SyncConfig{InstanceSrc: "regress-transient-refill", BatchSize: 10}, nil, nil)
	w2.SetClock(func() time.Time { return fixed })
	k2 := keyOf(fpByte(92), qcByte(92))
	qm2 := NewQualityMinute(fixed.Unix(), k2)
	qm2.SetAttempts(1)
	require.NoError(t, rec2.EnqueueQualityMinute(qm2))
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
		t.Fatal("doPG transient did not complete")
	}
	require.Equal(t, int64(0), w2.poison.Load(), "transient/database-wide must not be poison")
	require.False(t, isRowDataError(&pgconn.PgError{Code: "40P01"}))
	require.False(t, isRowDataError(&pgconn.PgError{Code: "55P03"}))
	require.False(t, isRowDataError(context.DeadlineExceeded))
	rec2.mu.Lock()
	rem2 := 0
	for _, rows := range rec2.pendingQuality {
		rem2 += len(rows)
	}
	rec2.mu.Unlock()
	require.Equal(t, 1, rem2, "transient must be refilled")
	require.Equal(t, 0, len(pgTransient.quality))

	pgTransient2 := newPgRowFake()
	pgTransient2.failAll = &pgconn.PgError{Code: "22P02", Message: "invalid text representation"}
	require.True(t, isRowDataError(pgTransient2.failAll), "22 class must be poison")
	pgTransient2Fail := &pgconn.PgError{Code: "23505", Message: "unique_violation"}
	require.True(t, isRowDataError(pgTransient2Fail))
}

func TestRegression_PGChunkConstraintOnlyPoisonRowDropped(t *testing.T) {
	_, rdb := newMiniRedis(t)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	pg := newPgRowFake()
	k1 := keyOf(fpByte(93), qcByte(93))
	k2 := keyOf(fpByte(94), qcByte(94))
	pg.rowErr[string(k1.Fingerprint[:])] = &pgconn.PgError{Code: "23514", Message: "check constraint fails"}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "regress-chunk-poison", BatchSize: 10}, nil, nil)
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
		t.Fatal("doPG chunk did not complete")
	}
	require.Equal(t, int64(1), w.poison.Load())
	rec.mu.Lock()
	rem := 0
	for _, rows := range rec.pendingQuality {
		rem += len(rows)
	}
	rec.mu.Unlock()
	require.Equal(t, 0, rem, "poison row dropped, good row persisted, no refill of poison")
	require.Equal(t, 1, len(pg.quality))
}

func TestRegression_PGChunkMiddlePoisonDoesNotDuplicatePrefix(t *testing.T) {
	// Given: a sequential writer persists the first row, then rejects the
	// middle row as poison, while the final row is valid.
	_, rdb := newMiniRedis(t)
	fixed := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	pg := newPgRowFake()
	k1 := keyOf(fpByte(95), qcByte(95))
	k2 := keyOf(fpByte(96), qcByte(96))
	k3 := keyOf(fpByte(97), qcByte(97))
	pg.rowErr[string(k2.Fingerprint[:])] = &pgconn.PgError{Code: "23514", Message: "check constraint fails"}
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	w := NewSyncWorker(rec, rdb, pg, SyncConfig{InstanceSrc: "regress-middle-poison", BatchSize: 10}, nil, nil)
	w.SetClock(func() time.Time { return fixed })
	for i, k := range []Key{k1, k2, k3} {
		qm := NewQualityMinute(fixed.Unix(), k)
		qm.SetAttempts(int64(i + 1))
		require.NoError(t, rec.EnqueueQualityMinute(qm))
	}

	// When: the whole batch is flushed and the poison row is isolated.
	w.doPG(context.Background())

	// Then: each valid row is persisted exactly once and the poison row is
	// dropped without being refilled.
	require.Equal(t, int64(1), w.poison.Load())
	require.Len(t, pg.quality, 2)
	attempts := []int64{pg.quality[0].Attempts, pg.quality[1].Attempts}
	require.ElementsMatch(t, []int64{1, 3}, attempts)
}
