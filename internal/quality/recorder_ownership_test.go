// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func TestQualityRecorder_OwnershipForeignCellRejected(t *testing.T) {
	r1, err := NewRecorder(10)
	require.NoError(t, err)
	r2, err := NewRecorder(10)
	require.NoError(t, err)
	k := keyOf(fp(1), qc(1))
	foreign := r2.GetOrCreateCell(k)
	require.NotNil(t, foreign)
	var ctx AttemptContext
	ok := r1.InitAttemptContext(foreign, &ctx)
	require.False(t, ok, "foreign cell from another recorder must be rejected")
	require.True(t, ctx.IsZero())
	require.Equal(t, int64(0), r1.GlobalInflight())
	var ctx2 AttemptContext
	ok2 := r2.InitAttemptContext(foreign, &ctx2)
	require.True(t, ok2)
	tt := int64(100)
	ctx2.Complete(true, &tt, 1, 0, 0)
}

func TestQualityRecorder_StaleOldPointerRejected(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	k := keyOf(fp(2), qc(2))
	cellOld := r.GetOrCreateCell(k)
	require.NotNil(t, cellOld)
	var ctx AttemptContext
	require.True(t, r.InitAttemptContext(cellOld, &ctx))
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	r.Retire(k)
	require.Equal(t, 0, r.ActiveCount())
	require.Equal(t, 1, r.RetiredCount())
	cellNew := r.GetOrCreateCell(k)
	require.NotNil(t, cellNew)
	require.NotSame(t, cellOld, cellNew)
	var stale AttemptContext
	require.False(t, r.InitAttemptContext(cellOld, &stale), "stale pointer from retired incarnation must be rejected")
	require.True(t, stale.IsZero())
	var fresh AttemptContext
	require.True(t, r.InitAttemptContext(cellNew, &fresh))
	fresh.Complete(true, &tt, 1, 0, 0)
	a, _, _, _, _, _, _, _, _, _, ok := r.CellStats(k)
	require.True(t, ok)
	require.Equal(t, int64(1), a)
}

func TestQualityRecorder_CloseOwnershipBlocksUntilCompletion(t *testing.T) {
	r, err := NewRecorder(10)
	require.NoError(t, err)
	k := keyOf(fp(3), qc(3))
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, r.InitAttemptContext(cell, &ctx))
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close returned before completion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after inflight drained")
	}
	require.NotNil(t, r.Snapshot())
	require.NotNil(t, r.finalSnapshot.Load())
}

func TestQualityRecorder_CloseWithContextSnapshotLifecycle(t *testing.T) {
	r, err := NewRecorder(10)
	require.NoError(t, err)
	k := keyOf(fp(4), qc(4))
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, r.InitAttemptContext(cell, &ctx))
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = r.CloseWithContext(cctx)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
	tt := int64(100)
	ctx.Complete(true, &tt, 1, 0, 0)
	require.NoError(t, r.CloseWithContext(context.Background()))
	snap := r.Snapshot()
	require.NotNil(t, snap)
}

func TestQualityRecorder_SnapshotIncludesActiveCell(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 29, 12, 34, 0, 0, time.UTC)
	r.now = func() time.Time { return fixed }
	k := keyOf(fp(5), qc(5))
	cell := r.GetOrCreateCell(k)
	var ctx AttemptContext
	require.True(t, r.InitAttemptContext(cell, &ctx))
	tt := int64(100)
	ctx.Complete(true, &tt, 10, 1, 0)
	require.NoError(t, r.CloseWithContext(context.Background()))
	snap := r.Snapshot()
	require.NotNil(t, snap)
	found := false
	for _, rows := range snap.Quality {
		if qm, ok := rows[k]; ok {
			found = true
			require.Equal(t, int64(1), qm.Attempts())
			require.Equal(t, int64(1), qm.Successes())
			qm.SetAttempts(99999)
			break
		}
	}
	require.True(t, found, "active cell projection missing in final snapshot")
	snap2 := r.Snapshot()
	for _, rows := range snap2.Quality {
		if qm, ok := rows[k]; ok {
			require.NotEqual(t, int64(99999), qm.Attempts(), "snapshot must be deep copy")
		}
	}
	require.NoError(t, r.Close())
}

func TestQualityRecorder_SnapshotDeepCopyLifecycle(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	require.NoError(t, r.AddQualityRow(1000, keyOf(fp(9), qc(9))))
	// v3-hygiene: the legacy edges-array vehicle is deleted — a live
	// consumer row populates the flow minute instead.
	require.NoError(t, foldConsumerRows(r.FlowOwner(), 1000, []repository.RoutingFlowRow{ownerTestRow(11)}))
	snap := r.Snapshot()
	for _, rows := range snap.Quality {
		for _, qm := range rows {
			qm.SetAttempts(7777)
		}
	}
	for _, fm := range snap.Flow {
		fm.SetFlowRows([]repository.RoutingFlowRow{ownerTestRow(7777)})
	}
	snap2 := r.Snapshot()
	for _, rows := range snap2.Quality {
		for _, qm := range rows {
			require.NotEqual(t, int64(7777), qm.Attempts())
		}
	}
	for _, fm := range snap2.Flow {
		for _, row := range fm.FlowRows() {
			require.NotEqual(t, int64(7777), row.AccountID)
		}
	}
}
