// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// 订单仓储的真实 PostgreSQL 面（spec 支付网关 v2 §3-3 / §4-4 / §8）。
//
// 基座与 redemption_repo_test.go 同款：newPGReposShared → pgtest.Clone，未设
// TEST_DATABASE_URL 时 envOrSkip 直接 t.Skip（skip ≠ green）。
//
// 为什么必须在真库上跑（D 批的两个洞都在这三条 SQL 里，fake store 不执行它们）：
//   - M14「MarkOrderPaid 不判状态」：条件迁移 `WHERE status='pending'` 只在真库
//     上才可能 0 行；
//   - F36/N1「closed 分支写不进 fail_reason」：`WHERE status <> 'paid'` 的谓词
//     语义（尤其**不含** status='pending'）只有真库能判；
//   - P32「状态字段不变」：写 fail_reason 的 UPDATE 不得顺手覆盖 status /
//     paid_at / redemption_code_id。

// pgOrderFor 建一条可插入订单的参数（pending/paid/closed 通用）。out_trade_no 用
// 真实形状 `P{UTC yyyyMMddHHmmss}{10 位大写 base32}`，方便对照失败日志。
// user_id 与 redemption_code_id 在 orders 表上都没有外键（spec §3-3：账户/订单
// 快照不建 FK），因此这里不需要种用户行。
func pgOrderFor(outTradeNo string, status domain.OrderStatus) *domain.Order {
	return &domain.Order{
		OutTradeNo:  outTradeNo,
		UserID:      42,
		Provider:    domain.ProviderEpayV2,
		AccountKey:  "pg_order_account",
		Status:      status,
		CreditMilli: 100_000,
		PayMinor:    720,
		Currency:    "CNY",
		RateMicro:   7_200_000,
		PayType:     "alipay",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
	}
}

// TestOrderNotifyFailureOnClosedOrder_PG `MarkOrderNotifyFailure` 在 closed 订单上
// **真的能写**（F36/N1：WHERE 含 closed、排除 paid），并且只写三个审计列——状态
// 字段一个都不许动（P32 的同款不变量）。
func TestOrderNotifyFailureOnClosedOrder_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	// 现实形态：closed 单的 paid_at / redemption_code_id 都是 NULL。
	t.Run("现实形态（paid_at/redemption_code_id 为 NULL）", func(t *testing.T) {
		created, err := repos.CreateOrder(ctx, pgOrderFor("P20261112030405ABCDEFGHIJ", domain.OrderStatusClosed))
		require.NoError(t, err)
		require.Equal(t, domain.OrderStatusClosed, created.Status)

		require.NoError(t, repos.MarkOrderNotifyFailure(ctx, created.OutTradeNo, domain.OrderFailReasonClosed))

		got, err := repos.GetOrderByID(ctx, created.ID)
		require.NoError(t, err)
		require.NotNil(t, got.FailReason, "closed 分支必须真的写进 fail_reason（F36/N1：谓词不含 status='pending'）")
		require.Equal(t, domain.OrderFailReasonClosed, *got.FailReason)
		require.Equal(t, 1, got.FailCount, "fail_count = 1")
		require.NotNil(t, got.LastNotifyAt, "last_notify_at 落库")
		require.WithinDuration(t, time.Now(), *got.LastNotifyAt, time.Minute)

		// 状态字段一个都不许动（P32：status/paid_at/redemption_code_id 不变）。
		require.Equal(t, domain.OrderStatusClosed, got.Status, "status 不变（写失败记录 ≠ 状态迁移）")
		require.Nil(t, got.PaidAt, "paid_at 不变")
		require.Nil(t, got.RedemptionCodeID, "redemption_code_id 不变")

		// 第二次失败 → 累加（尽力计数，F48/F21：不承担对账职责但要能看出来）。
		require.NoError(t, repos.MarkOrderNotifyFailure(ctx, created.OutTradeNo, domain.OrderFailReasonClosed))
		again, err := repos.GetOrderByID(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, 2, again.FailCount, "fail_count 单调累加")
		require.Equal(t, domain.OrderStatusClosed, again.Status)
	})

	// 哨兵形态：paid_at / redemption_code_id / trade_no 故意种成非 NULL——
	// 现实形态判不出「被覆盖成 NULL」，这一条能。若 UPDATE 误带这三列（或整行写回），
	// 断言必红。
	t.Run("哨兵形态（非 NULL 也不被覆盖）", func(t *testing.T) {
		sentinelPaidAt := time.Date(2026, 11, 12, 3, 4, 5, 0, time.UTC)
		sentinelCodeID := int64(9001)
		sentinelTradeNo := "trade-sentinel"

		o := pgOrderFor("P20261112030405KLMNOPQRST", domain.OrderStatusClosed)
		o.PaidAt = &sentinelPaidAt
		o.RedemptionCodeID = &sentinelCodeID
		o.TradeNo = &sentinelTradeNo
		created, err := repos.CreateOrder(ctx, o)
		require.NoError(t, err)

		require.NoError(t, repos.MarkOrderNotifyFailure(ctx, created.OutTradeNo, domain.OrderFailReasonClosed))

		got, err := repos.GetOrderByID(ctx, created.ID)
		require.NoError(t, err)
		require.NotNil(t, got.FailReason, "closed 分支必须真的写进 fail_reason（F36/N1）")
		require.Equal(t, domain.OrderFailReasonClosed, *got.FailReason)
		require.Equal(t, 1, got.FailCount)
		require.NotNil(t, got.LastNotifyAt)

		require.Equal(t, domain.OrderStatusClosed, got.Status, "status 不变")
		require.NotNil(t, got.PaidAt, "paid_at 未被覆盖")
		require.True(t, sentinelPaidAt.Equal(*got.PaidAt), "paid_at 值不变")
		require.NotNil(t, got.RedemptionCodeID, "redemption_code_id 未被覆盖")
		require.Equal(t, sentinelCodeID, *got.RedemptionCodeID, "redemption_code_id 值不变")
		require.NotNil(t, got.TradeNo, "trade_no 未被覆盖")
		require.Equal(t, sentinelTradeNo, *got.TradeNo)
	})
}

// TestOrderNotifyFailureSkipsPaidOrder_PG `WHERE status <> 'paid'`：已入账订单不被
// 失败记录污染（否则 paid 单会带上 fail_reason，管理面误报「已付款未入账」）。
func TestOrderNotifyFailureSkipsPaidOrder_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	paidAt := time.Date(2026, 11, 12, 3, 4, 5, 0, time.UTC)
	codeID := int64(4242)
	o := pgOrderFor("P20261112030405VWXYZ23456", domain.OrderStatusPaid)
	o.PaidAt = &paidAt
	o.RedemptionCodeID = &codeID
	created, err := repos.CreateOrder(ctx, o)
	require.NoError(t, err)

	// 命中 0 行不是错误：这是尽力写路径，调用方拿到 nil 继续回 200。
	require.NoError(t, repos.MarkOrderNotifyFailure(ctx, created.OutTradeNo, domain.OrderFailReasonClosed),
		"0 行不报错")

	got, err := repos.GetOrderByID(ctx, created.ID)
	require.NoError(t, err)
	require.Zero(t, got.FailCount, "paid 单 fail_count 保持 0")
	require.Nil(t, got.FailReason, "paid 单不得写入 fail_reason")
	require.Nil(t, got.LastNotifyAt)
	require.Equal(t, domain.OrderStatusPaid, got.Status)
	require.NotNil(t, got.RedemptionCodeID)
	require.Equal(t, codeID, *got.RedemptionCodeID)

	// 未知单号同样 0 行且不报错（单号未经验签 → 不得用伪造签名污染任何订单）。
	require.NoError(t, repos.MarkOrderNotifyFailure(ctx, "P20261112030405ZZZZZZZZZZ", domain.OrderFailReasonSignature))
}

// TestMarkOrderPaidOnClosedOrderConflicts_PG `MarkOrderPaid` 在 closed 订单上命中
// 0 行 → ErrConflict（M14 判定点；spec §13 P4b 的 repo 级用例）。
func TestMarkOrderPaidOnClosedOrderConflicts_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	created, err := repos.CreateOrder(ctx, pgOrderFor("P20261112030405MNOPQRSTUV", domain.OrderStatusClosed))
	require.NoError(t, err)

	const codeID = int64(777)

	// 非事务面直调：0 行 → ErrConflict（409 族，不是静默成功）。
	require.ErrorIs(t, repos.MarkOrderPaid(ctx, created.ID, codeID, "trade-1", nil), repository.ErrConflict)

	// 事务内同样 0 行（CompleteOrder 的真实调用形状；ErrConflict 触发重试/人工）。
	err = repos.WithTx(ctx, func(tx repository.TxStore) error {
		return tx.MarkOrderPaid(ctx, created.ID, codeID, "trade-2", nil)
	})
	require.ErrorIs(t, err, repository.ErrConflict, "事务内同样 0 行 → ErrConflict")

	got, err := repos.GetOrderByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusClosed, got.Status, "closed 不可迁 paid")
	require.Nil(t, got.RedemptionCodeID, "被拒的迁移不得绑定凭据")
	require.Nil(t, got.PaidAt)
	require.Nil(t, got.TradeNo)
}

// TestMarkOrderPaidOnPendingOrderBindsCode_PG 上一条的对照组：pending 能迁 paid 并
// 绑定凭据/流水（说明 closed 的 ErrConflict 是状态机判决，不是方法坏了）。
func TestMarkOrderPaidOnPendingOrderBindsCode_PG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	created, err := repos.CreateOrder(ctx, pgOrderFor("P20261112030405WXYZ234567", domain.OrderStatusPending))
	require.NoError(t, err)

	const codeID = int64(555)
	rawNotify := `{"trade_no":"trade-ok","status":"paid"}`
	require.NoError(t, repos.WithTx(ctx, func(tx repository.TxStore) error {
		return tx.MarkOrderPaid(ctx, created.ID, codeID, "trade-ok", &rawNotify)
	}))

	got, err := repos.GetOrderByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, got.Status, "pending → paid")
	require.NotNil(t, got.RedemptionCodeID)
	require.Equal(t, codeID, *got.RedemptionCodeID, "凭据随订单落库")
	require.NotNil(t, got.PaidAt, "paid_at 由本次迁移写入")
	require.NotNil(t, got.TradeNo)
	require.Equal(t, "trade-ok", *got.TradeNo)
	require.NotNil(t, got.RawNotify)

	// 已 paid 再迁一次 → 0 行 → ErrConflict（「已处理」信号，不重复绑码）。
	require.ErrorIs(t, repos.MarkOrderPaid(ctx, created.ID, codeID+1, "trade-again", nil), repository.ErrConflict)
	after, err := repos.GetOrderByID(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, codeID, *after.RedemptionCodeID, "二次迁移未改写凭据")
}
