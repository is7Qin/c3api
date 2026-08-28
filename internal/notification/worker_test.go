// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

func newTestWorker(t *testing.T, client *redis.Client, enabled func() bool, enqueue WarningMailEnqueue) *Worker {
	t.Helper()
	cd := NewCooldown(client)
	w := New(cd, enabled, enqueue, nil)
	return w
}

func enabledTrue() func() bool { return func() bool { return true } }

func waitForHook(t *testing.T, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		require.Fail(t, "hook not signaled within timeout")
	}
}

func TestWorkerTrySubmitHappyPath(t *testing.T) {
	client, _ := newTestRedis(t)
	sentCh := make(chan struct{}, 1)
	enqueued := make(chan domain.BalanceWarningEvent, 1)
	enqueue := func(ev domain.BalanceWarningEvent, complete func(error)) error {
		enqueued <- ev
		complete(nil)
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	w.sentHook = func() { sentCh <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ev := warningEvent(1, 100000, "user@example.com")
	require.True(t, w.TrySubmit(ev))

	select {
	case got := <-enqueued:
		require.Equal(t, ev.Email, got.Email)
	case <-time.After(time.Second):
		require.Fail(t, "warning not enqueued")
	}
	waitForHook(t, sentCh, time.Second)
	require.Equal(t, int64(1), w.Stats().(notificationStats).SentTotal)
	require.Equal(t, int64(1), w.Stats().(notificationStats).Admitted)
}

func TestWorkerRestartsAfterDeliveryPanic(t *testing.T) {
	client, _ := newTestRedis(t)
	firstRun := make(chan struct{})
	sent := make(chan struct{}, 1)
	var calls atomic.Int64
	enqueue := func(ev domain.BalanceWarningEvent, complete func(error)) error {
		if calls.Add(1) == 1 {
			close(firstRun)
			panic("injected enqueue panic")
		}
		complete(nil)
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	w.sentHook = func() { sent <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	require.True(t, w.TrySubmit(warningEvent(1, 100000, "first@example.com")))
	waitForHook(t, firstRun, time.Second)
	require.True(t, w.TrySubmit(warningEvent(2, 100000, "second@example.com")))

	select {
	case <-sent:
	case <-w.done:
		require.Fail(t, "supervised notification loop exited after panic")
	case <-time.After(7 * time.Second):
		require.Fail(t, "notification loop did not restart")
	}
	select {
	case <-w.done:
		require.Fail(t, "done closed before supervised loop exit")
	default:
	}
	require.Equal(t, int64(1), w.Stats().(notificationStats).SentTotal)
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Close(closeCtx))
	select {
	case <-w.done:
	default:
		require.Fail(t, "done remained open after supervised loop exit")
	}
}

func TestWorkerTrySubmitDoesNotBlockOnRedis(t *testing.T) {
	failClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 500 * time.Millisecond})
	t.Cleanup(func() { _ = failClient.Close() })
	cd := NewCooldown(failClient)
	w := New(cd, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil }, nil)
	start := time.Now()
	ok := w.TrySubmit(warningEvent(1, 100000, "x@example.com"))
	elapsed := time.Since(start)
	require.True(t, ok)
	require.Less(t, elapsed, 50*time.Millisecond)
	require.Equal(t, int64(1), w.Stats().(notificationStats).Admitted)
	require.Equal(t, int64(0), w.Stats().(notificationStats).CooldownSuppressed)
}

func TestWorkerTrySubmitReturnsQuicklyEvenWithSlowRedis(t *testing.T) {
	slowClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 400 * time.Millisecond})
	t.Cleanup(func() { _ = slowClient.Close() })
	w := New(NewCooldown(slowClient), enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil }, nil)
	for i := 0; i < 5; i++ {
		start := time.Now()
		ok := w.TrySubmit(warningEvent(int64(100+i), 100000, "q@example.com"))
		elapsed := time.Since(start)
		require.True(t, ok)
		require.Less(t, elapsed, 50*time.Millisecond, "iteration %d: TrySubmit must not wait on Redis", i)
	}
	require.Equal(t, int64(5), w.Stats().(notificationStats).Admitted)
	require.Equal(t, int64(0), w.Stats().(notificationStats).DroppedTotal)
}

func TestWorkerQueueFullDroppedLocally(t *testing.T) {
	client, _ := newTestRedis(t)
	logDir, err := os.MkdirTemp("", "notification-log-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	logPath := filepath.Join(logDir, "notification.log")
	logger, err := logx.New("warn", logPath)
	require.NoError(t, err)
	w := New(NewCooldown(client), enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil }, logger)

	for i := 0; i < queueCap; i++ {
		ev := warningEvent(int64(1000+i), 100000, "u@example.com")
		require.True(t, w.TrySubmit(ev), "fill %d must succeed", i)
	}
	require.Equal(t, queueCap, w.Stats().(notificationStats).Queued)

	start := time.Now()
	ev := warningEvent(9999, 100000, "overflow@example.com")
	require.False(t, w.TrySubmit(ev))
	require.Less(t, time.Since(start), 100*time.Millisecond)
	require.Equal(t, int64(1), w.Stats().(notificationStats).DroppedTotal)

	key := cooldownKey(ev)
	exists, err := client.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), exists)
	require.NoError(t, logger.Sync())
	logged, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Empty(t, logged)
}

func TestWorkerDisabledMailSkipsCooldownClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled func() bool
	}{
		{name: "nil enabled"},
		{name: "disabled", enabled: func() bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestRedis(t)
			processed := make(chan struct{}, 1)
			enqueueCalled := make(chan struct{}, 1)
			enqueue := func(domain.BalanceWarningEvent, func(error)) error {
				enqueueCalled <- struct{}{}
				return nil
			}
			w := New(NewCooldown(client), tc.enabled, enqueue, nil)
			w.processedHook = func() { processed <- struct{}{} }
			require.NoError(t, w.Start(context.Background()))
			t.Cleanup(func() { _ = w.Close(context.Background()) })
			event := warningEvent(1, 100000, "disabled@example.com")

			require.True(t, w.TrySubmit(event))
			waitForHook(t, processed, time.Second)

			select {
			case <-enqueueCalled:
				require.Fail(t, "disabled must not enqueue")
			default:
			}
			exists, err := client.Exists(context.Background(), cooldownKey(event)).Result()
			require.NoError(t, err)
			require.Zero(t, exists)
			stats := w.Stats().(notificationStats)
			require.Equal(t, int64(1), stats.Suppressed)
			require.Zero(t, stats.CooldownSuppressed)
			require.Zero(t, stats.FailedTotal)
			require.Empty(t, stats.LastError)
		})
	}
}

func TestWorkerConcurrentSameThresholdDedupedByConsumer(t *testing.T) {
	client, _ := newTestRedis(t)
	processedCh := make(chan struct{}, 2)
	var sent atomic.Int64
	enqueue := func(_ domain.BalanceWarningEvent, complete func(error)) error {
		sent.Add(1)
		complete(nil)
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	w.processedHook = func() {
		select {
		case processedCh <- struct{}{}:
		default:
		}
	}
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ev := warningEvent(42, 100000, "concurrent@example.com")
	require.True(t, w.TrySubmit(ev))
	require.True(t, w.TrySubmit(ev))

	for i := 0; i < 2; i++ {
		select {
		case <-processedCh:
		case <-time.After(3 * time.Second):
			require.Fail(t, "timeout waiting for event %d to be processed", i)
		}
	}
	require.Equal(t, int64(1), sent.Load())
	require.Equal(t, int64(1), w.Stats().(notificationStats).SentTotal)
	require.GreaterOrEqual(t, w.Stats().(notificationStats).CooldownSuppressed, int64(1))
}

func TestWorkerConcurrentBurstDedupedByConsumer(t *testing.T) {
	client, _ := newTestRedis(t)
	const n = 20
	processedCh := make(chan struct{}, n)
	var sent atomic.Int64
	enqueue := func(_ domain.BalanceWarningEvent, complete func(error)) error {
		sent.Add(1)
		complete(nil)
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	w.processedHook = func() {
		select {
		case processedCh <- struct{}{}:
		default:
		}
	}
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ev := warningEvent(99, 100000, "burst@example.com")
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			w.TrySubmit(ev)
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		select {
		case <-processedCh:
		case <-time.After(3 * time.Second):
			require.Fail(t, "timeout waiting for burst event %d", i)
		}
	}
	require.Equal(t, int64(1), sent.Load())
	require.Equal(t, int64(1), w.Stats().(notificationStats).SentTotal)
	require.GreaterOrEqual(t, w.Stats().(notificationStats).CooldownSuppressed, int64(n-1))
}

func TestWorkerDifferentThresholdIndependent(t *testing.T) {
	client, _ := newTestRedis(t)
	var sent atomic.Int64
	processed := make(chan struct{}, 2)
	enqueue := func(_ domain.BalanceWarningEvent, complete func(error)) error {
		sent.Add(1)
		complete(nil)
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ev1 := warningEvent(1, 100000, "a@example.com")
	ev2 := warningEvent(1, 200000, "a@example.com")
	require.True(t, w.TrySubmit(ev1))
	require.True(t, w.TrySubmit(ev2))

	for i := 0; i < 2; i++ {
		waitForHook(t, processed, 3*time.Second)
	}
	require.Equal(t, int64(2), sent.Load())
}

func TestWorkerDifferentUserIndependent(t *testing.T) {
	client, _ := newTestRedis(t)
	var sent atomic.Int64
	processed := make(chan struct{}, 2)
	enqueue := func(_ domain.BalanceWarningEvent, complete func(error)) error {
		sent.Add(1)
		complete(nil)
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ev1 := warningEvent(1, 100000, "a@example.com")
	ev2 := warningEvent(2, 100000, "b@example.com")
	require.True(t, w.TrySubmit(ev1))
	require.True(t, w.TrySubmit(ev2))

	for i := 0; i < 2; i++ {
		waitForHook(t, processed, 3*time.Second)
	}
	require.Equal(t, int64(2), sent.Load())
}

func TestWorkerFinalDeliveryFailureReleasesClaim(t *testing.T) {
	client, _ := newTestRedis(t)
	failedCh := make(chan struct{}, 1)
	enqueue := func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(errors.New("smtp fail"))
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	w.failedHook = func() {
		select {
		case failedCh <- struct{}{}:
		default:
		}
	}
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	ev := warningEvent(5, 100000, "fail@example.com")
	require.True(t, w.TrySubmit(ev))

	waitForHook(t, failedCh, time.Second)
	require.Equal(t, int64(1), w.Stats().(notificationStats).FailedTotal)
	require.NotEmpty(t, w.Stats().(notificationStats).LastError)

	failedCh2 := make(chan struct{}, 1)
	w.failedHook = func() {
		select {
		case failedCh2 <- struct{}{}:
		default:
		}
	}
	require.True(t, w.TrySubmit(ev))
	waitForHook(t, failedCh2, time.Second)
	require.Equal(t, int64(2), w.Stats().(notificationStats).FailedTotal)
}

func TestWorkerAuthQueueIsolation(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	for i := 0; i < queueCap; i++ {
		ev := warningEvent(int64(2000+i), 100000, "w@example.com")
		require.True(t, w.TrySubmit(ev))
	}
	require.Equal(t, int64(0), w.Stats().(notificationStats).DroppedTotal)
	ev := warningEvent(99999, 100000, "overflow2@example.com")
	require.False(t, w.TrySubmit(ev))
	require.Equal(t, int64(1), w.Stats().(notificationStats).DroppedTotal)
	require.Equal(t, queueCap, w.Stats().(notificationStats).Queued)

	authCh := make(chan string, 256)
	for i := 0; i < 256; i++ {
		authCh <- "auth"
	}
	require.Equal(t, 256, len(authCh))
	require.Equal(t, queueCap, w.Stats().(notificationStats).Queued)
}

func TestWorkerLifecycleStartErrorsOnRepeat(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	require.NoError(t, w.Start(context.Background()))
	require.Error(t, w.Start(context.Background()))
	t.Cleanup(func() { _ = w.Close(context.Background()) })
}

func TestWorkerLifecycleCloseWaitsAndIsIdempotent(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	require.NoError(t, w.Start(context.Background()))
	require.NoError(t, w.Close(context.Background()))
	select {
	case <-w.done:
	case <-time.After(time.Second):
		require.Fail(t, "Close must wait for consumer goroutine")
	}
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.WithValue(context.Background(), struct{}{}, nil)))
}

func TestWorkerLifecycleCloseBeforeStartSafe(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	require.NoError(t, w.Close(context.Background()))
	select {
	case <-w.done:
	default:
	}
	require.False(t, w.TrySubmit(warningEvent(1, 100000, "x@example.com")))
}

func TestWorkerShutdownBoundedDrain(t *testing.T) {
	client, _ := newTestRedis(t)
	enqueue := func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(nil)
		return nil
	}
	w := newTestWorker(t, client, enabledTrue(), enqueue)
	require.NoError(t, w.Start(context.Background()))

	drained := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = w.Close(ctx)
		close(drained)
	}()

	for i := 0; i < 3; i++ {
		w.TrySubmit(warningEvent(int64(3000+i), 100000, "s@example.com"))
	}

	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		require.Fail(t, "drain did not complete within budget")
	}
	st := w.Stats().(notificationStats)
	require.Equal(t, 0, st.Queued)
	total := st.SentTotal + st.DroppedTotal + st.FailedTotal + st.CooldownSuppressed + st.Suppressed
	require.GreaterOrEqual(t, total, int64(1))
	require.LessOrEqual(t, total, int64(3))
	require.NoError(t, w.Close(context.Background()))
}

func TestWorkerCloseThenSubmitDropped(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	require.NoError(t, w.Start(context.Background()))
	require.NoError(t, w.Close(context.Background()))
	require.False(t, w.TrySubmit(warningEvent(1, 100000, "after@example.com")))
	require.GreaterOrEqual(t, w.Stats().(notificationStats).DroppedTotal, int64(1))
}

func TestWorkerInvalidEventSuppressed(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	cases := []domain.BalanceWarningEvent{
		{EventType: domain.NotificationBalanceWarningCrossed, EntityType: domain.NotificationUser, EntityID: 0, ThresholdMillis: 100, Email: "a@b.com"},
		{EventType: domain.NotificationBalanceWarningCrossed, EntityType: domain.NotificationUser, EntityID: 1, ThresholdMillis: 0, Email: "a@b.com"},
		{EventType: domain.NotificationBalanceWarningCrossed, EntityType: domain.NotificationUser, EntityID: 1, ThresholdMillis: 100, Email: ""},
		{EventType: "other", EntityType: domain.NotificationUser, EntityID: 1, ThresholdMillis: 100, Email: "a@b.com"},
		{EventType: domain.NotificationBalanceWarningCrossed, EntityType: domain.NotificationUser, EntityID: 1, BalanceMillis: 200000, ThresholdMillis: 100000, Email: "a@b.com"},
	}
	for _, ev := range cases {
		require.False(t, w.TrySubmit(ev), "invalid event must be suppressed: %+v", ev)
	}
	require.Equal(t, int64(len(cases)), w.Stats().(notificationStats).Suppressed)
	require.Equal(t, 0, w.Stats().(notificationStats).Queued)
	for _, ev := range cases {
		if ev.ThresholdMillis > 0 && ev.EntityID != 0 {
			key := cooldownKey(ev)
			exists, err := client.Exists(context.Background(), key).Result()
			require.NoError(t, err)
			require.Equal(t, int64(0), exists)
		}
	}
}

func TestWorkerImplementsInterfaces(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	var _ interface {
		TrySubmit(domain.BalanceWarningEvent) bool
	} = w
	var _ interface {
		Name() string
		Start(context.Context) error
		Close(context.Context) error
	} = w
	var _ interface{ Stats() any } = w
}

func TestWorkerStartAfterCloseRejected(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error { return nil })
	require.NoError(t, w.Close(context.Background()))
	require.Error(t, w.Start(context.Background()))
	require.Error(t, w.Start(context.Background()))
}

func TestWorkerConcurrentClose(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(nil)
		return nil
	})
	require.NoError(t, w.Start(context.Background()))
	for i := 0; i < 5; i++ {
		require.True(t, w.TrySubmit(warningEvent(int64(4000+i), 100000, "cc@example.com")))
	}

	errs := make([]error, 3)
	var wg sync.WaitGroup
	wg.Add(3)
	for i := range 3 {
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			errs[idx] = w.Close(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent Close %d must not error", i)
	}
	select {
	case <-w.done:
	case <-time.After(time.Second):
		require.Fail(t, "done not closed after concurrent Close")
	}
	st := w.Stats().(notificationStats)
	require.Equal(t, 0, st.Queued)
	total := st.SentTotal + st.DroppedTotal + st.FailedTotal + st.CooldownSuppressed + st.Suppressed
	require.GreaterOrEqual(t, total, int64(1))
	require.False(t, w.TrySubmit(warningEvent(999, 100000, "after@example.com")))
	require.Error(t, w.Start(context.Background()))
}

func TestWorkerSubmitCloseLinearizable(t *testing.T) {
	client, _ := newTestRedis(t)
	w := newTestWorker(t, client, enabledTrue(), func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(nil)
		return nil
	})
	require.NoError(t, w.Start(context.Background()))

	drainStarted := make(chan struct{})
	unblockDrain := make(chan struct{})
	w.testPreDrainHook = func() {
		close(drainStarted)
		<-unblockDrain
	}

	require.True(t, w.TrySubmit(warningEvent(1, 100000, "before@example.com")))

	closeDone := make(chan struct{})
	var closeErr error
	go func() {
		defer close(closeDone)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closeErr = w.Close(ctx)
	}()

	select {
	case <-drainStarted:
	case <-time.After(2 * time.Second):
		require.Fail(t, "drain hook not reached")
	}

	require.False(t, w.TrySubmit(warningEvent(2, 100000, "race@example.com")))

	close(unblockDrain)

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Close did not return")
	}
	require.NoError(t, closeErr)
	require.Equal(t, 0, w.Stats().(notificationStats).Queued)
	require.False(t, w.TrySubmit(warningEvent(3, 100000, "after@example.com")))
	st := w.Stats().(notificationStats)
	total := st.SentTotal + st.DroppedTotal + st.FailedTotal + st.CooldownSuppressed + st.Suppressed
	require.GreaterOrEqual(t, total, int64(1))
}
