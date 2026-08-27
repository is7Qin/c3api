// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type warningCompletionRecorder struct {
	calls   atomic.Int64
	results chan error
}

func newWarningCompletionRecorder() *warningCompletionRecorder {
	return &warningCompletionRecorder{results: make(chan error, 2)}
}

func (r *warningCompletionRecorder) complete(err error) {
	r.calls.Add(1)
	r.results <- err
}

func (r *warningCompletionRecorder) await(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.results:
		return err
	case <-time.After(time.Second):
		require.Fail(t, "warning completion was not called")
		return nil
	}
}

func testWarningEvent() domain.BalanceWarningEvent {
	return domain.BalanceWarningEvent{EntityID: 1, Email: "warning@example.com", BalanceMillis: 1, ThresholdMillis: 2}
}

func TestMailWorker_warning_success_completes_once_with_nil(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	stub := startTestSTUB(t)
	_, port := stubAddr(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled":      "true",
		"mail.smtp_host":    "127.0.0.1",
		"mail.smtp_port":    port,
		"mail.from_address": "from@example.com",
		"mail.tls":          "none",
	})
	mw := NewMailWorker(svc)
	recorder := newWarningCompletionRecorder()
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete))
	require.NoError(t, mw.Start(context.Background()))
	require.NoError(t, recorder.await(t))
	require.NoError(t, mw.Close(context.Background()))
	require.Equal(t, int64(1), recorder.calls.Load())
}

func TestMailWorker_warning_final_failure_completes_once_with_error(t *testing.T) {
	useZeroMailBackoff(t)
	fs := newFakeStore()
	svc := newMailService(t, fs)
	stub := newFlakyStub(t, 3)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled":      "true",
		"mail.smtp_host":    "127.0.0.1",
		"mail.smtp_port":    flakyPort(stub),
		"mail.from_address": "from@example.com",
		"mail.tls":          "none",
	})
	mw := NewMailWorker(svc)
	recorder := newWarningCompletionRecorder()
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete))
	require.NoError(t, mw.Start(context.Background()))
	require.Error(t, recorder.await(t))
	require.NoError(t, mw.Close(context.Background()))
	require.Equal(t, int64(1), recorder.calls.Load())
	require.Equal(t, int64(3), stub.accepted.Load())
}

func TestMailWorker_selected_warning_cancellation_completes_once(t *testing.T) {
	useZeroMailBackoff(t)
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	selected := make(chan struct{})
	release := make(chan struct{})
	mw.testWarningSelected = func() {
		close(selected)
		<-release
	}
	recorder := newWarningCompletionRecorder()
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete))
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, mw.Start(ctx))

	select {
	case <-selected:
	case <-time.After(time.Second):
		require.Fail(t, "warning was not selected")
	}
	cancel()
	close(release)
	require.ErrorIs(t, recorder.await(t), context.Canceled)
	require.NoError(t, mw.Close(context.Background()))
	require.Equal(t, int64(1), recorder.calls.Load())
	require.Equal(t, "mail_delivery_canceled", mw.Stats().(mailStats).LastError)
}

func TestMailWorker_warning_admission_drop_completes_once(t *testing.T) {
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	for range mailWarningQueueCap {
		require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), nil))
	}
	recorder := newWarningCompletionRecorder()
	err := mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete)
	require.ErrorIs(t, err, ErrMailQueueFull)
	require.Equal(t, int64(1), recorder.calls.Load())
	require.ErrorIs(t, recorder.await(t), ErrMailQueueFull)

	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, mw.Close(closeCtx), context.Canceled)
	require.Equal(t, int64(1), recorder.calls.Load())
}

func TestMailWorker_warning_admission_after_close_completes_once(t *testing.T) {
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	require.NoError(t, mw.Close(context.Background()))
	recorder := newWarningCompletionRecorder()

	err := mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete)
	require.ErrorIs(t, err, ErrMailQueueFull)
	require.ErrorIs(t, recorder.await(t), ErrMailQueueFull)
	require.Equal(t, int64(1), recorder.calls.Load())
}

func TestMailWorker_warning_smtp_cancellation_completes_once(t *testing.T) {
	useZeroMailBackoff(t)
	originalTimeout := mailSendTimeout
	mailSendTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mailSendTimeout = originalTimeout })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	accepted := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		<-release
	}()
	t.Cleanup(func() {
		close(release)
		_ = listener.Close()
		<-done
	})

	fs := newFakeStore()
	svc := newMailService(t, fs)
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled":      "true",
		"mail.smtp_host":    "127.0.0.1",
		"mail.smtp_port":    port,
		"mail.from_address": "from@example.com",
		"mail.tls":          "none",
	})
	mw := NewMailWorker(svc)
	t.Cleanup(func() { _ = mw.Close(context.Background()) })
	recorder := newWarningCompletionRecorder()
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), recorder.complete))
	require.NoError(t, mw.Start(context.Background()))
	select {
	case <-accepted:
	case <-time.After(time.Second):
		require.Fail(t, "warning did not enter SMTP")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, mw.Close(closeCtx), context.DeadlineExceeded)
	require.NoError(t, mw.Close(context.Background()))
	require.ErrorIs(t, recorder.await(t), context.Canceled)
	require.Equal(t, int64(1), recorder.calls.Load())
}

func TestMailWorker_drain_timeout_drops_auth_and_completes_warnings_once(t *testing.T) {
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	first := newWarningCompletionRecorder()
	second := newWarningCompletionRecorder()
	require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode}))
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), first.complete))
	require.NoError(t, mw.EnqueueBalanceWarning(testWarningEvent(), second.complete))
	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, mw.Close(closeCtx), context.Canceled)
	require.ErrorIs(t, first.await(t), context.Canceled)
	require.ErrorIs(t, second.await(t), context.Canceled)
	require.Equal(t, int64(1), first.calls.Load())
	require.Equal(t, int64(1), second.calls.Load())
	require.Equal(t, int64(3), mw.Stats().(mailStats).DroppedTotal)
	require.Zero(t, mw.Stats().(mailStats).Queued)
	require.Zero(t, mw.Stats().(mailStats).WarningQueued)
}
