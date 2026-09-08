// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"sync"
	"testing"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/stretchr/testify/require"
)

func TestFlowOwner_SubmitCloseBarrierConservesAcceptedSubmissions(t *testing.T) {
	// Given: all submitters and the close operation are released together.
	rec, err := NewRecorder(1000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	const submissions = 128
	ready := make(chan struct{}, submissions)
	closeReady := make(chan struct{})
	start := make(chan struct{})
	results := make(chan SubmitResult, submissions)
	var wg sync.WaitGroup
	wg.Add(submissions)
	for i := range submissions {
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			results <- owner.Submit(int64(i), []repository.RoutingFlowRow{ownerTestRow(int64(i))})
		}(i)
	}
	closeDone := make(chan error, 1)
	go func() {
		close(closeReady)
		<-start
		closeDone <- rec.Close()
	}()
	for range submissions {
		<-ready
	}
	<-closeReady

	// When: Submit and Close cross their finalization boundary concurrently.
	close(start)
	wg.Wait()
	require.NoError(t, <-closeDone)
	for range submissions {
		<-results
	}

	// Then: every accepted submission is processed or explicitly residual.
	stats := owner.SnapshotStats()
	require.Equal(t, stats.Accepted, stats.Processed+stats.ResidualSubmissions)
}

func TestFlowOwner_CloseLinearizesAgainstBlockedSubmit(t *testing.T) {
	// Given: a submitter is held before the final nonblocking send, while close
	// starts and waits for the write side of the same fence.
	rec, err := NewRecorder(1000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	owner.submitFence.RLock()
	submitDone := make(chan SubmitResult, 1)
	go func() {
		submitDone <- owner.Submit(100, []repository.RoutingFlowRow{ownerTestRow(1)})
	}()
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()

	// When: the blocked submit/close pair is released together.
	owner.submitFence.RUnlock()

	// Then: close and submit have one linearization point, and accepted work is
	// either drained as residual or rejected before close returns.
	result := <-submitDone
	require.Contains(t, []SubmitResult{SubmitAccepted, SubmitClosed, SubmitOverflowed}, result)
	require.NoError(t, <-closeDone)
	stats := owner.SnapshotStats()
	require.Equal(t, stats.Accepted, stats.Processed+stats.ResidualSubmissions)
	require.Zero(t, stats.Queued)
}
