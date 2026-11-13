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

// decodeFeatures 断言 /api/user/features 的 200 JSON 形状：恰好两个键 supplier 与
// payment，返回键→值。用 map 而非生成类型解码，多出的键（无意泄出的配置）会让
// len 断言失败。
func decodeFeatures(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"), "Content-Type=%q", rec.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 2, rec.Body.String())
	for _, k := range []string{"supplier", "payment"} {
		_, ok := body[k]
		require.True(t, ok, "missing %q: %s", k, rec.Body.String())
	}
	return body
}

// registerFeaturesUser 注册一个用户并返回 JWT（features 是非公开路径）。
func registerFeaturesUser(t *testing.T, do func(method, path, body, token string) *httptest.ResponseRecorder) string {
	t.Helper()
	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"f@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Token
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
			token := registerFeaturesUser(t, do)

			rec := do(http.MethodGet, "/api/user/features", "", token)
			require.Equal(t, tc.want, decodeFeatures(t, rec)["supplier"])
		})
	}
}

// TestUserFeaturesPaymentFollowsSetting payment 读运行时设置 payment.enabled：缺省
// 关；经 UpdateSetting 打开/关闭后同一路由下一次请求即反映（不重建路由 = 不重启）。
// 构造期传入的 Payment 被忽略（只认设置），supplier 不受支付开关影响。
func TestUserFeaturesPaymentFollowsSetting(t *testing.T) {
	do, _, _, svc := newTestUserRouterWithFeatures(t, userapi.Features{Supplier: true, Payment: true})
	token := registerFeaturesUser(t, do)
	get := func() map[string]any {
		return decodeFeatures(t, do(http.MethodGet, "/api/user/features", "", token))
	}

	body := get()
	require.Equal(t, false, body["payment"], "缺省关；构造期 Payment 不生效")
	require.Equal(t, true, body["supplier"])

	_, err := svc.UpdateSetting(t.Context(), domain.SettingPaymentEnabled, "true")
	require.NoError(t, err)
	body = get()
	require.Equal(t, true, body["payment"], "打开即生效")
	require.Equal(t, true, body["supplier"])

	_, err = svc.UpdateSetting(t.Context(), domain.SettingPaymentEnabled, "false")
	require.NoError(t, err)
	require.Equal(t, false, get()["payment"], "关闭即生效")
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
	require.Equal(t, true, decodeFeatures(t, rec)["supplier"])
}
