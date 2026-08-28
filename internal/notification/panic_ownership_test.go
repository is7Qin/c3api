// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	miniredisServer "github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestWorkerEnqueuePanicReleasesClaimOnceAndSupervisorRestarts(t *testing.T) {
	client, redisServer := newTestRedis(t)
	require.NoError(t, compareDeleteLua.Load(context.Background(), client).Err())
	var releaseAttempts atomic.Int64
	redisServer.Server().SetPreHook(func(_ *miniredisServer.Peer, command string, _ ...string) bool {
		if strings.EqualFold(command, "evalsha") {
			releaseAttempts.Add(1)
		}
		return false
	})
	firstEnqueue := make(chan struct{})
	failed := make(chan struct{}, 1)
	sent := make(chan struct{}, 1)
	var enqueueCalls atomic.Int64
	worker := newTestWorker(t, client, enabledTrue(), func(_ domain.BalanceWarningEvent, complete func(error)) error {
		if enqueueCalls.Add(1) == 1 {
			close(firstEnqueue)
			panic("private enqueue panic detail")
		}
		complete(nil)
		return nil
	})
	worker.failedHook = func() { failed <- struct{}{} }
	worker.sentHook = func() { sent <- struct{}{} }
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Close(context.Background())) })
	event := warningEvent(10, 100_000, "panic@example.com")

	require.True(t, worker.TrySubmit(event))
	waitForHook(t, firstEnqueue, time.Second)
	waitForHook(t, failed, time.Second)
	require.Equal(t, int64(1), releaseAttempts.Load())
	require.False(t, redisServer.Exists(cooldownKey(event)))
	stats := readNotificationStats(t, worker)
	require.Equal(t, int64(1), stats.FailedTotal)
	require.Equal(t, "mail_enqueue_panicked", stats.LastError)
	require.NotContains(t, stats.LastError, "private")

	require.True(t, worker.TrySubmit(event))
	waitForHook(t, sent, 7*time.Second)
	stats = readNotificationStats(t, worker)
	require.Equal(t, int64(1), stats.FailedTotal)
	require.Equal(t, int64(1), worker.Stats().(notificationStats).SentTotal)
	require.Equal(t, int64(1), releaseAttempts.Load())
}

func TestWorkerCallbackThenPanicRecordsOneTerminalOutcome(t *testing.T) {
	client, redisServer := newTestRedis(t)
	worker := newTestWorker(t, client, enabledTrue(), func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(nil)
		panic("private panic after callback")
	})
	event := warningEvent(11, 100_000, "callback-panic@example.com")
	panicked := false

	func() {
		defer func() { panicked = recover() != nil }()
		worker.handleEvent(context.Background(), event)
	}()

	require.True(t, panicked)
	stats := readNotificationStats(t, worker)
	require.Zero(t, stats.FailedTotal)
	require.Equal(t, int64(1), worker.Stats().(notificationStats).SentTotal)
	require.Empty(t, stats.LastError)
	require.True(t, redisServer.Exists(cooldownKey(event)))
}

func TestWorkerConcurrentCloseReturnsFixedErrorAfterDrainPanic(t *testing.T) {
	client, redisServer := newTestRedis(t)
	worker := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error {
		panic("private drain panic detail")
	})
	event := warningEvent(12, 100_000, "drain-panic@example.com")
	require.True(t, worker.TrySubmit(event))
	drainEntered := make(chan struct{}, 2)
	allowDrain := make(chan struct{})
	worker.testPreDrainHook = func() {
		drainEntered <- struct{}{}
		<-allowDrain
	}
	type closeOutcome struct {
		err      error
		panicked bool
	}
	results := make(chan closeOutcome, 2)
	for range 2 {
		go func() {
			outcome := closeOutcome{}
			defer func() {
				outcome.panicked = recover() != nil
				results <- outcome
			}()
			outcome.err = worker.Close(context.Background())
		}()
	}
	waitForHook(t, drainEntered, time.Second)
	waitForHook(t, drainEntered, time.Second)
	close(allowDrain)

	for range 2 {
		select {
		case outcome := <-results:
			require.False(t, outcome.panicked)
			require.EqualError(t, outcome.err, "notification_drain_panicked")
		case <-time.After(time.Second):
			require.Fail(t, "Close caller stranded after drain panic")
		}
	}
	worker.testPreDrainHook = nil
	require.True(t, worker.drainDone)
	require.EqualError(t, worker.Close(context.Background()), "notification_drain_panicked")
	stats := readNotificationStats(t, worker)
	require.Equal(t, int64(1), stats.FailedTotal)
	require.Equal(t, "notification_drain_panicked", stats.LastError)
	require.NotContains(t, stats.LastError, "private")
	require.False(t, redisServer.Exists(cooldownKey(event)))
}
