// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package handler

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
)

type handlerSMTPStub struct {
	ln   net.Listener
	msgs chan string
	done chan struct{}
}

func startHandlerStub(t *testing.T) *handlerSMTPStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &handlerSMTPStub{ln: ln, msgs: make(chan string, 10), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
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
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close(); <-s.done })
	return s
}

func handlerStubPort(s *handlerSMTPStub) string {
	return strconv.Itoa(s.ln.Addr().(*net.TCPAddr).Port)
}

func TestAdminMailTemplatesRoundTrip(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodGet, "/api/admin/mail/templates", "")
	require.Equal(t, http.StatusOK, rec.Code, "GET templates: %s", rec.Body.String())
	var list []MailTemplate
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 2, "always 2 purposes")
	var reg *MailTemplate
	for i := range list {
		if list[i].Purpose == "register_code" {
			reg = &list[i]
			break
		}
	}
	require.NotNil(t, reg)
	rec = do(http.MethodPut, "/api/admin/mail/templates/register_code", `{"subject":"custom subj","body_text":"code {{code}} ttl {{ttl_minutes}}"}`)
	require.Equal(t, http.StatusOK, rec.Code, "PUT: %s", rec.Body.String())
	var updated MailTemplate
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, "custom subj", updated.Subject)
	rec = do(http.MethodGet, "/api/admin/mail/templates", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	for _, it := range list {
		if it.Purpose == "register_code" {
			require.Equal(t, "custom subj", it.Subject)
		}
	}
	rec = do(http.MethodPut, "/api/admin/mail/templates/register_code", `{"subject":"","body_text":""}`)
	require.Equal(t, http.StatusOK, rec.Code, "restore: %s", rec.Body.String())
	var restored MailTemplate
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &restored))
	rec = do(http.MethodPut, "/api/admin/mail/templates/invalid", `{"subject":"x","body_text":"y"}`)
	require.NotEqual(t, http.StatusOK, rec.Code, "invalid purpose should not 200")
}

func TestUserMailEndpointsRoundTrip(t *testing.T) {
	do, fs, _, svc := newTestUserRouter(t)
	stub := startHandlerStub(t)
	port := handlerStubPort(stub)
	fs.settings["mail.enabled"] = &domain.Setting{Key: "mail.enabled", Type: domain.SettingTypeSwitch, Value: "true"}
	fs.settings["mail.register_verification"] = &domain.Setting{Key: "mail.register_verification", Type: domain.SettingTypeSwitch, Value: "true"}
	fs.settings["mail.smtp_host"] = &domain.Setting{Key: "mail.smtp_host", Type: domain.SettingTypeString, Value: "127.0.0.1"}
	fs.settings["mail.smtp_port"] = &domain.Setting{Key: "mail.smtp_port", Type: domain.SettingTypeNumber, Value: port}
	fs.settings["mail.from_address"] = &domain.Setting{Key: "mail.from_address", Type: domain.SettingTypeString, Value: "from@example.com"}
	fs.settings["mail.tls"] = &domain.Setting{Key: "mail.tls", Type: domain.SettingTypeString, Value: "none"}
	fs.settings["signup_enabled"] = &domain.Setting{Key: "signup_enabled", Type: domain.SettingTypeSwitch, Value: "true"}
	require.NoError(t, svc.ReloadSettings(t.Context()))

	t.Run("register-code sends and 429 on rapid resend", func(t *testing.T) {
		rec := do(http.MethodPost, "/api/user/auth/register-code", `{"email":"u1@example.com"}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "first send: %s", rec.Body.String())
		var sr userapi.SentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sr))
		require.True(t, sr.Sent)
		select {
		case <-stub.msgs:
		case <-time.After(time.Second):
		}
		rec = do(http.MethodPost, "/api/user/auth/register-code", `{"email":"u1@example.com"}`, "")
		require.Equal(t, http.StatusTooManyRequests, rec.Code, "rate limit: %s", rec.Body.String())
	})

	t.Run("register-code duplicate suppress still 200", func(t *testing.T) {
		if u, _ := fs.GetUserByEmail(t.Context(), "exist@example.com"); u == nil {
			_, _ = fs.CreateUser(t.Context(), &domain.User{Email: "exist@example.com", PasswordHash: "h", Role: domain.RoleUser, Status: domain.UserStatusActive})
		}
		rec := do(http.MethodPost, "/api/user/auth/register-code", `{"email":"exist@example.com"}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "duplicate suppress: %s", rec.Body.String())
		var sr2 userapi.SentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sr2))
		require.True(t, sr2.Sent, "suppressed still {sent:true}")
	})

	t.Run("register with missing code -> 400 sentinel", func(t *testing.T) {
		rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"needcode@example.com","password":"pass1234"}`, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "missing code: %s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "email verification required")
	})

	t.Run("register with wrong code -> 400", func(t *testing.T) {
		rec := do(http.MethodPost, "/api/user/auth/register-code", `{"email":"valid2@example.com"}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "send code: %s", rec.Body.String())
		select {
		case <-stub.msgs:
		case <-time.After(time.Second):
		}
		rec = do(http.MethodPost, "/api/user/auth/register", `{"email":"valid2@example.com","password":"pass1234","code":"000000"}`, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "wrong code: %s", rec.Body.String())
	})

	t.Run("forgot-password byte identical existing vs nonexistent", func(t *testing.T) {
		if u, _ := fs.GetUserByEmail(t.Context(), "exist@example.com"); u == nil {
			_, _ = fs.CreateUser(t.Context(), &domain.User{Email: "exist@example.com", PasswordHash: "h", Role: domain.RoleUser, Status: domain.UserStatusActive})
		}
		rec1 := do(http.MethodPost, "/api/user/auth/forgot-password", `{"email":"exist@example.com"}`, "")
		rec2 := do(http.MethodPost, "/api/user/auth/forgot-password", `{"email":"nope@example.com"}`, "")
		require.Equal(t, http.StatusOK, rec1.Code)
		require.Equal(t, http.StatusOK, rec2.Code)
		require.Equal(t, rec1.Body.String(), rec2.Body.String(), "anti-enumeration: responses must be byte-identical")
		var r1, r2 userapi.SentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &r1))
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &r2))
		require.True(t, r1.Sent)
		require.True(t, r2.Sent)
	})

	t.Run("reset-password invalid code -> 400", func(t *testing.T) {
		rec := do(http.MethodPost, "/api/user/auth/reset-password", `{"email":"exist@example.com","code":"000000","new_password":"newpass123"}`, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "invalid code: %s", rec.Body.String())
	})

	t.Run("register-code invalid email -> 400", func(t *testing.T) {
		rec := do(http.MethodPost, "/api/user/auth/register-code", `{"email":"not-an-email"}`, "")
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("register-code signup disabled -> 403", func(t *testing.T) {
		fs.settings["signup_enabled"] = &domain.Setting{Key: "signup_enabled", Type: domain.SettingTypeSwitch, Value: "false"}
		require.NoError(t, svc.ReloadSettings(t.Context()))
		rec := do(http.MethodPost, "/api/user/auth/register-code", `{"email":"any@example.com"}`, "")
		require.Equal(t, http.StatusForbidden, rec.Code, "signup disabled: %s", rec.Body.String())
		fs.settings["signup_enabled"] = &domain.Setting{Key: "signup_enabled", Type: domain.SettingTypeSwitch, Value: "true"}
		require.NoError(t, svc.ReloadSettings(t.Context()))
	})
}
