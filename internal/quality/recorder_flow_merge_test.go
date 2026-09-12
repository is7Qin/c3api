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
	require.NoError(t, foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	}))
	require.NoError(t, foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	}))
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
	require.NoError(t, foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "429", false),
		flowTestRow(minute, 2, 20, "success", true),
	}))
	require.NoError(t, foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 30, "5xx", true),
	}))
	fm, ok := r.FlowMinute(minute.Unix())
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 3, "distinct edges remain distinct")
	require.Equal(t, int64(10), rows[0].AccountID, "first inserted account stays first in old-first order")
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
	require.NoError(t, foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	}))
	// v3-F1: the live charge accrues at the tick-owned fold, not at ingestion —
	// unfolded cells carry no minute/entry charge; the first read folds them.
	require.Equal(t, int64(0), r.PendingBytes(), "unfolded cells carry no live charge")
	fmLive, ok := r.FlowMinute(minute.Unix())
	require.True(t, ok)
	require.Len(t, fmLive.FlowRows(), 1)
	// liveCharge is the minute charge plus identity charges: one retained
	// identity costs exactly one minute charge plus one identity charge.
	require.Equal(t, int64(EstimatedFlowMinuteBytes+EstimatedFlowRowBytes), r.PendingBytes())
	require.NoError(t, foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 20, "success", true),
	}))
	require.Equal(t, 1, r.MinuteBucketCount(), "same-minute merge must not open a new bucket")
	_, ok = r.FlowMinute(minute.Unix())
	require.True(t, ok)
	require.Equal(t, int64(EstimatedFlowMinuteBytes+2*EstimatedFlowRowBytes), r.PendingBytes(),
		"a distinct identity adds exactly one identity charge")
	// A duplicate same-minute merge folds in place and adds no new identity
	// charge.
	require.NoError(t, foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
		flowTestRow(minute, 1, 10, "success", true),
	}))
	require.Equal(t, 1, r.MinuteBucketCount(), "duplicate merge must not open a new bucket")
	require.Equal(t, int64(EstimatedFlowMinuteBytes+2*EstimatedFlowRowBytes), r.PendingBytes(),
		"duplicate merge must not re-charge the minute")
	fm, ok := r.FlowMinute(minute.Unix())
	require.True(t, ok)
	rows := fm.FlowRows()
	require.Len(t, rows, 2)
	require.Equal(t, int64(2), rows[0].ChainCount, "duplicate folds sum")
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
			_ = foldConsumerRows(r.FlowOwner(), minute.Unix(), []repository.RoutingFlowRow{
				flowTestRow(minute, 1, accountID, outcome, true),
			})
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
