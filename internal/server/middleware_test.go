// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// fakeMgmt 管理 key 快照 provider（adminAuth mk- 分支用例）：按 Bearer mk-… 明文
// 查表；仅 status==active 命中（模拟 proxy.Auth.AuthenticateManagement 的禁用过滤）。
type fakeMgmt struct {
	metas map[string]domain.ManagementKeyMeta
}

func (f fakeMgmt) AuthenticateManagement(r *http.Request) (domain.ManagementKeyMeta, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(raw, "mk-") {
		return domain.ManagementKeyMeta{}, false
	}
	m, ok := f.metas[raw]
	if !ok || m.Status != domain.ManagementKeyStatusActive {
		return domain.ManagementKeyMeta{}, false
	}
	return m, true
}

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
// 2026-08-18，强制条款）：level=debug 时输出 JSON 行含
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

// TestAdminAuth 管理面鉴权契约（静态 token 已删除，spec 2026-10-09）：/admin 仅
// 接受 platform_admin JWT 或 platform_admin 身份的**管理 key mk-**；任意非空
// Bearer（含非 JWT 垃圾串/尾空值/未知 mk-）恒 401（前缀先判、失败不回退）；mk-
// owner 非 platform_admin 或 key 禁用 → 401；provider 缺失 → mk- 全拒。
func TestAdminAuth(t *testing.T) {
	iss := auth.NewIssuer("secret")
	adminTok, err := iss.Issue(1, "admin@example.com", string(domain.RolePlatformAdmin), 0)
	require.NoError(t, err)
	userTok, err := iss.Issue(2, "user@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	snaps := fakeUserStatus{roles: map[int64]domain.Role{
		1: domain.RolePlatformAdmin, // adminTok
		3: domain.RolePlatformAdmin, // mk-admin owner
		4: domain.RoleUser,          // mk-user owner
	}}
	mgmt := fakeMgmt{metas: map[string]domain.ManagementKeyMeta{
		"mk-admin":    {ID: 10, UserID: 3, Status: domain.ManagementKeyStatusActive},
		"mk-user":     {ID: 11, UserID: 4, Status: domain.ManagementKeyStatusActive},
		"mk-disabled": {ID: 12, UserID: 3, Status: domain.ManagementKeyStatusDisabled},
	}}

	for _, tc := range []struct {
		name string
		opts Options
		auth string
		want int
	}{
		{"no header", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "", 401},
		{"non-JWT garbage", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer garbage", 401},
		{"bare Bearer", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer", 401},
		// "Bearer "（尾空）在 httptest 直达头值（无 textproto 修剪）下，若无守卫会
		// 等于 "Bearer "+""——守卫为回归点（h2 下真实存在，见 middleware.go 注释）
		{"Bearer empty value", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer ", 401},
		{"non-bearer scheme", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Basic xyz", 401},
		// 快照 role 覆盖 claims.Role——user 1 快照 platform_admin 才放行
		{"platform_admin JWT", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer " + adminTok, 200},
		{"user JWT", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer " + userTok, 401},
		// 管理 key：owner 快照 role 判定
		{"mk platform_admin owner", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer mk-admin", 200},
		{"mk user owner (降权)", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer mk-user", 401},
		{"mk disabled", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer mk-disabled", 401},
		{"mk unknown no fallback", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, mgmt}}, "Bearer mk-ghost", 401},
		{"mk prefix but mgmt provider missing (merged fake, mgmt=false)", Options{JWTIssuer: iss, Auth: fakeSnapshot{snaps, fakeMgmt{}}}, "Bearer mk-admin", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.AdminHandler = admin
			s := NewServer(tc.opts)
			req := httptest.NewRequest(http.MethodGet, "/api/admin/groups", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}
