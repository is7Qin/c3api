// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func flowTestRoute(seed byte) domain.RouteClassIDVal {
	var v domain.RouteClassIDVal
	for i := range v {
		v[i] = seed + byte(i)
	}
	return v
}

func flowTestFP(seed byte) domain.CandidateFingerprintVal {
	var v domain.CandidateFingerprintVal
	for i := range v {
		v[i] = seed + byte(i)
	}
	return v
}

func flowTestRow(minute time.Time, ordinal int16, accountID int64, outcome string, terminal bool) repository.RoutingFlowRow {
	return repository.RoutingFlowRow{
		IdentityVersion:      int16(domain.RoutingIdentityVersion),
		RouteClassID:         flowTestRoute(1),
		TerminalMinute:       minute,
		Ordinal:              ordinal,
		Lane:                 "primary",
		AccountID:            accountID,
		TransitionReason:     "initial",
		Outcome:              outcome,
		IsTerminal:           terminal,
		Generation:           1,
		CandidateFingerprint: flowTestFP(byte(accountID)),
		ChainCount:           1,
	}
}

func TestRecorder_FlowSameMinuteIdenticalEdgesSumChainCount(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	minute := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	require.NoError(t, r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	})))
	require.NoError(t, r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	})))
	fm, ok := r.FlowMinute(minute.Unix())
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 1, "identical edge identity must merge into one row")
	require.Equal(t, int64(2), rows[0].ChainCount, "identical edges sum ChainCount")
}

func TestRecorder_FlowSameMinuteDistinctEdgesStayDistinct(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	minute := time.Date(2026, 8, 31, 10, 5, 0, 0, time.UTC)
	require.NoError(t, r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "429", false),
		flowTestRow(minute, 2, 20, "success", true),
	})))
	require.NoError(t, r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 30, "5xx", true),
	})))
	fm, ok := r.FlowMinute(minute.Unix())
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 3, "distinct edges remain distinct")
	require.Equal(t, int64(30), rows[0].AccountID, "incoming contribution leads the merged order")
	var sum int64
	for _, row := range rows {
		sum += row.ChainCount
	}
	require.Equal(t, int64(3), sum, "every recorded request is conserved")
}

func TestRecorder_FlowSameMinuteMergeKeepsCapacityAccounting(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	minute := time.Date(2026, 8, 31, 10, 10, 0, 0, time.UTC)
	require.NoError(t, r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	})))
	bytesAfterFirst := r.PendingBytes()
	require.NoError(t, r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 20, "success", true),
	})))
	require.Equal(t, 1, r.MinuteBucketCount(), "same-minute merge must not open a new bucket")
	require.Equal(t, bytesAfterFirst, r.PendingBytes(), "same-minute merge must not re-charge the minute")
}

func TestRecorder_FlowEmptySnapshotNeverErasesConservedRows(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	minute := time.Date(2026, 8, 31, 10, 15, 0, 0, time.UTC)
	require.NoError(t, r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	})))
	require.NoError(t, r.EnqueueFlowMinute(NewEmptyFlowSnapshot(minute.Unix())))
	fm, ok := r.FlowMinute(minute.Unix())
	require.True(t, ok)
	require.False(t, fm.IsEmptySnapshot(), "an empty marker must not clobber recorded rows")
	require.Len(t, fm.FlowRows(), 1)
}

func TestRecorder_FlowConcurrentSameMinuteRequestsAreConserved(t *testing.T) {
	r, err := NewRecorder(50000)
	require.NoError(t, err)
	minute := time.Date(2026, 8, 31, 10, 20, 0, 0, time.UTC)
	const n = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			accountID := int64(10 + i%4) // four shared edges: identical identities must sum
			outcome := "success"
			if i%3 == 0 {
				outcome = "429"
			}
			_ = r.EnqueueFlowMinute(NewFlowSnapshot(minute.Unix(), []repository.RoutingFlowRow{
				flowTestRow(minute, 1, accountID, outcome, true),
			}))
		}(i)
	}
	close(start)
	wg.Wait()
	fm, ok := r.FlowMinute(minute.Unix())
	require.True(t, ok)
	var sum int64
	for _, row := range fm.FlowRows() {
		sum += row.ChainCount
	}
	require.Equal(t, int64(n), sum, "every concurrent same-minute request is conserved exactly once")
}
