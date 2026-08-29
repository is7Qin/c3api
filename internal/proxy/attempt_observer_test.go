// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/rule"
)

func newObserverQualityContext(t *testing.T) *quality.AttemptContext {
	t.Helper()
	rec, err := quality.NewRecorder(32)
	require.NoError(t, err)
	key := quality.CanonicalKey([32]byte{1}, [32]byte{2}, [32]byte{3})
	ctx := rec.Begin(key)
	require.False(t, ctx.IsZero())
	t.Cleanup(func() { require.NoError(t, rec.Close()) })
	return ctx
}

func TestAttemptObserver_concurrentCompletionRecordsEachSideEffectOnce(t *testing.T) {
	ctx := newObserverQualityContext(t)
	var healthCalls atomic.Int32
	var flowCalls atomic.Int32
	var releaseCalls atomic.Int32
	observer := NewAttemptObserver(ctx,
		func(AttemptOutcome, AttemptHealthEvent) { healthCalls.Add(1) },
		func(AttemptOutcome) { flowCalls.Add(1) },
		func() { releaseCalls.Add(1) },
	)
	outcome := validBase()
	health := &AttemptHealthEvent{Kind: rule.KindOK}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			require.NoError(t, observer.Complete(outcome, health))
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, int32(1), healthCalls.Load())
	require.Equal(t, int32(1), flowCalls.Load())
	require.Equal(t, int32(1), releaseCalls.Load())
	require.True(t, ctx.IsCompleted())
}

func TestAttemptObserver_clientCancellationCancelsQualityAndSkipsHealth(t *testing.T) {
	ctx := newObserverQualityContext(t)
	var healthCalls atomic.Int32
	var flowCalls atomic.Int32
	var releaseCalls atomic.Int32
	observer := NewAttemptObserver(ctx,
		func(AttemptOutcome, AttemptHealthEvent) { healthCalls.Add(1) },
		func(AttemptOutcome) { flowCalls.Add(1) },
		func() { releaseCalls.Add(1) },
	)
	outcome := validBase()
	outcome.Result = ResultClientCancel
	outcome.Commit = CommitResponseStarted
	outcome.HTTPStatus = 0
	outcome.Terminal = true
	require.NoError(t, outcome.Validate())

	require.NoError(t, observer.Cancel(outcome))
	require.NoError(t, observer.Cancel(outcome))
	require.Equal(t, int32(0), healthCalls.Load())
	require.Equal(t, int32(1), flowCalls.Load())
	require.Equal(t, int32(1), releaseCalls.Load())
	require.True(t, ctx.IsCompleted())
}

func TestAttemptObserver_retryableHealthUsesTypedRetryMatrix(t *testing.T) {
	ctx := newObserverQualityContext(t)
	var gotHealth AttemptHealthEvent
	var healthCalls atomic.Int32
	observer := NewAttemptObserver(ctx,
		func(_ AttemptOutcome, event AttemptHealthEvent) {
			gotHealth = event
			healthCalls.Add(1)
		},
		nil,
		nil,
	)
	outcome := dispatchedFailedBase(429, CommitUpstreamResponded, false)
	require.NoError(t, observer.Complete(outcome, &AttemptHealthEvent{Kind: rule.Kind429}))
	require.Equal(t, int32(1), healthCalls.Load())
	require.True(t, gotHealth.Retryable)
}

func TestAttemptObserver_rejectsInvalidFactsBeforeCompleting(t *testing.T) {
	ctx := newObserverQualityContext(t)
	var releaseCalls atomic.Int32
	observer := NewAttemptObserver(ctx, nil, nil, func() { releaseCalls.Add(1) })
	invalid := validBase()
	invalid.Commit = CommitNotSent
	invalid.BusinessFrameSent = false
	require.Error(t, observer.Complete(invalid, nil))
	require.False(t, ctx.IsCompleted())
	require.Equal(t, int32(0), releaseCalls.Load())
	require.NoError(t, observer.Complete(validBase(), nil))
	require.Equal(t, int32(1), releaseCalls.Load())
}
