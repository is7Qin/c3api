// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// key 改组 + 组名回填的真实 PG 锚（spec §7）：UpdateKey 落 group_id；
// GetKey/ListKeysByUser/ListKeys 读路径经过滤式 eager-load 回填 GroupName；
// 组软删 → 边被过滤 → GroupName 空（不报错）。基座同 pg_key_test（每测试私有克隆）。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGUpdateKeySetsGroup UpdateKey 带 GroupID patch 落库（返回行 + 后续 GetKey
// 均为新组 id）。
func TestPGUpdateKeySetsGroup(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "key-switch@example.com")
	g1 := seedPGGroup(t, repos, "ks-g1")
	g2 := seedPGGroup(t, repos, "ks-g2")

	k, err := repos.Keys.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g1.ID, Name: "ks", KeyRaw: "ck-ks-switch",
		Status: domain.KeyStatusActive,
	})
	require.NoError(t, err)
	require.Equal(t, g1.ID, k.GroupID)

	updated, err := repos.UpdateKey(ctx, &repository.KeyPatch{ID: k.ID, GroupID: &g2.ID})
	require.NoError(t, err)
	require.Equal(t, g2.ID, updated.GroupID, "UpdateKey 返回行落新组")
	require.Empty(t, updated.GroupName, "写路径行无组边——组名由 service 层回填（此处恒空）")

	got, err := repos.GetKey(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, g2.ID, got.GroupID, "持久化新组（后续读一致）")
}

// TestPGKeyGroupNameFill 读路径组名回填：GetKey/ListKeysByUser/ListKeys 行经存活
// 过滤 eager-load 回填 GroupName；组软删 → 过滤 → GroupName 空（不 500）；
// GroupID 始终回显。
func TestPGKeyGroupNameFill(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "key-gname@example.com")
	g := seedPGGroup(t, repos, "kg-name")

	k, err := repos.Keys.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "kg", KeyRaw: "ck-kg-name",
		Status: domain.KeyStatusActive,
	})
	require.NoError(t, err)

	// GetKey：存活组 → GroupName 回填
	got, err := repos.GetKey(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, g.Name, got.GroupName, "GetKey 回填组名")

	// ListKeysByUser：行查询回填组名
	rows, total, err := repos.ListKeysByUser(ctx, u.ID, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	require.Equal(t, g.Name, rows[0].GroupName, "ListKeysByUser 行回填组名")

	// ListKeys（管理面）：同款行回填
	adminRows, adminTotal, err := repos.ListKeys(ctx, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), adminTotal)
	require.Len(t, adminRows, 1)
	require.Equal(t, g.Name, adminRows[0].GroupName, "ListKeys 行回填组名")

	// 组软删 → 边被过滤 → GroupName 空（不报错）；GroupID 仍回显
	require.NoError(t, repos.DeleteGroup(ctx, g.ID))
	got, err = repos.GetKey(ctx, k.ID)
	require.NoError(t, err, "组软删不影响 key 读取")
	require.Empty(t, got.GroupName, "软删组 → 过滤 → 组名空")
	require.Equal(t, g.ID, got.GroupID, "GroupID 仍回显（回退 #id 依据）")

	rows, _, err = repos.ListKeysByUser(ctx, u.ID, repository.ListQuery{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].GroupName, "列表行组名同为空")
}
