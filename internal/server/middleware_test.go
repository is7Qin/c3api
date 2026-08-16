// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
)

// TestAdminAuthEmptyTokenContract admin.token 可空语义契约（spec 2026-08-15）：
// 空 token = 不启用静态路径，/admin 仅接受 platform_admin JWT——任意非空
// Bearer（含非 JWT 垃圾串/尾空值）恒 401；platform_admin JWT 通过；token
// 非空时旧行为不变（匹配 → 通过，不匹配 → 401）。
func TestAdminAuthEmptyTokenContract(t *testing.T) {
	iss := auth.NewIssuer("secret")
	adminTok, err := iss.Issue(1, "admin@example.com", string(domain.RolePlatformAdmin))
	require.NoError(t, err)
	userTok, err := iss.Issue(2, "user@example.com", string(domain.RoleUser))
	require.NoError(t, err)
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	for _, tc := range []struct {
		name string
		opts Options
		auth string
		want int
	}{
		// 空 token：静态路径永不匹配 → 任意非 Bearer 前缀/Bearer 垃圾串 401
		{"empty token: no header", Options{JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "", 401},
		{"empty token: non-JWT garbage", Options{JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "Bearer garbage", 401},
		{"empty token: bare Bearer", Options{JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "Bearer", 401},
		// "Bearer "（尾空）在 httptest 直达头值（无 textproto 修剪）下，若无空
		// 守卫会等于 "Bearer "+"" ——守卫即此回归点（h2 下真实存在，见 middleware.go 注释）
		{"empty token: Bearer empty value", Options{JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "Bearer ", 401},
		{"empty token: non-bearer scheme", Options{JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "Basic xyz", 401},
		// F1：快照 role 覆盖 claims.Role——user 1 快照角色 platform_admin 才放行
		{"empty token: platform_admin JWT", Options{JWTIssuer: iss, UserStatus: fakeUserStatus{roles: map[int64]domain.Role{1: domain.RolePlatformAdmin}}}, "Bearer " + adminTok, 200},
		{"empty token: user JWT", Options{JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "Bearer " + userTok, 401},
		// 非空 token：旧行为不变
		{"token set: matching", Options{AdminToken: "tok", JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "Bearer tok", 200},
		{"token set: mismatch", Options{AdminToken: "tok", JWTIssuer: iss, UserStatus: fakeUserStatus{}}, "Bearer nope", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.AdminHandler = admin
			s := NewServer(tc.opts)
			req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}
