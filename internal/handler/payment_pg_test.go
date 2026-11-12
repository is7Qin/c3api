// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// payment_pg_test.go P32（spec v2 §13，执行面 PG）：回调路径账户与订单快照的
// account_key 不一致（**等额同币**、且用 backup 自己的 pid + 私钥重签）→
// `200 fail` + `fail_reason=account`，而【 status / paid_at / redemption_code_id
// 一个都不许变】。
//
// 为什么必须 PG：这三条断言是"审计列可写、状态列不可写"的**行级**区分
// （§12 B42）。fake 面的 MarkOrderNotifyFailure 是同一份语义的替身，只有真实
// SQL 才能证明 UPDATE 的 WHERE 子句真的只碰审计列。
//
// 无 TEST_DATABASE_URL 时 `pgtest.Clone` 直接 t.Skip（skip ≠ green）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	paymentapi "github.com/is7qin/c3api/internal/handler/payment"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/service/epay"
	"github.com/is7qin/c3api/internal/settingssnap"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"

	// Registers pgtest's repository-dependent hooks.
	_ "github.com/is7qin/c3api/internal/testsupport/pgtest/pgrepo"
)

// TestPaymentNotifyAccountMismatchIsAccount_PG P32：两个真实账户 + 一张真实订单，
// 用 backup 凭据打回调 → 只落 fail_reason=account，状态列全不变。
func TestPaymentNotifyAccountMismatchIsAccount_PG(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.Clone(t)
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	repos, err := repository.NewWithPG(ctx, entsql.OpenDB("postgres", db), false, pool)
	require.NoError(t, err)

	keys := epayTestKeys(t)
	mainCfg := epayAccountConfigPID(t, "https://gw.example.invalid", "1001")
	backupCfg := epayAccountConfigPID(t, "https://gw.example.invalid", "2002")
	mainAcct, err := repos.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key: paymentMainAccount, Provider: domain.ProviderEpayV2, MerchantRef: "1001",
		DisplayName: "main", Enabled: true, Priority: 0, Config: mainCfg,
	})
	require.NoError(t, err)
	backupAcct, err := repos.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key: "epay_backup", Provider: domain.ProviderEpayV2, MerchantRef: "2002",
		DisplayName: "backup", Enabled: true, Priority: 1, Config: backupCfg,
	})
	require.NoError(t, err)
	for _, acct := range []*domain.PaymentAccount{mainAcct, backupAcct} {
		_, chErr := repos.UpsertPaymentChannel(ctx, acct.ID, &domain.PaymentChannel{
			PayType: epay.PayTypeAlipay, Enabled: true, Currency: "CNY",
			RateMicro: 7_200_000, MinUSD: 1, MaxUSD: 1000,
		})
		require.NoError(t, chErr)
	}

	user, err := repos.CreateUser(ctx, &domain.User{
		Email: "pg-payer@example.com", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.UserStatusActive,
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	order, err := repos.CreateOrder(ctx, &domain.Order{
		OutTradeNo: "P20260101000032ABCDEFGHJK", UserID: user.ID,
		Provider: domain.ProviderEpayV2, AccountKey: paymentMainAccount,
		Status: domain.OrderStatusPending,
		// 1.00 USD @7.2：credit_milli = 100_000，pay_minor = 720（= 7.20 CNY）。
		CreditMilli: 100_000, PayMinor: 720, Currency: "CNY",
		RateMicro: 7_200_000, PayType: epay.PayTypeAlipay,
		CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	})
	require.NoError(t, err)

	// 装配真实 service + 公开回调面（协议实现与 main 同源）。
	snap := settingssnap.New(repos, nil)
	svc := service.New(service.Deps{
		Store: repos, Invalidate: service.NopInvalidator{},
		EmailCodeStore: testEmailCodes, SettingsSnapshot: snap,
		PaymentProviders: []domain.PaymentProvider{epay.New(nil)},
	})
	pay := paymentapi.Router(svc, nil)

	// 回调打到 **epay_backup**（等额同币），用 backup 自己的 pid 与私钥签名。
	params := notifyQuery(order.OutTradeNo)
	params.Set("pid", "2002")
	sign, err := epay.SignValues(params, keys.privatePEM)
	require.NoError(t, err)
	params.Set("sign", sign)
	params.Set("sign_type", "RSA")
	rec := httptest.NewRecorder()
	pay.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/payment/epay_backup/notify?"+params.Encode(), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String(), "账户不一致必须是 200 fail（网关不再重试，人工介入）")

	stored, err := repos.GetOrderByID(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPending, stored.Status, "状态列不许变")
	require.Nil(t, stored.PaidAt, "paid_at 不许变")
	require.Nil(t, stored.RedemptionCodeID, "redemption_code_id 不许变（没有码就没有补账）")
	require.NotNil(t, stored.FailReason, "审计列必须可写")
	require.Equal(t, domain.OrderFailReasonAccount, *stored.FailReason)
	require.Equal(t, 1, stored.FailCount)
	require.NotNil(t, stored.LastNotifyAt)

	// 余额分文未动。
	after, err := repos.GetUser(ctx, user.ID)
	require.NoError(t, err)
	require.Zero(t, after.Balance, "账户不一致绝不入账")

	// 同一订单用正确账户（main）回调仍然入账成功——证明上一步拦的是"账户"而非"金额"。
	okParams := notifyQuery(order.OutTradeNo)
	okSign, err := epay.SignValues(okParams, keys.privatePEM)
	require.NoError(t, err)
	okParams.Set("sign", okSign)
	okParams.Set("sign_type", "RSA")
	rec = httptest.NewRecorder()
	pay.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+okParams.Encode(), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String())

	credited, err := repos.GetOrderByID(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, credited.Status)
	require.NotNil(t, credited.RedemptionCodeID)
	after, err = repos.GetUser(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, order.CreditMilli, after.Balance, "入账金额 = credit_milli（不是 pay_minor）")

	// 便于人工排查的轨迹：raw_notify 是清洗后的 JSON（不带密钥）。
	if credited.RawNotify != nil {
		var decoded map[string]string
		require.NoError(t, json.Unmarshal([]byte(*credited.RawNotify), &decoded))
		require.Equal(t, order.OutTradeNo, decoded["out_trade_no"])
	}
}
