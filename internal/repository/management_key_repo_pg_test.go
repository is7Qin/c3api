// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/repository"
)

// authMgmtOK 用真实 proxy.Auth 快照判定某明文管理 key 是否可鉴权。
func authMgmtOK(a *proxy.Auth, raw string) bool {
	r := httptest.NewRequest(http.MethodGet, "/api/admin/groups", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	_, ok := a.AuthenticateManagement(r)
	return ok
}

// TestManagementKeyDeletedNotRevivablePG 回归（安全不变量 A4）：软删后 PUT 不得复活
// 该 key——UpdateManagementKey 过滤 deleted_at（已删 → ErrNotFound），且本实例快照
// 保持不可用（AuthenticateManagement false）。修复前 UpdateWhere 缺 DeletedAtIsNil，
// 已删行仍匹配 → 更新成功且被无条件 Upsert 回快照 → 已删 key 复活可用。
func TestManagementKeyDeletedNotRevivablePG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u, err := repos.CreateUser(ctx, &domain.User{Email: "mk-del@example.com", PasswordHash: "x", Role: domain.RolePlatformAdmin, Status: domain.UserStatusActive})
	require.NoError(t, err)
	k, err := repos.CreateManagementKey(ctx, &domain.ManagementKey{UserID: u.ID, Name: "n", KeyRaw: "mk-del-1", Status: domain.ManagementKeyStatusActive})
	require.NoError(t, err)

	// 真实 Auth 快照（loader = 本仓储，构造注入）→ 首载后 key 可用。
	auth := proxy.NewAuth(repos.Keys, repos.Users, repos.ManagementKeys, nil, false)
	require.NoError(t, auth.Reload(ctx))
	require.True(t, authMgmtOK(auth, "mk-del-1"), "首载后 active key 可用")

	// 软删（service 同步删除快照）→ 即时不可用。
	raw, err := repos.DeleteManagementKey(ctx, u.ID, k.ID)
	require.NoError(t, err)
	auth.DeleteManagementKey(raw)
	require.False(t, authMgmtOK(auth, "mk-del-1"), "软删后即时 401")

	// 对已删 id 执行 PUT（UpdateManagementKey）→ ErrNotFound（不得复活）。
	name := "renamed"
	_, err = repos.UpdateManagementKey(ctx, &repository.ManagementKeyPatch{UserID: u.ID, ID: k.ID, Name: &name})
	require.ErrorIs(t, err, repository.ErrNotFound, "已删 key 不可更新")
	require.False(t, authMgmtOK(auth, "mk-del-1"), "已删 key 保持不可用（不可复活）")

	// 佐证：重新 LoadManagementKeys 仍不含它。
	m, err := repos.ManagementKeys.LoadManagementKeys(ctx)
	require.NoError(t, err)
	_, ok := m["mk-del-1"]
	require.False(t, ok)
}

// TestManagementKeyRepoPG 管理 key 仓储（spec 2026-10-09）：CRUD + LoadManagementKeys
// （软删过滤）+ owner 作用域（越域 ErrNotFound）+ 事务回滚。
func TestManagementKeyRepoPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u, err := repos.CreateUser(ctx, &domain.User{Email: "mk-owner@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)

	k, err := repos.CreateManagementKey(ctx, &domain.ManagementKey{
		UserID: u.ID, Name: "n", KeyRaw: "mk-aaa", Status: domain.ManagementKeyStatusActive,
	})
	require.NoError(t, err)

	// list 仅 owner 自身。
	rows, err := repos.ListManagementKeysByUser(ctx, u.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	other, err := repos.ListManagementKeysByUser(ctx, u.ID+1)
	require.NoError(t, err)
	require.Empty(t, other)

	// LoadManagementKeys 含 active key。
	m, err := repos.ManagementKeys.LoadManagementKeys(ctx)
	require.NoError(t, err)
	_, ok := m["mk-aaa"]
	require.True(t, ok)

	// 更新为 disabled（owner 作用域）。
	dis := domain.ManagementKeyStatusDisabled
	upd, err := repos.UpdateManagementKey(ctx, &repository.ManagementKeyPatch{UserID: u.ID, ID: k.ID, Status: &dis})
	require.NoError(t, err)
	require.Equal(t, dis, upd.Status)

	// 越域更新 → ErrNotFound。
	_, err = repos.UpdateManagementKey(ctx, &repository.ManagementKeyPatch{UserID: u.ID + 999, ID: k.ID, Status: &dis})
	require.ErrorIs(t, err, repository.ErrNotFound)

	// 事务回滚：创建随外层事务回滚而回滚。
	err = repos.WithTx(ctx, func(tx repository.TxStore) error {
		_, e := tx.CreateManagementKey(ctx, &domain.ManagementKey{
			UserID: u.ID, Name: "tx", KeyRaw: "mk-tx", Status: domain.ManagementKeyStatusActive,
		})
		require.NoError(t, e)
		return errors.New("rollback")
	})
	require.Error(t, err)
	m2, err := repos.ManagementKeys.LoadManagementKeys(ctx)
	require.NoError(t, err)
	_, ok = m2["mk-tx"]
	require.False(t, ok, "事务回滚 → 行回滚（notify 快照不得含它）")

	// 软删：Delete 返回 raw，LoadManagementKeys 过滤已删。
	raw, err := repos.DeleteManagementKey(ctx, u.ID, k.ID)
	require.NoError(t, err)
	require.Equal(t, "mk-aaa", raw)
	m3, err := repos.ManagementKeys.LoadManagementKeys(ctx)
	require.NoError(t, err)
	_, ok = m3["mk-aaa"]
	require.False(t, ok, "软删过滤（deleted_at IS NULL）")

	// 越域删除 → ErrNotFound。
	_, err = repos.DeleteManagementKey(ctx, u.ID+999, k.ID)
	require.ErrorIs(t, err, repository.ErrNotFound)
}
