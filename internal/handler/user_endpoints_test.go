// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	return domain.UserSnapshot{Status: u.Status, Role: u.Role, TokenVersion: u.TokenVersion}, true
}

// newTestUserRouter /user 测试路由（真实 svc + fake store + 真实 Issuer）。
// svc 一并返回：settings 快照测试需经 UpdateSetting（快照重载）改配置。
func newTestUserRouter(t *testing.T) (func(method, path, body, token string) *httptest.ResponseRecorder, *fakeStore, *auth.Issuer, *service.Service) {
	t.Helper()
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	svc.SetEmailCodeStore(store) // 验证码迁 Redis 后独立注入面（fake 即实现，spec §2.2）
	mw := service.NewMailWorker(svc)
	require.NoError(t, mw.Start(t.Context()))
	t.Cleanup(func() { _ = mw.Close(context.Background()) })
	svc.SetMailEnqueue(mw.Enqueue)
	iss := auth.NewIssuer("test-secret")
	router := userapi.Router(svc, iss, fakeUserStatus{store: store}, nil)
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
	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "register: %s", rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token, "注册即登录")
	require.Equal(t, "new@example.com", *resp.User.Email)
	require.Equal(t, userapi.UserRole("platform_admin"), *resp.User.Role, "空表首个注册 = platform_admin")

	// 第二个注册 → 普通 user（bootstrap 之后恒 user）
	rec = do(http.MethodPost, "/api/user/auth/register", `{"email":"second@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "second register: %s", rec.Body.String())
	var resp2 userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp2))
	require.Equal(t, userapi.UserRole("user"), *resp2.User.Role, "非空表注册恒为普通 user")

	// me（注册返回的 JWT 直接可用）
	rec = do(http.MethodGet, "/api/user/auth/me", "", resp.Token)
	require.Equal(t, http.StatusOK, rec.Code, "me: %s", rec.Body.String())
	var me userapi.User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, "new@example.com", *me.Email)

	// me 无 token → 401
	rec = do(http.MethodGet, "/api/user/auth/me", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 重复注册 → 409
	rec = do(http.MethodPost, "/api/user/auth/register", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusConflict, rec.Code, "dup register: %s", rec.Body.String())

	// 登录成功
	rec = do(http.MethodPost, "/api/user/auth/login", `{"email":"new@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "login: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token)

	// 口令错误 → 401
	rec = do(http.MethodPost, "/api/user/auth/login", `{"email":"new@example.com","password":"wrong"}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 邮箱格式非法 → 400
	rec = do(http.MethodPost, "/api/user/auth/register", `{"email":"not-an-email","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad email: %s", rec.Body.String())

	// 密码超长（>72 字节）→ 400
	longPass := strings.Repeat("a", 73)
	rec = do(http.MethodPost, "/api/user/auth/register", `{"email":"long@example.com","password":"`+longPass+`"}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, "long password: %s", rec.Body.String())
}

// 注册开关（settings.signup_enabled）关闭 → 403（UpdateSetting 后快照即时生效）。
func TestUserRegisterSignupDisabled(t *testing.T) {
	do, _, _, svc := newTestUserRouter(t)
	_, err := svc.UpdateSetting(t.Context(), "signup_enabled", "false")
	require.NoError(t, err)
	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"x@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusForbidden, rec.Code, "signup disabled: %s", rec.Body.String())
}

// 禁用用户登录 → 401（与口令错误同文案，防枚举）。
func TestUserLoginDisabled(t *testing.T) {
	do, store, iss, _ := newTestUserRouter(t)
	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"d@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, 200, rec.Code)
	u, err := store.GetUserByEmail(t.Context(), "d@example.com")
	require.NoError(t, err)
	st := domain.UserStatusDisabled
	_, err = store.UpdateUser(t.Context(), &repository.UserPatch{ID: u.ID, Status: &st})
	require.NoError(t, err)
	rec = do(http.MethodPost, "/api/user/auth/login", `{"email":"d@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "disabled login: %s", rec.Body.String())

	// 禁用用户的既有 JWT → me 401（快照校验；ver 0 = 注册时默认版本）
	token, err := iss.Issue(u.ID, u.Email, string(u.Role), u.TokenVersion)
	require.NoError(t, err)
	rec = do(http.MethodGet, "/api/user/auth/me", "", token)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "disabled me: %s", rec.Body.String())
}

// resetCodeSHA 验证码 SHA256（service.hashCode 同算法——email_codes.CodeSHA256
// 比对键；handler 测试面独立小助手避免跨包引私有符号）。
func resetCodeSHA(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// 改密即撤销·自助路径（spec 2026-08-25-jwt-password-revocation §5.4 流程级）：
// ChangePassword 成功后携带旧 token 的请求 → 401；新密码 Login → 200 新票可用。
func TestChangePasswordRevokesOldJWT(t *testing.T) {
	do, _, _, _ := newTestUserRouter(t)

	// 注册即登录 → token A（ver = 创建默认 0）
	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"revoke@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var reg userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg))
	tokenA := reg.Token

	// 旧 token 当前有效
	rec = do(http.MethodGet, "/api/user/auth/me", "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "改密前旧 token 有效: %s", rec.Body.String())

	// 自助改密（凭旧密码 + 旧 token）
	rec = do(http.MethodPost, "/api/user/auth/change-password",
		`{"old_password":"s3cret-pass","new_password":"n3w-secret"}`, tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "change-password: %s", rec.Body.String())

	// 旧 token 立即失效（token_version 已 bump，快照 ver=1 ≠ claims.Ver=0）
	rec = do(http.MethodGet, "/api/user/auth/me", "", tokenA)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "改密后旧 token 必须 401")

	// 新密码重新登录 → 新票可用
	rec = do(http.MethodPost, "/api/user/auth/login", `{"email":"revoke@example.com","password":"n3w-secret"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "新密码登录: %s", rec.Body.String())
	var login userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &login))
	rec = do(http.MethodGet, "/api/user/auth/me", "", login.Token)
	require.Equal(t, http.StatusOK, rec.Code, "新票可用")
}

// 改密即撤销·邮箱重置路径（spec §5.4）：ResetPassword 成功后旧 token → 401；
// 新密码 Login → 200。
func TestResetPasswordRevokesOldJWT(t *testing.T) {
	do, store, _, _ := newTestUserRouter(t)

	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"resetrev@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var reg userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg))
	tokenA := reg.Token

	// 直播重置验证码（验证码存储 fake 即实现，spec emailcode-redis-migration §2.2）
	code := "654321"
	_, err := store.UpsertEmailCode(t.Context(), "resetrev@example.com",
		string(domain.EmailCodeReset), resetCodeSHA(code), time.Now().Add(10*time.Minute))
	require.NoError(t, err)

	rec = do(http.MethodPost, "/api/user/auth/reset-password",
		`{"email":"resetrev@example.com","code":"`+code+`","new_password":"n3w-secret"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "reset-password: %s", rec.Body.String())

	// 旧 token 立即失效
	rec = do(http.MethodGet, "/api/user/auth/me", "", tokenA)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "重置密码后旧 token 必须 401")

	// 新密码重新登录 → 可用
	rec = do(http.MethodPost, "/api/user/auth/login", `{"email":"resetrev@example.com","password":"n3w-secret"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "重置后新密码登录: %s", rec.Body.String())
}

// --- 管理面 settings ---

func TestAdminSettings(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// GET：全部默认值（注册表逐项）
	rec := do(http.MethodGet, "/api/admin/settings", "")
	require.Equal(t, http.StatusOK, rec.Code, "get: %s", rec.Body.String())
	var rows []Setting
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, len(domain.DefaultSettings))
	require.Equal(t, "signup_enabled", *rows[0].Key)
	require.Equal(t, "true", *rows[0].Value)

	// PUT：switch 合法值
	rec = do(http.MethodPut, "/api/admin/settings", `{"key":"signup_enabled","value":"false"}`)
	require.Equal(t, http.StatusOK, rec.Code, "put: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Equal(t, "false", *rows[0].Value)

	// PUT：switch 非法值 → 400（类型化校验）
	rec = do(http.MethodPut, "/api/admin/settings", `{"key":"signup_enabled","value":"maybe"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad switch: %s", rec.Body.String())

	// PUT：未知 key → 400
	rec = do(http.MethodPut, "/api/admin/settings", `{"key":"unknown_key","value":"1"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "unknown key: %s", rec.Body.String())

	// PUT：number 类型校验（number 内置项传非数字 → 400；合法数字 → 200）
	rec = do(http.MethodPut, "/api/admin/settings", `{"key":"default_user_max_concurrency","value":"abc"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "number 传非数字必须 400")
	rec = do(http.MethodPut, "/api/admin/settings", `{"key":"default_user_max_concurrency","value":"5"}`)
	require.Equal(t, http.StatusOK, rec.Code, "number 合法值: %s", rec.Body.String())
}
