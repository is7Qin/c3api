// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

// payment_admin_route_test.go 钉住支付账户管理面的**挂载位置**（spec v2 §10-3）：
//
//  1. `/api/admin/payment/*` 必须在 **adminAuth 组内**（无凭据 → 401）：账户 config
//     里有商户私钥，落到无鉴权路径等于把凭据交给公网。
//  2. 该前缀由 PaymentAdminHandler 接管，其余 `/api/admin/*` 仍归 AdminHandler
//     （两条 Handle 前缀不同、互不吞并）。
import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
)

func TestPaymentAdminRouteInAdminGroup(t *testing.T) {
	iss := auth.NewIssuer("secret")
	tok, err := iss.Issue(1, "admin@example.com", string(domain.RolePlatformAdmin), 0)
	require.NoError(t, err)
	snaps := fakeUserStatus{roles: map[int64]domain.Role{1: domain.RolePlatformAdmin}}

	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Handler", name)
			w.WriteHeader(http.StatusOK)
		})
	}
	s := NewServer(Options{
		JWTIssuer:           iss,
		Auth:                fakeSnapshot{snaps, fakeMgmt{}},
		AdminHandler:        mark("admin"),
		PaymentAdminHandler: mark("payment-admin"),
	})

	cases := []struct {
		name, path, auth string
		want             int
		wantHandler      string
	}{
		{"payment admin requires auth", "/api/admin/payment/accounts", "", http.StatusUnauthorized, ""},
		{"payment admin routed", "/api/admin/payment/accounts", "Bearer " + tok, http.StatusOK, "payment-admin"},
		{"orders still admin", "/api/admin/orders", "Bearer " + tok, http.StatusOK, "admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			if tc.wantHandler != "" {
				require.Equal(t, tc.wantHandler, rec.Header().Get("X-Handler"),
					"路由必须落到对应 handler（前缀互不吞并）")
			}
		})
	}
}
