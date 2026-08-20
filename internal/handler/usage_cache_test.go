// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// TestGetUsageLogsCacheTokens /api/admin/usage_logs 响应含 cache read/creation 字段
// （toAPIUsageLog 手写映射接线，评审 I-3）。
func TestGetUsageLogsCacheTokens(t *testing.T) {
	store := newFakeStore()
	store.logs = []*domain.UsageLog{{
		ID: 1, RequestID: "r1", GroupID: 1, AccountID: 2, Model: "m",
		Format: domain.FormatOpenAIChat, StatusCode: 200, ErrorType: domain.ErrNone,
		InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		CacheReadTokens: 4, CacheCreationTokens: 2,
		CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}}
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())

	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage_logs?from=2026-08-07T00:00:00Z&to=2026-08-07T23:59:59Z", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, "logs: %s", rec.Body.String())
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	require.NotNil(t, body.Rows[0].CacheReadTokens, "响应含 CacheReadTokens")
	require.NotNil(t, body.Rows[0].CacheCreationTokens)
	require.Equal(t, int64(4), *body.Rows[0].CacheReadTokens)
	require.Equal(t, int64(2), *body.Rows[0].CacheCreationTokens)
}
