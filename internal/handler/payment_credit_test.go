// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// payment_credit_test.go 建单与入账的**账本形状**用例（v1 §11 的 P1/P2/P11/P13/
// P22/P23，全部跑 fake 面）：
//
//   - P1  pay_url 必须能用**商户公钥**验签通过，且订单快照齐全；
//   - P2  入账四件套：码行（balance/max_uses=1/used_count=1/remark）、use 行、
//         balance_logs 行（source=redemption、note=码文本+订单号）、订单 paid；
//   - P11 扩展字段**参与签名**（签名后加字段会让验签失败，故必须"先加再签"）；
//   - P13 改汇率后查旧单：快照不变（不重算、不重签）；
//   - P22 入账金额 = credit_milli（不是 pay_minor 反算回去的 1059）；
//   - P23 half-up 舍入后的 115 分实付 + 16000 毫分入账，两个数不许互换。
//
// 这几条是"越算越错"的温床：金额/汇率/毫分与分之间任何一次单位串台都能把某个
// 数字变成"看起来对"的另一个数，所以每个断言都写死具体整数。

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/service/epay"
)

// TestPaymentPayURLVerifiesWithMerchantPublicKey P1：pay_url 的全部参数用**商户
// 公钥**验签必须通过（签错算法/漏参数/拼错待签串都会红），订单快照齐全。
func TestPaymentPayURLVerifiesWithMerchantPublicKey(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.15", epay.PayTypeAlipay)

	payURL, err := url.Parse(created.PayUrl)
	require.NoError(t, err)
	require.NotEmpty(t, payURL.Host, "pay_url 必须是绝对 URL（网关跳转需要绝对地址）")
	require.NoError(t, epay.VerifyValues(payURL.Query(), epayTestKeys(t).publicPEM),
		"pay_url 必须能用商户公钥验签通过: %s", created.PayUrl)
	require.Equal(t, "RSA", payURL.Query().Get("sign_type"))
	require.Equal(t, created.OrderNo, payURL.Query().Get("out_trade_no"))
	require.Equal(t, "8.28", payURL.Query().Get("money"), "签名里的 money 是**渠道金额**（CNY 分）")
	require.Equal(t, "8.28", created.PayAmount)

	// 订单快照齐全（provider/account/currency/rate/pay_minor/expires_at）。
	require.Equal(t, domain.OrderStatusPending, order.Status)
	require.Equal(t, domain.ProviderEpayV2, order.Provider)
	require.Equal(t, paymentMainAccount, order.AccountKey)
	require.Equal(t, "CNY", order.Currency)
	require.Equal(t, int64(7_200_000), order.RateMicro)
	require.Equal(t, int64(115_000), order.CreditMilli, "1.15 USD = 115 分 = 115,000 毫分")
	require.Equal(t, int64(828), order.PayMinor, "1.15 USD @7.2 = 8.28 CNY = 828 分（half-up）")
	require.False(t, order.ExpiresAt.IsZero())
	require.True(t, order.ExpiresAt.After(order.CreatedAt))
}

// TestPaymentCompleteWritesLedger P2：入账的**四件套**一次到位，且每行的形状
// 都能被运维直接读懂（remark 带订单号，note 带码文本）。
func TestPaymentCompleteWritesLedger(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.15", epay.PayTypeAlipay)

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+
			signedNotify(t, notifyQueryM(created.OrderNo, "8.28")).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String())

	// ① 码行：balance 类型、面额 = credit_milli、max_uses=1、已用 1 次、remark=order <no>。
	require.Len(t, p.store.codes, 1)
	var code *domain.RedemptionCode
	for _, c := range p.store.codes {
		code = c
	}
	require.Equal(t, domain.RedemptionTypeBalance, code.Type)
	require.Equal(t, order.CreditMilli, code.Value, "码面额必须是 credit_milli")
	require.Equal(t, 1, code.MaxUses)
	require.Equal(t, 1, code.UsedCount)
	require.NotNil(t, code.Remark)
	require.Equal(t, "order "+order.OutTradeNo, *code.Remark)

	// ② use 行：一条，指向同一张码与同一个用户。
	require.Len(t, p.store.uses, 1)
	for _, use := range p.store.uses {
		require.Equal(t, code.ID, use.CodeID)
		require.Equal(t, p.userID, use.UserID)
		require.Equal(t, code.Value, use.Value)
	}

	// ③ balance_logs：source=redemption、note 前缀是码文本且含订单号。
	require.Len(t, p.store.balanceLogs, 1)
	log := p.store.balanceLogs[0]
	require.Equal(t, domain.BalanceSourceRedemption, log.Source)
	require.Equal(t, order.CreditMilli, log.Amount)
	require.Equal(t, order.CreditMilli, log.BalanceAfter)
	require.NotNil(t, log.Note)
	require.Contains(t, *log.Note, code.Code, "note 必须以码文本开头（可审计定位）")
	require.Contains(t, *log.Note, order.OutTradeNo, "note 必须含订单号")

	// ④ 订单 paid + 指向码。
	stored, err := p.store.GetOrderByID(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, stored.Status)
	require.NotNil(t, stored.RedemptionCodeID)
	require.Equal(t, code.ID, *stored.RedemptionCodeID)
}

// TestPaymentNotifyExtensionFieldIsSigned P11/B32：扩展字段**参与签名**——
// 「先加字段再签名」必须入账（若实现把未知字段排除在待签串之外，这条会红）。
func TestPaymentNotifyExtensionFieldIsSigned(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)

	rec := postNotify(t, p, paymentMainAccount, func(v url.Values) {
		v.Set("out_trade_no", created.OrderNo)
		v.Set("site_name", "示例站点")   // 扩展字段（网关会带 site_name/param 等）
		v.Set("param", "trace-12345")
		v.Set("buyer", "payer@example.com")
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String(), "扩展字段参与签名时必须验签通过并入账")

	stored, err := p.store.GetOrderByID(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, stored.Status)
	require.Equal(t, int64(100_000), balanceOf(t, p))
}

// TestPaymentOrderSnapshotIsImmutable P13/B15：改渠道汇率后查旧单 → 快照不变
// （rate/pay_amount 不重算；重签 pay_url 也用快照汇率）。
func TestPaymentOrderSnapshotIsImmutable(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	require.Equal(t, "7.200000", created.Rate)

	rec := p.do(p.adminPay, http.MethodPut, "/api/admin/payment/accounts/"+paymentMainAccount+"/channels/"+epay.PayTypeAlipay,
		`{"enabled":true,"currency":"CNY","rate_micro":9000000,"min_usd":1,"max_usd":1000}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = p.do(p.user, http.MethodGet, "/api/user/orders/"+itoa(order.ID), "", p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var detail userapi.UserOrder
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	require.Equal(t, "7.200000", detail.Rate, "旧单必须保留快照汇率（改汇率不得回算历史）")
	require.Equal(t, "7.20", detail.PayAmount, "旧单实付金额同样不许重算")

	stored, err := p.store.GetOrderByID(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, int64(7_200_000), stored.RateMicro)
	require.Equal(t, int64(720), stored.PayMinor)

	// 新单才用新汇率（9.0）——快照的本意就是"各算各的"。
	_, second := createOrderOn(t, p, "1.00", epay.PayTypeAlipay)
	require.Equal(t, int64(9_000_000), second.RateMicro)
	require.Equal(t, int64(900), second.PayMinor)
}

// TestPaymentCreditUsesCreditMilliNotPayMinor P22/B34：rate_micro=3777777 + 0.01 USD
// （种订单行）→ 入账金额必须是 credit_milli=1000，绝不是 pay_minor 反算的 1059。
func TestPaymentCreditUsesCreditMilliNotPayMinor(t *testing.T) {
	p := newPaymentHarness(t)
	order := seedPricedOrder(t, p, 22, "P20260101000022ABCDEFGHJK", 3_777_777, 1000, 4)

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+
			signedNotify(t, notifyQueryM(order.OutTradeNo, "0.04")).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String())

	require.Equal(t, int64(1000), balanceOf(t, p),
		"入账必须等于 credit_milli（1000 毫分），不是 pay_minor×rate 反算的 1059")
	require.Equal(t, int64(4), order.PayMinor)
}

// TestPaymentCreditHalfUpRounding P23/B4：money="1.15"（订单 pay_minor=115、
// credit_milli=16000）→ 余额 +16000 毫分，且订单 pay_minor 仍是 115。
func TestPaymentCreditHalfUpRounding(t *testing.T) {
	p := newPaymentHarness(t)
	order := seedPricedOrder(t, p, 23, "P20260101000023ABCDEFGHJK", 7_200_000, 16_000, 115)

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+
			signedNotify(t, notifyQueryM(order.OutTradeNo, "1.15")).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String())

	require.Equal(t, int64(16_000), balanceOf(t, p), "入账 16000 毫分（不是 115）")
	stored, err := p.store.GetOrderByID(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, int64(115), stored.PayMinor)
	require.Equal(t, domain.OrderStatusPaid, stored.Status)
}

// seedPricedOrder 种一张**指定定价**的 pending 订单（P22/P23 的金额形态经
// CreateOrder 造不出来：那两条要的是"种行"的历史订单）。
func seedPricedOrder(t *testing.T, p *paymentHarness, id int64, outTradeNo string, rateMicro, creditMilli, payMinor int64) *domain.Order {
	t.Helper()
	order := seedOrder(t, p, id, outTradeNo, paymentMainAccount)
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	row := p.store.orders[order.ID]
	row.RateMicro = rateMicro
	row.CreditMilli = creditMilli
	row.PayMinor = payMinor
	return row
}
