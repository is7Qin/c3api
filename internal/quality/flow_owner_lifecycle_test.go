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

func TestFlowAccumulator_InputIndependence(t *testing.T) {
	prevIn, prevEx := int64(1), int64(2)
	incoming := []repository.RoutingFlowRow{ownerTestRow(10)}
	incoming[0].PreviousAccountID = &prevIn
	existing := []repository.RoutingFlowRow{ownerTestRow(20)}
	existing[0].PreviousAccountID = &prevEx
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	minute := fixed.Truncate(time.Minute).Unix()
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), minute, existing))
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), minute, incoming))
	got, ok := rec.FlowMinute(minute)
	require.True(t, ok)
	merged := got.FlowRows()
	require.Len(t, merged, 2)
	// v3-F1: cells carry no insertion order — tick expansion sorts
	// deterministically (ordinal, lane, account), replacing first-insertion
	// order.
	require.Equal(t, int64(10), merged[0].AccountID, "deterministic sort keeps the smaller account first")
	require.Equal(t, int64(20), merged[1].AccountID)
	incoming[0].ChainCount = 999
	existing[0].ChainCount = 999
	prevIn = 42
	prevEx = 43
	require.Equal(t, int64(1), merged[0].ChainCount, "mutating input rows must not change retained counts")
	require.Equal(t, int64(1), merged[1].ChainCount)
	require.Equal(t, int64(1), *merged[0].PreviousAccountID)
	require.Equal(t, int64(2), *merged[1].PreviousAccountID)
}

func TestFlowAccumulator_NoRowCapFoldsExactly(t *testing.T) {
	// v3-F1: the per-minute distinct-identity cap is deleted (no-eviction
	// exactness) — kept and incoming identities all fold; nothing drops.
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	fixed := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	minute := fixed.Truncate(time.Minute).Unix()
	kept := []repository.RoutingFlowRow{ownerTestRow(30)}
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), minute, kept))
	over := []repository.RoutingFlowRow{ownerTestRow(40), ownerTestRow(50), ownerTestRow(60)}
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), minute, over))
	got, ok := rec.FlowMinute(minute)
	require.True(t, ok)
	rows := got.FlowRows()
	require.Len(t, rows, 4, "every offered identity folds without a cap")
	stats := rec.FlowOwner().SnapshotStats()
	require.Equal(t, int64(4), stats.EdgeRowsAccepted)
	require.Zero(t, stats.EdgeRowsDropped)
}

func TestFlowOwner_CloseIdempotentAndJoins(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	unstarted := rec.FlowOwner()
	require.NoError(t, unstarted.Close(context.Background()))
	require.NoError(t, unstarted.Close(context.Background()))
	require.Error(t, unstarted.Start(context.Background()))

	rec2, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec2.FlowOwner()
	require.NoError(t, owner.Start(context.Background()))
	require.NoError(t, owner.Close(context.Background()))
	// v3-F1: no owner loop remains — Close is synchronous, so there is no
	// goroutine to join; the lifecycle flips suffice.
	require.False(t, owner.running.Load())
	require.NoError(t, owner.Close(context.Background()))
	require.Error(t, owner.Start(context.Background()))
}

func TestRecorder_CloseFoldRaceConservesExactly(t *testing.T) {
	// v3-F1: the submit-fence race moves to the walk closed-gate — concurrent
	// folds racing recorder Close land exactly once or count dropped, and the
	// equation holds over all of them.
	rec, err := NewRecorder(1000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	const chains = 128
	var wg sync.WaitGroup
	wg.Add(chains)
	for i := range chains {
		go func(i int) {
			defer wg.Done()
			foldOneRow(t, owner, int64(i), ownerTestRow(int64(i)))
		}(i)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- rec.Close() }()
	wg.Wait()
	require.NoError(t, <-closeDone)
	// Drain every pending cell through the live shells so the equation is
	// exact at this point (post-freeze folds bypass the frozen snapshot).
	for i := range chains {
		_, _ = rec.FlowMinute(int64(i))
	}
	for i := range chains {
		if fm, ok := rec.Snapshot().Flow[int64(i)]; ok {
			require.Len(t, fm.FlowRows(), 1)
		}
	}
	// The reads above fold every pending cell, so the equation is exact at
	// this drained point.
	stats := owner.SnapshotStats()
	require.Equal(t, stats.Accepted, stats.Processed+stats.ResidualSubmissions)
}

func TestRecorder_FinalizationDropsLateFlowFold(t *testing.T) {
	// v3-hygiene: the EnqueueFlowMinute rejection contract moves to the
	// live walk closed-gate — post-finalization folds land nowhere and count
	// dropped (same precedent as TestFlowOwner_FoldRejectsAfterRecorderFinalization).
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), 100, []repository.RoutingFlowRow{ownerTestRow(11)}))
	require.NoError(t, rec.Close())
	foldOneRow(t, rec.FlowOwner(), 100, ownerTestRow(12))
	stats := rec.FlowOwner().SnapshotStats()
	require.Equal(t, int64(1), stats.EdgeRowsDropped, "late fold counts dropped")
	require.Equal(t, int64(1), stats.EdgeRowsAccepted, "late fold accepts nothing")
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 1)
	require.Equal(t, int64(11), rows[0].AccountID)
}
