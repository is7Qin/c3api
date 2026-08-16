// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// #17 评审 I-2 补锚：AddQuotaUsed（单条 CASE 批量更新）真实 PG 单测——多 key
// 批量增量、已删 key 静默跳过、零增量跳过、断言累加值与 updated_at 更新。
// 此前该语句仅靠 e2e 间接覆盖（TestBillingE2E 冲突路径依赖 flush 周期巧合）；
// 既有 TestPGKeyLifecycle 只锚单 key 增量 + 缺失 key 跳过，未断言 updated_at。
// 统计侧冲突累加由 statfix 的 TestPGStatUpsertConflictAccumulates 覆盖。
// 基座约定同 pg_stat_test：newPGRepos 每测试重建 schema（本包 PG 测试串行，
// 无表级冲突）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestPGAddQuotaUsedBatch(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "quota-batch@example.com")
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "qg", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)

	mk := func(name string, used int64) *domain.Key {
		k, err := repos.CreateKey(ctx, &domain.Key{
			UserID: u.ID, GroupID: g.ID, Name: name,
			KeyRaw: "ck-" + name,
			Status: domain.KeyStatusActive, Quota: 1000, QuotaUsed: used,
		})
		require.NoError(t, err)
		return k
	}
	k1 := mk("k1", 10)
	k2 := mk("k2", 20)
	k3 := mk("k3", 30)   // 零增量：无回写价值，跳过（不落 SQL）
	del := mk("del", 40) // 已软删 key：行保留（审计），回写仍生效；缺失 key 才跳过
	require.NoError(t, repos.DeleteKey(ctx, del.ID))

	first, err := repos.GetKey(ctx, k1.ID)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond) // updated_at 更新断言的时间差

	// 多 key 单条 CASE 批量：k1+5、k2+7、k3+0（跳过）、软删 del+3（行在 → 回写）
	require.NoError(t, repos.Keys.AddQuotaUsed(ctx, map[int64]int64{k1.ID: 5, k2.ID: 7, k3.ID: 0, del.ID: 3, 999999: 9}))

	got1, err := repos.GetKey(ctx, k1.ID)
	require.NoError(t, err)
	require.Equal(t, int64(15), got1.QuotaUsed, "批量累加（10+5）")
	require.True(t, got1.UpdatedAt.After(first.UpdatedAt), "updated_at 随批量更新")

	got2, err := repos.GetKey(ctx, k2.ID)
	require.NoError(t, err)
	require.Equal(t, int64(27), got2.QuotaUsed, "批量累加（20+7）")

	got3, err := repos.GetKey(ctx, k3.ID)
	require.NoError(t, err)
	require.Equal(t, int64(30), got3.QuotaUsed, "零增量跳过（+0 不落 SQL）")

	gotDel, err := repos.GetKey(ctx, del.ID)
	require.NoError(t, err)
	require.Equal(t, int64(43), gotDel.QuotaUsed, "软删 key 行保留 → 回写生效（审计保留语义）")
	require.NotNil(t, gotDel.DeletedAt, "软删 key 带 deleted_at")
}

// TestPGKeyQuotaUsed #14 T3b 预算复核点读锚：QuotaUsed 单列读返回 DB 权威值
//（与 GetKey 全行读同源）；AddQuotaUsed 增量后复核读到新值（复核协议依赖：
// 复核时刻 SELECT quota_used 是"剩余额"判定依据）；缺失 key → ErrNotFound。
func TestPGKeyQuotaUsed(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "quota-reclaim@example.com")
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "qg2", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	k, err := repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "rk",
		KeyRaw: "ck-reclaim",
		Status: domain.KeyStatusActive, Quota: 1000, QuotaUsed: 40,
	})
	require.NoError(t, err)

	used, err := repos.Keys.QuotaUsed(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, int64(40), used, "复核读到 DB 权威值")

	// 增量回写后复核读到新值（usage 落库面 → 复核判定面一致）
	require.NoError(t, repos.Keys.AddQuotaUsed(ctx, map[int64]int64{k.ID: 25}))
	used, err = repos.Keys.QuotaUsed(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, int64(65), used, "复核读到增量后新值")

	_, err = repos.Keys.QuotaUsed(ctx, 999999)
	require.Error(t, err, "缺失 key → 复核读失败（gate 按 DB 错策略处理）")
}
