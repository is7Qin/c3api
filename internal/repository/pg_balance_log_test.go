// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGCreateBalanceLog 余额变动记录落库（真实 PostgreSQL）：user_id/amount/
// balance_after/source/operator_id/note 逐字段回读；note nil 可选列不落值。
func TestPGCreateBalanceLog(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	uid := seedPGUser(t, repos, "bal@example.com").ID
	note := "ABC-1234"
	require.NoError(t, repos.CreateBalanceLog(ctx, &domain.BalanceLog{
		UserID: uid, Amount: 12345, BalanceAfter: 12345,
		Source: domain.BalanceSourceRedemption, OperatorID: 7, Note: &note,
	}))
	require.NoError(t, repos.CreateBalanceLog(ctx, &domain.BalanceLog{
		UserID: uid, Amount: -500, BalanceAfter: 11845,
		Source: domain.BalanceSourceAdminAdjust, OperatorID: 0, Note: nil,
	}))

	rows, total, err := repos.ListUserBalanceLogs(ctx, uid, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, rows, 2)
	// 缺省 ORDER BY id DESC：后插入的在前。
	require.Equal(t, int64(-500), rows[0].Amount)
	require.Equal(t, domain.BalanceSourceAdminAdjust, rows[0].Source)
	require.Nil(t, rows[0].Note)
	require.Equal(t, int64(12345), rows[1].Amount)
	require.Equal(t, domain.BalanceSourceRedemption, rows[1].Source)
	require.Equal(t, int64(7), rows[1].OperatorID)
	require.NotNil(t, rows[1].Note)
	require.Equal(t, "ABC-1234", *rows[1].Note)
	require.Equal(t, uid, rows[1].UserID)
}

// TestPGListUserBalanceLogs 分页/排序/total：
//   - limit/offset 归一（≤0→20、<0→0）；
//   - WHERE user_id 过滤（不串用户）；
//   - sort 白名单（非法 → ErrInvalidSort）。
func TestPGListUserBalanceLogs(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	uid := seedPGUser(t, repos, "logs@example.com").ID
	other := seedPGUser(t, repos, "other@example.com").ID
	for i := 1; i <= 5; i++ {
		require.NoError(t, repos.CreateBalanceLog(ctx, &domain.BalanceLog{
			UserID: uid, Amount: int64(i) * 100, BalanceAfter: int64(i) * 100,
			Source: domain.BalanceSourceAdminAdjust, OperatorID: 0,
		}))
	}
	// 其它用户的行不计入 total/rows。
	require.NoError(t, repos.CreateBalanceLog(ctx, &domain.BalanceLog{
		UserID: other, Amount: 999, BalanceAfter: 999, Source: domain.BalanceSourceSignupDefault,
	}))

	// 缺省分页：全部 5 行（total 与行集同条件；other 不计入）。
	rows, total, err := repos.ListUserBalanceLogs(ctx, uid, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	require.Len(t, rows, 5)

	// limit/offset 分页：id DESC 下 offset=1 limit=2 → 第 2/3 行（amount 400,300）。
	page, total, err := repos.ListUserBalanceLogs(ctx, uid, repository.ListQuery{Limit: 2, Offset: 1})
	require.NoError(t, err)
	require.Equal(t, int64(5), total)
	require.Len(t, page, 2)
	require.Equal(t, int64(400), page[0].Amount)
	require.Equal(t, int64(300), page[1].Amount)

	// Limit<=0 → 归一 20；Offset<0 → 归一 0。
	all, _, err := repos.ListUserBalanceLogs(ctx, uid, repository.ListQuery{Limit: -1, Offset: -5})
	require.NoError(t, err)
	require.Len(t, all, 5, "limit<=0→20、offset<0→0")

	// sort=amount asc：升序。
	asc, _, err := repos.ListUserBalanceLogs(ctx, uid, repository.ListQuery{Sort: "amount", Order: "asc"})
	require.NoError(t, err)
	require.Len(t, asc, 5)
	require.Equal(t, int64(100), asc[0].Amount)
	require.Equal(t, int64(500), asc[4].Amount)

	// 白名单外 sort → ErrInvalidSort。
	_, _, err = repos.ListUserBalanceLogs(ctx, uid, repository.ListQuery{Sort: "note"})
	require.ErrorIs(t, err, repository.ErrInvalidSort)
}

// TestPGBalanceLogIndexExists 建表 + (user_id,id) 索引存在（A1）。
func TestPGBalanceLogIndexExists(t *testing.T) {
	newPGReposShared(t) // 确保 schema 与表已建立
	ctx := context.Background()
	pool := pgSharedPool(t)

	var relname string
	require.NoError(t, pool.QueryRow(ctx, `SELECT relname FROM pg_class WHERE oid = 'balance_logs'::regclass`).Scan(&relname))
	require.Equal(t, "balance_logs", relname, "表存在")

	var indexdef string
	err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'balance_logs' AND indexname = 'balancelog_user_id_id'`).Scan(&indexdef)
	require.NoError(t, err, "(user_id,id) 索引存在")
	require.Contains(t, indexdef, "user_id", "索引覆盖 user_id")
	require.Contains(t, indexdef, "id", "索引覆盖 id")
}
