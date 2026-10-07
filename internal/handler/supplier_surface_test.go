// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
)

func okStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

// TestSupplierSurfaceGuardDefaultDeny A14④：default-deny 允许清单。
func TestSupplierSurfaceGuardDefaultDeny(t *testing.T) {
	g := SupplierSurfaceGuard(okStub())
	cases := []struct {
		method, path string
		want         int
	}{
		// 账号端点子集 ⇒ 可达。
		{"GET", SupplierSurfaceBaseURL + "/accounts", 200},
		{"POST", SupplierSurfaceBaseURL + "/accounts", 200},
		{"POST", SupplierSurfaceBaseURL + "/accounts/batch-update", 200},
		{"POST", SupplierSurfaceBaseURL + "/accounts/batch-delete", 200},
		{"POST", SupplierSurfaceBaseURL + "/accounts/batch-import-codex-oauth", 200},
		{"POST", SupplierSurfaceBaseURL + "/accounts/batch-import-codex-pat", 200},
		{"GET", SupplierSurfaceBaseURL + "/accounts/usage", 200},
		{"GET", SupplierSurfaceBaseURL + "/accounts/42", 200},
		{"PATCH", SupplierSurfaceBaseURL + "/accounts/42", 200},
		{"DELETE", SupplierSurfaceBaseURL + "/accounts/42", 200},
		{"GET", SupplierSurfaceBaseURL + "/accounts/42/ext", 200},
		{"PUT", SupplierSurfaceBaseURL + "/accounts/42/ext", 200},
		{"GET", SupplierSurfaceBaseURL + "/accounts/42/groups", 200},
		{"POST", SupplierSurfaceBaseURL + "/accounts/42/recover", 200},
		// groups GET 只读候选（可达）。
		{"GET", SupplierSurfaceBaseURL + "/groups", 200},
		// templates GET 只读（可达）。
		{"GET", SupplierSurfaceBaseURL + "/templates", 200},
		{"GET", SupplierSurfaceBaseURL + "/templates/7", 200},

		// 管理面资源 ⇒ 不可达（default-deny）。
		{"GET", SupplierSurfaceBaseURL + "/users", 404},
		{"GET", SupplierSurfaceBaseURL + "/settings", 404},
		{"GET", SupplierSurfaceBaseURL + "/prices", 404},
		{"GET", SupplierSurfaceBaseURL + "/rules", 404},
		{"GET", SupplierSurfaceBaseURL + "/ops/workers", 404},
		{"GET", SupplierSurfaceBaseURL + "/mail/templates", 404},
		{"GET", SupplierSurfaceBaseURL + "/redemption-codes", 404},
		// 组写面/assignments 不可达。
		{"POST", SupplierSurfaceBaseURL + "/groups", 404},
		{"PUT", SupplierSurfaceBaseURL + "/groups/1", 404},
		{"GET", SupplierSurfaceBaseURL + "/groups/1/assignments", 404},
		{"PUT", SupplierSurfaceBaseURL + "/groups/1/assignments", 404},
		// 模板写面不可达。
		{"POST", SupplierSurfaceBaseURL + "/templates", 404},
		{"PUT", SupplierSurfaceBaseURL + "/templates/1", 404},
		{"DELETE", SupplierSurfaceBaseURL + "/templates/1", 404},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		require.Equal(t, tc.want, rec.Code, "%s %s", tc.method, tc.path)
	}
}

// TestAccountScopeFromDefault 缺省 = 管理面全量。
func TestAccountScopeFromDefault(t *testing.T) {
	s := AccountScopeFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	require.False(t, s.Set, "缺省管理面全量")
	require.True(t, s.MatchesOwner(0))
	require.True(t, s.MatchesOwner(999))
}

type fakeUsers struct{ sn domain.UserSnapshot }

func (f fakeUsers) UserSnapshot(int64) (domain.UserSnapshot, bool) { return f.sn, true }

// TestSupplierScopeInject 作用域注入：RequireJWT → SupplierScopeInject ⇒
// AccountScopeFrom = {jwtUser, true}。
func TestSupplierScopeInject(t *testing.T) {
	iss := auth.NewIssuer("s")
	tok, err := iss.Issue(77, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	users := fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}}
	var got domain.AccountScope
	h := auth.RequireJWT(iss, users)(SupplierScopeInject(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = AccountScopeFrom(r.Context())
	})))
	req := httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.True(t, got.Set)
	require.Equal(t, int64(77), got.OwnerUID)
	require.True(t, got.MatchesOwner(77))
	require.False(t, got.MatchesOwner(88), "他人归属越域")
}
