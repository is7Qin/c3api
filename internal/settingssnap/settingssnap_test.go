// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package settingssnap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type fakeLoader struct {
	rows []*domain.Setting
	err  error
}

func (f fakeLoader) GetAllSettings(_ context.Context) ([]*domain.Setting, error) {
	return f.rows, f.err
}

func newLoadedSnapshot(t *testing.T, rows []*domain.Setting) *Snapshot {
	t.Helper()
	s := New(fakeLoader{rows: rows}, nil)
	require.NoError(t, s.Load(context.Background()))
	return s
}

func setting(key, val string) *domain.Setting {
	return &domain.Setting{Key: key, Value: val}
}

func TestReloadAndValue(t *testing.T) {
	s := newLoadedSnapshot(t, []*domain.Setting{
		setting("a", "1"),
		setting("b", "hello"),
	})
	require.Equal(t, "1", s.Value("a"))
	require.Equal(t, "hello", s.Value("b"))
	require.Equal(t, "", s.Value("missing"))
}

func TestReloadReplacesPreviousSnapshot(t *testing.T) {
	s := newLoadedSnapshot(t, []*domain.Setting{setting("a", "1")})
	require.Equal(t, "1", s.Value("a"))

	s.store = fakeLoader{rows: []*domain.Setting{setting("a", "2")}}
	require.NoError(t, s.Reload(context.Background()))
	require.Equal(t, "2", s.Value("a"))
}

func TestReloadNilStoreIsNoop(t *testing.T) {
	s := New(nil, nil)
	require.NoError(t, s.Reload(context.Background()))
	require.Equal(t, "", s.Value("any"))
}

func TestValueNilReceiver(t *testing.T) {
	var s *Snapshot
	require.Equal(t, "", s.Value("x"))
}

func TestValueUninitializedSnapshot(t *testing.T) {
	s := New(fakeLoader{rows: nil}, nil)
	require.Equal(t, "", s.Value("x"))
}

func TestInt(t *testing.T) {
	tests := []struct {
		name string
		rows []*domain.Setting
		key  string
		want int64
	}{
		{"valid", []*domain.Setting{setting("n", "42")}, "n", 42},
		{"negative", []*domain.Setting{setting("n", "-7")}, "n", -7},
		{"missing key", []*domain.Setting{setting("n", "42")}, "other", 0},
		{"non-numeric", []*domain.Setting{setting("n", "abc")}, "n", 0},
		{"empty string", []*domain.Setting{setting("n", "")}, "n", 0},
		{"overflow", []*domain.Setting{setting("n", "99999999999999999999")}, "n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newLoadedSnapshot(t, tt.rows)
			require.Equal(t, tt.want, s.Int(tt.key))
		})
	}
}

func TestMailConfig(t *testing.T) {
	baseRows := func(overrides ...*domain.Setting) []*domain.Setting {
		m := map[string]string{
			"mail.enabled":       "true",
			"mail.smtp_host":     "smtp.example.com",
			"mail.from_address":  "noreply@example.com",
			"mail.smtp_port":     "587",
			"mail.smtp_username": "user",
			"mail.smtp_password": "pass",
			"mail.tls":           "starttls",
		}
		for _, o := range overrides {
			m[o.Key] = o.Value
		}
		rows := make([]*domain.Setting, 0, len(m))
		for k, v := range m {
			rows = append(rows, setting(k, v))
		}
		return rows
	}

	t.Run("fully configured", func(t *testing.T) {
		s := newLoadedSnapshot(t, baseRows())
		host, port, user, pass, from, tls, ok := s.MailConfig()
		require.True(t, ok)
		require.Equal(t, "smtp.example.com", host)
		require.Equal(t, 587, port)
		require.Equal(t, "user", user)
		require.Equal(t, "pass", pass)
		require.Equal(t, "noreply@example.com", from)
		require.Equal(t, "starttls", tls)
	})

	t.Run("disabled", func(t *testing.T) {
		s := newLoadedSnapshot(t, baseRows(setting("mail.enabled", "false")))
		_, _, _, _, _, _, ok := s.MailConfig()
		require.False(t, ok)
	})

	t.Run("missing host", func(t *testing.T) {
		s := newLoadedSnapshot(t, baseRows(setting("mail.smtp_host", "")))
		_, _, _, _, _, _, ok := s.MailConfig()
		require.False(t, ok)
	})

	t.Run("missing from", func(t *testing.T) {
		s := newLoadedSnapshot(t, baseRows(setting("mail.from_address", "")))
		_, _, _, _, _, _, ok := s.MailConfig()
		require.False(t, ok)
	})

	for _, tc := range []struct {
		name string
		port string
	}{
		{"non-numeric port", "abc"},
		{"zero port", "0"},
		{"negative port", "-1"},
		{"port exceeds 65535", "65536"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLoadedSnapshot(t, baseRows(setting("mail.smtp_port", tc.port)))
			_, _, _, _, _, _, ok := s.MailConfig()
			require.False(t, ok)
		})
	}

	t.Run("nil receiver", func(t *testing.T) {
		var s *Snapshot
		_, _, _, _, _, _, ok := s.MailConfig()
		require.False(t, ok)
	})
}
