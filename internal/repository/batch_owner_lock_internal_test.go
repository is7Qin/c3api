// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// batch_owner_lock_internal_test.go N1 归属锁集合协议的**确定性**回归（真实 PG）。
// 需要设置包内测试注入点 ownerLockSetTestBarrier，故本文件用内包 package repository
// （外部的 repository_test 无法访问未导出变量）；自建独立 schema，不 DROP public。
//
// 跑法：TEST_DATABASE_URL=... go test ./internal/repository/ -run 'BatchOwnerLockSet' -count=1 -v

import (
	"context"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

const batchLockTestSchema = "batch_lock_test"

// newBatchLockTestRepo 独立 schema 上的仓库（供内包 N1 测试）。
func newBatchLockTestRepo(t *testing.T) *Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + batchLockTestSchema
	} else {
		dsn += "?search_path=" + batchLockTestSchema
	}
	ctx := context.Background()
	pool, err := OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+batchLockTestSchema+` CASCADE; CREATE SCHEMA `+batchLockTestSchema+`;`)
	require.NoError(t, err)
	repos, err := NewWithPG(context.Background(), entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	return repos
}

func seedBatchLockUser(t *testing.T, repos *Repository, email string) *domain.User {
	t.Helper()
	u, err := repos.CreateUser(context.Background(), &domain.User{
		Email: email, PasswordHash: "h-" + email, Role: domain.RoleSupplier, Status: domain.UserStatusActive,
	})
	require.NoError(t, err)
	return u
}

// TestBatchOwnerLockSetInterleave N1 确定性交错（无 sleep）：在批量写事务的
// 「读当前归属」与「锁 accounts」之间，用独立连接提交一次转属 O→T。旧协议读到旧
// owner O 后，会在**已持 accounts** 的情况下补锁最终 owner T（accounts→users 反序，
// 与「持 T 等 accounts」的禁用/转属路径构成 ABBA）；新协议发现最终 owner 不在预锁
// 集合后**整体重启**，按统一升序把 T 纳入预锁再锁 accounts，收敛且不补锁。
func TestBatchOwnerLockSetInterleave(t *testing.T) {
	repos := newBatchLockTestRepo(t)
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name: "lock-tpl", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping: domain.ModelMapping{},
	})
	require.NoError(t, err)
	owner := seedBatchLockUser(t, repos, "lock-owner@example.com")
	target := seedBatchLockUser(t, repos, "lock-target@example.com")
	acc, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "lock-acc", TemplateID: tpl.ID, UpstreamKey: "sk-l", MaxConcurrency: 8, Enabled: true, SupplierUserID: owner.ID,
	})
	require.NoError(t, err)

	var barrierFired bool
	ownerLockSetTestBarrier = func() {
		if barrierFired {
			return
		}
		barrierFired = true
		ownerLockSetTestBarrier = nil // 只触发一次：内层调用不得重入（same goroutine）
		// 与批量读交错：读归属之后、锁 accounts 之前，用独立连接提交一次转属 O→T。
		_, xerr := repos.UpdateAccountsBatch(ctx, []int64{acc.ID}, AccountPatch{SupplierUserID: &target.ID})
		require.NoError(t, xerr)
	}
	t.Cleanup(func() { ownerLockSetTestBarrier = nil })

	enabled := true
	results, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, AccountPatch{Enabled: &enabled})
	require.NoError(t, err, "转属交错下批量写必须重启收敛，不得报错/死锁")
	require.Len(t, results, 1)
	moved, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, moved.SupplierUserID, "转属应已提交")
	require.True(t, moved.Enabled, "普通配置写照常生效")
}
