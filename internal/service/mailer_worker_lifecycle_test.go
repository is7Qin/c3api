// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func useZeroMailBackoff(t *testing.T) {
	t.Helper()
	original := mailRetryBackoff
	mailRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { mailRetryBackoff = original })
}

func TestMailWorker_lifecycle_rejects_repeated_start_and_start_after_close(t *testing.T) {
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	require.NoError(t, mw.Start(context.Background()))
	require.Error(t, mw.Start(context.Background()))
	require.NoError(t, mw.Close(context.Background()))
	require.Error(t, mw.Start(context.Background()))
}

func TestMailWorker_close_before_start_and_double_close_drain_once(t *testing.T) {
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	var drains atomic.Int64
	mw.testDrainStarted = func() { drains.Add(1) }

	require.NoError(t, mw.Close(context.Background()))
	require.NoError(t, mw.Close(context.Background()))
	require.Equal(t, int64(1), drains.Load())
}

func TestMailWorker_concurrent_close_drains_once(t *testing.T) {
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	var drains atomic.Int64
	mw.testDrainStarted = func() { drains.Add(1) }

	const callers = 8
	errs := make([]error, callers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := range callers {
		go func() {
			defer wait.Done()
			<-start
			errs[index] = mw.Close(context.Background())
		}()
	}
	close(start)
	wait.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), drains.Load())
}

func TestMailWorker_start_racing_close_is_single_lifecycle(t *testing.T) {
	for range 64 {
		mw := NewMailWorker(newMailService(t, newFakeStore()))
		start := make(chan struct{})
		var startErr, closeErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			startErr = mw.Start(context.Background())
		}()
		go func() {
			defer wait.Done()
			<-start
			closeErr = mw.Close(context.Background())
		}()
		close(start)
		wait.Wait()

		require.NoError(t, closeErr)
		if startErr != nil {
			require.ErrorContains(t, startErr, "already closed")
		}
		require.Error(t, mw.Start(context.Background()))
	}
}

func TestMailWorker_close_wait_for_sender_is_bounded_by_caller_context(t *testing.T) {
	useZeroMailBackoff(t)
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	selected := make(chan struct{})
	release := make(chan struct{})
	mw.testWarningSelected = func() {
		close(selected)
		<-release
	}
	completed := make(chan error, 1)
	event := domain.BalanceWarningEvent{EntityID: 1, Email: "warning@example.com", BalanceMillis: 1, ThresholdMillis: 2}
	require.NoError(t, mw.EnqueueBalanceWarning(event, func(err error) { completed <- err }))
	require.NoError(t, mw.Start(context.Background()))

	select {
	case <-selected:
	case <-time.After(time.Second):
		require.Fail(t, "warning was not selected")
	}
	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, mw.Close(closeCtx), context.Canceled)
	close(release)
	require.NoError(t, mw.Close(context.Background()))

	select {
	case err := <-completed:
		require.Error(t, err)
	case <-time.After(time.Second):
		require.Fail(t, "selected warning was stranded")
	}
}

func TestMailWorker_admissions_racing_close_have_terminal_outcomes(t *testing.T) {
	useZeroMailBackoff(t)
	for range 64 {
		mw := NewMailWorker(newMailService(t, newFakeStore()))
		recorder := newWarningCompletionRecorder()
		start := make(chan struct{})
		var authErr, warningErr, closeErr error
		var wait sync.WaitGroup
		wait.Add(3)
		go func() {
			defer wait.Done()
			<-start
			authErr = mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode})
		}()
		go func() {
			defer wait.Done()
			<-start
			warningErr = mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete)
		}()
		go func() {
			defer wait.Done()
			<-start
			closeErr = mw.Close(context.Background())
		}()
		close(start)
		wait.Wait()

		require.NoError(t, closeErr)
		require.True(t, authErr == nil || errors.Is(authErr, ErrMailQueueFull))
		require.True(t, warningErr == nil || errors.Is(warningErr, ErrMailQueueFull))
		require.Equal(t, int64(1), recorder.calls.Load())
		select {
		case err := <-recorder.results:
			require.Error(t, err)
		default:
			require.Fail(t, "warning admission was stranded")
		}
		stats := mw.Stats().(mailStats)
		require.Zero(t, stats.Queued)
		require.Zero(t, stats.WarningQueued)
		require.Equal(t, int64(2), stats.SentTotal+stats.FailedTotal+stats.DroppedTotal)
	}
}
