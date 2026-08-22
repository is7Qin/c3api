// SPDX-License-Identifier: AGPL-3.0-or-later
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
		// never respond, just block until client timeout
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
	svc := newMailService(t, fs)
	stub := newSMTPStub(t, false)
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	// seed custom template with all placeholders
	_, err := fs.UpsertEmailTemplate(context.Background(), string(domain.EmailTemplateRegisterCode), "subj {{app_name}} {{ttl_minutes}}", "hello {{code}} ttl={{ttl_minutes}} app={{app_name}} tail")
	require.NoError(t, err)
	err = svc.sendMail(context.Background(), "to@example.com", domain.EmailCodeRegister, "998877")
	require.NoError(t, err)
	select {
	case msg := <-stub.msgs:
		require.Contains(t, msg, "998877", "code must be in delivered body")
		// go-mail quoted-printable may encode, but numbers stay plain
		require.Contains(t, msg, "from@example.com")
		require.Contains(t, msg, "to@example.com")
	case <-time.After(2 * time.Second):
		require.Fail(t, "smtp stub did not receive delivered message")
	}
	// also verify starttls with same plain stub should at least attempt (may fail handshake, but we only assert none succeeds)
}

func TestMailerTimeoutNeverRespondingStub(t *testing.T) {
	orig := mailSendTimeout
	mailSendTimeout = 500 * time.Millisecond
	t.Cleanup(func() { mailSendTimeout = orig })

	fs := newFakeStore()
	svc := newMailService(t, fs)
	stub := newSMTPStub(t, true) // hang
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.sendMail(context.Background(), "to@example.com", domain.EmailCodeRegister, "112233")
	}()
	select {
	case err := <-errCh:
		require.Error(t, err, "hang stub must return error")
		// error should be due to timeout / context, not ErrMailNotConfigured
		require.NotErrorIs(t, err, ErrMailNotConfigured)
	case <-time.After(2 * time.Second):
		require.Fail(t, "sendMail with hang stub did not return within expected timeout")
	}
}

func TestMailerErrNotConfiguredWhenDisabled(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "false", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": "2525", "mail.from_address": "from@example.com", "mail.tls": "none",
	})
	err := svc.sendMail(context.Background(), "to@example.com", domain.EmailCodeRegister, "123456")
	require.ErrorIs(t, err, ErrMailNotConfigured)
}
