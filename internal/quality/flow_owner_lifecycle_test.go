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
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(minute, existing)))
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(minute, incoming)))
	got, ok := rec.FlowMinute(minute)
	require.True(t, ok)
	merged := got.FlowRows()
	require.Len(t, merged, 2)
	require.Equal(t, int64(20), merged[0].AccountID, "old-first order keeps the first-inserted identity first")
	require.Equal(t, int64(10), merged[1].AccountID)
	incoming[0].ChainCount = 999
	existing[0].ChainCount = 999
	prevIn = 42
	prevEx = 43
	require.Equal(t, int64(1), merged[0].ChainCount, "mutating input rows must not change retained counts")
	require.Equal(t, int64(1), merged[1].ChainCount)
	require.Equal(t, int64(2), *merged[0].PreviousAccountID)
	require.Equal(t, int64(1), *merged[1].PreviousAccountID)
}

func TestFlowAccumulator_SameMinuteRowCapDropsOnlyIncoming(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	rec.flowRowCap = 2
	rec.flowRowBytesCap = 2 * EstimatedFlowRowBytes
	fixed := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	minute := fixed.Truncate(time.Minute).Unix()
	kept := []repository.RoutingFlowRow{ownerTestRow(30)}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(minute, kept)))
	over := []repository.RoutingFlowRow{ownerTestRow(40), ownerTestRow(50), ownerTestRow(60)}
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(minute, over)))
	got, ok := rec.FlowMinute(minute)
	require.True(t, ok)
	rows := got.FlowRows()
	require.Len(t, rows, 2, "only the over-cap incoming identities drop; conserved state untouched")
	require.Equal(t, int64(30), rows[0].AccountID)
	require.Equal(t, int64(40), rows[1].AccountID)
	stats := rec.FlowOwner().SnapshotStats()
	require.Equal(t, int64(2), stats.EdgeRowsDropped, "exactly the two over-cap identities are classified dropped")
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
	select {
	case <-owner.loopDone:
	default:
		t.Fatal("owner goroutine must be joined before Close returns")
	}
	require.False(t, owner.running.Load())
	require.NoError(t, owner.Close(context.Background()))
	require.Error(t, owner.Start(context.Background()))
}

func TestRecorder_CloseSubmitRaceHasNoAcceptedLateLoss(t *testing.T) {
	rec, err := NewRecorder(1000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	const submissions = 128
	var wg sync.WaitGroup
	wg.Add(submissions)
	for i := range submissions {
		go func(i int) {
			defer wg.Done()
			owner.Submit(int64(i), []repository.RoutingFlowRow{ownerTestRow(int64(i))})
		}(i)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- rec.Close() }()
	wg.Wait()
	require.NoError(t, <-closeDone)
	stats := owner.SnapshotStats()
	require.Equal(t, stats.Accepted, stats.Processed+stats.ResidualSubmissions)
	for i := range submissions {
		if fm, ok := rec.Snapshot().Flow[int64(i)]; ok {
			require.Len(t, fm.FlowRows(), 1)
		}
	}
}

func TestRecorder_FinalizationRejectsLateFlowEnqueue(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	require.NoError(t, rec.EnqueueFlowMinute(NewFlowSnapshot(100, []repository.RoutingFlowRow{ownerTestRow(11)})))
	require.NoError(t, rec.Close())
	err = rec.EnqueueFlowMinute(NewFlowSnapshot(100, []repository.RoutingFlowRow{ownerTestRow(12)}))
	require.ErrorIs(t, err, ErrCapacity)
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 1)
	require.Equal(t, int64(11), rows[0].AccountID)
}
