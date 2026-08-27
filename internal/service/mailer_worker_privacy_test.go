// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

func newMailWorkerFileLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mail-worker-log-")
	require.NoError(t, err)
	path := filepath.Join(dir, "mail.json")
	logger, err := logx.New("info", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logger, path
}

func readMailWorkerLog(t *testing.T, logger *logx.Logger, path string) string {
	t.Helper()
	require.NoError(t, logger.Sync())
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

func TestMailWorker_auth_failure_observability_uses_safe_category(t *testing.T) {
	useZeroMailBackoff(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())

	logger, path := newMailWorkerFileLogger(t)
	fs := newFakeStore()
	svc := newMailService(t, fs)
	svc.log = logger
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled":      "true",
		"mail.smtp_host":    "127.0.0.1",
		"mail.smtp_port":    port,
		"mail.from_address": "from@example.com",
		"mail.tls":          "none",
	})
	mw := NewMailWorker(svc)
	recipient := "private-recipient@example.com"
	mw.process(context.Background(), MailSendTask{To: recipient, Purpose: domain.EmailTemplateRegisterCode, Code: "private-code", TTLMin: 10})

	output := readMailWorkerLog(t, logger, path)
	require.Equal(t, "mail_delivery_failed", mw.Stats().(mailStats).LastError)
	require.Contains(t, output, `"failure_category":"mail_delivery_failed"`)
	require.Contains(t, output, `"purpose":"register_code"`)
	require.Contains(t, output, `"attempt":3`)
	require.NotContains(t, output, recipient)
	require.NotContains(t, output, "private-code")
	require.NotContains(t, output, "127.0.0.1")
	require.NotContains(t, output, port)
}

func TestMailWorker_warning_failure_preserves_error_and_sanitizes_last_error(t *testing.T) {
	mw := NewMailWorker(newMailService(t, newFakeStore()))
	synthetic := errors.New("dial secret.smtp.internal for private-recipient@example.com: rejected")

	returned := mw.warningFailure(synthetic)

	require.ErrorIs(t, returned, synthetic)
	lastError := mw.Stats().(mailStats).LastError
	require.Equal(t, "mail_delivery_failed", lastError)
	require.NotContains(t, lastError, "secret.smtp.internal")
	require.NotContains(t, lastError, "private-recipient@example.com")
}

func TestMailWorker_success_log_omits_delivery_details(t *testing.T) {
	logger, path := newMailWorkerFileLogger(t)
	fs := newFakeStore()
	svc := newMailService(t, fs)
	svc.log = logger
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
	recipient := "private-success@example.com"

	require.NoError(t, mw.deliver(context.Background(), MailSendTask{To: recipient, Purpose: domain.EmailTemplateRegisterCode, Code: "private-success-code", TTLMin: 10}))

	output := readMailWorkerLog(t, logger, path)
	require.Contains(t, output, `"msg":"mail sent"`)
	require.Contains(t, output, `"purpose":"register_code"`)
	require.NotContains(t, output, recipient)
	require.NotContains(t, output, "private-success-code")
	require.NotContains(t, output, "127.0.0.1")
	require.NotContains(t, output, port)
}
