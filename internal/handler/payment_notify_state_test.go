// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// payment_notify_state_test.go 回调**状态机与审计**的验收用例（spec v1 §11 P5–P29 与
// v2 §8 错误映射表）：金额/验签/状态/过期/关单/用户态/注入故障/关单前置查询。
//
// 全部跑 fake 面（真实 service + 真实 epay 签名链 + 内存订单表）。判据是**订单行的
// 可观测字段**（status / fail_reason / fail_count / raw_notify / trade_no /
// redemption_code_id）与**余额/凭码行数**——不是日志、不是返回值形状。
//
// 「不写审计」的那几条（验签失败/重复键/必填缺失）是资损口径：单号还没被证明属于
// 我们，落 fail_reason 等于允许伪造签名污染任意订单行。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service/epay"
)

// postNotify 用默认商户密钥签一组通知（mutate 在签名**之前**应用）并打回调。
func postNotify(t *testing.T, p *paymentHarness, account string, mutate func(url.Values)) *httptest.ResponseRecorder {
	t.Helper()
	params := notifyQuery(testOrderNo)
	if mutate != nil {
		mutate(params)
	}
	return p.do(p.pay, http.MethodGet, "/api/payment/"+account+"/notify?"+signedNotify(t, params).Encode(), "", "")
}

// notifyQueryM notifyQuery 的金额可变版本（P5/P6b 要按分差驱动金额比对）。
func notifyQueryM(outTradeNo, money string) url.Values {
	params := notifyQuery(outTradeNo)
	params.Set("money", money)
	return params
}

// orderSnapshot 订单行的「状态机 + 审计」字段快照（P6 的"完全不变"判据）。
type orderSnapshot struct {
	Status           domain.OrderStatus
	FailReason       string
	FailCount        int
	HasLastNotifyAt  bool
	HasRawNotify     bool
	HasTradeNo       bool
	HasPaidAt        bool
	HasRedemptionCde bool
}

func snapshotOrder(t *testing.T, p *paymentHarness, id int64) orderSnapshot {
	t.Helper()
	o, err := p.store.GetOrderByID(t.Context(), id)
	require.NoError(t, err)
	snap := orderSnapshot{
		Status: o.Status, FailCount: o.FailCount,
		HasLastNotifyAt: o.LastNotifyAt != nil, HasRawNotify: o.RawNotify != nil,
		HasTradeNo: o.TradeNo != nil, HasPaidAt: o.PaidAt != nil,
		HasRedemptionCde: o.RedemptionCodeID != nil,
	}
	if o.FailReason != nil {
		snap.FailReason = *o.FailReason
	}
	return snap
}

// balanceOf 读收款人余额（入账的唯一外部可观测结果）。
func balanceOf(t *testing.T, p *paymentHarness) int64 {
	t.Helper()
	u, err := p.store.GetUser(t.Context(), p.userID)
	require.NoError(t, err)
	return u.Balance
}

// TestPaymentNotifyAmountMismatch P5/P6b（杀金额等值变异）：签名合法但金额差 1 分
// → 200 fail + `fail_reason=amount` + `fail_count=1`，订单仍 pending、余额不变。
func TestPaymentNotifyAmountMismatch(t *testing.T) {
	for _, money := range []string{"7.19", "1.00", "7.210"} {
		t.Run("money="+money, func(t *testing.T) {
			p := newPaymentHarness(t)
			created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
			require.Equal(t, int64(720), order.PayMinor)

			rec := p.do(p.pay, http.MethodGet,
				"/api/payment/"+paymentMainAccount+"/notify?"+
					signedNotify(t, notifyQueryM(created.OrderNo, money)).Encode(), "", "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, "fail", rec.Body.String())

			snap := snapshotOrder(t, p, order.ID)
			require.Equal(t, domain.OrderStatusPending, snap.Status, "金额不等不得迁移状态")
			require.Equal(t, domain.OrderFailReasonAmount, snap.FailReason, "金额漂移必须落 fail_reason=amount")
			require.Equal(t, 1, snap.FailCount, "fail_count 必须 +1（运维闭环的唯一计数信号）")
			require.True(t, snap.HasLastNotifyAt)
			require.False(t, snap.HasRedemptionCde)
			require.Zero(t, balanceOf(t, p), "金额不等绝不入账")
		})
	}
}

// TestPaymentNotifyBadSignatureLeavesRowUntouched P6：签名错 → 200 fail，订单行
// **完全不变**（连 fail_reason / fail_count / last_notify_at 都不写）。
func TestPaymentNotifyBadSignatureLeavesRowUntouched(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	before := snapshotOrder(t, p, order.ID)

	// 签名后再篡改金额：签名与内容不再匹配。
	tampered := signedNotify(t, notifyQuery(created.OrderNo))
	tampered.Set("money", "7.19")
	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+tampered.Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	require.Equal(t, before, snapshotOrder(t, p, order.ID),
		"验签失败不得改动订单行（单号未验证 → 写审计等于允许伪造签名污染任意订单）")
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifyStatusNotPaid P8：`trade_status=WAIT`（验签通过）→ 200 fail +
// `fail_reason=status`，不入账。
func TestPaymentNotifyStatusNotPaid(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)

	rec := postNotify(t, p, paymentMainAccount, func(v url.Values) {
		v.Set("out_trade_no", created.OrderNo)
		v.Set("trade_status", "WAIT")
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	snap := snapshotOrder(t, p, order.ID)
	require.Equal(t, domain.OrderStatusPending, snap.Status)
	require.Equal(t, domain.OrderFailReasonStatus, snap.FailReason)
	require.Equal(t, 1, snap.FailCount)
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifyExpiredOrderStillCredits P9/B10：「过期」不是状态，只是
// `pending && expires_at < now` 的派生视图 → 过期单收到成功通知照样入账。
func TestPaymentNotifyExpiredOrderStillCredits(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)

	// 把订单挪到一小时前过期（时间不是状态：只改过期时刻，不改 status）。
	p.store.mu.Lock()
	p.store.orders[order.ID].ExpiresAt = time.Now().UTC().Add(-time.Hour)
	p.store.mu.Unlock()

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String(), "过期单必须照样入账")

	snap := snapshotOrder(t, p, order.ID)
	require.Equal(t, domain.OrderStatusPaid, snap.Status)
	require.True(t, snap.HasPaidAt)
	require.True(t, snap.HasRedemptionCde)
	require.Equal(t, int64(100_000), balanceOf(t, p))
}

// TestPaymentNotifyClosedOrderAuditsClosed P10/B11：已关单收到成功通知 → 200 fail +
// **`fail_reason=closed` 确实落库** + `fail_count=1`，不入账（资损信号，须人工介入）。
func TestPaymentNotifyClosedOrderAuditsClosed(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)

	closed, err := p.store.CloseOrder(t.Context(), order.ID, nil)
	require.NoError(t, err)
	require.True(t, closed)

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	snap := snapshotOrder(t, p, order.ID)
	require.Equal(t, domain.OrderStatusClosed, snap.Status, "closed 是终态，不得回退")
	require.Equal(t, domain.OrderFailReasonClosed, snap.FailReason, "已付款的关单必须留下人工介入信号")
	require.Equal(t, 1, snap.FailCount)
	require.False(t, snap.HasRedemptionCde)
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifyAccountMismatchIsNotAmount P21/B42（杀 M23）：回调落在**另一个**
// 账户上（等额同币、该账户自己的合法签名）→ 200 fail + `fail_reason=account`，
// 绝不是 amount，也不入账。
func TestPaymentNotifyAccountMismatchIsNotAmount(t *testing.T) {
	p := newPaymentHarness(t)
	// 额外一个合法账户（不同 pid、同一对密钥），用它自己的 pid 签名。
	seedEpayAccount(t, p, "epay_other", "3001", true, 1, 7_200_000, epay.PayTypeAlipay)

	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	require.Equal(t, paymentMainAccount, order.AccountKey)

	params := notifyQuery(created.OrderNo)
	params.Set("pid", "3001") // 用 epay_other 的身份（金额/币种与订单完全一致）
	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/epay_other/notify?"+signedNotify(t, params).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	snap := snapshotOrder(t, p, order.ID)
	require.Equal(t, domain.OrderStatusPending, snap.Status)
	require.Equal(t, domain.OrderFailReasonAccount, snap.FailReason,
		"等额同币的跨账户回调必须归因为 account（不是 amount）")
	require.Equal(t, 1, snap.FailCount)
	require.False(t, snap.HasRedemptionCde)
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifyProviderMismatch §6-2 第 9 步：路径账户的协议与订单快照不符 →
// 200 fail + `fail_reason=provider`（与 account 分开归因，便于查"协议错放"）。
func TestPaymentNotifyProviderMismatch(t *testing.T) {
	p := newPaymentHarness(t)
	// 种一行 provider 快照与账户不同的订单（历史脏数据/协议迁移残留的形态）。
	order := seedOrder(t, p, 21, "P20260101000021ABCDEFGHJK", paymentMainAccount)
	p.store.mu.Lock()
	p.store.orders[order.ID].Provider = domain.ProviderName("epay_v1_retired")
	p.store.mu.Unlock()

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+
			signedNotify(t, notifyQuery("P20260101000021ABCDEFGHJK")).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	snap := snapshotOrder(t, p, order.ID)
	require.Equal(t, domain.OrderStatusPending, snap.Status)
	require.Equal(t, domain.OrderFailReasonProvider, snap.FailReason)
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifyUnavailableUser P26/B30：用户被禁后通知到达 → 不入账、fail +
// `fail_reason=user`（钱已收、解禁后可补码并 resolve）。
func TestPaymentNotifyUnavailableUser(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)

	p.store.mu.Lock()
	p.store.users[p.userID].Status = domain.UserStatusDisabled
	p.store.mu.Unlock()

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	snap := snapshotOrder(t, p, order.ID)
	require.Equal(t, domain.OrderStatusPending, snap.Status, "被禁用户不得抵消订单状态")
	require.Equal(t, domain.OrderFailReasonUser, snap.FailReason)
	require.Equal(t, 1, snap.FailCount)
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifySignTypeMustBeRSA P27（杀 M29 的 handler 面）：`sign_type=MD5`
// 但 RSA 签名本身合法 → 200 fail，且**不写审计**（算法混淆属输入错误）。
func TestPaymentNotifySignTypeMustBeRSA(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	before := snapshotOrder(t, p, order.ID)

	params := signedNotify(t, notifyQuery(created.OrderNo))
	params.Set("sign_type", "MD5")
	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+params.Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	require.Equal(t, before, snapshotOrder(t, p, order.ID), "sign_type 不符不得写审计")
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifyMissingMoneyResigned P28/B38：剔除 `money` 后**用商户私钥重签**
// → 200 fail、不入账。重签是关键：不重签测到的是验签失败，而不是"必填存在性"。
func TestPaymentNotifyMissingMoneyResigned(t *testing.T) {
	p := newPaymentHarness(t)
	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	before := snapshotOrder(t, p, order.ID)

	params := notifyQuery(created.OrderNo)
	params.Del("money")
	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, params).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	require.Equal(t, before, snapshotOrder(t, p, order.ID), "重签后的必填缺失也不得写审计")
	require.Zero(t, balanceOf(t, p))
}

// TestPaymentNotifyTransientErrorsRetried P25/B33（F38）：连续 3 次 40P01 → 500 +
// `fail_reason=internal` + 无码 + 余额不变；2 次 40P01 + 1 次成功 → 只入账一次。
func TestPaymentNotifyTransientErrorsRetried(t *testing.T) {
	deadlock := func() error { return &pgconn.PgError{Code: "40P01"} }

	t.Run("三次全死锁", func(t *testing.T) {
		p := newPaymentHarness(t)
		created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
		p.store.orderErrs = newErrQueue(deadlock(), deadlock(), deadlock())

		rec := p.do(p.pay, http.MethodGet,
			"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
		require.Equal(t, http.StatusInternalServerError, rec.Code,
			"重试耗尽的唯一出口是 500（让网关重试），不是 200: %s", rec.Body.String())
		require.Equal(t, "fail", rec.Body.String())

		snap := snapshotOrder(t, p, order.ID)
		require.Equal(t, domain.OrderStatusPending, snap.Status)
		require.Equal(t, domain.OrderFailReasonInternal, snap.FailReason)
		require.False(t, snap.HasRedemptionCde)
		require.Zero(t, balanceOf(t, p))
	})

	t.Run("两次死锁后成功", func(t *testing.T) {
		p := newPaymentHarness(t)
		created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
		p.store.orderErrs = newErrQueue(deadlock(), deadlock())

		rec := p.do(p.pay, http.MethodGet,
			"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "success", rec.Body.String(), "可重试冲突必须被重试掉")

		snap := snapshotOrder(t, p, order.ID)
		require.Equal(t, domain.OrderStatusPaid, snap.Status)
		require.Equal(t, int64(100_000), balanceOf(t, p), "只入账一次")
	})
}

// TestPaymentCloseRefusesPaidOrder P29（杀 M31）：本地 pending 但**网关 replied paid**
// → 关单必须拒绝并转入账（success 语义），订单 paid 且有 1 张码；
// 而本地已 paid 的单再关 → 409（只有 pending 可关）。
func TestPaymentCloseRefusesPaidOrder(t *testing.T) {
	provider := newCountingProvider("counting_v1")
	provider.paidMinor = 7200
	p := newPaymentHarnessBare(t, provider)

	acct, err := p.store.CreatePaymentAccount(t.Context(), &domain.PaymentAccount{
		Key: "acct_a", Provider: "counting_v1", MerchantRef: "a",
		DisplayName: "a", Enabled: true, Priority: 0,
		Config: []byte(`{"merchant_ref":"a"}`),
	})
	require.NoError(t, err)
	_, err = p.store.UpsertPaymentChannel(t.Context(), acct.ID, &domain.PaymentChannel{
		PayType: epay.PayTypeAlipay, Enabled: true, Currency: "CNY",
		RateMicro: 7_200_000, MinUSD: 1, MaxUSD: 1000,
	})
	require.NoError(t, err)

	order := seedOrder(t, p, 29, "P20260101000029ABCDEFGHJK", "acct_a")
	require.Equal(t, int64(7200), order.PayMinor)

	rec := p.do(p.admin, http.MethodPost, "/api/admin/orders/29/close", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "网关报 paid：关单必须被拒绝并转入账: %s", rec.Body.String())
	var result OrderCloseResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Equal(t, OrderStatus("paid"), result.Status, "拒绝关单 → 状态是 paid")
	require.True(t, result.Credited, "本次调用必须完成入账")

	stored, err := p.store.GetOrderByID(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, stored.Status)
	require.NotNil(t, stored.RedemptionCodeID, "paid 订单必须有且只有 1 张到账码")
	require.Len(t, p.store.codes, 1)
	require.Equal(t, int64(1_000_000), balanceOf(t, p), "入账金额 = credit_milli")

	// 已 paid 的单再关 → 409（不是"再关一次"）。
	rec = p.do(p.admin, http.MethodPost, "/api/admin/orders/29/close", "", "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// TestPaymentMethodsOnlySellableChannels P17/B19：methods 只列**可售**渠道
// （停用渠道/非法币种不出现），`max_usd` 是**生效**上限（被汇率可表示范围收敛）。
func TestPaymentMethodsOnlySellableChannels(t *testing.T) {
	p := newPaymentHarnessBare(t, epay.New(nil))
	ctx := t.Context()

	cases := []struct {
		key     string
		pid     string
		enabled bool
		payType string
		amount  int64
		rate    int64
		chOn    bool
		cur     string
	}{
		{"acct_ok", "3001", true, epay.PayTypeAlipay, 1, 7_200_000, true, "CNY"},   // max_usd 见下方
		{"acct_off", "3002", true, epay.PayTypeWxpay, 1, 7_200_000, false, "CNY"},  // 渠道停用
		{"acct_badcur", "3003", true, epay.PayTypeWxpay, 1, 7_200_000, true, "JPY"}, // 非白名单币种
	}
	for _, c := range cases {
		acct := seedEpayAccount(t, p, c.key, c.pid, c.enabled, 1, c.rate)
		// 配置 1e7 超出 7.2 汇率的可表示范围 → methods 必须收敛到 6405119。
		maxUSD := int64(10_000_000)
		_, err := p.store.UpsertPaymentChannel(ctx, acct.ID, &domain.PaymentChannel{
			PayType: c.payType, Enabled: c.chOn, Currency: c.cur,
			RateMicro: c.rate, MinUSD: c.amount, MaxUSD: maxUSD,
		})
		require.NoError(t, err)
	}

	rec := p.do(p.user, http.MethodGet, "/api/user/payment/methods", "", p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var methods []userapi.PaymentMethod
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &methods))

	byType := make(map[string]userapi.PaymentMethod, len(methods))
	for _, m := range methods {
		byType[m.PayType] = m
	}
	require.Contains(t, byType, epay.PayTypeAlipay, "可售渠道必须出现")
	require.NotContains(t, byType, epay.PayTypeWxpay,
		"停用渠道/JPY 渠道都不可售——wxpay 只有这两行，必须整项消失")
	require.Equal(t, int64(6405119), byType[epay.PayTypeAlipay].MaxUsd,
		"max_usd 必须是**生效**上限（配置 1e7 → 收敛到 (MaxInt64/2)/rate/1e5 = 6405119）")
}

// TestPaymentOrderRecordsChosenProvider P19/B45：methods 给的 pay_type 建单后，
// 落库 `provider` 必须与 methods 的 provider 一致（不是"当前默认协议"）。
func TestPaymentOrderRecordsChosenProvider(t *testing.T) {
	p := newPaymentHarness(t)
	rec := p.do(p.user, http.MethodGet, "/api/user/payment/methods", "", p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var methods []userapi.PaymentMethod
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &methods))
	require.NotEmpty(t, methods)

	for _, m := range methods {
		created, order := createOrderOn(t, p, "1.00", m.PayType)
		require.Equal(t, m.Provider, string(order.Provider),
			"订单的 provider 快照必须等于 methods 声明的协议（%s）", m.PayType)
		require.Equal(t, m.Rate, created.Rate)
	}
}

// createOrderOn 建单但不假设订单表只有 1 行（P19 会连建多张）。
func createOrderOn(t *testing.T, p *paymentHarness, amountUSD, payType string) (userapi.CreateOrderResponse, *domain.Order) {
	t.Helper()
	rec := p.do(p.user, http.MethodPost, "/api/user/orders",
		`{"amount_usd":"`+amountUSD+`","pay_type":"`+payType+`"}`, p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created userapi.CreateOrderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	orders, _, err := p.store.ListOrders(t.Context(), repository.OrderQuery{OutTradeNo: created.OrderNo})
	require.NoError(t, err)
	require.Len(t, orders, 1)
	return created, orders[0]
}
