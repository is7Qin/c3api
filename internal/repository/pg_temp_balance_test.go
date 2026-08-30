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
)

// TestPGCreateTempBalance 临时额度落库（真实 PostgreSQL，评审 B1 基座）：
// user_id/amount/expires_at/note 逐字段回读；nil 可选列不落值（永久语义）。
func TestPGCreateTempBalance(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	u := seedPGUser(t, repos, "bonus@example.com")

	// 带到期 + note（注册赠品形态）
	expires := time.Now().AddDate(0, 0, 30).Truncate(time.Second)
	note := "signup bonus"
	err := repos.Users.CreateTempBalance(ctx, u.ID, 1000, &expires, &note)
	require.NoError(t, err)

	rows, err := repos.Client.TempBalance.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	tb := rows[0]
	require.Equal(t, u.ID, tb.UserID)
	require.Equal(t, int64(1000), tb.Amount)
	require.NotNil(t, tb.ExpiresAt)
	require.WithinDuration(t, expires, *tb.ExpiresAt, time.Second, "expires_at 落库")
	require.NotNil(t, tb.Note)
	require.Equal(t, "signup bonus", *tb.Note)

	// 用户过滤查询（Phase 5 FEFO 扣费的数据源形态）
	byUser, err := repos.Client.TempBalance.Query().
		Where(tempbalance.UserIDEQ(u.ID)).
		Order(tempbalance.ByExpiresAt()).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, byUser, 1)
	require.Equal(t, int64(1000), byUser[0].Amount)

	// nil 可选列：expires_at/note 不落值（nil = 永久）
	u2 := seedPGUser(t, repos, "perm@example.com")
	err = repos.CreateTempBalance(ctx, u2.ID, 500, nil, nil)
	require.NoError(t, err)
	rows, err = repos.Client.TempBalance.Query().Where(tempbalance.UserIDEQ(u2.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Nil(t, rows[0].ExpiresAt, "expires_at 未提供 → NULL")
	require.Nil(t, rows[0].Note, "note 未提供 → NULL")

	// 门面委托等价（repos.CreateTempBalance == repos.Users.CreateTempBalance）
	err = repos.CreateTempBalance(ctx, u.ID, 1, nil, nil)
	require.NoError(t, err)
	rows, err = repos.Client.TempBalance.Query().Where(tempbalance.UserIDEQ(u.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2, "门面委托同样落库")
}

// TestPGCreateTempBalanceForeignKey 外键约束：user_id 不存在 → 报错
// （注册流程先 CreateUser 拿 id，防误用孤立赠品行）。
func TestPGCreateTempBalanceForeignKey(t *testing.T) {
	repos := newPGReposShared(t)
	err := repos.CreateTempBalance(context.Background(), 999999, 100, nil, nil)
	require.Error(t, err, "user_id 外键不存在必须报错")
}
