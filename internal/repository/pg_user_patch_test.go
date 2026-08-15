// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGUpdateUserPatchConditional A-P1-1 patch 条件写语义（真实 PG）：
// 显式字段才写（role 只改 role 不触碰 balance）；balance/max_concurrency
// 带旧值条件——旧值不满足（期间有扣费/并发变更）→ ErrConflict 不覆盖；
// 缺失用户 → ErrNotFound；旧值缺失 → 显式拒绝（防未来调用方退回无条件写）。
func TestPGUpdateUserPatchConditional(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "patch-cond@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	t.Run("role-only patch touches nothing else", func(t *testing.T) {
		role := domain.RolePlatformAdmin
		updated, err := repos.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Role: &role})
		require.NoError(t, err)
		require.Equal(t, domain.RolePlatformAdmin, updated.Role)
		require.Equal(t, int64(100000), updated.Balance, "只改 role 不触碰 balance")
		require.Equal(t, domain.UserStatusActive, updated.Status, "只改 role 不触碰 status")
	})

	t.Run("balance with matching old succeeds", func(t *testing.T) {
		oldBal := int64(100000)
		newBal := int64(50000)
		updated, err := repos.UpdateUser(ctx, &repository.UserPatch{
			ID: u.ID, Balance: &newBal, OldBalance: &oldBal,
		})
		require.NoError(t, err)
		require.Equal(t, int64(50000), updated.Balance)
	})

	t.Run("stale old balance → ErrConflict", func(t *testing.T) {
		stale := int64(100000) // 期间有扣费：当前 50000 ≠ 快照
		newBal := int64(99999)
		_, err := repos.UpdateUser(ctx, &repository.UserPatch{
			ID: u.ID, Balance: &newBal, OldBalance: &stale,
		})
		require.ErrorIs(t, err, repository.ErrConflict, "陈旧快照条件写必须拒绝（余额复活面）")
		got, err := repos.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(50000), got.Balance, "余额不被覆盖")
	})

	t.Run("stale old max_concurrency → ErrConflict", func(t *testing.T) {
		// max_concurrency 显式条件：先原子改为 3（UpdateUserMaxConcurrency 累加）
		require.NoError(t, repos.UpdateUserMaxConcurrency(ctx, u.ID, 3))
		stale := 0
		mc := 8
		_, err := repos.UpdateUser(ctx, &repository.UserPatch{
			ID: u.ID, MaxConcurrency: &mc, OldMaxConcurrency: &stale,
		})
		require.ErrorIs(t, err, repository.ErrConflict)
		got, err := repos.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, 3, got.MaxConcurrency, "条件不满足不覆盖")
	})

	t.Run("missing user → ErrNotFound", func(t *testing.T) {
		role := domain.RoleUser
		_, err := repos.UpdateUser(ctx, &repository.UserPatch{ID: 99999, Role: &role})
		require.ErrorIs(t, err, repository.ErrNotFound)
	})

	t.Run("new value without old → explicit error", func(t *testing.T) {
		newBal := int64(1)
		_, err := repos.UpdateUser(ctx, &repository.UserPatch{ID: u.ID, Balance: &newBal})
		require.Error(t, err)
		require.Contains(t, err.Error(), "old value")
	})

	t.Run("empty patch no-op succeeds", func(t *testing.T) {
		updated, err := repos.UpdateUser(ctx, &repository.UserPatch{ID: u.ID})
		require.NoError(t, err)
		require.Equal(t, u.ID, updated.ID)
		got, err := repos.GetUser(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, int64(50000), got.Balance, "空 patch 不触碰任何列")
	})
}

// TestPGUpdateUserVsDeductAndLogInterleave v02 点名无回归网：GET 快照(100000)
// → 扣费(40000) → 陈旧快照条件写必须 ErrConflict（余额不复活、usage_logs 消费
// 与余额一致）；新鲜快照条件写成功（管理员显式意图建立在当前值上）。
func TestPGUpdateUserVsDeductAndLogInterleave(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "patch-deduct@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 100000))

	// 模拟：handler GET 后、条件写前 flusher 已扣费落账
	snapshot := int64(100000)
	_, _, err := repos.DeductAndLog(ctx, u.ID, 40000, []*domain.UsageLog{logFor(u.ID, "pre")})
	require.NoError(t, err)

	// 陈旧快照条件写 → 拒绝，余额保持扣费后值（修复前：无条件写回复活 100000）
	newBal := int64(50000)
	_, err = repos.UpdateUser(ctx, &repository.UserPatch{
		ID: u.ID, Balance: &newBal, OldBalance: &snapshot,
	})
	require.ErrorIs(t, err, repository.ErrConflict)
	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(60000), got.Balance, "100000-40000：扣费不被覆盖")

	// 新鲜快照（重读）条件写 → 成功
	fresh := got.Balance
	updated, err := repos.UpdateUser(ctx, &repository.UserPatch{
		ID: u.ID, Balance: &newBal, OldBalance: &fresh,
	})
	require.NoError(t, err)
	require.Equal(t, int64(50000), updated.Balance)
	require.Equal(t, int64(1), countLogs(t, repos, u.ID), "消费日志在（账实一致）")
}

// TestPGUpdateUserPatchConcurrentDeduct 条件写与并发扣费交错（多轮 stress）：
// 条件写要么成功（New = 重读 + 5000，建立在当前值上）要么 ErrConflict 重读
// 重试——任何交错下最终余额 = 100000 + 5000 - 20×1000（成功）或
// 100000 - 20×1000（全冲突），扣费零丢失（修复前无条件写回会吞并发增量）。
func TestPGUpdateUserPatchConcurrentDeduct(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "patch-race@example.com")
	const base, perDeduct, nDeducts = int64(100000), int64(1000), 20
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, base))

	var wg sync.WaitGroup
	wg.Add(nDeducts)
	for i := 0; i < nDeducts; i++ {
		go func(n int) {
			defer wg.Done()
			_, _, err := repos.DeductAndLog(ctx, u.ID, perDeduct,
				[]*domain.UsageLog{logFor(u.ID, fmt.Sprintf("race%d", n))})
			require.NoError(t, err)
		}(i)
	}
	// 条件写：重读重试 ≤3（对齐 service 语义），成功后退出
	ok := false
	for attempt := 0; attempt < 4; attempt++ {
		cur, err := repos.GetUser(ctx, u.ID)
		require.NoError(t, err)
		newBal := cur.Balance + 5000
		_, err = repos.UpdateUser(ctx, &repository.UserPatch{
			ID: u.ID, Balance: &newBal, OldBalance: &cur.Balance,
		})
		if err == nil {
			ok = true
			break
		}
		require.ErrorIs(t, err, repository.ErrConflict)
	}
	wg.Wait()

	got, err := repos.GetUser(ctx, u.ID)
	require.NoError(t, err)
	want := base - nDeducts*perDeduct
	if ok {
		want += 5000
	}
	require.Equal(t, want, got.Balance, "条件写成功则新值建在扣费后当前值上；两种交错下扣费均零丢失")
	require.Equal(t, int64(nDeducts), countLogs(t, repos, u.ID))
}

// TestPGUpdateKeyVsAddQuotaUsedInterleave A-P2-5 无回归网：UpdateKey 不再写
// quota_used（剥离 SetQuotaUsed）——与 AddQuotaUsed 并发交错时 Recorder 增量
// 零丢失；返回行 QuotaUsed 为 DB 新鲜值（ent Save re-SELECT）。
func TestPGUpdateKeyVsAddQuotaUsedInterleave(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "kquota@example.com")
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{Name: "kq", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	k, err := repos.CreateKey(ctx, &domain.Key{
		UserID: u.ID, GroupID: g.ID, Name: "kq", KeyRaw: "gk-kq",
		Status: domain.KeyStatusActive, MaxConcurrency: 2, Quota: 1000, QuotaUsed: 10,
	})
	require.NoError(t, err)

	// 交错：AddQuotaUsed 增量流（Recorder 节奏）+ UpdateKey 携带请求起始陈旧
	// 快照（QuotaUsed=10——修复前会覆盖增量致永久少记）
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			require.NoError(t, repos.Keys.AddQuotaUsed(ctx, map[int64]int64{k.ID: 5}))
		}
	}()
	for i := 0; i < 10; i++ {
		stale := *k // 请求起始快照（QuotaUsed=10）
		_, err := repos.UpdateKey(ctx, &stale)
		require.NoError(t, err)
	}
	wg.Wait()

	got, err := repos.GetKey(ctx, k.ID)
	require.NoError(t, err)
	require.Equal(t, int64(10+20*5), got.QuotaUsed, "增量不丢：UpdateKey 不再覆盖 quota_used")

	// 返回行 QuotaUsed = DB 新鲜值（ent Save re-SELECT → upsertKeyMeta 同步最新）
	updated, err := repos.UpdateKey(ctx, got)
	require.NoError(t, err)
	require.Equal(t, int64(110), updated.QuotaUsed, "返回行携带 DB 新鲜 quota_used")
	require.Equal(t, "kq", updated.Name)
}
