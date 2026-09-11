// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// v3-F1: the submit fence and its barrier tests are deleted with the Submit
// queue (§5.1 DELETION LIST). The remaining contract — concurrent folds
// racing recorder Close conserve exactly with the equation holding — is
// retargeted onto the walk closed-gate below.

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlowOwner_FoldCloseBarrierConservesAcceptedFacts(t *testing.T) {
	// Given: all folders and the close operation are released together.
	rec, err := NewRecorder(1000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	const chains = 128
	ready := make(chan struct{}, chains)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(chains)
	for i := range chains {
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			foldOneRow(t, owner, int64(i), ownerTestRow(int64(i)))
		}(i)
	}
	closeDone := make(chan error, 1)
	go func() {
		for range chains {
			<-ready
		}
		close(start)
		closeDone <- rec.Close()
	}()

	// When: folds and Close cross the finalization boundary concurrently.
	wg.Wait()
	require.NoError(t, <-closeDone)

	// Then: every landed fact is folded or explicitly residual, and the
	// equation holds at the drained point.
	for i := range chains {
		_, _ = rec.FlowMinute(int64(i))
	}
	stats := owner.SnapshotStats()
	require.Equal(t, stats.Accepted, stats.Processed+stats.ResidualSubmissions)
}

func TestFlowOwner_CloseGatesLateFolds(t *testing.T) {
	// Given: the recorder is already closed.
	rec, err := NewRecorder(1000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	require.NoError(t, rec.Close())

	// When: a fold arrives after finalization.
	foldOneRow(t, owner, 100, ownerTestRow(1))

	// Then: it lands nowhere and counts dropped — the closed-gate replaces
	// the old SubmitClosed linearization point.
	stats := owner.SnapshotStats()
	require.Equal(t, int64(0), stats.Accepted)
	require.Equal(t, int64(1), stats.EdgeRowsDropped)
	require.Equal(t, int64(1), stats.Overflowed)
	require.Zero(t, stats.Queued)
}
