// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// payment_complete_conflict_test.go M1：applier 内部的 repository.ErrConflict 必须
// 保持仓储哨兵身份，才能被 CompleteOrder 的 isRetryableTxErr 认出来重试。
//
// 回归目标（applyInTx 若走 mapRepoErr 就会破）：mapRepoErr 把
// repository.ErrConflict 改写成 service.ErrConflict，isRetryableTxErr 只认前者，
// 于是「一次可重试的事务冲突」直接变成 5xx 上抛——用户已付款，订单还停在
// pending，必须人工介入。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// newPaymentSvc 构造支付编排用的 Service（fake 面：store + inv 记录器，无 pub/log）。
func newPaymentSvc() (*Service, *fakeStore, *invRecorder) {
	fs := newFakeStore()
	rec := &invRecorder{}
	return &Service{store: fs, inv: rec, log: nil}, fs, rec
}

// seedPendingOrder 造一张 pending 订单（1.00 USD @7.2 → 1000 毫分 / 7200 分 CNY）。
func seedPendingOrder(t *testing.T, fs *fakeStore, userID int64) *domain.Order {
	t.Helper()
	now := time.Now().UTC()
	o := &domain.Order{
		OutTradeNo: "P20261112010101ABCDEFGHJK", UserID: userID,
		Provider: domain.ProviderEpayV2, AccountKey: "epay_main",
		Status: domain.OrderStatusPending,
		// 1.00 USD：credit_milli = 100*1000；pay_minor = 7200（@7.2）。
		CreditMilli: 100_000, PayMinor: 7200, Currency: "CNY", RateMicro: 7_200_000,
		PayType: "alipay", CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}
	created, err := fs.CreateOrder(context.Background(), o)
	require.NoError(t, err)
	return created
}

// paidInput 与 seedPendingOrder 对齐的成功通知。
func paidInput(outTradeNo string) CompleteInput {
	return CompleteInput{
		Provider: domain.ProviderEpayV2, AccountKey: "epay_main",
		OutTradeNo: outTradeNo, TradeNo: "T-1",
		MoneyMinor: 7200, PaidCurrency: "CNY", Status: domain.NotifyStatusPaid,
	}
}

// TestCompleteOrderRetriesApplierConflict M1 主用例：第 1 次事务里 applier 返回
// repository.ErrConflict → CompleteOrder 必须**换新事务重试**并成功入账（余额只加
// 一次、订单 paid、第一次的码随回滚丢弃）。
func TestCompleteOrderRetriesApplierConflict(t *testing.T) {
	svc, fs, rec := newPaymentSvc()
	ctx := context.Background()

	u := seedUser(t, fs, "payer@example.com", 0, 0)
	o := seedPendingOrder(t, fs, u.ID)
	// applier 路径（applyBalance → writeBalanceLog → CreateBalanceLog）注入一次冲突。
	fs.balanceLogErrs = &errQueue{errs: []error{
		fmt.Errorf("%w: applier internal conflict", repository.ErrConflict),
	}}

	alreadyPaid, err := svc.CompleteOrder(ctx, paidInput(o.OutTradeNo))
	require.NoError(t, err, "可重试冲突必须被重试掉（不能被 mapRepoErr 改写成不可重试错误）")
	require.False(t, alreadyPaid)

	got, err := fs.GetOrderByID(ctx, o.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, got.Status, "重试成功后订单必须置 paid")
	require.NotNil(t, got.RedemptionCodeID)

	user, err := fs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), user.Balance, "余额只加一次（失败那次随事务回滚）")

	require.Len(t, fs.balanceLogs, 1, "余额变动记录只应有一条")
	require.Len(t, fs.codes, 1, "第一次尝试生成的码必须随回滚丢弃")
	require.Len(t, fs.uses, 1)
	require.NotEmpty(t, rec.calls, "入账成功后必须刷新 auth 快照")
}

// TestCompleteOrderApplierConflictKeepsRepoError M1 哨兵身份：持续冲突时
// CompleteOrder 仍然**只**返回 repository.ErrConflict（不被改写成
// service.ErrConflict），这是上游按「可重试」归因的唯一依据。
func TestCompleteOrderApplierConflictKeepsRepoError(t *testing.T) {
	svc, fs, _ := newPaymentSvc()
	ctx := context.Background()

	u := seedUser(t, fs, "payer2@example.com", 0, 0)
	o := seedPendingOrder(t, fs, u.ID)
	// 永久冲突：3 次尝试全部失败（有界重试，不允许无限循环）。
	fs.createBalanceLogErr = fmt.Errorf("%w: persistent conflict", repository.ErrConflict)

	_, err := svc.CompleteOrder(ctx, paidInput(o.OutTradeNo))
	require.ErrorIs(t, err, repository.ErrConflict, "仓储冲突哨兵必须原样透出")
	require.False(t, errors.Is(err, ErrConflict), "不得被 mapRepoErr 改写成 service.ErrConflict")

	got, err := fs.GetOrderByID(ctx, o.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPending, got.Status, "全部失败 → 订单留在 pending")

	user, err := fs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Zero(t, user.Balance, "失败事务不产生余额变动")
	require.Empty(t, fs.balanceLogs)
	require.Empty(t, fs.codes, "失败的尝试不留码")
}
