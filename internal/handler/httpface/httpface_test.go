// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpface

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	serviceerr "github.com/is7qin/c3api/internal/service/errors"
)

// TestWriteJSON 契约：Content-Type application/json + encoder 编码含尾换行
// （与 handler/server 历史 writeJSON 副本逐字节一致——各包既有用例零回归的前提）。
func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]any{"ok": true})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "{\"ok\":true}\n", rec.Body.String(), "encoder 编码必须含尾换行")
}

// TestWriteErr JSON 信封 {"error": msg} + encoder 编码（含尾换行）。
func TestWriteErr(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErr(rec, http.StatusForbidden, "forbidden")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "{\"error\":\"forbidden\"}\n", rec.Body.String())
}

// TestWriteServiceErr 映射表全分支（含 default internal error）+ %w 包装链
// 命中 + Content-Type/尾换行契约。
func TestWriteServiceErr(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"not found", serviceerr.ErrNotFound, http.StatusNotFound, "{\"error\":\"service: not found\"}\n"},
		{"not found wrapped", fmt.Errorf("id=5 missing: %w", serviceerr.ErrNotFound), http.StatusNotFound, "{\"error\":\"id=5 missing: service: not found\"}\n"},
		{"invalid input", serviceerr.ErrInvalidInput, http.StatusBadRequest, "{\"error\":\"service: invalid input\"}\n"},
		{"conflict", serviceerr.ErrConflict, http.StatusConflict, "{\"error\":\"service: conflict\"}\n"},
		{"invalid credentials", serviceerr.ErrInvalidCredentials, http.StatusUnauthorized, "{\"error\":\"service: invalid email or password\"}\n"},
		{"signup disabled", serviceerr.ErrSignupDisabled, http.StatusForbidden, "{\"error\":\"service: signup disabled\"}\n"},
		{"default internal error", errors.New("boom"), http.StatusInternalServerError, "{\"error\":\"internal error\"}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteServiceErr(rec, tc.err)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			require.Equal(t, tc.body, rec.Body.String(), "响应体必须逐字节精确")
		})
	}
}
