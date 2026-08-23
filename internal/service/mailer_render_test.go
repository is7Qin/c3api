// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRenderTemplateMatrix(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	ctx := context.Background()

	t.Run("default template fallback and all vars substituted", func(t *testing.T) {
		// no DB rows → fallback to DefaultEmailTemplate
		subj, body, err := svc.RenderTemplate(ctx, domain.EmailTemplateRegisterCode, map[string]string{
			"code": "123456", "ttl_minutes": "10", "app_name": "c3api",
		})
		require.NoError(t, err)
		require.Contains(t, subj, "c3api")
		require.Contains(t, body, "123456")
		require.Contains(t, body, "10")
		require.NotContains(t, body, "{{code}}", "placeholder must be replaced")
		require.NotContains(t, body, "{{ttl_minutes}}")
		require.NotContains(t, body, "{{app_name}}")
	})

	t.Run("unknown placeholder left intact", func(t *testing.T) {
		// seed custom template with unknown placeholder
		_, err := fs.UpsertEmailTemplate(ctx, string(domain.EmailTemplateRegisterCode), "subj {{unknown}} {{app_name}}", "body {{code}} and {{unknown}} end")
		require.NoError(t, err)
		subj, body, err := svc.RenderTemplate(ctx, domain.EmailTemplateRegisterCode, map[string]string{
			"code": "999", "ttl_minutes": "10", "app_name": "c3api",
		})
		require.NoError(t, err)
		require.Contains(t, subj, "{{unknown}}", "unknown placeholder must stay")
		require.Contains(t, subj, "c3api")
		require.Contains(t, body, "999")
		require.Contains(t, body, "{{unknown}}")
	})

	t.Run("missing var key → empty substitution safety", func(t *testing.T) {
		_, err := fs.UpsertEmailTemplate(ctx, string(domain.EmailTemplateResetCode), "subj {{code}}", "code={{code}} ttl={{ttl_minutes}} app={{app_name}}")
		require.NoError(t, err)
		// omit ttl_minutes and app_name → should become empty, not panic
		subj, body, err := svc.RenderTemplate(ctx, domain.EmailTemplateResetCode, map[string]string{
			"code": "111",
		})
		require.NoError(t, err)
		require.Contains(t, subj, "111")
		require.Contains(t, body, "code=111")
		require.Contains(t, body, "ttl=") // empty
		require.Contains(t, body, "app=")
		require.NotContains(t, body, "{{ttl_minutes}}")
	})

	t.Run("reset_code default vars", func(t *testing.T) {
		// delete custom rows to hit default
		_ = fs.DeleteEmailTemplate(ctx, string(domain.EmailTemplateRegisterCode))
		_ = fs.DeleteEmailTemplate(ctx, string(domain.EmailTemplateResetCode))
		subj, body, err := svc.RenderTemplate(ctx, domain.EmailTemplateResetCode, map[string]string{
			"code": "000001", "ttl_minutes": "10", "app_name": domain.AppName,
		})
		require.NoError(t, err)
		require.Contains(t, subj, domain.AppName)
		require.Contains(t, body, "000001")
	})

	t.Run("custom template overrides default", func(t *testing.T) {
		_, err := fs.UpsertEmailTemplate(ctx, string(domain.EmailTemplateRegisterCode), "custom {{app_name}}", "custom body {{code}}")
		require.NoError(t, err)
		subj, body, err := svc.RenderTemplate(ctx, domain.EmailTemplateRegisterCode, map[string]string{
			"code": "777", "ttl_minutes": "10", "app_name": "myapp",
		})
		require.NoError(t, err)
		require.Equal(t, "custom myapp", subj)
		require.Equal(t, "custom body 777", body)
	})
}

func TestMailConfigAndTLSMapping(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)

	t.Run("not configured when disabled or missing fields", func(t *testing.T) {
		setMailSettings(t, fs, svc, map[string]string{
			"mail.enabled": "false", "mail.smtp_host": "h", "mail.from_address": "f@a.com", "mail.smtp_port": "587", "mail.tls": "none",
		})
		_, _, _, _, _, _, ok := svc.mailConfig()
		require.False(t, ok)
		// SendRegisterCode pre-check should fail with ErrMailNotConfigured
		setMailSettings(t, fs, svc, map[string]string{
			"signup_enabled": "true", "mail.register_verification": "true",
			"mail.enabled": "false", "mail.smtp_host": "h", "mail.from_address": "f@a.com", "mail.smtp_port": "587", "mail.tls": "none",
		})
		mw := NewMailWorker(svc)
		require.NoError(t, mw.Start(context.Background()))
		t.Cleanup(func() { _ = mw.Close(context.Background()) })
		svc.SetMailEnqueue(mw.Enqueue)
		require.ErrorIs(t, svc.SendRegisterCode(context.Background(), "a@b.com"), ErrMailNotConfigured)

		setMailSettings(t, fs, svc, map[string]string{
			"mail.enabled": "true", "mail.smtp_host": "", "mail.from_address": "f@a.com", "mail.smtp_port": "587", "mail.tls": "none",
		})
		_, _, _, _, _, _, ok = svc.mailConfig()
		require.False(t, ok, "empty host → not ok")
		setMailSettings(t, fs, svc, map[string]string{
			"mail.enabled": "true", "mail.smtp_host": "h", "mail.from_address": "", "mail.smtp_port": "587", "mail.tls": "none",
		})
		_, _, _, _, _, _, ok = svc.mailConfig()
		require.False(t, ok, "empty from → not ok")
	})

	t.Run("tls policy propagated", func(t *testing.T) {
		for _, pol := range []string{"starttls", "implicit", "none", ""} {
			setMailSettings(t, fs, svc, map[string]string{
				"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": "2525", "mail.from_address": "f@a.com", "mail.tls": pol,
			})
			_, _, _, _, _, gotPol, ok := svc.mailConfig()
			require.True(t, ok)
			// empty maps to default starttls branch in sendMail, but config returns "" → send will treat as starttls
			if pol == "" {
				require.Equal(t, "", gotPol)
			} else {
				require.Equal(t, pol, gotPol)
			}
		}
	})

	t.Run("port validation", func(t *testing.T) {
		setMailSettings(t, fs, svc, map[string]string{
			"mail.enabled": "true", "mail.smtp_host": "h", "mail.from_address": "f@a.com", "mail.smtp_port": "0", "mail.tls": "none",
		})
		_, _, _, _, _, _, ok := svc.mailConfig()
		require.False(t, ok, "port 0 invalid")
		setMailSettings(t, fs, svc, map[string]string{
			"mail.enabled": "true", "mail.smtp_host": "h", "mail.from_address": "f@a.com", "mail.smtp_port": "99999", "mail.tls": "none",
		})
		_, _, _, _, _, _, ok = svc.mailConfig()
		require.False(t, ok, "port >65535 invalid")
	})
}

func TestMailSendNonePolicyDelivers(t *testing.T) {
	fs := newFakeStore()
	svc, mw := newMailServiceWithWorker(t, fs)
	stub := startTestSTUB(t)
	_, port := stubAddr(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port, "mail.from_address": "from@example.com", "mail.tls": "none",
	})
	_, err := fs.UpsertEmailTemplate(context.Background(), string(domain.EmailTemplateRegisterCode), "subj {{app_name}}", "code={{code}} ttl={{ttl_minutes}}")
	require.NoError(t, err)
	require.NoError(t, mw.Enqueue(MailSendTask{To: "to@example.com", Purpose: domain.EmailTemplateRegisterCode, Code: "424242", TTLMin: 10}))
	select {
	case msg := <-stub.msgs:
		require.Contains(t, msg, "424242")
		require.Contains(t, msg, "from@example.com")
		require.Contains(t, msg, "to@example.com")
	case <-time.After(2 * time.Second):
		require.Fail(t, "smtp stub did not receive message")
	}
}
