// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// /api/admin/keys 管理端全量列表查询真实 PG 单测（spec 2026-08-16 用户规格）：
// ListKeys 三筛选参数（name 模糊/user_id/group_id 等值）AND 组合 + 软删过滤 +
// 分页 total 不裁剪 + sort 白名单 id/name/created_at（非法 → ErrInvalidSort）
// + order 非法 → error。基座同 pg_key_test（newPGRepos 每测试重建 schema）。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestPGListKeysAdmin(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u1 := seedPGUser(t, repos, "keys-admin-1@example.com")
	u2 := seedPGUser(t, repos, "keys-admin-2@example.com")
	g1, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "akg1", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	g2, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "akg2", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)

	mk := func(userID, groupID int64, name string) *domain.Key {
		k, err := repos.CreateKey(ctx, &domain.Key{
			UserID: userID, GroupID: groupID, Name: name,
			KeyRaw: "sk-admin-" + name,
			Status: domain.KeyStatusActive, Quota: 1000, QuotaUsed: 5,
		})
		require.NoError(t, err)
		return k
	}
	k1 := mk(u1.ID, g1.ID, "alpha")
	mk(u1.ID, g2.ID, "beta-test")
	mk(u2.ID, g1.ID, "gamma-test")
	del := mk(u2.ID, g2.ID, "delta-test")
	require.NoError(t, repos.DeleteKey(ctx, del.ID)) // 软删：行保留，列表过滤

	// 全量：软删过滤 → 3 行；count 同谓词（total 不含已删）
	rows, total, err := repos.ListKeys(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, rows, 3)

	// name 模糊（NameContainsFold = ILIKE '%name%'，不区分大小写）
	rows, total, err = repos.ListKeys(ctx, repository.ListQuery{Name: "TEST"})
	require.NoError(t, err)
	require.Equal(t, int64(2), total, "name=TEST → beta-test/gamma-test（delta 已软删）")
	for _, k := range rows {
		require.Contains(t, k.Name, "test", "模糊命中（大小写不敏感）")
	}

	// user_id 等值收窄
	rows, total, err = repos.ListKeys(ctx, repository.ListQuery{UserID: u1.ID})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	for _, k := range rows {
		require.Equal(t, u1.ID, k.UserID)
	}

	// group_id 等值收窄
	rows, total, err = repos.ListKeys(ctx, repository.ListQuery{GroupID: g1.ID})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	for _, k := range rows {
		require.Equal(t, g1.ID, k.GroupID)
	}

	// AND 组合：name + user_id + group_id → 唯一命中 beta-test
	rows, total, err = repos.ListKeys(ctx, repository.ListQuery{Name: "test", UserID: u1.ID, GroupID: g2.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	require.Equal(t, "beta-test", rows[0].Name)

	// 零值不过滤：user_id=0 / group_id=0 等同不传（全量 3）
	rows, total, err = repos.ListKeys(ctx, repository.ListQuery{UserID: 0, GroupID: 0})
	require.NoError(t, err)
	require.Equal(t, int64(3), total)

	// 分页：limit=2 → rows 2、total 恒 3（count 不裁剪）；offset 翻页
	rows, total, err = repos.ListKeys(ctx, repository.ListQuery{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, int64(3), total, "total = 满足筛选总数，不分页裁剪")
	require.Len(t, rows, 2)
	rows, total, err = repos.ListKeys(ctx, repository.ListQuery{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, rows, 1)

	// sort 白名单三键可用（缺省 sort=id + 空 order → desc——id 倒序）
	rows, _, err = repos.ListKeys(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, k1.ID+2, rows[0].ID, "缺省 id desc → 最新在前")
	rows, _, err = repos.ListKeys(ctx, repository.ListQuery{Sort: "name", Order: "asc"})
	require.NoError(t, err)
	require.Equal(t, "alpha", rows[0].Name, "sort=name asc → 字典序首行")
	_, _, err = repos.ListKeys(ctx, repository.ListQuery{Sort: "created_at"})
	require.NoError(t, err, "sort=created_at 白名单键可用")

	// sort 白名单外（用户端 8 键含 status/quota，管理端 3 键白名单不含）→ ErrInvalidSort
	for _, s := range []string{"status", "quota", "quota_used", "updated_at", "max_concurrency"} {
		_, _, err = repos.ListKeys(ctx, repository.ListQuery{Sort: s})
		require.ErrorIs(t, err, repository.ErrInvalidSort, "sort=%s 必须在 id/name/created_at 白名单内", s)
	}

	// order 非法 → error
	_, _, err = repos.ListKeys(ctx, repository.ListQuery{Order: "sideways"})
	require.Error(t, err, "order 非法必须报错")
}
