// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func TestMergeFlowRowsBounded_InputIndependence(t *testing.T) {
	prevIn, prevEx := int64(1), int64(2)
	incoming := []repository.RoutingFlowRow{ownerTestRow(10)}
	incoming[0].PreviousAccountID = &prevIn
	existing := []repository.RoutingFlowRow{ownerTestRow(20)}
	existing[0].PreviousAccountID = &prevEx
	merged, dropped := mergeFlowRowsBounded(existing, incoming, 8, 16*EstimatedFlowRowBytes)
	require.Zero(t, dropped)
	require.Len(t, merged, 2)
	require.LessOrEqual(t, cap(merged), 8)
	require.Equal(t, int64(10), merged[0].AccountID)
	incoming[0].ChainCount = 999
	existing[0].ChainCount = 999
	prevIn = 42
	prevEx = 43
	require.Equal(t, int64(1), merged[0].ChainCount)
	require.Equal(t, int64(1), merged[1].ChainCount)
	require.Equal(t, int64(1), *merged[0].PreviousAccountID)
	require.Equal(t, int64(2), *merged[1].PreviousAccountID)
	full := []repository.RoutingFlowRow{ownerTestRow(30)}
	over := []repository.RoutingFlowRow{ownerTestRow(40), ownerTestRow(50), ownerTestRow(60)}
	kept, dropped := mergeFlowRowsBounded(full, over, 2, 16*EstimatedFlowRowBytes)
	require.Equal(t, int64(1), dropped)
	require.Len(t, kept, 2)
	require.Equal(t, int64(40), kept[0].AccountID)
	require.Equal(t, int64(50), kept[1].AccountID)
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
