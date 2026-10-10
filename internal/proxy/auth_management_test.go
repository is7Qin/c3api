// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type fakeMgmtLoader struct {
	m map[string]domain.ManagementKeyMeta
}

func (f fakeMgmtLoader) LoadManagementKeys(context.Context) (map[string]domain.ManagementKeyMeta, error) {
	return f.m, nil
}

func mgmtReq(auth string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/user/keys", nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

// TestAuthenticateManagementPrefixFirst 前缀先判、失败不回退：仅 Bearer mk-…
// 进管理 key 表；非 mk-（含 JWT 字节/裸串）恒 false；查表失败/禁用亦 false。
func TestAuthenticateManagementPrefixFirst(t *testing.T) {
	a := NewAuth(nil, nil, nil, nil, true)
	a.UpsertManagementKey("mk-abc", domain.ManagementKeyMeta{ID: 1, UserID: 7, Status: domain.ManagementKeyStatusActive})
	a.UpsertManagementKey("mk-off", domain.ManagementKeyMeta{ID: 2, UserID: 8, Status: domain.ManagementKeyStatusDisabled})

	// 命中（active）。
	meta, ok := a.AuthenticateManagement(mgmtReq("Bearer mk-abc"))
	require.True(t, ok)
	require.Equal(t, int64(7), meta.UserID)

	// 前缀不匹配：JWT 形 / 裸串 / 无前缀 → false（不回落 JWT）。
	_, ok = a.AuthenticateManagement(mgmtReq("Bearer eyJhbGciOi"))
	require.False(t, ok)
	_, ok = a.AuthenticateManagement(mgmtReq("Bearer ck-abc"))
	require.False(t, ok)
	_, ok = a.AuthenticateManagement(mgmtReq("mk-abc")) // 非 Bearer
	require.False(t, ok)
	_, ok = a.AuthenticateManagement(mgmtReq(""))
	require.False(t, ok)

	// 禁用 key → false（即使前缀匹配）。
	_, ok = a.AuthenticateManagement(mgmtReq("Bearer mk-off"))
	require.False(t, ok)

	// 未知 mk- → false。
	_, ok = a.AuthenticateManagement(mgmtReq("Bearer mk-ghost"))
	require.False(t, ok)
}

// TestManagementKeyUpsertDeleteImmediate 本实例增量：Upsert 即时可用，删除即时
// 失效（跨实例经 Reload 收敛）。
func TestManagementKeyUpsertDeleteImmediate(t *testing.T) {
	a := NewAuth(nil, nil, nil, nil, true)
	a.UpsertManagementKey("mk-x", domain.ManagementKeyMeta{ID: 1, UserID: 5, Status: domain.ManagementKeyStatusActive})
	_, ok := a.AuthenticateManagement(mgmtReq("Bearer mk-x"))
	require.True(t, ok)

	// 同 raw Upsert 覆盖为 disabled → 即时 401。
	a.UpsertManagementKey("mk-x", domain.ManagementKeyMeta{ID: 1, UserID: 5, Status: domain.ManagementKeyStatusDisabled})
	_, ok = a.AuthenticateManagement(mgmtReq("Bearer mk-x"))
	require.False(t, ok)

	a.DeleteManagementKey("mk-x")
	_, ok = a.AuthenticateManagement(mgmtReq("Bearer mk-x"))
	require.False(t, ok)
}

// TestReloadLoadsManagementKeys NewAuth 构造参数注入 mgmt loader 后 Reload 装载 mgmt
// 快照（同一把锁整体换引用）。
func TestReloadLoadsManagementKeys(t *testing.T) {
	a := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{}}, noopUserLoader{users: map[int64]domain.UserSnapshot{}}, fakeMgmtLoader{m: map[string]domain.ManagementKeyMeta{
		"mk-loaded": {ID: 9, UserID: 3, Status: domain.ManagementKeyStatusActive},
	}}, nil, true)
	require.NoError(t, a.Reload(context.Background()))
	meta, ok := a.AuthenticateManagement(mgmtReq("Bearer mk-loaded"))
	require.True(t, ok)
	require.Equal(t, int64(3), meta.UserID)
}

// gatedMgmtLoader 门控 loader：读取快照后经 read 放信号，阻塞在 release 上——
// 让测试精确插桩在"Reload 已从 DB 读、尚未换引用"的窗口内执行本地变更（确定性
// 复现，非概率）。m 在 LoadManagementKeys 内拷贝一次，避免与测试共享/并发读。
type gatedMgmtLoader struct {
	m       map[string]domain.ManagementKeyMeta
	read    chan struct{}
	release chan struct{}
}

func (g *gatedMgmtLoader) LoadManagementKeys(context.Context) (map[string]domain.ManagementKeyMeta, error) {
	out := make(map[string]domain.ManagementKeyMeta, len(g.m))
	for k, v := range g.m {
		out[k] = v
	}
	close(g.read) // 已读 DB 快照（Reload 随后才拿 a.mu）
	<-g.release   // 等测试完成本地变更后放行
	return out, nil
}

// TestReloadDoesNotResurrectDeletedManagementKey 复现 MAJOR-1（A4）：Reload 在
// DB 读与换引用之间，本地 DeleteManagementKey 即时生效；若 Reload 仍把"变更前
// 读取的" active 快照换回，已删 key 会被复活为可用凭据。修复（生成计数器）后：
// 本地变更赢，live map 保留 → 删除即时、不被过期 reload 覆盖。
func TestReloadDoesNotResurrectDeletedManagementKey(t *testing.T) {
	loader := &gatedMgmtLoader{
		m:       map[string]domain.ManagementKeyMeta{"mk-live": {ID: 1, UserID: 5, Status: domain.ManagementKeyStatusActive}},
		read:    make(chan struct{}),
		release: make(chan struct{}),
	}
	a := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{}}, noopUserLoader{users: map[int64]domain.UserSnapshot{}}, loader, nil, true)
	// live map 先持有一条 active key（模拟上一轮快照）。
	a.UpsertManagementKey("mk-live", domain.ManagementKeyMeta{ID: 1, UserID: 5, Status: domain.ManagementKeyStatusActive})
	require.True(t, mustAuthMgmt(t, a, "Bearer mk-live"))

	done := make(chan error, 1)
	go func() { done <- a.Reload(context.Background()) }()

	<-loader.read // Reload 已读 DB（含 active 行），尚未换引用
	a.DeleteManagementKey("mk-live")
	require.False(t, mustAuthMgmt(t, a, "Bearer mk-live"), "本地删除须即时生效")
	close(loader.release) // 放行 Reload 完成换引用
	require.NoError(t, <-done)

	_, ok := a.AuthenticateManagement(mgmtReq("Bearer mk-live"))
	require.False(t, ok, "已删 key 不得被过期 reload 复活（A4）")
}

// TestReloadDoesNotResurrectDisabledManagementKey 同上，禁用（status→disabled）
// 变体：本地禁用后，过期 reload 不得把它恢复为 active。
func TestReloadDoesNotResurrectDisabledManagementKey(t *testing.T) {
	loader := &gatedMgmtLoader{
		m:       map[string]domain.ManagementKeyMeta{"mk-live": {ID: 1, UserID: 5, Status: domain.ManagementKeyStatusActive}},
		read:    make(chan struct{}),
		release: make(chan struct{}),
	}
	a := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{}}, noopUserLoader{users: map[int64]domain.UserSnapshot{}}, loader, nil, true)
	a.UpsertManagementKey("mk-live", domain.ManagementKeyMeta{ID: 1, UserID: 5, Status: domain.ManagementKeyStatusActive})
	require.True(t, mustAuthMgmt(t, a, "Bearer mk-live"))

	done := make(chan error, 1)
	go func() { done <- a.Reload(context.Background()) }()

	<-loader.read
	// 本地禁用（同 raw Upsert 覆盖 status）。
	a.UpsertManagementKey("mk-live", domain.ManagementKeyMeta{ID: 1, UserID: 5, Status: domain.ManagementKeyStatusDisabled})
	require.False(t, mustAuthMgmt(t, a, "Bearer mk-live"), "本地禁用须即时生效")
	close(loader.release)
	require.NoError(t, <-done)

	_, ok := a.AuthenticateManagement(mgmtReq("Bearer mk-live"))
	require.False(t, ok, "已禁用 key 不得被过期 reload 复活（A4）")
}

func mustAuthMgmt(t *testing.T, a *Auth, auth string) bool {
	t.Helper()
	_, ok := a.AuthenticateManagement(mgmtReq(auth))
	return ok
}
