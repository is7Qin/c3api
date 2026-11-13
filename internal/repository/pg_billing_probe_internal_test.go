// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// P0-A 内部门禁（spec-commit-state-and-round-billing §0，G0-e/G0-g）：两处断言
// 需要触达包内符号（谓词常量本身、billingProbe），故与外部行为门禁分文件。
//
// 内部包测试不能 blank-import pgtest/pgrepo（import cycle：pgrepo → repository），
// 因此不能用 pgtest.Clone（它需要数据层钩子）；改用 pgtest.CloneEmpty + 本包
// NewWithPG(..., migrate=true) + 分区 bootstrap，等价地得到可用 schema。

import (
	"context"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// g0ProbeSeedTime 固定种子时刻（分区 bootstrap 与时区无关的 UTC 日界）。
var g0ProbeSeedTime = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

// g0InternalDB 空库 clone + ent 迁移 + usage/err 分区 bootstrap。
func g0InternalDB(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	dsn := pgtest.CloneEmpty(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() }) // LIFO：先关 *sql.DB 再关池
	repos, err := NewWithPG(context.Background(), entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)
	require.NoError(t, repos.EnsureUsageLogPartitioned(context.Background(), g0ProbeSeedTime))
	require.NoError(t, repos.EnsureErrLogPartitioned(context.Background(), g0ProbeSeedTime))
	return repos, pool
}

// g0PredicateCount 在指定表上执行谓词计数（谓词来自唯一事实源常量）。
func g0PredicateCount(t *testing.T, pool *pgxpool.Pool, table, predicate, rid string) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+table+" WHERE request_id = $1 AND "+predicate, rid).Scan(&n)
	require.NoError(t, err)
	return n
}

// TestPGInternalBillablePredicateG0eCoalesce NULL + 5xx 必须被 notBillableErrSQL
// 命中：证明 COALESCE 生效。同一断言同时跑「去掉 COALESCE」的变体并要求 0 行
// ——删除 COALESCE 后本用例必红（NULL NOT IN (...) 求值为 NULL，静默漏行）。
func TestPGInternalBillablePredicateG0eCoalesce(t *testing.T) {
	repos, pool := g0InternalDB(t)
	ctx := context.Background()

	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{{
		RequestID: "g0e-null", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
		ErrorType: domain.Err5xx, CreatedAt: g0ProbeSeedTime.Add(time.Minute),
	}}))
	row, err := repos.Client.ErrLog.Query().Where().First(ctx)
	require.NoError(t, err)
	require.Nil(t, row.CommitState, "种子行 commit_state 为 NULL（历史/未知）")

	require.Equal(t, int64(1),
		g0PredicateCount(t, pool, "err_logs", notBillableErrSQL, "g0e-null"),
		"NULL 经 COALESCE 落成空串 ⇒ 稳定命中不可计费补集")
	require.Zero(t,
		g0PredicateCount(t, pool, "err_logs", billableSQL, "g0e-null"),
		"NULL 不得被算成可计费")

	withoutCoalesce := strings.ReplaceAll(notBillableErrSQL, "COALESCE(commit_state, '')", "commit_state")
	require.NotEqual(t, notBillableErrSQL, withoutCoalesce, "变体替换必须生效（防断言空转）")
	require.Zero(t,
		g0PredicateCount(t, pool, "err_logs", withoutCoalesce, "g0e-null"),
		"去掉 COALESCE 后 NULL NOT IN (...) = NULL ⇒ 漏行（本断言即 G0-e 的红线）")
}

// TestPGInternalBillingProbeG0gDeliveredBacklog 供应商提现门禁的 backlog 必须
// 计入「已交付失败（5xx + response_started + cost>0）」——旧 error_type 白名单
// 会少算，导致门禁在仍有可计费行时放行。
func TestPGInternalBillingProbeG0gDeliveredBacklog(t *testing.T) {
	repos, pool := g0InternalDB(t)
	ctx := context.Background()

	cs := func(s string) *string { return &s }
	mk := func(rid string, et domain.ErrorType, commit *string, cost int64, at time.Time) *domain.UsageLog {
		return &domain.UsageLog{
			RequestID: rid, UserID: 42, Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			ErrorType: et, CommitState: commit, Cost: cost, CallCount: 1, CreatedAt: at,
		}
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		mk("g0g-delivered", domain.Err5xx, cs("response_started"), 130, g0ProbeSeedTime.Add(time.Minute)),
		mk("g0g-not-sent", domain.Err5xx, cs("not_sent"), 130, g0ProbeSeedTime.Add(2*time.Minute)),
		mk("g0g-null", domain.Err5xx, nil, 130, g0ProbeSeedTime.Add(3*time.Minute)),
		mk("g0g-zero", domain.ErrNone, cs("response_started"), 0, g0ProbeSeedTime.Add(4*time.Minute)),
		mk("g0g-success", domain.ErrNone, nil, 130, g0ProbeSeedTime.Add(5*time.Minute)),
	}))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	sr := &SupplierRepo{}
	probe, err := sr.billingProbe(ctx, tx)
	require.NoError(t, err)
	require.Equal(t, int64(2), probe.BacklogRows,
		"backlog = 已交付失败 + 成功行；not_sent/NULL/零价不计")
	require.NotNil(t, probe.OldestCreatedAt)
	require.Equal(t, g0ProbeSeedTime.Add(time.Minute), probe.OldestCreatedAt.UTC(),
		"队头 = 最老可计费行 created_at")
	require.False(t, probe.ObservedAt.IsZero(), "采样时刻来自 statement_timestamp()")
}

// TestPGInternalBillablePredicateG0hErrNoneExcluded err_logs 里若有
// error_type='none' 的脏行（生产写路径不产生，但历史/手工可能存在），
// 必须**不计入** err 统计源：notBillableErrSQL 强制 `error_type <> 'none'`。
// 同时断言它在 billableSQL 下命中——说明去掉 `<> 'none'` 会让它同时被
// usage 源命中（双计），这正是该子句存在的理由。
// 变异验证：删除 `error_type <> 'none' AND ` ⇒ 第一条断言 0→1，必红。
func TestPGInternalBillablePredicateG0hErrNoneExcluded(t *testing.T) {
	repos, pool := g0InternalDB(t)
	ctx := context.Background()

	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{{
		RequestID: "g0h-none", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
		ErrorType: domain.ErrNone, CreatedAt: g0ProbeSeedTime.Add(2 * time.Minute),
	}}))

	require.Zero(t,
		g0PredicateCount(t, pool, "err_logs", notBillableErrSQL, "g0h-none"),
		"error_type='none' 不得计入 err 统计源（否则与 usage 源双计）")
	require.Equal(t, int64(1),
		g0PredicateCount(t, pool, "err_logs", billableSQL, "g0h-none"),
		"同一行在 billableSQL 下命中——这就是 err 源必须再加 <>'none' 的原因")
}
