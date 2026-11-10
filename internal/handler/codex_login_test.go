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

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// —— codex 设备码登录 HTTP 面（生成路由 → handler 解码/组装 → service 编排）——
//
// 协议面注入 fake（真实 SDK 行为由 sdkbridge/device 测试覆盖），这里钉住的是
// 线缆契约：路由可达、请求字段解析、状态映射、以及「校验失败不消费上游授权码」。

// handlerCodexLoginProvider 脚本化协议面（handler 包测试替身）。
type handlerCodexLoginProvider struct {
	status   domain.CodexLoginStatus
	tokens   *domain.CodexLoginTokens
	startErr error
	pollErr  error
	pollHits int
}

func (f *handlerCodexLoginProvider) StartDeviceFlow(context.Context) (*domain.CodexDeviceCode, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &domain.CodexDeviceCode{
		AuthID: "auth-1", UserCode: "ABCD-EFGHI", VerificationURL: "https://auth.example/device",
		Interval: 7_000_000_000, ExpiresIn: 900_000_000_000,
	}, nil
}

func (f *handlerCodexLoginProvider) PollDeviceFlow(context.Context, string, string) (domain.CodexLoginStatus, *domain.CodexLoginTokens, error) {
	f.pollHits++
	if f.pollErr != nil {
		return "", nil, f.pollErr
	}
	return f.status, f.tokens, nil
}

// newCodexLoginRouter 建管理面路由 + fake 协议面；模板/分组预置（路由面只校验。
func newCodexLoginRouter(t *testing.T) (*fakeStore, *handlerCodexLoginProvider, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "codex-oauth", CredentialType: credential.TypeCodexOAuth}
	store.tpls[2] = &domain.Template{ID: 2, Name: "codex-pat", CredentialType: credential.TypeCodexPAT}
	store.groups[7] = &domain.Group{ID: 7, Name: "g", Visibility: domain.GroupVisibilityPublic}
	provider := &handlerCodexLoginProvider{}
	svc := service.New(service.Deps{
		Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Auth: &fakeKeys{}, EmailCodeStore: store, CodexLogin: provider,
	})
	h := New(svc)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	return store, provider, do
}

// TestPostAccountsCodexDeviceLogin 发起：返回完成所需凭据；不落库、不轮询。
func TestPostAccountsCodexDeviceLogin(t *testing.T) {
	store, provider, do := newCodexLoginRouter(t)

	rec := do(http.MethodPost, "/api/admin/accounts/codex-device-login", "")
	require.Equal(t, http.StatusOK, rec.Code, "start: %s", rec.Body.String())
	var out CodexDeviceLoginStart
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "auth-1", out.DeviceAuthId, "完成端点需要回传的设备凭据")
	require.Equal(t, "ABCD-EFGHI", out.UserCode)
	require.Equal(t, "https://auth.example/device", out.VerificationUri)
	require.Equal(t, 7, out.IntervalSeconds)
	require.Equal(t, 900, out.ExpiresInSeconds)
	require.Equal(t, 0, provider.pollHits, "发起不轮询")
	require.Empty(t, store.accs, "发起不落库")

	// 上游故障 → 502（不把上游原文透给客户端）。
	provider.startErr = context.DeadlineExceeded
	rec = do(http.MethodPost, "/api/admin/accounts/codex-device-login", "")
	require.Equal(t, http.StatusBadGateway, rec.Code, "start upstream: %s", rec.Body.String())
	require.NotContains(t, rec.Body.String(), "deadline", "上游细节只进日志")
}

// TestPostAccountsCodexDeviceLoginComplete 完成：pending 可再调；authorized 当场
// 落库并回账号定位；模板错配 400 且**不消费**上游授权码。
func TestPostAccountsCodexDeviceLoginComplete(t *testing.T) {
	const completePath = "/api/admin/accounts/codex-device-login/complete"
	store, provider, do := newCodexLoginRouter(t)

	provider.status = domain.CodexLoginPending
	rec := do(http.MethodPost, completePath, `{"device_auth_id":"auth-1","user_code":"ABCD-EFGHI","template_id":1}`)
	require.Equal(t, http.StatusOK, rec.Code, "pending: %s", rec.Body.String())
	var pending CodexDeviceLoginResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pending))
	require.Equal(t, CodexDeviceLoginResultStatus("pending"), pending.Status)
	require.Nil(t, pending.AccountId)
	require.Empty(t, store.accs)

	// 模板类型错配：400，且必须在轮询之前拒绝（一次性授权码不能被烧掉）。
	provider.status = domain.CodexLoginAuthorized
	provider.tokens = &domain.CodexLoginTokens{AccessToken: "at", RefreshToken: "rt", AccountID: "acc-1", Email: "a@example.com"}
	rec = do(http.MethodPost, completePath, `{"device_auth_id":"auth-1","user_code":"ABCD-EFGHI","template_id":2}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "wrong template: %s", rec.Body.String())
	require.Equal(t, 1, provider.pollHits, "类型校验先于轮询（此前仅 pending 那次）")

	rec = do(http.MethodPost, completePath, `{"device_auth_id":"","user_code":"x","template_id":1}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "missing device_auth_id: %s", rec.Body.String())
	rec = do(http.MethodPost, completePath, `{"device_auth_id":"a","user_code":"u","template_id":1,"unknown":1}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "unknown field must fail: %s", rec.Body.String())

	// 正常完成：authorized + 账号已落库。
	rec = do(http.MethodPost, completePath, `{"device_auth_id":"auth-1","user_code":"ABCD-EFGHI","template_id":1,"group_id":7}`)
	require.Equal(t, http.StatusOK, rec.Code, "authorized: %s", rec.Body.String())
	var done CodexDeviceLoginResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &done))
	require.Equal(t, CodexDeviceLoginResultStatus("authorized"), done.Status)
	require.NotNil(t, done.AccountId)
	require.NotNil(t, done.Imported)
	require.True(t, *done.Imported)
	require.Equal(t, "a@example.com", *done.Email)
	require.Equal(t, "acc-1", *done.CodexAccountId)

	ext, err := store.FindAccountExtByCodexKey(context.Background(), "a@example.com", "acc-1")
	require.NoError(t, err)
	require.Equal(t, *done.AccountId, ext.AccountID)
	require.Equal(t, "rt", *ext.CodexOAuthRefreshToken)

	// 过期/拒绝 → 200 + 终态（客户端据此重新发起）。
	provider.status = domain.CodexLoginExpired
	rec = do(http.MethodPost, completePath, `{"device_auth_id":"auth-1","user_code":"ABCD-EFGHI","template_id":1}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var expired CodexDeviceLoginResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &expired))
	require.Equal(t, CodexDeviceLoginResultStatus("expired"), expired.Status)

	// 未装配 → 503（部署形态缺失，不是瞬时错误）。
	bare := New(service.New(service.Deps{Store: store, EmailCodeStore: store}))
	rr := chi.NewRouter()
	rr.Mount("/", bare.Router())
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/codex-device-login", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec2 := httptest.NewRecorder()
	rr.ServeHTTP(rec2, req)
	require.Equal(t, http.StatusServiceUnavailable, rec2.Code, "unassembled: %s", rec2.Body.String())
}
