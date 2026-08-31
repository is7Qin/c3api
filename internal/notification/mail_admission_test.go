// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestWorker_claims_before_warning_mail_admission_and_keeps_success_claim(t *testing.T) {
	client, _ := newTestRedis(t)
	observed := make(chan bool, 1)
	processed := make(chan struct{}, 1)
	w := New(NewCooldown(client), func() bool { return true }, func(event domain.BalanceWarningEvent, complete func(error)) error {
		exists, err := client.Exists(context.Background(), cooldownKey(event)).Result()
		require.NoError(t, err)
		observed <- exists == 1
		complete(nil)
		return nil
	}, nil)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, w.Close(context.Background())) })
	event := warningEvent(1, 100000, "user@example.com")
	require.True(t, w.TrySubmit(event))
	require.True(t, <-observed)
	waitForHook(t, processed, time.Second)
	exists, err := client.Exists(context.Background(), cooldownKey(event)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists)
}

func TestWorker_failed_warning_mail_admission_releases_claim(t *testing.T) {
	client, _ := newTestRedis(t)
	processed := make(chan struct{}, 1)
	w := New(NewCooldown(client), func() bool { return true }, func(domain.BalanceWarningEvent, func(error)) error {
		return errors.New("queue full")
	}, nil)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, w.Close(context.Background())) })
	event := warningEvent(1, 100000, "user@example.com")
	require.True(t, w.TrySubmit(event))
	waitForHook(t, processed, time.Second)
	exists, err := client.Exists(context.Background(), cooldownKey(event)).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

func TestWorker_completion_failure_releases_claim_success_retains(t *testing.T) {
	client, _ := newTestRedis(t)
	processed := make(chan struct{}, 4)
	// completion failure path
	w := New(NewCooldown(client), func() bool { return true }, func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(errors.New("smtp fail"))
		return nil
	}, nil)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	ev := warningEvent(10, 100000, "release@example.com")
	require.True(t, w.TrySubmit(ev))
	waitForHook(t, processed, time.Second)
	exists, err := client.Exists(context.Background(), cooldownKey(ev)).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "completion error must release claim")
	require.Equal(t, int64(1), w.Stats().(notificationStats).FailedTotal)
	require.NoError(t, w.Close(context.Background()))

	// success retains
	client2, _ := newTestRedis(t)
	w2 := New(NewCooldown(client2), func() bool { return true }, func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(nil)
		return nil
	}, nil)
	w2.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w2.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, w2.Close(context.Background())) })
	ev2 := warningEvent(11, 100000, "retain@example.com")
	require.True(t, w2.TrySubmit(ev2))
	waitForHook(t, processed, time.Second)
	exists, err = client2.Exists(context.Background(), cooldownKey(ev2)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "success must retain claim")
	require.Equal(t, int64(1), w2.Stats().(notificationStats).SentTotal)
}

func TestWorker_once_guards_duplicate_callback(t *testing.T) {
	client, _ := newTestRedis(t)
	processed := make(chan struct{}, 1)
	w := New(NewCooldown(client), func() bool { return true }, func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(nil)
		complete(errors.New("second call must be ignored"))
		complete(nil)
		return nil
	}, nil)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, w.Close(context.Background())) })
	ev := warningEvent(20, 100000, "dup@example.com")
	require.True(t, w.TrySubmit(ev))
	waitForHook(t, processed, time.Second)
	require.Equal(t, int64(1), w.Stats().(notificationStats).SentTotal)
	require.Equal(t, int64(0), w.Stats().(notificationStats).FailedTotal)
	exists, err := client.Exists(context.Background(), cooldownKey(ev)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "duplicate callbacks must not release success claim")
}

func TestWorker_once_guards_malicious_callback_then_error(t *testing.T) {
	client, _ := newTestRedis(t)
	processed := make(chan struct{}, 1)
	w := New(NewCooldown(client), func() bool { return true }, func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(nil)
		return errors.New("enqueue error after callback")
	}, nil)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, w.Close(context.Background())) })
	ev := warningEvent(21, 100000, "malicious@example.com")
	require.True(t, w.TrySubmit(ev))
	waitForHook(t, processed, time.Second)
	require.Equal(t, int64(1), w.Stats().(notificationStats).SentTotal)
	require.Equal(t, int64(0), w.Stats().(notificationStats).FailedTotal)
	exists, err := client.Exists(context.Background(), cooldownKey(ev)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists, "once must guard callback-then-error race")

	// reverse: error then callback (if enqueue impl calls callback async and returns error synchronously,
	// the once on error path must also guard)
	client2, _ := newTestRedis(t)
	processed2 := make(chan struct{}, 1)
	var completed atomic.Int64
	w2 := New(NewCooldown(client2), func() bool { return true }, func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(errors.New("late failure"))
		return errors.New("immediate enqueue error")
	}, nil)
	w2.processedHook = func() { processed2 <- struct{}{} }
	require.NoError(t, w2.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, w2.Close(context.Background())) })
	ev2 := warningEvent(22, 100000, "malicious2@example.com")
	require.True(t, w2.TrySubmit(ev2))
	waitForHook(t, processed2, time.Second)
	require.Equal(t, int64(1), completed.Add(0)+w2.Stats().(notificationStats).FailedTotal, "exactly one terminal outcome must be recorded")
	require.Equal(t, int64(1), w2.Stats().(notificationStats).FailedTotal)
}

func TestWorker_disabled_no_claim(t *testing.T) {
	for _, enabled := range []func() bool{nil, func() bool { return false }} {
		client, _ := newTestRedis(t)
		called := make(chan struct{}, 1)
		enqueue := func(_ domain.BalanceWarningEvent, _ func(error)) error {
			called <- struct{}{}
			return nil
		}
		w := New(NewCooldown(client), enabled, enqueue, nil)
		ev := warningEvent(30, 100000, "disabled@example.com")
		require.False(t, w.TrySubmit(ev))
		select {
		case <-called:
			require.Fail(t, "disabled must not call enqueue")
		default:
		}
		exists, err := client.Exists(context.Background(), cooldownKey(ev)).Result()
		require.NoError(t, err)
		require.Zero(t, exists)
		st := w.Stats().(notificationStats)
		require.Equal(t, int64(1), st.Suppressed)
		require.Zero(t, st.Admitted)
		require.Zero(t, st.DroppedTotal)
		require.Zero(t, st.Queued)
	}
}

func TestWorker_enqueue_error_and_callback_once_duplicate_error_path(t *testing.T) {
	client, _ := newTestRedis(t)
	processed := make(chan struct{}, 1)
	w := New(NewCooldown(client), func() bool { return true }, func(_ domain.BalanceWarningEvent, complete func(error)) error {
		complete(errors.New("callback failure"))
		return errors.New("also enqueue error")
	}, nil)
	w.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, w.Close(context.Background())) })
	ev := warningEvent(40, 100000, "once@example.com")
	require.True(t, w.TrySubmit(ev))
	waitForHook(t, processed, time.Second)
	require.Equal(t, int64(1), w.Stats().(notificationStats).FailedTotal, "duplicate terminal paths must count as one failure")
	exists, err := client.Exists(context.Background(), cooldownKey(ev)).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}
