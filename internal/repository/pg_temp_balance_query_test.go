// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/ent/tempbalance"
	"github.com/is7qin/c3api/internal/repository"
)

// 临时额度查询（spec 2026-08-15）：用户侧有效过滤 + FEFO 排序；管理侧全量
// 视角 + user_id 筛选 + sort/order 白名单 + 分页。真实 PostgreSQL（评审 B1
// 基座，同 pg_temp_balance_test.go）。

// seedPGTempBalanceRows 建 N 笔临时额度行（指定到期/金额/备注）；落库后回查
// 该用户行数确认一致（断言只依赖内容，不依赖 id 顺序）。
func seedPGTempBalanceRows(t *testing.T, repos *repository.Repository, userID int64, rows []struct {
	amount    int64
	expiresAt *time.Time
	note      *string
}) {
	t.Helper()
	for _, r := range rows {
		err := repos.CreateTempBalance(context.Background(), userID, r.amount, r.expiresAt, r.note)
		require.NoError(t, err)
	}
	n, err := repos.Client.TempBalance.Query().Where(tempbalance.UserIDEQ(userID)).Count(context.Background())
	require.NoError(t, err)
	require.Equal(t, len(rows), n, "该用户落库行数")
}

// TestPGListUserTempBalances 用户侧有效过滤：过期/用尽（amount=0）/负扣减行
// 隐藏，永久（expires_at NULL）保留；FEFO 排序（不同到期升序 + 永久最后）。
func TestPGListUserTempBalances(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "tb@example.com")

	now := time.Now().Truncate(time.Second)
	noteBonus := "signup bonus"
	noteRedeem := "redemption code"
	seedPGTempBalanceRows(t, repos, u.ID, []struct {
		amount    int64
		expiresAt *time.Time
		note      *string
	}{
		{amount: 300000, expiresAt: ptrTime(now.AddDate(0, 0, 5)), note: &noteBonus},    // 5 天后到期
		{amount: 150000, expiresAt: ptrTime(now.AddDate(0, 0, 2)), note: &noteRedeem},   // 2 天后到期（先扣）
		{amount: 500000, expiresAt: nil, note: &noteBonus},                              // 永久（最后）
		{amount: 400000, expiresAt: ptrTime(now.AddDate(0, 0, -1)), note: &noteBonus},   // 已过期 → 隐藏
		{amount: 0, expiresAt: ptrTime(now.AddDate(0, 0, 30)), note: &noteRedeem},       // 已用尽 → 隐藏
		{amount: -100000, expiresAt: ptrTime(now.AddDate(0, 0, 30)), note: &noteRedeem}, // 负扣减记录 → 隐藏
	})

	rows, err := repos.ListUserTempBalances(ctx, u.ID)
	require.NoError(t, err)
	require.Len(t, rows, 3, "仅有效行：过期/用尽/负扣减隐藏")
	// FEFO：expires_at 升序 + 永久最后
	require.Equal(t, int64(150000), rows[0].Amount, "2 天后到期先")
	require.Equal(t, int64(300000), rows[1].Amount, "5 天后到期次")
	require.Nil(t, rows[2].ExpiresAt, "永久最后")
	require.Equal(t, int64(500000), rows[2].Amount)

	// 其他用户（无行）→ 空列表不报错
	u2 := seedPGUser(t, repos, "tb2@example.com")
	empty, err := repos.ListUserTempBalances(ctx, u2.ID)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestPGListTempBalances 管理侧全量视角：过期/用尽/负扣减行可见；user_id 筛选；
// sort 白名单 + order；分页 total 与行集同条件；非法 sort → ErrInvalidSort。
func TestPGListTempBalances(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u1 := seedPGUser(t, repos, "adm1@example.com")
	u2 := seedPGUser(t, repos, "adm2@example.com")

	now := time.Now().Truncate(time.Second)
	note := "signup bonus"
	seedPGTempBalanceRows(t, repos, u1.ID, []struct {
		amount    int64
		expiresAt *time.Time
		note      *string
	}{
		{amount: 100000, expiresAt: ptrTime(now.AddDate(0, 0, 5)), note: &note},
		{amount: 200000, expiresAt: ptrTime(now.AddDate(0, 0, -1)), note: &note}, // 已过期——全量视角仍可见
		{amount: 0, expiresAt: ptrTime(now.AddDate(0, 0, 30)), note: &note},      // 已用尽——仍可见
		{amount: -50000, expiresAt: nil, note: &note},                            // 负扣减——仍可见
	})
	seedPGTempBalanceRows(t, repos, u2.ID, []struct {
		amount    int64
		expiresAt *time.Time
		note      *string
	}{
		{amount: 70000, expiresAt: nil, note: &note},
	})

	// 全量视角：两用户 5 行全可见，默认 expires_at asc（-1d 最早 → 30d →
	// NULLS LAST：u1 负扣减永久行后、u2 永久行最后）。
	rows, total, err := repos.ListTempBalances(ctx, repository.ListQuery{Limit: 20, Sort: "expires_at", Order: "asc"}, 0)
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	require.Len(t, rows, 5)
	require.Equal(t, int64(200000), rows[0].Amount, "过期行可见且最早到期在前")
	require.Equal(t, int64(100000), rows[1].Amount)
	require.Equal(t, int64(0), rows[2].Amount, "用尽行可见")
	require.Nil(t, rows[3].ExpiresAt)
	require.Nil(t, rows[4].ExpiresAt, "两永久行（NULLS LAST）末尾")
	require.Equal(t, int64(70000), rows[4].Amount, "u2 永久行")

	// user_id 筛选（0 = 全部；>0 = 单用户）
	rows, total, err = repos.ListTempBalances(ctx, repository.ListQuery{Limit: 20, Sort: "expires_at", Order: "asc"}, u2.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	require.Equal(t, u2.ID, rows[0].UserID)

	// sort 白名单 + order：amount desc
	rows, total, err = repos.ListTempBalances(ctx, repository.ListQuery{Limit: 20, Sort: "amount", Order: "desc"}, 0)
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	require.Equal(t, int64(200000), rows[0].Amount, "amount desc 最大在前")

	// created_at 白名单键可用
	_, _, err = repos.ListTempBalances(ctx, repository.ListQuery{Limit: 20, Sort: "created_at", Order: "desc"}, 0)
	require.NoError(t, err)

	// 非法 sort → ErrInvalidSort；非法 order → 错误
	_, _, err = repos.ListTempBalances(ctx, repository.ListQuery{Limit: 20, Sort: "id"}, 0)
	require.ErrorIs(t, err, repository.ErrInvalidSort, "白名单不含 id")
	_, _, err = repos.ListTempBalances(ctx, repository.ListQuery{Limit: 20, Sort: "amount", Order: "sideways"}, 0)
	require.Error(t, err)

	// 分页：page_size=2 → 行集 2、total 恒 5（满足筛选总数）
	rows, total, err = repos.ListTempBalances(ctx, repository.ListQuery{Limit: 2, Offset: 2, Sort: "expires_at", Order: "asc"}, 0)
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	require.Len(t, rows, 2)
	require.Equal(t, int64(0), rows[0].Amount, "偏移 2 后首行 = 用尽行")
}
