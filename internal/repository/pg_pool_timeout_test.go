// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// OpenPG 连接池防卡死参数生效断言（F-P2-4，P2）：lock_timeout=5s 为会话级
// GUC——pgx DSN query 参数 → 连接启动包 → 会话生效，必须真实连接 SHOW 才可
// 证伪（非解析层自证）；MaxConnLifetime=30m 为 pgxpool 配置断言（用户裁决
// 2026-08-13 实施，滚动轮换）。statement_timeout 断言保持 PG 默认 0 = 降级
// 回归锚：session 级 statement_timeout 与 admin 面 ScanStats 大窗口聚合实测
// 冲突（57014，见 f1-impl-report.md）→ 不设会话级，计费路径执行时长由
// 结算语句 per-query 10s ctx 超时兜底（settleTimeout，
// TestPGBillingDeductLockTimeout）。纯读测试（SHOW），不动表/schema——与既有
// PG 测试共享测试库串行无冲突。基座约定同 pg_account_groups_test：
// TEST_DATABASE_URL 未设置 → t.Skip。

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

func TestPGOpenPGTimeoutParams(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 2)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// 用户 DSN 未显式含超时参数 → OpenPG 统一补丁生效（会话级 GUC，SHOW 实证）
	var st, lt string
	require.NoError(t, pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&st))
	require.NoError(t, pool.QueryRow(ctx, "SHOW lock_timeout").Scan(&lt))
	require.Equal(t, "5s", lt, "lock_timeout 会话级生效")
	require.Equal(t, "0", st, "statement_timeout 保持 PG 默认 0——降级形态（不设会话级，见文件头注释）")

	// MaxConnLifetime 30m 滚动轮换（pgxpool 配置断言）
	require.Equal(t, 30*time.Minute, pool.Config().MaxConnLifetime, "MaxConnLifetime 滚动轮换配置")
}

// TestPGBillingDeductLockTimeout 计费结算在锁竞争下有限失败（F-P2-4 核心验收）：
// 管理员 pg_dump/长事务持锁期间 FEFO/余额 UPDATE 曾无限卡锁等待（PG 默认
// lock_timeout=0）→ flush worker 全阻塞 → 全局计费停摆。修复后双兜底：池级
// lock_timeout=5s 会话 GUC（锁等待 5s 即 55P03 报错回滚）+ 结算语句 per-query
// 10s 超时（settleTimeout）。本测试模拟管理员长事务持有 users 行锁，断言结算
// 有限时间内失败（旧行为：无限阻塞）——失败语义 = 行保持 unbilled 游标重放
//（不丢不重；毒行梯子不隔离：ctx 截止上抛）。
func TestPGBillingDeductLockTimeout(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "lock-contention@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	// 种子 unbilled 行先行（F2 单写点：usage flusher 落库 → 游标消费）
	seedUnbilled(t, repos, fullLogFor(u.ID, "lock-contention-req"))
	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)

	// 模拟管理员长事务：UPDATE 未提交 → users 行锁保持
	holder, err := pgTestPool(t).Acquire(ctx)
	require.NoError(t, err)
	defer holder.Release()
	_, err = holder.Exec(ctx, "BEGIN")
	require.NoError(t, err)
	_, err = holder.Exec(ctx, "UPDATE users SET balance = balance WHERE id = $1", u.ID)
	require.NoError(t, err)
	defer func() { _, _ = holder.Exec(ctx, "ROLLBACK") }() // nolint:errcheck

	start := time.Now()
	_, err = repos.SettleBalanceBatch(ctx, len(rows))
	elapsed := time.Since(start)
	t.Logf("settle under lock contention: err=%v elapsed=%v", err, elapsed)
	require.Error(t, err, "锁竞争下结算必须有限失败（55P03 lock_timeout 或 ctx 截止）")
	require.Less(t, elapsed, 15*time.Second, "5s lock_timeout + 10s per-query 双兜底：≤15s（旧行为无限阻塞）")

	_, lagN, lagErr := repos.UnbilledLag(ctx)
	require.NoError(t, lagErr)
	require.Equal(t, int64(1), lagN, "事务回滚行保持 unbilled——游标下周期重放（不丢不重）")
}
