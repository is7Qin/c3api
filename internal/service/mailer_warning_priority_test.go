// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestMailWorker_auth_queue_precedes_warning_backlog(t *testing.T) {
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
	warning := domain.BalanceWarningEvent{EntityID: 1, Email: "warning@example.com", BalanceMillis: 1, ThresholdMillis: 2}
	require.NoError(t, mw.EnqueueBalanceWarning(warning, nil))
	require.NoError(t, mw.EnqueueBalanceWarning(warning, nil))
	require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "123456", TTLMin: 10}))
	require.NoError(t, mw.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, mw.Close(context.Background())) })

	message := <-stub.msgs
	require.True(t, strings.Contains(message, "auth@example.com"), "auth mail must precede warning backlog")
}

func TestMailWorker_auth_arriving_after_warning_selection_precedes_warning(t *testing.T) {
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
	selected := make(chan struct{})
	release := make(chan struct{})
	mw.testWarningSelected = func() {
		close(selected)
		<-release
	}
	warning := domain.BalanceWarningEvent{EntityID: 1, Email: "warning@example.com", BalanceMillis: 1, ThresholdMillis: 2}
	require.NoError(t, mw.EnqueueBalanceWarning(warning, nil))
	require.NoError(t, mw.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, mw.Close(context.Background())) })

	select {
	case <-selected:
	case <-time.After(time.Second):
		require.Fail(t, "warning was not selected")
	}
	require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "123456", TTLMin: 10}))
	close(release)

	select {
	case message := <-stub.msgs:
		require.Contains(t, message, "auth@example.com")
	case <-time.After(time.Second):
		require.Fail(t, "first mail was not delivered")
	}
}

func TestMailWorker_warning_queue_does_not_consume_auth_capacity(t *testing.T) {
	svc := newMailService(t, newFakeStore())
	mw := NewMailWorker(svc)
	event := domain.BalanceWarningEvent{EntityID: 1, Email: "warning@example.com", BalanceMillis: 1, ThresholdMillis: 2}
	for range mailWarningQueueCap {
		require.NoError(t, mw.EnqueueBalanceWarning(event, nil))
	}
	require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode}))
	stats := mw.Stats().(mailStats)
	require.Equal(t, mailQueueCap, stats.QueueCap)
	require.Equal(t, 1, stats.Queued)
	require.Equal(t, mailWarningQueueCap, stats.WarningQueued)
}

func TestMailWorker_auth_queue_does_not_consume_warning_capacity(t *testing.T) {
	svc := newMailService(t, newFakeStore())
	mw := NewMailWorker(svc)
	for range mailQueueCap {
		require.NoError(t, mw.Enqueue(MailSendTask{To: "auth@example.com", Purpose: domain.EmailTemplateRegisterCode}))
	}
	event := domain.BalanceWarningEvent{EntityID: 1, Email: "warning@example.com", BalanceMillis: 1, ThresholdMillis: 2}
	require.NoError(t, mw.EnqueueBalanceWarning(event, nil))
	stats := mw.Stats().(mailStats)
	require.Equal(t, mailQueueCap, stats.Queued)
	require.Equal(t, 1, stats.WarningQueued)
}
