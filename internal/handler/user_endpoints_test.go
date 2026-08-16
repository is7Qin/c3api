// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// fakeUserStatus 快照 provider（测试替身：直读 fake store 当前状态，
// 语义等同 Auth 快照在 invalidate 后反映 DB 状态；status+role 单次查找）。
type fakeUserStatus struct{ store *fakeStore }

func (f fakeUserStatus) UserSnapshot(userID int64) (domain.UserSnapshot, bool) {
	u, err := f.store.GetUser(context.Background(), userID)
	if err != nil {
		return domain.UserSnapshot{}, false
	}
	return domain.UserSnapshot{Status: u.Status, Role: u.Role}, true
}

// newTestUserRouter /user 测试路由（真实 svc + fake store + 真实 Issuer）。
// svc 一并返回：settings 快照测试需经 UpdateSetting（快照重载）改配置。
func newTestUserRouter(t *testing.T) (func(method, path, body, token string) *httptest.ResponseRecorder, *fakeStore, *auth.Issuer, *service.Service) {
	t.Helper()
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	iss := auth.NewIssuer("test-secret")
	router := userapi.Router(svc, iss, fakeUserStatus{store: store}, nil) // nil = 不限速（F3 节流测试见 TestUserPublicRateLimit）
	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	return do, store, iss, svc
}

func TestUserRegisterLoginMe(t *testing.T) {
	do, _, _, _ := newTestUserRouter(t)

	// 注册成功（注册即登录：返回 JWT + 用户）。空表首个注册 = platform_admin
	// （bootstrap，spec 2026-08-15）；后续注册恒为普通 user。
	rec := do(http.MethodPost, "/user/auth/register", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "register: %s", rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token, "注册即登录")
	require.Equal(t, "new@example.com", *resp.User.Email)
	require.Equal(t, userapi.UserRole("platform_admin"), *resp.User.Role, "空表首个注册 = platform_admin")

	// 第二个注册 → 普通 user（bootstrap 之后恒 user）
	rec = do(http.MethodPost, "/user/auth/register", `{"email":"second@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "second register: %s", rec.Body.String())
	var resp2 userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp2))
	require.Equal(t, userapi.UserRole("user"), *resp2.User.Role, "非空表注册恒为普通 user")

	// me（注册返回的 JWT 直接可用）
	rec = do(http.MethodGet, "/user/auth/me", "", resp.Token)
	require.Equal(t, http.StatusOK, rec.Code, "me: %s", rec.Body.String())
	var me userapi.User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, "new@example.com", *me.Email)

	// me 无 token → 401
	rec = do(http.MethodGet, "/user/auth/me", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 重复注册 → 409
	rec = do(http.MethodPost, "/user/auth/register", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusConflict, rec.Code, "dup register: %s", rec.Body.String())

	// 登录成功
	rec = do(http.MethodPost, "/user/auth/login", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "login: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token)

	// 口令错误 → 401
	rec = do(http.MethodPost, "/user/auth/login", `{"email":"new@example.com","password":"wrong"}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 邮箱格式非法 → 400
	rec = do(http.MethodPost, "/user/auth/register", `{"email":"not-an-email","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad email: %s", rec.Body.String())

	// 密码超长（>72 字节）→ 400
	longPass := strings.Repeat("a", 73)
	rec = do(http.MethodPost, "/user/auth/register", `{"email":"long@example.com","password":"`+longPass+`"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "long password: %s", rec.Body.String())
}

// 注册开关（settings.signup_enabled）关闭 → 403（UpdateSetting 后快照即时生效）。
func TestUserRegisterSignupDisabled(t *testing.T) {
	do, _, _, svc := newTestUserRouter(t)
	_, err := svc.UpdateSetting(t.Context(), "signup_enabled", "false")
	require.NoError(t, err)
	rec := do(http.MethodPost, "/user/auth/register", `{"email":"x@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusForbidden, rec.Code, "signup disabled: %s", rec.Body.String())
}

// 禁用用户登录 → 401（与口令错误同文案，防枚举）。
func TestUserLoginDisabled(t *testing.T) {
	do, store, iss, _ := newTestUserRouter(t)
	rec := do(http.MethodPost, "/user/auth/register", `{"email":"d@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, 200, rec.Code)
	u, err := store.GetUserByEmail(t.Context(), "d@example.com")
	require.NoError(t, err)
	st := domain.UserStatusDisabled
	_, err = store.UpdateUser(t.Context(), &repository.UserPatch{ID: u.ID, Status: &st})
	require.NoError(t, err)
	rec = do(http.MethodPost, "/user/auth/login", `{"email":"d@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "disabled login: %s", rec.Body.String())

	// 禁用用户的既有 JWT → me 401（快照校验）
	token, err := iss.Issue(u.ID, u.Email, string(u.Role))
	require.NoError(t, err)
	rec = do(http.MethodGet, "/user/auth/me", "", token)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "disabled me: %s", rec.Body.String())
}

// F3：公开面 bcrypt 节流（Router 注入严格桶）——同 IP 超速 429（限流在
// handler 之前：超速请求不触达 bcrypt）；不同 IP 独立计数；宽桶正常速率
// 零影响。限流器单元测试见 internal/handler/user/ratelimit_test.go。
func TestUserPublicRateLimit(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	iss := auth.NewIssuer("test-secret")
	strict := userapi.NewIPRateLimiter(1000, 2, time.Minute, 1000) // burst 2
	router := userapi.Router(svc, iss, fakeUserStatus{store: store}, strict)
	login := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/user/auth/login", strings.NewReader(`{"email":"x@example.com","password":"wrong"}`))
		req.RemoteAddr = ip + ":12345"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	// burst 2：同 IP 第 3 个公开请求 429（前两个 401 = 口令错误但已过节流）
	require.Equal(t, http.StatusUnauthorized, login("203.0.113.10").Code)
	require.Equal(t, http.StatusUnauthorized, login("203.0.113.10").Code)
	require.Equal(t, http.StatusTooManyRequests, login("203.0.113.10").Code, "同 IP 超速 → 429")
	// 不同 IP 独立计数
	require.Equal(t, http.StatusUnauthorized, login("203.0.113.11").Code)

	// 正常速率零影响：宽桶下连续公开请求全放行（401 = 口令错误路径照常）
	wide := userapi.NewIPRateLimiter(1000, 1000, time.Minute, 1000)
	wideRouter := userapi.Router(svc, iss, fakeUserStatus{store: store}, wide)
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/user/auth/login", strings.NewReader(`{"email":"x@example.com","password":"wrong"}`))
		req.RemoteAddr = "203.0.113.12:12345"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		wideRouter.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "宽桶下 #%d 不受限", i+1)
	}
}

// --- 管理面 settings ---

func TestAdminSettings(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// GET：全部默认值（注册表逐项）
	rec := do(http.MethodGet, "/admin/settings", "")
	require.Equal(t, http.StatusOK, rec.Code, "get: %s", rec.Body.String())
	var rows []Setting
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, len(domain.DefaultSettings))
	require.Equal(t, "signup_enabled", *rows[0].Key)
	require.Equal(t, "true", *rows[0].Value)

	// PUT：switch 合法值
	rec = do(http.MethodPut, "/admin/settings", `{"key":"signup_enabled","value":"false"}`)
	require.Equal(t, http.StatusOK, rec.Code, "put: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Equal(t, "false", *rows[0].Value)

	// PUT：switch 非法值 → 400（类型化校验）
	rec = do(http.MethodPut, "/admin/settings", `{"key":"signup_enabled","value":"maybe"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad switch: %s", rec.Body.String())

	// PUT：未知 key → 400
	rec = do(http.MethodPut, "/admin/settings", `{"key":"unknown_key","value":"1"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "unknown key: %s", rec.Body.String())

	// PUT：number 类型校验（number 内置项传非数字 → 400；合法数字 → 200）
	rec = do(http.MethodPut, "/admin/settings", `{"key":"default_user_max_concurrency","value":"abc"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "number 传非数字必须 400")
	rec = do(http.MethodPut, "/admin/settings", `{"key":"default_user_max_concurrency","value":"5"}`)
	require.Equal(t, http.StatusOK, rec.Code, "number 合法值: %s", rec.Body.String())
}
