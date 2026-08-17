// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// newFileLogger creates a logger that writes JSON lines to a fresh temp
// file and returns the logger plus the file path（复用 pkg/logx/logx_test.go
// 的 newFileLogger 模式；Windows 下 zap 保持 sink 文件打开，dir 清理 best-effort）。
func newFileLogger(t *testing.T, level string) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "server-test-")
	require.NoError(t, err)
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New(level, out)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logger, out
}

// TestAccessLogDebugFields accessLog 的 Debug 字段构造 level 守卫（spec
// 2026-08-18，评审 M-1 强制条款）：level=debug 时输出 JSON 行含
// "msg":"http request" 且 5 字段键齐全（request_id/method/path/status/
// duration）；level=info 时整段跳过（无输出）。可捕获面：发射级别误抬高
// （如守卫写死放行 debug）→ info 子用例出现输出即失败；字段漏写 → debug
// 子用例键缺失即失败。不可区分面：守卫整体缺失/级别写错由 zap 自身 level
// 过滤兜底，输出与正确接线一致，超出本测试声称范围。
func TestAccessLogDebugFields(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  bool // true = 期望输出 http request 行
	}{
		{"debug", true},
		{"info", false},
	} {
		t.Run(tc.level, func(t *testing.T) {
			logger, out := newFileLogger(t, tc.level)
			h := accessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
			h.ServeHTTP(httptest.NewRecorder(), req)
			require.NoError(t, logger.Sync())

			b, err := os.ReadFile(out)
			require.NoError(t, err)
			line := string(b)
			if !tc.want {
				require.NotContains(t, line, "http request")
				return
			}
			require.Contains(t, line, `"msg":"http request"`)
			for _, key := range []string{"request_id", "method", "path", "status", "duration"} {
				require.Contains(t, line, `"`+key+`":`)
			}
		})
	}
}

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
