// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// payment_code_conflict_test.go P15（v1 §11，fake 面）：到账码文本唯一冲突时，
// 换号重试必须**另开事务 + 另生成一张码**——诱饵码不得被消耗、新码恰好用 1 次、
// 余额只加一次、订单 paid。
//
// 判别点：把「同事务内重试码冲突」写成循环（v1 M11）会拿同一张码重试 → 要么永久
// 冲突，要么把诱饵码 used_count 变成 1 而余额没加对。所以断言里同时钉住
// **诱饵行的 used_count** 和**新行的 used_count**。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestCompleteOrderRetriesCodeConflict P15：诱饵码冲突 → 换新码入账。
func TestCompleteOrderRetriesCodeConflict(t *testing.T) {
	const bait = "BAIT-CODE-0000-0000"
	const fresh = "FRESH-CODE-1111-1111"

	svc, fs, _ := newPaymentSvc()
	ctx := context.Background()
	u := seedUser(t, fs, "codeconflict@example.com", 0, 0)
	o := seedPendingOrder(t, fs, u.ID)

	// 诱饵：库里已存在同文本的码（占位，未被使用）。
	fs.codes[900] = &domain.RedemptionCode{
		ID: 900, Code: bait, Type: domain.RedemptionTypeBalance,
		Value: 1, MaxUses: 1, UsedCount: 0,
		Status: domain.RedemptionStatusActive,
	}

	// 第一次生成诱饵（撞唯一冲突）→ 第二次生成新码（成功）。
	calls := 0
	old := newOrderCode
	newOrderCode = func() (string, error) {
		calls++
		if calls == 1 {
			return bait, nil
		}
		return fresh, nil
	}
	t.Cleanup(func() { newOrderCode = old })

	alreadyPaid, err := svc.CompleteOrder(ctx, paidInput(o.OutTradeNo))
	require.NoError(t, err, "唯一冲突必须换号重试掉（有界 3 次）")
	require.False(t, alreadyPaid)
	require.GreaterOrEqual(t, calls, 2, "必须重新生成一张码（同码重试是死循环）")

	// 诱饵行原封不动：used_count 仍是 0（它从未被用过）。
	require.Equal(t, 0, fs.codes[900].UsedCount, "诱饵码不得被消耗")
	require.Equal(t, int64(1), fs.codes[900].Value)

	// 新码：1 行、used_count=1、面额 = credit_milli。
	var created *domain.RedemptionCode
	for _, c := range fs.codes {
		if c.Code == fresh {
			created = c
		}
	}
	require.NotNil(t, created, "新码必须落库")
	require.Equal(t, 1, created.UsedCount)
	require.Equal(t, o.CreditMilli, created.Value)

	// 余额只加一次 + 订单 paid 指向新码。
	user, err := fs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, o.CreditMilli, user.Balance)

	stored, err := fs.GetOrderByID(ctx, o.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, stored.Status)
	require.NotNil(t, stored.RedemptionCodeID)
	require.Equal(t, created.ID, *stored.RedemptionCodeID)
	require.Len(t, fs.uses, 1)

	// 失败尝试不留脏数据：uses/balance_logs 各 1 行，码表=诱饵+新码。
	require.Len(t, fs.balanceLogs, 1)
	require.Len(t, fs.codes, 2, "只应多出新码一张（失败尝试的码随回滚丢弃）")
}
