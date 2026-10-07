// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAdminFundsCommandsRequireNamedJWT I5（spec 2026-10-09 §6.5）：所有资金命令
// 在缺少具名 JWT 操作者（静态 admin token 路径）时一律 403——在触及 service 之前
// 就被 requireFundsActor 拦下（AdminAPI{} 无 svc，若继续执行会 panic；未 panic 即
// 证明已拦下）。
func TestAdminFundsCommandsRequireNamedJWT(t *testing.T) {
	api := &AdminAPI{}
	calls := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"approve", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdApprove(w, r, 1) }},
		{"reject", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdReject(w, r, 1) }},
		{"claim", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdClaim(w, r, 1) }},
		{"confirm-failed", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdConfirmFailed(w, r, 1) }},
		{"paid", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdPaid(w, r, 1) }},
		{"admin-request", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsAdminRequest(w, r) }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/supplier/settlements/1/"+tc.name, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code, "静态 admin token（无具名操作者）⇒ 403")
		})
	}
}

// TestFundsActorRoundTrip 具名操作者经 context 透传（adminAuth JWT 路径注入）。
func TestFundsActorRoundTrip(t *testing.T) {
	ctx := domain.WithFundsActor(httptest.NewRequest(http.MethodGet, "/", nil).Context(), domain.FundsActor{UserID: 7, TokenVersion: 3})
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	actor, ok := fundsActorFrom(req)
	require.True(t, ok)
	require.Equal(t, int64(7), actor.UserID)
	require.Equal(t, int64(3), actor.TokenVersion)
}
