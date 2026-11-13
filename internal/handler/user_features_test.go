// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/service"
)

// decodeFeatures 断言 /api/user/features 的 200 JSON 形状：恰好一个键 supplier，
// 返回其值。用 map 而非生成类型解码，多出的键（无意泄出的配置）会让 len 断言失败。
func decodeFeatures(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"), "Content-Type=%q", rec.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 1, rec.Body.String())
	v, ok := body["supplier"]
	require.True(t, ok, rec.Body.String())
	return v
}

// TestUserFeaturesReflectsConfig 开关原样回显构造期注入值（main 装配取
// supplier.enabled）：开启 ⇒ true，零值 ⇒ false。
func TestUserFeaturesReflectsConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features userapi.Features
		want     bool
	}{
		{"supplier enabled", userapi.Features{Supplier: true}, true},
		{"zero value", userapi.Features{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			do, _, _, _ := newTestUserRouterWithFeatures(t, tc.features)
			rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"f@example.com","password":"s3cret-pass"}`, "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp userapi.UserAuthResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

			rec = do(http.MethodGet, "/api/user/features", "", resp.Token)
			require.Equal(t, tc.want, decodeFeatures(t, rec))
		})
	}
}

// TestUserFeaturesRequiresIdentity 非公开路径：无凭据 ⇒ 401（不是公开开关端点）。
func TestUserFeaturesRequiresIdentity(t *testing.T) {
	do, _, _, _ := newTestUserRouterWithFeatures(t, userapi.Features{Supplier: true})
	rec := do(http.MethodGet, "/api/user/features", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

// TestUserFeaturesManagementKey 鉴权与 /api/user/* 其余路径同一 RequireIdentity：
// mk- 管理 key 以 owner 身份同样可读。
func TestUserFeaturesManagementKey(t *testing.T) {
	store := newFakeStore()
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Auth: &fakeKeys{}, EmailCodeStore: store})
	iss := auth.NewIssuer("features-mk-secret")
	users := fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleUser}}
	mgmt := fakeMgmt{metas: map[string]domain.ManagementKeyMeta{
		"mk-feat": {ID: 1, UserID: 42, Status: domain.ManagementKeyStatusActive},
	}}
	router := userapi.Router(svc, iss, fakeSnapshot{users, mgmt}, nil, nil, userapi.Features{Supplier: true})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, mkReq(http.MethodGet, "/api/user/features", "Bearer mk-feat", ""))
	require.Equal(t, true, decodeFeatures(t, rec))
}
