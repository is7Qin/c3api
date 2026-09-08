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
	require.Equal(t, SubmitAccepted, owner.Submit(100, rows))
	rows[0].AccountID = 99
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Equal(t, int64(11), fm.FlowRows()[0].AccountID)
}

func TestFlowOwner_SubmitRejectsOversizedRowsBeforeCopy(t *testing.T) {
	// Given: request-bound flow caps smaller than the offered submission.
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	rec.flowRowCap = 8
	rec.flowRowBytesCap = 8 * EstimatedFlowRowBytes
	rows := make([]repository.RoutingFlowRow, 10000)
	owner := rec.FlowOwner()

	// When: the oversized submission reaches the nonblocking boundary.
	result := owner.Submit(100, rows)

	// Then: it is rejected without queue or byte reservation, and counted once.
	require.Equal(t, SubmitOverflowed, result)
	stats := owner.SnapshotStats()
	require.Equal(t, int64(0), stats.Accepted)
	require.Equal(t, int64(1), stats.Overflowed)
	require.Equal(t, int64(len(rows)), stats.EdgeRowsDropped)
	require.Equal(t, 0, stats.Queued)
	require.Equal(t, int64(0), stats.PendingBytes)
}

func TestFlowOwner_BoundedOverflowCountedOnce(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	for i := range FlowOwnerQueueCap {
		require.Equal(t, SubmitAccepted, owner.Submit(int64(i), nil))
	}
	require.Equal(t, SubmitOverflowed, owner.Submit(FlowOwnerQueueCap, nil))
	stats := owner.SnapshotStats()
	require.Equal(t, int64(FlowOwnerQueueCap), stats.Accepted)
	require.Equal(t, int64(1), stats.Overflowed)
}

func TestFlowOwner_QueueByteReservationRollsBackOnOverflow(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	row := ownerTestRow(11)
	for range FlowOwnerQueueCap {
		require.Equal(t, SubmitAccepted, owner.Submit(100, []repository.RoutingFlowRow{row}))
	}
	want := int64(FlowOwnerQueueCap) * EstimatedFlowRowBytes
	require.Equal(t, want, owner.queuedBytes.Load())
	require.Equal(t, SubmitOverflowed, owner.Submit(100, []repository.RoutingFlowRow{row}))
	require.Equal(t, want, owner.queuedBytes.Load(), "a failed nonblocking send must release its reservation")
	require.Equal(t, want, rec.PendingBytes())
}

func TestFlowOwner_MergesSameMinuteIdentity(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	row := ownerTestRow(11)
	require.Equal(t, SubmitAccepted, owner.Submit(100, []repository.RoutingFlowRow{row}))
	require.Equal(t, SubmitAccepted, owner.Submit(100, []repository.RoutingFlowRow{row}))
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 1)
	require.Equal(t, int64(2), fm.FlowRows()[0].ChainCount)
}

func TestFlowOwner_CloseReportsResidualOnDeadline(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	row := ownerTestRow(11)
	require.Equal(t, SubmitAccepted, owner.Submit(100, []repository.RoutingFlowRow{row}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, owner.Close(ctx), context.Canceled)
	stats := owner.SnapshotStats()
	require.Equal(t, int64(1), stats.ResidualSubmissions)
	require.Equal(t, int64(1), stats.ResidualRows)
	_, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.NotEqual(t, int64(0), stats.CloseUnixMs)
}

func TestFlowOwner_StartCloseJoinsAndDrains(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	require.NoError(t, owner.Start(context.Background()))
	require.Equal(t, SubmitAccepted, owner.Submit(100, []repository.RoutingFlowRow{ownerTestRow(11)}))
	require.NoError(t, owner.Close(context.Background()))
	require.False(t, owner.running.Load())
	require.Equal(t, int64(1), owner.processed.Load())
	_, ok := rec.FlowMinute(100)
	require.True(t, ok)
}

func TestFlowOwner_SubmitRejectsAfterRecorderFinalization(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	require.NoError(t, rec.Close())
	require.Equal(t, SubmitClosed, rec.FlowOwner().Submit(100, []repository.RoutingFlowRow{ownerTestRow(11)}))
	stats := rec.FlowOwner().SnapshotStats()
	require.Equal(t, int64(1), stats.Overflowed)
}

func TestFlowOwner_PreservesPreviousAccountPointerOwnership(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	previous := int64(7)
	row := ownerTestRow(11)
	row.PreviousAccountID = &previous
	require.Equal(t, SubmitAccepted, rec.FlowOwner().Submit(100, []repository.RoutingFlowRow{row}))
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

func TestFlowOwner_SameMinuteRowCapAccountsDroppedRows(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	rec.flowRowCap = 2
	rec.flowRowBytesCap = 2 * EstimatedFlowRowBytes
	owner := rec.FlowOwner()
	rows := []repository.RoutingFlowRow{ownerTestRow(1), ownerTestRow(2), ownerTestRow(3)}
	rows[1].CandidateFingerprint[0]++
	rows[2].CandidateFingerprint[0]++
	require.Equal(t, SubmitOverflowed, owner.Submit(100, rows))
	stats := owner.SnapshotStats()
	require.Equal(t, int64(0), stats.EdgeRowsAccepted)
	require.Equal(t, int64(3), stats.EdgeRowsDropped)
}

// TestFlowOwner_QueuedBytesReconcileOnDrain pins the byte accounting that the
// stats refactor must preserve exactly: queued backlog charges
// len(rows)*EstimatedFlowRowBytes, a consumer drain moves it to the
// per-minute charge with zero loss/double-count, and taking the bucket
// releases everything.
func TestFlowOwner_QueuedBytesReconcileOnDrain(t *testing.T) {
	rec, err := NewRecorder(10)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	require.Equal(t, SubmitAccepted, owner.Submit(100, []repository.RoutingFlowRow{ownerTestRow(1), ownerTestRow(2)}))
	require.Equal(t, int64(2*EstimatedFlowRowBytes), owner.SnapshotStats().PendingBytes,
		"undrained queue backlog must be visible in pending_bytes")
	require.Equal(t, int64(2*EstimatedFlowRowBytes), rec.PendingBytes())
	fm, ok := rec.FlowMinute(100)
	require.True(t, ok)
	require.Len(t, fm.FlowRows(), 2)
	require.Equal(t, int64(0), owner.queuedBytes.Load())
	require.Equal(t, int64(EstimatedFlowMinuteBytes), owner.SnapshotStats().PendingBytes,
		"drain converts queue bytes to the minute charge without loss or double count")
	taken := owner.takePending()
	require.Len(t, taken, 1, "takePending transfers ownership of the bucket")
	require.Equal(t, int64(0), owner.SnapshotStats().PendingBytes)
}

// TestFlowOwner_EnqueueDoesNotAliasInputRows locks the invariant that makes
// the owner's row-adoption safe: mergeLocked never retains the caller's row
// slice or PreviousAccountID pointers, on either the new-bucket or the
// same-minute merge path.
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
