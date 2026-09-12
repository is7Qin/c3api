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

func TestQualityRecorder_Complete_HoldsMutex_NoDeadlock(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(101)
	q := qc(101)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, r.InitAttemptContext(cell, &ctx))
	r.mu.Lock()
	done := make(chan struct{})
	go func() {
		tt := int64(100)
		ctx.Complete(true, &tt, 1, 0, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Complete blocked on recorder mutex, hot path must be lock-free")
	}
	r.mu.Unlock()
	<-done
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
}

func TestQualityRecorder_ConcurrentRetirePinPressure(t *testing.T) {
	r, err := NewRecorder(1000)
	require.NoError(t, err)
	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			f := fp(byte(seed % 20))
			q := qc(byte(seed % 20))
			k := keyOf(f, q)
			cell := r.GetOrCreateCell(k)
			if cell == nil {
				return
			}
			var ctx AttemptContext
			ok := r.InitAttemptContext(cell, &ctx)
			if ok {
				tt := int64(50 + seed%100)
				ctx.Complete(true, &tt, 1, 0, 0)
			}
		}(i)
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			f := fp(byte(seed % 20))
			q := qc(byte(seed % 20))
			r.Retire(keyOf(f, q))
		}(i)
	}
	wg.Wait()
	require.LessOrEqual(t, r.GlobalInflight(), int64(0))
	require.Equal(t, int64(0), r.GlobalInflight())
}

func TestQualityRecorder_CloseAdmissionRace(t *testing.T) {
	r, err := NewRecorder(10)
	require.NoError(t, err)
	f := fp(110)
	q := qc(110)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, r.InitAttemptContext(cell, &ctx))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = r.CloseWithContext(context.Background())
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			k2 := keyOf(fp(byte(120+i)), qc(byte(120+i)))
			ac := r.Begin(k2)
			if !ac.IsZero() {
				ac.Cancel()
			}
		}
	}()
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	wg.Wait()
	err = r.CloseWithContext(context.Background())
	require.NoError(t, err)
	snap := r.Snapshot()
	require.NotNil(t, snap)
	a, s, _, _, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
	require.Equal(t, int64(1), s)
	require.NotNil(t, snap)
}

func TestQualityRecorder_SnapshotImmutability(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	require.NoError(t, r.AddQualityRow(1000, keyOf(fp(1), qc(1))))
	// v3-hygiene: the legacy edges-array vehicle is deleted — a live
	// consumer row populates the flow minute instead.
	require.NoError(t, foldConsumerRows(r.FlowOwner(), 1000, []repository.RoutingFlowRow{ownerTestRow(11)}))
	snap := r.Snapshot()
	for _, rows := range snap.Quality {
		for _, qm := range rows {
			qm.SetAttempts(9999)
			qm.SetSuccesses(9999)
		}
	}
	for _, fm := range snap.Flow {
		fm.SetCount(0, 9999)
		fm.SetEdge(0, 9999)
	}
	snap2 := r.Snapshot()
	for _, rows := range snap2.Quality {
		for _, qm := range rows {
			require.NotEqual(t, int64(9999), qm.Attempts())
		}
	}
	for _, fm := range snap2.Flow {
		require.NotEqual(t, int64(9999), fm.Count(0))
	}
	r2, err := NewRecorder(50000)
	require.NoError(t, err)
	require.NoError(t, r2.AddQualityRow(2000, keyOf(fp(2), qc(2))))
	f := fp(2)
	q := qc(2)
	k2 := keyOf(f, q)
	cell := r2.GetOrCreateCell(k2)
	var ctx AttemptContext
	require.True(t, r2.InitAttemptContext(cell, &ctx))
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	require.NoError(t, r2.Close())
	final := r2.Snapshot()
	for _, rows := range final.Quality {
		for _, qm := range rows {
			qm.SetAttempts(8888)
		}
	}
	final2 := r2.Snapshot()
	for _, rows := range final2.Quality {
		for _, qm := range rows {
			require.NotEqual(t, int64(8888), qm.Attempts())
		}
	}
	require.NoError(t, r2.Close())
	require.NoError(t, r2.CloseWithContext(context.Background()))
}

func TestQualityRecorder_CloseWaitsZeroWithoutMissedCloseRace(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	f := fp(130)
	q := qc(130)
	k := keyOf(f, q)
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, r.InitAttemptContext(cell, &ctx))
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		tt := int64(100)
		ctx.Complete(true, &tt, 1, 0, 0)
	}()
	start := time.Now()
	err = r.CloseWithContext(context.Background())
	require.NoError(t, err)
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 40*time.Millisecond)
	close(done)
	snap := r.Snapshot()
	require.NotNil(t, snap)
	require.NotNil(t, r.finalSnapshot.Load())
}
