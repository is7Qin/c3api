// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// P0-A 回归门禁（spec-commit-state-and-round-billing §0，G0-a~G0-d/G0-f）：
// 写侧已按交付事实计费（forward.go:462-473：error_type='none' 或 commit_state
// ∈ {sent_ambiguous,response_started} → usage_logs），读侧账本
// 与统计必须用同一谓词（billable_sql.go 的 billableSQL / notBillableErrSQL），
// 否则「已交付的失败行」永久滞留 usagelog_unbilled_id 部分索引且滞 lag 不可见。
//
// 每个用例独立 PG clone；固定时刻造数（分区显式预建）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/usagelog"
)

// g0CommitState 交付事实字面量（domain.CommitState 是 *string；值域由 proxy
// commitStateBillable 定义，见 internal/proxy/forward.go:483-493）。
func g0CommitState(s string) *string { return &s }

// g0FailureLog 构造失败行（可计费失败 = 已写帧；未交付失败 = not_sent）。
func g0FailureLog(rid string, at time.Time, userID int64, cs *string, cost int64) *domain.UsageLog {
	l := logFor(userID, rid)
	l.ErrorType = domain.Err5xx
	l.CommitState = cs
	l.Cost = cost
	l.CreatedAt = at
	return l
}

// TestPGBillablePredicateG0aDeliveredFailureSettled G0-a：已交付失败
// （5xx + response_started + cost>0 + billed=false）必须被账本取到、结算后
// billed=true 且余额实际下降，UnbilledLag 归零。
func TestPGBillablePredicateG0aDeliveredFailureSettled(t *testing.T) {
	repos := newPGReposShared(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "g0a-billable@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	row := g0FailureLog("g0a-delivered", cursorSeedTime.Add(time.Minute), u.ID,
		g0CommitState("response_started"), 250_000)
	seedUnbilled(t, repos, row)

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1, "已交付失败行必须进取批（billableSQL 覆盖 commit_state）")
	require.Equal(t, row.RequestID, requestIDOf(t, repos, rows[0].ID))

	res, err := repos.SettleBalanceBatch(ctx, 100, 1, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Marked, "结算标记一行")
	require.Len(t, res.Balances, 1)
	require.Equal(t, u.ID, res.Balances[0].UserID)
	require.Equal(t, int64(750_000), res.Balances[0].Balance, "余额实际下降 cost")

	require.True(t, usageLogByID(t, repos, rows[0].ID).Billed, "billed=true 退出部分索引")
	require.Equal(t, int64(750_000), cursorBalance(t, repos, u.ID))

	_, lagOK, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.False(t, lagOK, "已交付失败被消费后 lag 归零")
}

// TestPGBillablePredicateG0bUndeliveredFailureNotInUsage G0-b：未交付失败
// （5xx + not_sent）按写侧路由只在 err_logs——usage_logs 0 行、账本取不到。
func TestPGBillablePredicateG0bUndeliveredFailureNotInUsage(t *testing.T) {
	repos := newPGReposShared(t)
	seedAggWindow(t, repos, cursorSeedTime, cursorSeedTime.Add(time.Hour))
	ctx := context.Background()

	// 写侧 routeLog：commitStateBillable(nil/not_sent)=false ⇒ 不 Record；
	// 而 ErrorType != none ⇒ 进 err_logs（双轨的另一轨）。
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{
		g0FailureLog("g0b-not-sent", cursorSeedTime.Add(time.Minute), 42,
			g0CommitState("not_sent"), 0),
	}))

	n, err := repos.Client.UsageLog.Query().Count(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "未交付失败不进 usage_logs（写侧唯一落库点已拦截）")

	rows := fetchAllUnbilled(t, repos)
	require.Empty(t, rows, "账本无可取行")

	_, lagOK, err := repos.UnbilledLag(ctx)
	require.NoError(t, err)
	require.False(t, lagOK)

	errN, err := repos.Client.ErrLog.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, errN, "错误明细在 err_logs")
}

// TestPGBillablePredicateG0cStatsMutualExclusion G0-c：统计两源互斥——同一
// attempt（5xx + response_started）写侧双轨（usage + err 同 request_id），统计
// 只经 usage 源计一次（cost/tokens 计入、请求数不重复）；not_sent attempt 只
// 经 err 源计一次（零 cost）。
func TestPGBillablePredicateG0cStatsMutualExclusion(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	h := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(2*time.Hour))

	deliveredAt := h.Add(time.Minute)
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		g0FailureLog("g0c-delivered", deliveredAt, 42, g0CommitState("response_started"), 50),
	}))
	deliveredErr := errLogRow("g0c-delivered", deliveredAt, domain.Err5xx, 0, 2, 42, 3, "gpt-4o")
	deliveredErr.CommitState = g0CommitState("response_started") // 写侧双轨同事实：同一 attempt
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{deliveredErr}))
	// not_sent：写侧只进 err_logs（usage 侧无行）。
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{
		errLogRow("g0c-not-sent", h.Add(time.Hour+time.Minute), domain.Err5xx, 0, 2, 42, 3, "gpt-4o"),
	}))

	rows, _, detail, err := repos.Stats.LoadAggRange(ctx, h, h.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 2, "两小时各一桶")
	require.Equal(t, int64(2), detail, "usage 可计费 1 行 + err 不可计费 1 行（两源互补，双计行只扫一次）")

	delivered := rows[0]
	require.Equal(t, h, delivered.BucketTime.UTC())
	require.Equal(t, int64(1), delivered.RequestCount,
		"同一 attempt 只计一次：usage 源计入、err 源排除（防双计）")
	require.Equal(t, int64(1), delivered.ErrorCount, "已交付失败仍属错误行")
	require.Equal(t, int64(50), delivered.Cost, "已交付失败 cost 计入 usage 源")
	require.Equal(t, int64(8), delivered.TotalTokens, "tokens 计入 usage 源")
	require.Equal(t, int64(1), delivered.CallCount)

	undelivered := rows[1]
	require.Equal(t, h.Add(time.Hour), undelivered.BucketTime.UTC())
	require.Equal(t, int64(1), undelivered.RequestCount, "not_sent 经 err 源计一次")
	require.Equal(t, int64(1), undelivered.ErrorCount)
	require.Zero(t, undelivered.Cost, "not_sent 不可计费：err 源只计数")
	require.Zero(t, undelivered.TotalTokens)
}

// TestPGBillablePredicateG0dNullCommitStateConservative G0-d：历史 NULL 行
// （commit_state IS NULL + 5xx）不回填、不重判——账本不计费、统计归 err 源且
// 不并入 usage 源（cost/tokens 不出现）。
func TestPGBillablePredicateG0dNullCommitStateConservative(t *testing.T) {
	repos := newPGReposShared(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	h := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	seedAggWindow(t, repos, h, h.Add(time.Hour))

	row := g0FailureLog("g0d-null", h.Add(time.Minute), 42, nil, 500)
	seedUnbilled(t, repos, row)
	require.NoError(t, repos.ErrLogs.InsertBatch(ctx, []*domain.UsageLog{
		errLogRow("g0d-null", h.Add(time.Minute), domain.Err5xx, 0, 2, 42, 3, "gpt-4o"),
	}))

	require.Empty(t, fetchAllUnbilled(t, repos),
		"NULL 是未知（保守不计费），不得因谓词改动被算成可计费")
	seeded, err := repos.Client.UsageLog.Query().Where(usagelog.RequestIDEQ("g0d-null")).Only(ctx)
	require.NoError(t, err)
	require.False(t, usageLogByID(t, repos, seeded.ID).Billed,
		"历史 NULL 行不被账本消费（不回填、不重判）")

	rows, _, _, err := repos.Stats.LoadAggRange(ctx, h, h.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(1), rows[0].RequestCount, "统计侧归 err 源恰一次")
	require.Equal(t, int64(1), rows[0].ErrorCount)
	require.Zero(t, rows[0].Cost, "不并入 usage 源（NULL 不是可计费）")
	require.Zero(t, rows[0].TotalTokens)

	got := usageLogByID(t, repos, seeded.ID)
	require.Nil(t, got.CommitState, "不回填 commit_state")
}

// TestPGBillablePredicateG0fZeroCostSwept G0-f：零成本 + 可计费（cost=0 +
// response_started）必须被 fetch 命中 → sweepZeroCost 纯标记 billed=true，
// 余额不变（结算车道 cost>0 不动，零价行走 MarkBilledBulk）。
func TestPGBillablePredicateG0fZeroCostSwept(t *testing.T) {
	repos := newPGReposShared(t)
	ensureCursorPartitions(t, repos)
	ctx := context.Background()

	u := seedPGUser(t, repos, "g0f-zero@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1_000_000))

	row := g0FailureLog("g0f-zero", cursorSeedTime.Add(2*time.Minute), u.ID,
		g0CommitState("response_started"), 0)
	seedUnbilled(t, repos, row)

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1, "单取批面无 cost>0 过滤：零价可计费行同样命中")
	require.Zero(t, rows[0].Cost)

	drained, quarantinedN := drainBillingCursor(t, repos)
	require.Equal(t, int64(1), drained, "零价行由 sweep 标记退出索引")
	require.Zero(t, quarantinedN)

	got := usageLogByID(t, repos, rows[0].ID)
	require.True(t, got.Billed, "sweep 后 billed=true")
	require.False(t, got.Overdraft, "纯标记不触碰 overdraft")
	require.Equal(t, int64(1_000_000), cursorBalance(t, repos, u.ID), "余额不变")
	require.Zero(t, cursorUnbilledCount(t, repos))
}
