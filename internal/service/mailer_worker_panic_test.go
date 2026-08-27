// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type panicOnceEmailTemplateStore struct {
	*fakeStore
	calls atomic.Int64
}

func (s *panicOnceEmailTemplateStore) GetEmailTemplate(ctx context.Context, purpose string) (*domain.EmailTemplate, error) {
	if s.calls.Add(1) == 1 {
		panic("test mail render panic")
	}
	return s.fakeStore.GetEmailTemplate(ctx, purpose)
}

func newPanicOnceMailService(t *testing.T) (*Service, *testSMTPStub) {
	t.Helper()
	store := newFakeStore()
	svc := newMailService(t, store)
	svc.store = &panicOnceEmailTemplateStore{fakeStore: store}
	stub := startTestSTUB(t)
	_, port := stubAddr(stub)
	setMailSettings(t, store, svc, map[string]string{
		"mail.enabled":      "true",
		"mail.smtp_host":    "127.0.0.1",
		"mail.smtp_port":    port,
		"mail.from_address": "from@example.com",
		"mail.tls":          "none",
	})
	return svc, stub
}

func awaitWarningResult(t *testing.T, results <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(timeout):
		require.Fail(t, "warning completion was not called")
		return nil
	}
}

func TestMailWorker_warning_render_panic_completes_once_and_loop_restarts(t *testing.T) {
	// Given
	useZeroMailBackoff(t)
	svc, stub := newPanicOnceMailService(t)
	mw := NewMailWorker(svc)
	t.Cleanup(func() { require.NoError(t, mw.Close(context.Background())) })
	first := newWarningCompletionRecorder()
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), first.complete))
	require.NoError(t, mw.Start(context.Background()))

	// When
	panicErr := awaitWarningResult(t, first.results, time.Second)
	require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "123456", TTLMin: 10}))

	// Then
	require.EqualError(t, panicErr, "mail worker panicked")
	select {
	case message := <-stub.msgs:
		require.Contains(t, message, "auth@example.com")
	case <-time.After(7 * time.Second):
		require.Fail(t, "auth mail was not processed after warning panic")
	}
	require.Equal(t, int64(1), first.calls.Load())
	require.Equal(t, "mail_worker_panicked", mw.Stats().(mailStats).LastError)
}

func TestMailWorker_warning_callback_panic_runs_once_and_loop_restarts(t *testing.T) {
	// Given
	useZeroMailBackoff(t)
	store := newFakeStore()
	svc := newMailService(t, store)
	stub := startTestSTUB(t)
	_, port := stubAddr(stub)
	setMailSettings(t, store, svc, map[string]string{
		"mail.enabled":      "true",
		"mail.smtp_host":    "127.0.0.1",
		"mail.smtp_port":    port,
		"mail.from_address": "from@example.com",
		"mail.tls":          "none",
	})
	mw := NewMailWorker(svc)
	t.Cleanup(func() { require.NoError(t, mw.Close(context.Background())) })
	var firstCalls atomic.Int64
	firstCalled := make(chan struct{})
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), func(error) {
		firstCalls.Add(1)
		close(firstCalled)
		panic("test warning callback panic")
	}))
	require.NoError(t, mw.Start(context.Background()))
	select {
	case <-firstCalled:
	case <-time.After(time.Second):
		require.Fail(t, "panicking callback was not called")
	}
	second := newWarningCompletionRecorder()

	// When
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), second.complete))
	secondErr := awaitWarningResult(t, second.results, 7*time.Second)

	// Then
	require.NoError(t, secondErr)
	require.Equal(t, int64(1), firstCalls.Load())
	require.Equal(t, int64(1), second.calls.Load())
	require.Equal(t, "mail_worker_panicked", mw.Stats().(mailStats).LastError)
}

func TestMailWorker_close_before_start_recovers_auth_drain_panic(t *testing.T) {
	// Given
	useZeroMailBackoff(t)
	svc, _ := newPanicOnceMailService(t)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode}))

	// When
	err := mw.Close(context.Background())

	// Then
	require.EqualError(t, err, "mail worker panicked")
	require.Equal(t, "mail_worker_panicked", mw.Stats().(mailStats).LastError)
	require.EqualError(t, mw.Close(context.Background()), "mail worker panicked")
}

func TestMailWorker_close_recovers_warning_drain_panic_and_completes_once(t *testing.T) {
	// Given
	useZeroMailBackoff(t)
	svc, _ := newPanicOnceMailService(t)
	mw := NewMailWorker(svc)
	recorder := newWarningCompletionRecorder()
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete))

	// When
	err := mw.Close(context.Background())

	// Then
	require.EqualError(t, err, "mail worker panicked")
	require.EqualError(t, awaitWarningResult(t, recorder.results, time.Second), "mail worker panicked")
	require.Equal(t, int64(1), recorder.calls.Load())
	require.Equal(t, "mail_worker_panicked", mw.Stats().(mailStats).LastError)
}

func TestMailWorker_concurrent_close_callers_share_drain_panic_error(t *testing.T) {
	// Given
	useZeroMailBackoff(t)
	svc, _ := newPanicOnceMailService(t)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode}))
	drainStarted := make(chan struct{})
	releaseDrain := make(chan struct{})
	mw.testDrainStarted = func() {
		close(drainStarted)
		<-releaseDrain
	}
	const callers = 8
	start := make(chan struct{})
	entered := make(chan struct{}, callers)
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			entered <- struct{}{}
			results <- mw.Close(context.Background())
		}()
	}

	// When
	close(start)
	select {
	case <-drainStarted:
	case <-time.After(time.Second):
		require.Fail(t, "drain owner did not start")
	}
	for range callers {
		<-entered
	}
	close(releaseDrain)

	// Then
	for range callers {
		select {
		case err := <-results:
			require.EqualError(t, err, "mail worker panicked")
		case <-time.After(time.Second):
			require.Fail(t, "concurrent Close remained blocked after drain panic")
		}
	}
	require.EqualError(t, mw.Close(context.Background()), "mail worker panicked")
}
