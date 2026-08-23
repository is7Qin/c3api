// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package service

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// smtpStub captures delivered mail for integration assertions.
type smtpStub struct {
	ln   net.Listener
	msgs chan string
	done chan struct{}
	hang bool // if true, accept but never respond (timeout path)
}

func newSMTPStub(t *testing.T, hang bool) *smtpStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &smtpStub{ln: ln, msgs: make(chan string, 10), done: make(chan struct{}), hang: hang}
	go s.serve()
	t.Cleanup(func() { ln.Close(); <-s.done })
	return s
}

func (s *smtpStub) serve() {
	defer close(s.done)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *smtpStub) handle(c net.Conn) {
	defer c.Close()
	if s.hang {
		select {}
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	_, _ = c.Write([]byte("220 stub ready\r\n"))
	var dataMode bool
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if dataMode {
			data.WriteString(line)
			if strings.Contains(data.String(), "\r\n.\r\n") {
				s.msgs <- data.String()
				_, _ = c.Write([]byte("250 OK\r\n"))
				dataMode = false
				data.Reset()
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "EHLO") || strings.HasPrefix(line, "HELO"):
			_, _ = c.Write([]byte("250 Hello\r\n"))
		case strings.HasPrefix(line, "MAIL FROM"):
			_, _ = c.Write([]byte("250 OK\r\n"))
		case strings.HasPrefix(line, "RCPT TO"):
			_, _ = c.Write([]byte("250 OK\r\n"))
		case strings.HasPrefix(line, "DATA"):
			_, _ = c.Write([]byte("354 End data\r\n"))
			dataMode = true
		case strings.HasPrefix(line, "QUIT"):
			_, _ = c.Write([]byte("221 Bye\r\n"))
			return
		default:
			_, _ = c.Write([]byte("250 OK\r\n"))
		}
	}
}

func stubPort(s *smtpStub) string { return strconv.Itoa(s.ln.Addr().(*net.TCPAddr).Port) }

func TestMailerNonePolicyDeliversRenderedBody(t *testing.T) {
	fs := newFakeStore()
	svc, mw := newMailServiceWithWorker(t, fs)
	stub := newSMTPStub(t, false)
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	_, err := fs.UpsertEmailTemplate(context.Background(), string(domain.EmailTemplateRegisterCode), "subj {{app_name}} {{ttl_minutes}}", "hello {{code}} ttl={{ttl_minutes}} app={{app_name}} tail")
	require.NoError(t, err)
	require.NoError(t, mw.Enqueue(MailSendTask{To: "to@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "998877", TTLMin: 10}))
	select {
	case msg := <-stub.msgs:
		require.Contains(t, msg, "998877")
		require.Contains(t, msg, "from@example.com")
		require.Contains(t, msg, "to@example.com")
	case <-time.After(3 * time.Second):
		require.Fail(t, "smtp stub did not receive delivered message")
	}
}

func TestMailerTimeoutNeverRespondingStub(t *testing.T) {
	origTimeout := mailSendTimeout
	mailSendTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mailSendTimeout = origTimeout })
	origBackoff := mailRetryBackoff
	mailRetryBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { mailRetryBackoff = origBackoff })

	fs := newFakeStore()
	svc := newMailService(t, fs)
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Start(context.Background()))
	t.Cleanup(func() { _ = mw.Close(context.Background()) })
	svc.SetMailEnqueue(mw.Enqueue)
	stub := newSMTPStub(t, true)
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	require.NoError(t, mw.Enqueue(MailSendTask{To: "to@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "112233", TTLMin: 10}))
	// wait for retries to exhaust
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if mw.Stats().(mailStats).FailedTotal > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int64(1), mw.Stats().(mailStats).FailedTotal)
	require.NotEmpty(t, mw.Stats().(mailStats).LastError)
}

func TestMailerErrNotConfiguredWhenDisabled(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	// wire worker so SendRegisterCode checks mailConfig before enqueue
	mw := NewMailWorker(svc)
	require.NoError(t, mw.Start(context.Background()))
	t.Cleanup(func() { _ = mw.Close(context.Background()) })
	svc.SetMailEnqueue(mw.Enqueue)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "false", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": "2525", "mail.from_address": "from@example.com", "mail.tls": "none",
		"signup_enabled": "true", "mail.register_verification": "true",
	})
	err := svc.SendRegisterCode(context.Background(), "to@example.com")
	require.ErrorIs(t, err, ErrMailNotConfigured)
}
