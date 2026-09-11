// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func ownerTestRow(account int64) repository.RoutingFlowRow {
	return repository.RoutingFlowRow{
		IdentityVersion:  int16(domain.RoutingIdentityVersion),
		Ordinal:          1,
		Lane:             "primary",
		AccountID:        account,
		TransitionReason: "initial",
		Outcome:          "success",
		IsTerminal:       true,
		Generation:       1,
		ChainCount:       1,
	}
}

func TestFlowOwner_SubmissionOwnsRows(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	rows := []repository.RoutingFlowRow{ownerTestRow(11)}
	// v3-F1: Submit successor — same row through the request walk.
	foldOneRow(t, owner, 100, rows[0])
	rows[0].AccountID = 99
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Equal(t, int64(11), fm.FlowRows()[0].AccountID)
}

func TestFlowOwner_WalkGuardsBeyondCapEight(t *testing.T) {
	// Given: a walk offering more than the cap-8 bound.
	// v3-F1: the oversized-Submit rejection contract is deleted with the
	// queue; the walk emits the first 8 facts and counts the rest
	// cap-overflow with zero cell adds.
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	ResetFlowChainCountersForTest()
	owner := rec.FlowOwner()
	rows := make([]repository.RoutingFlowRow, 100)
	for i := range rows {
		rows[i] = ownerTestRow(int64(i + 1))
	}

	// When: the oversized walk reaches the fold.
	foldRows(t, owner, 100, rows...)

	// Then: 8 emit, the rest count cap-overflow, nothing else moves.
	stats := owner.SnapshotStats()
	require.Equal(t, int64(8), stats.EdgeRowsAccepted)
	require.Equal(t, int64(100-8), stats.EdgeRowsDropped)
	require.Equal(t, int64(100-8), stats.Overflowed)
	require.Equal(t, int64(100-8), FlowChainCapacityOverflow())
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 8)
}

func TestFlowOwner_MergesSameMinuteIdentity(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	row := ownerTestRow(11)
	// v3-F1: Submit successor — same rows through the request walk.
	foldOneRow(t, owner, 100, row)
	foldOneRow(t, owner, 100, row)
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 1)
	require.Equal(t, int64(2), fm.FlowRows()[0].ChainCount)
}

func TestFlowOwner_CloseCancelledContextIsSafe(t *testing.T) {
	// v3-F1: the close-drain contract is deleted with the queue — Close is
	// synchronous and safe on any context, recording nothing by itself.
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	foldOneRow(t, owner, 100, ownerTestRow(11))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, owner.Close(ctx))
	stats := owner.SnapshotStats()
	require.Equal(t, int64(0), stats.ResidualSubmissions)
	require.Equal(t, int64(1), stats.EdgeRowsAccepted, "close itself reclassifies nothing")
	_, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.NotEqual(t, int64(0), stats.CloseUnixMs)
}

func TestFlowOwner_StartCloseLifecycle(t *testing.T) {
	// v3-F1: no owner loop remains — Start/Close flip the lifecycle and the
	// folded state stays retained past Close.
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	require.NoError(t, owner.Start(context.Background()))
	// v3-F1: Submit successor — same row through the request walk.
	foldOneRow(t, owner, 100, ownerTestRow(11))
	require.NoError(t, owner.Close(context.Background()))
	require.False(t, owner.running.Load())
	_, ok := rec.FlowMinute(100)
	require.True(t, ok, "folded state stays retained past Close")
}

func TestFlowOwner_FoldRejectsAfterRecorderFinalization(t *testing.T) {
	// v3-F1: the SubmitClosed contract moves to the walk closed-gate —
	// post-finalization facts land nowhere and count dropped.
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	require.NoError(t, rec.Close())
	foldOneRow(t, rec.FlowOwner(), 100, ownerTestRow(11))
	stats := rec.FlowOwner().SnapshotStats()
	require.Equal(t, int64(1), stats.Overflowed)
	require.Equal(t, int64(1), stats.EdgeRowsDropped)
	require.Equal(t, int64(0), stats.EdgeRowsAccepted)
}

func TestFlowOwner_PreservesPreviousAccountPointerOwnership(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	previous := int64(7)
	row := ownerTestRow(11)
	row.PreviousAccountID = &previous
	// v3-F1: Submit successor — the fact carries the value; the later caller
	// mutation stays invisible.
	foldOneRow(t, rec.FlowOwner(), 100, row)
	previous = 99
	got, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Equal(t, int64(7), *got.FlowRows()[0].PreviousAccountID)

	clone := got.Clone()
	*clone.flowRows[0].PreviousAccountID = 42
	require.Equal(t, int64(7), *got.flowRows[0].PreviousAccountID)
}

func TestFlowMinute_SetFlowRowsCopiesPreviousAccountPointer(t *testing.T) {
	// Given
	previous := int64(7)
	rows := []repository.RoutingFlowRow{ownerTestRow(11)}
	rows[0].PreviousAccountID = &previous
	fm := NewFlowMinute(100, [8]int64{})

	// When
	fm.SetFlowRows(rows)
	previous = 99
	rows[0].PreviousAccountID = nil

	// Then
	got := fm.FlowRows()
	require.Len(t, got, 1)
	require.NotNil(t, got[0].PreviousAccountID)
	require.Equal(t, int64(7), *got[0].PreviousAccountID)
}

func TestFlowOwner_NoRowCapFoldsExactly(t *testing.T) {
	// v3-F1: the per-minute distinct-identity cap is deleted (no-eviction
	// exactness) — every offered identity folds; nothing drops.
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	rows := []repository.RoutingFlowRow{ownerTestRow(1), ownerTestRow(2), ownerTestRow(3)}
	rows[1].CandidateFingerprint[0]++
	rows[2].CandidateFingerprint[0]++
	foldRows(t, owner, 100, rows...)
	stats := owner.SnapshotStats()
	require.Equal(t, int64(3), stats.EdgeRowsAccepted)
	require.Zero(t, stats.EdgeRowsDropped)
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 3)
}

// TestFlowOwner_SnapshotAckRetainsCumulativeOwner pins the v3 live-charge
// model: unfolded cells carry no minute/entry charge; the first tick-owned
// read folds them into the computed charge without loss or double count, and
// a snapshot/ack cycle leaves the cumulative owner state retained and clean
// with no next candidate.
func TestFlowOwner_SnapshotAckRetainsCumulativeOwner(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	// v3-F1: Submit successor — same rows through the request walk.
	foldRows(t, owner, 100, ownerTestRow(1), ownerTestRow(2))
	require.Equal(t, int64(0), owner.SnapshotStats().PendingBytes,
		"unfolded cells carry no live charge")
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 2)
	require.Equal(t, int64(EstimatedFlowMinuteBytes+2*EstimatedFlowRowBytes), owner.SnapshotStats().PendingBytes,
		"the tick-owned fold computes the live charge exactly once")
	snap, tok, ok := owner.snapshotForPG(100)
	require.True(t, ok, "dirty minute must offer exactly one snapshot")
	require.Len(t, snap.FlowRows(), 2)
	require.True(t, owner.ackPG(tok), "clean ack settles the lease")
	after, ok := rec.FlowMinute(100)
	require.True(t, ok, "snapshot/ack retains the cumulative owner state")
	require.Len(t, after.FlowRows(), 2)
	require.Empty(t, owner.pgCandidateMinutes(), "clean minute offers no next candidate")
	require.Equal(t, int64(EstimatedFlowMinuteBytes+2*EstimatedFlowRowBytes), owner.SnapshotStats().PendingBytes,
		"ack keeps the live charge stable")
}

// TestFlowOwner_EnqueueDoesNotAliasInputRows locks the invariant that keeps
// the fold exact: enqueue reverse-maps the caller's rows into value facts
// immediately, retaining no slice or PreviousAccountID pointer, on either
// the new-bucket or the same-minute path. (v3-F1: mergeLocked is deleted;
// the cell fold replaces it.)
func TestFlowOwner_EnqueueDoesNotAliasInputRows(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	previous := int64(7)
	incoming := []repository.RoutingFlowRow{ownerTestRow(11)}
	incoming[0].PreviousAccountID = &previous
	require.NoError(t, rec.EnqueueFlowMinute(&FlowMinute{minute: 100, flowRows: incoming}))
	incoming[0].AccountID = 99
	incoming[0].ChainCount = 99
	previous = 55
	got, ok := rec.FlowMinute(100)
	require.True(t, ok)
	rows := got.FlowRows()
	require.Equal(t, int64(11), rows[0].AccountID, "retained rows must not alias the caller's slice")
	require.Equal(t, int64(1), rows[0].ChainCount, "mutating input rows must not change retained counts")
	require.Equal(t, int64(7), *rows[0].PreviousAccountID, "retained rows must not alias the caller's pointers")
	second := []repository.RoutingFlowRow{ownerTestRow(12)}
	require.NoError(t, rec.EnqueueFlowMinute(&FlowMinute{minute: 100, flowRows: second}))
	second[0].AccountID = 98
	after, ok := rec.FlowMinute(100)
	require.True(t, ok)
	var sawTwelve, sawEleven bool
	for _, row := range after.FlowRows() {
		sawTwelve = sawTwelve || row.AccountID == 12
		sawEleven = sawEleven || row.AccountID == 11
	}
	require.True(t, sawTwelve, "same-minute merge must deep-copy the incoming row")
	require.True(t, sawEleven, "same-minute merge must keep the conserved row")
}
