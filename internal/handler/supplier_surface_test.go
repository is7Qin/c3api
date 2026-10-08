// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
)

// supplierRegisteredRoutes 供应商面**应有的**登记集（方法 + chi pattern），顺序无关。
// 唯一事实源是 openapi 里 tag `supplier` 的 path；本表是它的守卫副本——任一侧漂移
// （openapi 少/多登记一条，或生成面数量变化）都会让 A14④ 断言失败。
var supplierRegisteredRoutes = []string{
	"GET /api/user/supplier/accounts",
	"POST /api/user/supplier/accounts",
	"GET /api/user/supplier/accounts/usage",
	"POST /api/user/supplier/accounts/batch-update",
	"POST /api/user/supplier/accounts/batch-delete",
	"POST /api/user/supplier/accounts/batch-import-codex-oauth",
	"POST /api/user/supplier/accounts/batch-import-codex-pat",
	"GET /api/user/supplier/accounts/{id}",
	"PATCH /api/user/supplier/accounts/{id}",
	"DELETE /api/user/supplier/accounts/{id}",
	"GET /api/user/supplier/accounts/{id}/ext",
	"PUT /api/user/supplier/accounts/{id}/ext",
	"GET /api/user/supplier/accounts/{id}/groups",
	"POST /api/user/supplier/accounts/{id}/recover",
	"GET /api/user/supplier/groups",
	"GET /api/user/supplier/templates",
	"GET /api/user/supplier/templates/{id}",
	// 业务面（§6.1/§6.2）。
	"GET /api/user/supplier/overview",
	"GET /api/user/supplier/earnings",
	"GET /api/user/supplier/chunks",
	"GET /api/user/supplier/settlements",
	"POST /api/user/supplier/settlements",
}

// walkRoutes 收集已注册路由（method + pattern）。
func walkRoutes(t *testing.T, h http.Handler) []string {
	t.Helper()
	mux, ok := h.(chi.Routes)
	require.True(t, ok, "供应商面必须是 chi 路由（生成面 HandlerWithOptions）")
	var out []string
	require.NoError(t, chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	}))
	sort.Strings(out)
	return out
}

// TestSupplierSurfaceRegisteredRoutes A14④（结构性 default-deny）：生成面**只**注册
// tag `supplier` 的 op——登记集恰为 openapi 的供应商面清单，多一条少一条都算失败。
// 「未登记 ⇒ 未注册 ⇒ 404」即 default-deny 的全部机制（无手写允许清单/guard）。
func TestSupplierSurfaceRegisteredRoutes(t *testing.T) {
	api := newTestHandler(t)
	got := walkRoutes(t, api.SupplierSurfaceHandler())

	want := append([]string(nil), supplierRegisteredRoutes...)
	sort.Strings(want)
	require.Equal(t, want, got, "供应商面登记集必须 = openapi 中 tag supplier 的 path 集")
}

// TestSupplierSurfaceStructuralDefaultDeny A14④：管理面资源在供应商面**不可达**
// ——未登记（结构性 404）；组写面/assignments、模板写面、新增管理端点同理。
func TestSupplierSurfaceStructuralDefaultDeny(t *testing.T) {
	api := newTestHandler(t)
	h := api.SupplierSurfaceHandler()
	mux := h.(chi.Routes)

	reachable := []struct{ method, path string }{
		{"GET", "/api/user/supplier/accounts"},
		{"POST", "/api/user/supplier/accounts"},
		{"GET", "/api/user/supplier/accounts/usage"},
		{"POST", "/api/user/supplier/accounts/batch-update"},
		{"POST", "/api/user/supplier/accounts/batch-delete"},
		{"POST", "/api/user/supplier/accounts/batch-import-codex-oauth"},
		{"POST", "/api/user/supplier/accounts/batch-import-codex-pat"},
		{"GET", "/api/user/supplier/accounts/1"},
		{"PATCH", "/api/user/supplier/accounts/1"},
		{"DELETE", "/api/user/supplier/accounts/1"},
		{"GET", "/api/user/supplier/accounts/1/ext"},
		{"PUT", "/api/user/supplier/accounts/1/ext"},
		{"GET", "/api/user/supplier/accounts/1/groups"},
		{"POST", "/api/user/supplier/accounts/1/recover"},
		// groups GET 只读候选（可达）、templates GET 只读（可达）。
		{"GET", "/api/user/supplier/groups"},
		{"GET", "/api/user/supplier/templates"},
		{"GET", "/api/user/supplier/templates/1"},
		// 业务面（可达；空态零值/空列表，不 404）。
		{"GET", "/api/user/supplier/overview"},
		{"GET", "/api/user/supplier/earnings"},
		{"GET", "/api/user/supplier/chunks"},
		{"GET", "/api/user/supplier/settlements"},
	}
	for _, tc := range reachable {
		// 可达性按**路由命中**判定（避免与作用域 404——如越域 id——混淆：
		// 生成面命中即证明「已登记」，语义层的 404 另由作用域测试覆盖）。
		require.True(t, mux.Match(chi.NewRouteContext(), tc.method, tc.path),
			"%s %s 必须命中生成面（登记在 openapi 的供应商面）", tc.method, tc.path)
	}

	// 无路径参数的登记端点须真正跑通（≤ 证明登记的是**同一批管理面实现**，
	// 而非占位 stub）：账号/分组/模板列表在缺省作用域（管理面全量）下 200。
	// 业务面 4 个 GET 需具名 JWT（claims 取本人 uid），故其可达性只按路由命中
	// 断言（上面），运行态复用在 TestNewSupplierSurfaceGate 里验。
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/user/supplier/accounts"},
		{"GET", "/api/user/supplier/accounts/usage?account_ids=1&window=24h"},
		{"GET", "/api/user/supplier/groups"},
		{"GET", "/api/user/supplier/templates"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
	}

	blocked := []struct{ method, path string }{
		// 管理面资源 ⇒ 未登记 ⇒ 404。
		{"GET", "/api/user/supplier/users"},
		{"GET", "/api/user/supplier/settings"},
		{"GET", "/api/user/supplier/prices"},
		{"GET", "/api/user/supplier/rules"},
		{"GET", "/api/user/supplier/ops/workers"},
		{"GET", "/api/user/supplier/mail/templates"},
		{"GET", "/api/user/supplier/redemption-codes"},
		{"GET", "/api/user/supplier/keys"},
		{"GET", "/api/user/supplier/stats/trend"},
		{"GET", "/api/user/supplier/routing/flow"},
		{"GET", "/api/user/supplier/supplier/settlements"},
		{"GET", "/api/user/supplier/supplier/balances"},
		// 组写面/assignments ⇒ 不可达。
		{"POST", "/api/user/supplier/groups"},
		{"PUT", "/api/user/supplier/groups/1"},
		{"DELETE", "/api/user/supplier/groups/1"},
		{"GET", "/api/user/supplier/groups/1/assignments"},
		{"PUT", "/api/user/supplier/groups/1/assignments"},
		{"POST", "/api/user/supplier/groups/batch-delete"},
		// 模板写面 ⇒ 不可达。
		{"POST", "/api/user/supplier/templates"},
		{"PUT", "/api/user/supplier/templates/1"},
		{"DELETE", "/api/user/supplier/templates/1"},
		{"GET", "/api/user/supplier/templates/1/ext"},
		{"POST", "/api/user/supplier/templates/batch-delete"},
		// 反向断言：假设新增一个管理端点 ⇒ 默认不对供应商暴露（未登记）。
		{"GET", "/api/user/supplier/brand-new-admin-thing"},
	}
	for _, tc := range blocked {
		req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// 未登记 ⇒ chi 404；同路径仅登记了其它方法时 405。二者都表示「不可达」。
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, rec.Code,
			"%s %s 必须不可达（default-deny）", tc.method, tc.path)
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
// AccountScopeFrom = {jwtUser, true}（越域归属被阻）。
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

// TestNewSupplierSurfaceGate 整链门控（A13②方向）：无凭据 ⇒ 401 在生成面之前短死
// （不泄漏路由存在性）；具名供应商 JWT ⇒ 可达；user 角色 ⇒ 403。
func TestNewSupplierSurfaceGate(t *testing.T) {
	api := newTestHandler(t)
	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	h := NewSupplierSurface(api, iss, fakeUsers{sn: active})

	// 未鉴权 ⇒ 401（RequireJWT 最外层）。
	req := httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 具名供应商 JWT ⇒ 可达（生成面已注册）。
	tok, err := iss.Issue(77, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.NotContains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, rec.Code)

	// 非供应商角色（user）⇒ 403（RequireRole 快照基）。
	userTok, err := iss.Issue(88, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	plain := NewSupplierSurface(api, iss, fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleUser}})
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userTok)
	rec = httptest.NewRecorder()
	plain.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "user 角色不得进入供应商面")
}
