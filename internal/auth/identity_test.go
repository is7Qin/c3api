// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// fakeMgmt 管理 key 快照 provider（RequireIdentity mk- 路径用例）：仅 status==active
// 命中，模拟 proxy.Auth.AuthenticateManagement 的禁用过滤。
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

// fakeSnapshot 合并鉴权快照 provider（users + mgmt，spec 2026-10-09 §4.3）：
// RequireIdentity 现取单一 SnapshotProvider。嵌入两 fake 即同时满足两接口。
type fakeSnapshot struct {
	users fakeUserStatus
	mgmt  fakeMgmt
}

func (f fakeSnapshot) UserSnapshot(id int64) (domain.UserSnapshot, bool) {
	return f.users.UserSnapshot(id)
}

func (f fakeSnapshot) AuthenticateManagement(r *http.Request) (domain.ManagementKeyMeta, bool) {
	return f.mgmt.AuthenticateManagement(r)
}

type identityResult struct {
	code     int
	userID   int64
	role     string
	fundsOK  bool
	fundsVer int64
	fundsUID int64
}

func runIdentity(t *testing.T, iss *Issuer, p SnapshotProvider, auth string) identityResult {
	t.Helper()
	var res identityResult
	h := RequireIdentity(iss, p)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := ClaimsFrom(r.Context()); ok {
			res.userID, res.role = c.UserID, c.Role
		}
		if a, ok := domain.FundsActorFrom(r.Context()); ok {
			res.fundsOK, res.fundsUID, res.fundsVer = true, a.UserID, a.TokenVersion
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/user/keys", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res.code = rec.Code
	return res
}

// TestRequireIdentityJWTRegression JWT 路径回归 + FundsActor 注入（owner uid/ver）。
func TestRequireIdentityJWTRegression(t *testing.T) {
	iss := NewIssuer("s")
	tok, err := iss.Issue(7, "u@example.com", string(domain.RoleSupplier), 3)
	require.NoError(t, err)
	users := fakeUserStatus{snapshots: map[int64]domain.UserSnapshot{
		7: {Status: domain.UserStatusActive, Role: domain.RoleSupplier, TokenVersion: 3},
	}}

	res := runIdentity(t, iss, fakeSnapshot{users, fakeMgmt{}}, "Bearer "+tok)
	require.Equal(t, http.StatusOK, res.code)
	require.Equal(t, int64(7), res.userID)
	require.Equal(t, string(domain.RoleSupplier), res.role)
	require.True(t, res.fundsOK)
	require.Equal(t, int64(7), res.fundsUID)
	require.Equal(t, int64(3), res.fundsVer)

	// ver 不匹配（改密撤销）→ 401。
	users.snapshots[7] = domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier, TokenVersion: 4}
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, fakeSnapshot{users, fakeMgmt{}}, "Bearer "+tok).code)
}

// TestRequireIdentityManagementKey mk- 路径：owner 快照 role/ver 注入 claims +
// FundsActor；owner 禁用 → 401；key 禁用/未知 → 401；非 mk- 不误入。
func TestRequireIdentityManagementKey(t *testing.T) {
	iss := NewIssuer("s")
	users := fakeUserStatus{snapshots: map[int64]domain.UserSnapshot{
		11: {Status: domain.UserStatusActive, Role: domain.RolePlatformAdmin, TokenVersion: 5},
		12: {Status: domain.UserStatusDisabled, Role: domain.RoleUser, TokenVersion: 0},
	}}
	mgmt := fakeMgmt{metas: map[string]domain.ManagementKeyMeta{
		"mk-plat": {ID: 1, UserID: 11, Status: domain.ManagementKeyStatusActive},
		"mk-off":  {ID: 2, UserID: 11, Status: domain.ManagementKeyStatusDisabled},
		"mk-dis":  {ID: 3, UserID: 12, Status: domain.ManagementKeyStatusActive},
	}}

	// 命中：claims.Ver = owner 快照当前 token_version；Role = 快照 role。
	res := runIdentity(t, iss, fakeSnapshot{users, mgmt}, "Bearer mk-plat")
	require.Equal(t, http.StatusOK, res.code)
	require.Equal(t, int64(11), res.userID)
	require.Equal(t, string(domain.RolePlatformAdmin), res.role)
	require.True(t, res.fundsOK)
	require.Equal(t, int64(11), res.fundsUID)
	require.Equal(t, int64(5), res.fundsVer)

	// key 禁用 → 401。
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, fakeSnapshot{users, mgmt}, "Bearer mk-off").code)
	// 未知 mk- → 401（前缀先判、失败不回退 JWT）。
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, fakeSnapshot{users, mgmt}, "Bearer mk-ghost").code)
	// owner 禁用 → 401（fail-closed）。
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, fakeSnapshot{users, mgmt}, "Bearer mk-dis").code)
	// 非 mk- 的 Bearer（非 JWT 垃圾串）→ 走 JWT 校验 → 401。
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, fakeSnapshot{users, mgmt}, "Bearer garbage").code)
	// 无 Authorization → 401。
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, fakeSnapshot{users, mgmt}, "").code)
}

// TestRequireIdentityNilProviderFailsClosed 未装配 provider（nil）时 mk- Bearer
// 直接 401（fail-closed，不 panic）。注：“前缀先判、失败不回退 JWT”由结构保证
// （JWT 不可能以 mk- 开头），故此用例只证明 nil provider 的 fail-closed 行为。
func TestRequireIdentityNilProviderFailsClosed(t *testing.T) {
	iss := NewIssuer("s")
	// 未注入 provider（nil）→ mk- 直接 401（不 panic、不回落）。
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, nil, "Bearer mk-anything").code)
}

// TestRequireIdentityNilProvidersFailClosed 必需 provider 缺失 ⇒ 401 且不 panic
// （评审 MAJOR：nil interface/pointer 解引用会 panic）。合并 SnapshotProvider 后
// 「users/mgmt 缺失」等价于整个 provider nil；iss 仅 JWT 分支需（nil ⇒ JWT 401）；
// provider 非 nil 但 mgmt 恒 false 时，JWT 分支仍放行、mk- 分支 401。
func TestRequireIdentityNilProvidersFailClosed(t *testing.T) {
	iss := NewIssuer("s")
	tok, err := iss.Issue(7, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	users := fakeUserStatus{snapshots: map[int64]domain.UserSnapshot{
		7: {Status: domain.UserStatusActive, Role: domain.RoleUser},
	}}
	mgmt := fakeMgmt{metas: map[string]domain.ManagementKeyMeta{
		"mk-1": {ID: 1, UserID: 7, Status: domain.ManagementKeyStatusActive},
	}}

	// users 缺失 → JWT 与 mk- 两条分支均 401，不 panic。
	require.NotPanics(t, func() {
		require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, nil, "Bearer "+tok).code)
		require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, nil, "Bearer mk-1").code)
	})
	// iss 缺失 → 非 mk-（JWT）分支 401，不 panic。
	require.NotPanics(t, func() {
		require.Equal(t, http.StatusUnauthorized, runIdentity(t, nil, fakeSnapshot{users, mgmt}, "Bearer "+tok).code)
	})
	// mgmt 缺失：JWT 分支仍正常放行（仅 mk- 分支需要 mgmt）；mk- 分支 401。
	require.Equal(t, http.StatusOK, runIdentity(t, iss, fakeSnapshot{users, fakeMgmt{}}, "Bearer "+tok).code)
	require.Equal(t, http.StatusUnauthorized, runIdentity(t, iss, fakeSnapshot{users, fakeMgmt{}}, "Bearer mk-1").code)
}
