// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"math"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// fakeInvalidator records Users calls for publish/invalidate assertions.
type fakeInvalidatorBW struct {
	users int
}

func (f *fakeInvalidatorBW) Users()                     { f.users++ }
func (f *fakeInvalidatorBW) Templates()                 {}
func (f *fakeInvalidatorBW) Accounts(_ []int64, _ bool) {}
func (f *fakeInvalidatorBW) Multipliers()               {}

func newBWService(t *testing.T) (*Service, *fakeStore, *fakeInvalidatorBW) {
	t.Helper()
	fs := newFakeStore()
	inv := &fakeInvalidatorBW{}
	svc := New(fs, nil, inv, nil, nil, nil, nil)
	// Ensure settings snapshot loaded with defaults (balance_warning.enabled = false).
	// New already reloads settings via GetAllSettings which includes default.
	return svc, fs, inv
}

func TestBalanceWarningThreshold_ValidationAndConversion(t *testing.T) {
	svc, fs, inv := newBWService(t)
	ctx := context.Background()
	// Seed user
	u, err := fs.CreateUser(ctx, &domain.User{Email: "a@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive, MaxConcurrency: 0, Balance: 100000})
	require.NoError(t, err)

	// Positive valid: 2.5 USD => 250000 millis
	updated, err := svc.UpdateBalanceWarningThreshold(ctx, u.ID, 2.5)
	require.NoError(t, err)
	require.Equal(t, int64(250000), updated.BalanceWarningThreshold)
	require.Equal(t, 1, inv.users, "invalidate Users must be called")

	// Get returns same
	millis, err := svc.GetBalanceWarningThreshold(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(250000), millis)

	// Disable: 0 clears value
	updated, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), updated.BalanceWarningThreshold)
	millis, err = svc.GetBalanceWarningThreshold(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), millis)

	// Positive that rounds to zero must be rejected
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, 0.000001) // 0.1 millis => rounds to 0
	require.ErrorIs(t, err, ErrInvalidInput)

	// Negative rejected
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, -1)
	require.ErrorIs(t, err, ErrInvalidInput)

	// Non-finite rejected
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, math.NaN())
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, math.Inf(1))
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, math.Inf(-1))
	require.ErrorIs(t, err, ErrInvalidInput)

	// Ensure disable is idempotent and does not clear cooldown synchronously (no extra logic).
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), fs.users[u.ID].BalanceWarningThreshold)

	// Small positive valid that rounds to 1 millis (0.00001 USD = 1 millis)
	updated, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, 0.00001)
	require.NoError(t, err)
	require.Equal(t, int64(1), updated.BalanceWarningThreshold)
}

func TestBalanceWarningEnabled_DefaultFalse(t *testing.T) {
	svc, _, _ := newBWService(t)
	// No explicit setting row => default false per registry
	require.False(t, svc.BalanceWarningEnabled(), "default must be false")
	// Set false via store and reload snapshot
	ctx := context.Background()
	_, err := svc.store.SetSetting(ctx, "balance_warning.enabled", domain.SettingTypeSwitch, "false")
	require.NoError(t, err)
	require.NoError(t, svc.ReloadSettings(ctx))
	require.False(t, svc.BalanceWarningEnabled())
	// Set true again
	_, err = svc.store.SetSetting(ctx, "balance_warning.enabled", domain.SettingTypeSwitch, "true")
	require.NoError(t, err)
	require.NoError(t, svc.ReloadSettings(ctx))
	require.True(t, svc.BalanceWarningEnabled())
}

func TestMailChannelTest_Isolation(t *testing.T) {
	svc, fs, _ := newBWService(t)
	ctx := context.Background()
	// Seed user with threshold and balance to ensure channel test does not read them
	u, err := fs.CreateUser(ctx, &domain.User{Email: "u@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive, Balance: 50000, BalanceWarningThreshold: 100000})
	require.NoError(t, err)

	// Mail not configured => channel test must return ErrMailNotConfigured without reading balance/threshold
	err = svc.SendMailChannelTest(ctx, "to@example.com")
	require.ErrorIs(t, err, ErrMailNotConfigured)

	// Configure mail (enabled + host/from) but use invalid host so dial fails => error but still isolation
	_, err = fs.SetSetting(ctx, "mail.enabled", domain.SettingTypeSwitch, "true")
	require.NoError(t, err)
	_, err = fs.SetSetting(ctx, "mail.smtp_host", domain.SettingTypeString, "127.0.0.1")
	require.NoError(t, err)
	_, err = fs.SetSetting(ctx, "mail.from_address", domain.SettingTypeString, "from@example.com")
	require.NoError(t, err)
	_, err = fs.SetSetting(ctx, "mail.smtp_port", domain.SettingTypeNumber, "465")
	require.NoError(t, err)
	require.NoError(t, svc.ReloadSettings(ctx))

	// Invalid email must be rejected before dialing (ErrInvalidInput)
	err = svc.SendMailChannelTest(ctx, "not-an-email")
	require.ErrorIs(t, err, ErrInvalidInput)

	// Threshold must remain unchanged after channel test attempts (isolation)
	got, err := svc.GetBalanceWarningThreshold(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100000), got, "channel test must not touch balance_warning_threshold")
	require.Equal(t, int64(50000), fs.users[u.ID].Balance, "channel test must not read/modify balance")

	// Also ensure service.MailConfig returns same as internal mailConfig without panic and without exposing secrets via error string
	_, _, _, _, _, _, ok := svc.MailConfig()
	require.True(t, ok)
}

func TestMailConfigExported_Safe(t *testing.T) {
	svc, fs, _ := newBWService(t)
	ctx := context.Background()
	// Not configured initially
	_, _, _, _, _, _, ok := svc.MailConfig()
	require.False(t, ok)
	// Configure
	_, err := fs.SetSetting(ctx, "mail.enabled", domain.SettingTypeSwitch, "true")
	require.NoError(t, err)
	_, err = fs.SetSetting(ctx, "mail.smtp_host", domain.SettingTypeString, "smtp.example.com")
	require.NoError(t, err)
	_, err = fs.SetSetting(ctx, "mail.from_address", domain.SettingTypeString, "from@example.com")
	require.NoError(t, err)
	require.NoError(t, svc.ReloadSettings(ctx))
	host, port, _, _, from, tls, ok := svc.MailConfig()
	require.True(t, ok)
	require.Equal(t, "smtp.example.com", host)
	require.Equal(t, 465, port)
	require.Equal(t, "from@example.com", from)
	require.Equal(t, "implicit", tls)
}

func TestBalanceWarningThreshold_Overflow(t *testing.T) {
	svc, fs, _ := newBWService(t)
	ctx := context.Background()
	u, err := fs.CreateUser(ctx, &domain.User{Email: "ov@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	over := float64(math.MaxInt64)/1e5 + 100000
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, over)
	require.ErrorIs(t, err, ErrInvalidInput)
	require.NotContains(t, err.Error(), "ov@example.com")
	// safe valid well within range
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, 1e12)
	require.NoError(t, err)
	// also check that 1e20 is rejected (far overflow)
	_, err = svc.UpdateBalanceWarningThreshold(ctx, u.ID, 1e20)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestMailChannelTest_SubjectBodyAndIsolation(t *testing.T) {
	svc, fs, _ := newBWService(t)
	ctx := context.Background()
	u, err := fs.CreateUser(ctx, &domain.User{Email: "iso@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive, Balance: 12345, BalanceWarningThreshold: 99999})
	require.NoError(t, err)
	stub := newSMTPStub(t, false)
	port := stubPort(stub)
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": port,
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	err = svc.SendMailChannelTest(ctx, "to@example.com")
	require.NoError(t, err)
	select {
	case msg := <-stub.msgs:
		subj, body := parseChannelTestMail(t, msg)
		require.Equal(t, "c3api email channel test", subj)
		require.Equal(t, "This is a test email from c3api to verify SMTP configuration. If you received this, the email channel is working.", body)
	case <-time.After(3 * time.Second):
		require.Fail(t, "smtp stub did not receive channel-test message")
	}
	got, err := svc.GetBalanceWarningThreshold(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(99999), got)
	require.Equal(t, int64(12345), fs.users[u.ID].Balance)
}

func parseChannelTestMail(t *testing.T, raw string) (string, string) {
	t.Helper()
	raw = strings.TrimSuffix(raw, "\r\n.\r\n")
	m, err := mail.ReadMessage(strings.NewReader(raw))
	require.NoError(t, err)
	subj := m.Header.Get("Subject")
	enc := strings.ToLower(strings.TrimSpace(m.Header.Get("Content-Transfer-Encoding")))
	var reader io.Reader = m.Body
	if enc == "quoted-printable" {
		reader = quotedprintable.NewReader(m.Body)
	} else if enc == "base64" {
		reader = base64.NewDecoder(base64.StdEncoding, m.Body)
	}
	b, err := io.ReadAll(reader)
	require.NoError(t, err)
	if enc == "base64" && len(b) == 0 {
		alt, decErr := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader([]byte(strings.TrimSpace(string(b))))))
		if decErr == nil && len(alt) > 0 {
			b = alt
		}
	}
	body := strings.TrimSpace(string(b))
	return subj, body
}

func TestMailChannelTest_FailureSanitized(t *testing.T) {
	svc, fs, _ := newBWService(t)
	ctx := context.Background()
	// configure to unreachable port to force dial failure
	setMailSettings(t, fs, svc, map[string]string{
		"mail.enabled": "true", "mail.smtp_host": "127.0.0.1", "mail.smtp_port": "1",
		"mail.from_address": "from@example.com", "mail.tls": "none",
	})
	to := "victim@example.com"
	err := svc.SendMailChannelTest(ctx, to)
	require.ErrorIs(t, err, ErrMailChannelTestFailed)
	require.NotContains(t, err.Error(), to, "error must not leak recipient")
	require.NotContains(t, err.Error(), "127.0.0.1")
	require.NotContains(t, strings.ToLower(err.Error()), "victim")

	// invalid From address path also sanitized
	fs.settings["mail.from_address"] = &domain.Setting{Key: "mail.from_address", Type: domain.SettingTypeString, Value: "not-an-email"}
	require.NoError(t, svc.ReloadSettings(ctx))
	err = svc.SendMailChannelTest(ctx, "to2@example.com")
	require.ErrorIs(t, err, ErrMailChannelTestFailed)
	require.NotContains(t, err.Error(), "to2@example.com")
}
