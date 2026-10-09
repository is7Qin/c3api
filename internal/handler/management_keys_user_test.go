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

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	handleruser "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/service"
)

// mgmtUsers 角色映射快照 provider（统一 user 面 owner 解析用例；缺省 user）。
// AuthenticateManagement 恒 false——合并 fake，满足 auth.SnapshotProvider
// （spec 2026-10-09 §4.3：同一 auth 只传一次）。
type mgmtUsers struct{ roles map[int64]domain.Role }

func (p mgmtUsers) UserSnapshot(id int64) (domain.UserSnapshot, bool) {
	role := p.roles[id]
	if role == "" {
		role = domain.RoleUser
	}
	return domain.UserSnapshot{Status: domain.UserStatusActive, Role: role}, true
}

func (p mgmtUsers) AuthenticateManagement(*http.Request) (domain.ManagementKeyMeta, bool) {
	return domain.ManagementKeyMeta{}, false
}

func mkReq(method, path, authHdr, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if authHdr != "" {
		req.Header.Set("Authorization", authHdr)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// doUser 以统一 user 面路由发一个请求（path 相对 /api/user）。
func doUser(router http.Handler, authHdr, method, path, body string) (int, string) {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, mkReq(method, "/api/user"+path, authHdr, body))
	return rec.Code, rec.Body.String()
}

// TestUserFaceManagementKeys 统一 user 面（/api/user/management-keys）自服务：
// owner = auth.ClaimsFrom（RequireIdentity 注入）；user JWT 可 CRUD，明文 mk-。
func TestUserFaceManagementKeys(t *testing.T) {
	store := newFakeStore()
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Auth: &fakeKeys{}, EmailCodeStore: store})
	iss := auth.NewIssuer("mgmt-user-secret")
	tok, err := iss.Issue(42, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	router := handleruser.Router(svc, iss, mgmtUsers{}, nil, nil)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, mkReq(http.MethodPost, "/api/user/management-keys", "Bearer "+tok, `{"name":"mine"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"key_raw":"mk-`)
	require.Contains(t, rec.Body.String(), `"user_id":42`)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, mkReq(http.MethodGet, "/api/user/management-keys", "Bearer "+tok, ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"name":"mine"`)
}

// TestSupplierFaceManagementKeys 统一 user 面：supplier owner 的 mk- 亦可自服务
// CRUD /api/user/management-keys（可达 = owner 角色闭包，supplier ⊂ 可 user 面）。
func TestSupplierFaceManagementKeys(t *testing.T) {
	store := newFakeStore()
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Auth: &fakeKeys{}, EmailCodeStore: store})
	iss := auth.NewIssuer("mgmt-sup-secret")
	users := fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}}
	mgmt := fakeMgmt{metas: map[string]domain.ManagementKeyMeta{
		"mk-sup": {ID: 1, UserID: 42, Status: domain.ManagementKeyStatusActive},
	}}
	router := handleruser.Router(svc, iss, fakeSnapshot{users, mgmt}, nil, nil)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, mkReq(http.MethodPost, "/api/user/management-keys", "Bearer mk-sup", `{"name":"sup"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"key_raw":"mk-`)
	require.Contains(t, rec.Body.String(), `"user_id":42`)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, mkReq(http.MethodGet, "/api/user/management-keys", "Bearer mk-sup", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"name":"sup"`)
}

// TestAdminManagementKeysCRUD 统一 user 面：platform_admin JWT 可 CRUD
// /api/user/management-keys（不再 401），user JWT 亦可（自服务）。admin 面端点已删；
// 原「非 platform_admin 打 admin 面 → 401」断言随端点删除而去除。
func TestAdminManagementKeysCRUD(t *testing.T) {
	store := newFakeStore()
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Auth: &fakeKeys{}, EmailCodeStore: store})
	iss := auth.NewIssuer("mgmt-admin-secret")
	adminTok, err := iss.Issue(7, "admin@example.com", string(domain.RolePlatformAdmin), 0)
	require.NoError(t, err)
	userTok, err := iss.Issue(8, "user@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	router := handleruser.Router(svc, iss, mgmtUsers{roles: map[int64]domain.Role{7: domain.RolePlatformAdmin}}, nil, nil)
	admin := "Bearer " + adminTok

	// platform_admin JWT 自服务 create → 明文 mk-（owner = claims uid）。
	c, rb := doUser(router, admin, http.MethodPost, "/management-keys", `{"name":"ci"}`)
	require.Equal(t, http.StatusOK, c, rb)
	require.Contains(t, rb, `"key_raw":"mk-`)
	require.Contains(t, rb, `"user_id":7`)

	// list 含刚创建项。
	c, rb = doUser(router, admin, http.MethodGet, "/management-keys", "")
	require.Equal(t, http.StatusOK, c, rb)
	require.Contains(t, rb, `"name":"ci"`)

	// put（禁用）→ 即时快照更新（响应 status disabled）。
	c, rb = doUser(router, admin, http.MethodPut, "/management-keys/1", `{"status":"disabled"}`)
	require.Equal(t, http.StatusOK, c, rb)
	require.Contains(t, rb, `"status":"disabled"`)

	// delete → 软删；list 空。
	c, rb = doUser(router, admin, http.MethodDelete, "/management-keys/1", "")
	require.Equal(t, http.StatusOK, c, rb)
	require.Contains(t, rb, `"deleted":true`)
	c, rb = doUser(router, admin, http.MethodGet, "/management-keys", "")
	require.Equal(t, http.StatusOK, c, rb)
	require.Contains(t, rb, `"rows":[]`)

	// user JWT 亦可自服务（任意已登录身份可调用；端点不加 role gate）。
	c, rb = doUser(router, "Bearer "+userTok, http.MethodPost, "/management-keys", `{"name":"u"}`)
	require.Equal(t, http.StatusOK, c, rb)
	require.Contains(t, rb, `"user_id":8`)
}

// TestRemovedManagementKeyAPIsNotFound 端点收敛（spec §4.4 F6）：旧 admin API
// /api/admin/management-keys[/{id}] 与旧 supplier API
// /api/user/supplier/management-keys[/{id}] 未注册 ⇒ 用有效 platform_admin JWT / mk-
// 打旧 admin API、用有效 supplier JWT / mk- 打旧 supplier API，均 404（路由不存在，
// 非 401/403）。注：/api/admin/* 与 /api/user/supplier/* 的通用鉴权仍包裹整面。
func TestRemovedManagementKeyAPIsNotFound(t *testing.T) {
	api := newTestHandler(t)
	iss := auth.NewIssuer("mgmt-convergence-secret")
	adminTok, err := iss.Issue(7, "admin@example.com", string(domain.RolePlatformAdmin), 0)
	require.NoError(t, err)
	supTok, err := iss.Issue(42, "sup@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	mgmt := fakeMgmt{metas: map[string]domain.ManagementKeyMeta{
		"mk-admin": {ID: 10, UserID: 7, Status: domain.ManagementKeyStatusActive},
		"mk-sup":   {ID: 11, UserID: 42, Status: domain.ManagementKeyStatusActive},
	}}

	// 旧 admin API：adminAuth 放行后落生成面 → 路由已删 ⇒ 404。
	adminSrv := server.NewServer(server.Options{
		JWTIssuer:    iss,
		Auth:         fakeSnapshot{fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RolePlatformAdmin}}, mgmt},
		AdminHandler: api.Router(),
	})
	for _, tc := range []struct{ name, auth string }{
		{"platform_admin JWT", "Bearer " + adminTok},
		{"management key", "Bearer mk-admin"},
	} {
		t.Run("admin/"+tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			adminSrv.Handler().ServeHTTP(rec, mkReq(http.MethodGet, "/api/admin/management-keys", tc.auth, ""))
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		})
	}

	// 旧 supplier API：RequireIdentity + RequireRole 放行后落生成面 → 路由已删 ⇒ 404。
	supH := NewSupplierSurface(api, iss, fakeSnapshot{fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}}, mgmt})
	for _, tc := range []struct{ name, auth string }{
		{"supplier JWT", "Bearer " + supTok},
		{"management key", "Bearer mk-sup"},
	} {
		t.Run("supplier/"+tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			supH.ServeHTTP(rec, mkReq(http.MethodGet, SupplierSurfaceBaseURL+"/management-keys", tc.auth, ""))
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		})
	}
}
