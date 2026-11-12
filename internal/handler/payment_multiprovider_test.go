// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// payment_multiprovider_test.go 多上游（spec v2 §5-1 选路 / §10-3 账户管理）的
// 验收用例 P30–P45，全部跑 fake 面（真实 service + 内存账户/渠道 + 真实 epay 协议）。
//
// 这一批用例的判别点都在「同一个 pay_type 出现在多个账户」时**选了谁、用谁的汇率、
// 用谁的凭据签名、拿谁的账户去查单**——单账户 fixture 永远发现不了 last-wins、
// id 序、全局首渠道汇率、当前主账户查单这四类错。
//
// 坏配置一律**直接种行**（创建接口的 ParseConfig 会 400，经 API 造不出来）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	paymentadminapi "github.com/is7qin/c3api/internal/handler/paymentadmin"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/service/epay"
)

// seedEpayAccount 种一行**合法** epay 账户 + 每个 payType 一条 CNY 渠道
// （rateMicro 一致：本批用例只关心账户维度的差异）。
func seedEpayAccount(t *testing.T, p *paymentHarness, key, pid string, enabled bool, priority int, rateMicro int64, payTypes ...string) *domain.PaymentAccount {
	t.Helper()
	return seedRawEpayAccount(t, p, key, pid, enabled, priority,
		string(epayAccountConfigPID(t, "http://127.0.0.1:1", pid)), rateMicro, payTypes...)
}

// seedRawEpayAccount 同 seedEpayAccount，但 config 原样落库（坏配置 / 缺字段专用）。
func seedRawEpayAccount(t *testing.T, p *paymentHarness, key, pid string, enabled bool, priority int, rawConfig string, rateMicro int64, payTypes ...string) *domain.PaymentAccount {
	t.Helper()
	ctx := t.Context()
	acct, err := p.store.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key: key, Provider: domain.ProviderEpayV2, MerchantRef: pid,
		DisplayName: key, Enabled: enabled, Priority: priority,
		Config: []byte(rawConfig),
	})
	require.NoError(t, err)
	for _, payType := range payTypes {
		_, chErr := p.store.UpsertPaymentChannel(ctx, acct.ID, &domain.PaymentChannel{
			PayType: payType, Enabled: true, Currency: "CNY",
			RateMicro: rateMicro, MinUSD: 1, MaxUSD: 1000,
		})
		require.NoError(t, chErr)
	}
	return acct
}

// createOrder 走用户面建单，返回响应与落库订单（订单行是断言的主对象）。
func createOrder(t *testing.T, p *paymentHarness, amountUSD, payType string) (userapi.CreateOrderResponse, *domain.Order) {
	t.Helper()
	rec := p.do(p.user, http.MethodPost, "/api/user/orders",
		`{"amount_usd":"`+amountUSD+`","pay_type":"`+payType+`"}`, p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created userapi.CreateOrderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	orders, _, err := p.store.ListOrders(t.Context(), repository.OrderQuery{})
	require.NoError(t, err)
	require.Len(t, orders, 1, "一次建单只应落一行")
	require.Equal(t, created.OrderNo, orders[0].OutTradeNo)
	return created, orders[0]
}

// TestPaymentFallsBackToBackupAccount P30（对 M24，**不对 M21**）：主账户 priority=0
// 但 config 坏（种行）→ 选路跳过它，落备用账户；汇率用**备用账户**的 7.5
// （pay_minor=750），不是主账户的 7.2。
func TestPaymentFallsBackToBackupAccount(t *testing.T) {
	p := newPaymentHarnessBare(t, epay.New(nil))
	seedRawEpayAccount(t, p, "epay_bad_main", "2001", true, 0, "{not-a-json", 7_200_000, epay.PayTypeAlipay)
	seedEpayAccount(t, p, "epay_backup", "2002", true, 1, 7_500_000, epay.PayTypeAlipay)

	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	require.Equal(t, "epay_backup", created.Account, "坏配置的主账户必须被跳过")
	require.Equal(t, "epay_backup", order.AccountKey)
	require.Equal(t, int64(750), order.PayMinor, "pay_minor 必须用**选中账户**的 7.5（不是主账户的 7.2）")
	require.Equal(t, "7.50", created.PayAmount)
	require.Equal(t, int64(7_500_000), order.RateMicro)

	// 坏账户仍可观测：列表 config_error 非空（不能静默坏）。
	rec := p.do(p.adminPay, http.MethodGet, "/api/admin/payment/accounts", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var accounts []paymentadminapi.PaymentAccount
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accounts))
	require.Len(t, accounts, 2)
	for _, a := range accounts {
		if a.Key == "epay_bad_main" {
			require.NotNil(t, a.ConfigError, "坏配置账户必须在列表里标红（P33②/M34）")
		}
	}
}

// TestPaymentUsesPriorityNotID P31（杀 M21/M22）：B(7.5, priority=1) **先建**、A(7.2,
// priority=0) 后建——按 id 序会选 B，按 priority 序必须选 A，且汇率用 A 的 7.2。
func TestPaymentUsesPriorityNotID(t *testing.T) {
	p := newPaymentHarnessBare(t, epay.New(nil))
	backup := seedEpayAccount(t, p, "epay_backup", "2002", true, 1, 7_500_000, epay.PayTypeAlipay)
	main := seedEpayAccount(t, p, "epay_main", "2001", true, 0, 7_200_000, epay.PayTypeAlipay)
	require.Less(t, backup.ID, main.ID, "fixture 前提：B 的 id 更小（id 序会选错）")

	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	require.Equal(t, "epay_main", created.Account, "选路必须按 priority，不是 id")
	require.Equal(t, int64(720), order.PayMinor, "pay_minor 必须用选中账户 A 的 7.2")
	require.Equal(t, "7.20", created.PayAmount)
	require.Equal(t, int64(7_200_000), order.RateMicro)
}

// TestPaymentMethodsProjectsResolvedChannel P35（杀 M26/M32）：A 有 alipay(7.2)、
// B 有 wxpay(7.5)+alipay(7.5) → alipay 只出现一次且 `rate` 是**A 的** 7.2，
// wxpay 也出现。methods 必须投影同一个 ResolveChannel，不能自写过滤/last-wins。
func TestPaymentMethodsProjectsResolvedChannel(t *testing.T) {
	p := newPaymentHarnessBare(t, epay.New(nil))
	// 先种一个**停用**的账户（id 最小、汇率最低 6.0）：它不是可售来源，却会让
	// 「自写过滤（首个渠道 wins）」与「last-wins」两种实现露馅——一个取 6.0、
	// 一个取 7.5，只有投影 ResolveChannel 才会得到 7.2。
	seedEpayAccount(t, p, "epay_shadow", "3001", false, 0, 6_000_000, epay.PayTypeAlipay)
	seedEpayAccount(t, p, "epay_main", "2001", true, 0, 7_200_000, epay.PayTypeAlipay)
	seedEpayAccount(t, p, "epay_backup", "2002", true, 1, 7_500_000,
		epay.PayTypeWxpay, epay.PayTypeAlipay)

	rec := p.do(p.user, http.MethodGet, "/api/user/payment/methods", "", p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var methods []userapi.PaymentMethod
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &methods))

	byType := make(map[string]userapi.PaymentMethod, len(methods))
	for _, m := range methods {
		_, dup := byType[m.PayType]
		require.False(t, dup, "pay_type 必须唯一（distinct 投影）：%s", m.PayType)
		byType[m.PayType] = m
	}
	require.Len(t, methods, 2, "alipay 只能出现一次，wxpay 必须出现")
	alipay, ok := byType[epay.PayTypeAlipay]
	require.True(t, ok)
	require.Equal(t, "7.200000", alipay.Rate,
		"alipay 的 rate 必须是 **可售的优先账户 A** 的 7.2（不是停用账户的 6.0，也不是 last-wins 的 7.5）")
	require.Equal(t, int64(7_200_000), mustRateMicro(t, alipay.Rate))
	wxpay, ok := byType[epay.PayTypeWxpay]
	require.True(t, ok)
	require.Equal(t, "7.500000", wxpay.Rate)
	require.Equal(t, int64(1000), alipay.MaxUsd, "max_usd 必须是渠道配置的生效上限")
}

// mustRateMicro 把 methods 的六位小数定点串还原成 rate_micro（判据不依赖格式化实现）。
func mustRateMicro(t *testing.T, rate string) int64 {
	t.Helper()
	var units int64
	var frac string
	for i, r := range rate {
		if r == '.' {
			frac = rate[i+1:]
			head := rate[:i]
			for _, c := range head {
				units = units*10 + int64(c-'0')
			}
			break
		}
	}
	require.Len(t, frac, 6, "rate 必须是六位小数定点串: %q", rate)
	for _, c := range frac {
		units = units*10 + int64(c-'0')
	}
	return units
}

// TestPaymentAccountSoftDeleteKeepsOrder P36：DELETE（含**已有订单**的账户）恒为软停：
// 200、行仍在、enabled=false；重复 DELETE 幂等；既有订单不受影响。
func TestPaymentAccountSoftDeleteKeepsOrder(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()

	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	require.Equal(t, paymentMainAccount, created.Account)

	for i := range 2 {
		rec := p.do(p.adminPay, http.MethodDelete, "/api/admin/payment/accounts/"+paymentMainAccount, "", "")
		require.Equal(t, http.StatusOK, rec.Code, "第 %d 次 DELETE 必须是 200（幂等软停）: %s", i+1, rec.Body.String())
		acct, err := p.store.GetPaymentAccountByKey(ctx, paymentMainAccount)
		require.NoError(t, err, "行必须仍在（物理删除 = 回调 URL 变 404 = 掉单）")
		require.False(t, acct.Enabled)
	}

	stored, err := p.store.GetOrderByID(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPending, stored.Status, "停用账户不得改动既有订单")
	require.Equal(t, paymentMainAccount, stored.AccountKey, "订单快照不变（软停/改名后账本仍可读）")
}

// TestPaymentAccountDisabledStillCredits P18/B53：账户 `enabled=false` 但订单 pending，
// 收到成功回调**必须入账**——开关管"能不能卖"，不管"钱已经收了怎么结算"。
func TestPaymentAccountDisabledStillCredits(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()

	created, order := createOrder(t, p, "1.00", epay.PayTypeAlipay)
	rec := p.do(p.adminPay, http.MethodDelete, "/api/admin/payment/accounts/"+paymentMainAccount, "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String(), "停用账户的迟到回调仍必须入账")

	stored, err := p.store.GetOrderByID(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPaid, stored.Status)
	require.NotNil(t, stored.RedemptionCodeID)
	user, err := p.store.GetUser(ctx, p.userID)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), user.Balance)
}

// TestPaymentMethodsSurviveSamePriority P38（杀 M33）：三账户 priority 全相同 →
// 必须**稳定**选 id 最小者；重复选路 20 次都必须是它（随机 tie-break 不可能全中）。
func TestPaymentMethodsSurviveSamePriority(t *testing.T) {
	p := newPaymentHarnessBare(t, epay.New(nil))
	first := seedEpayAccount(t, p, "epay_first", "2001", true, 0, 7_200_000, epay.PayTypeAlipay)
	seedEpayAccount(t, p, "epay_second", "2002", true, 0, 7_500_000, epay.PayTypeAlipay)
	seedEpayAccount(t, p, "epay_third", "2003", true, 0, 7_600_000, epay.PayTypeAlipay)

	for i := range 20 {
		res, err := p.svc.ResolveChannel(t.Context(), epay.PayTypeAlipay, 1)
		require.NoError(t, err)
		require.Equal(t, "epay_first", res.AccountKey, "第 %d 次：同 priority 必须按 id ASC 稳定取 %d", i+1, first.ID)
		require.Equal(t, int64(7_200_000), res.Channel.RateMicro, "选中账户的汇率必须跟着账户走")
	}
}

// orderRaceStore 在建单落库的瞬间执行 hook：P41 要在「选路完成」与「BuildPayURL」
// 之间把账户改坏（禁用 + 换配置），且不依赖 sleep。
type orderRaceStore struct {
	*fakeStore
	hook func(ctx context.Context)
}

func (s *orderRaceStore) CreateOrder(ctx context.Context, o *domain.Order) (*domain.Order, error) {
	if s.hook != nil {
		s.hook(ctx)
	}
	return s.fakeStore.CreateOrder(ctx, o)
}

// TestPaymentCreateOrderUsesResolvedSnapshot P41/B50：建单瞬间账户被禁用 + 配置被改
// → **本次下单仍成功**（用选路快照的 cfg 签名），下一次选路才受影响。
func TestPaymentCreateOrderUsesResolvedSnapshot(t *testing.T) {
	p := newPaymentHarnessBare(t, epay.New(nil))
	seedEpayAccount(t, p, "epay_main", "2001", true, 0, 7_200_000, epay.PayTypeAlipay)
	seedEpayAccount(t, p, "epay_backup", "2002", true, 1, 7_500_000, epay.PayTypeAlipay)
	ctx := t.Context()

	race := &orderRaceStore{fakeStore: p.store}
	race.hook = func(ctx context.Context) {
		// 选路已经完成：现在把主账户停用，并把 config 换成坏的。
		_, err := p.store.UpdatePaymentAccount(ctx, "epay_main", repository.AccountUpdate{
			Enabled: new(false),
		})
		require.NoError(t, err)
	}
	svc := service.New(service.Deps{
		Store: race, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Auth: &fakeKeys{}, EmailCodeStore: p.store, SettingsSnapshot: p.snap,
		PaymentProviders: []domain.PaymentProvider{epay.New(nil)},
	})

	// 建单直接打 service（P41 的判别点是选路快照与 BuildPayURL 的时序，
	// 与 HTTP 外壳无关）。
	first, err := svc.CreateOrder(ctx, p.userID, service.CreateOrderInput{
		AmountUSD: "1.00", PayType: "alipay",
	})
	require.NoError(t, err, "账户在选路之后被停用，不得让本次下单失败")
	require.Equal(t, "epay_main", first.Order.AccountKey, "本次下单用的是选路快照")
	require.Equal(t, int64(720), first.Order.PayMinor, "签名用的是选路时的 cfg（快照汇率）")
	require.NotEmpty(t, first.PayURL)

	// 下次选路受影响：主账户已停用 → 落备用账户，汇率随之变化。
	second, err := svc.CreateOrder(ctx, p.userID, service.CreateOrderInput{
		AmountUSD: "1.00", PayType: "alipay",
	})
	require.NoError(t, err)
	require.Equal(t, "epay_backup", second.Order.AccountKey, "下一次选路必须看到停用后的世界")
	require.Equal(t, int64(750), second.Order.PayMinor)
}

// TestPaymentNotifyBrokenConfigKeepsOrder P42/B51：回调到达时账户 config 已坏 →
// **503**（网关重试），订单行**一个字段都不动**（既不入账也不写 fail_reason）。
func TestPaymentNotifyBrokenConfigKeepsOrder(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()
	order := seedOrder(t, p, 41, "P20260101000041ABCDEFGHJK", paymentBrokenAccount)

	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentBrokenAccount+"/notify?pid=1002&out_trade_no="+order.OutTradeNo+
			"&money=72.00&trade_status=TRADE_SUCCESS&sign=deadbeef&sign_type=RSA", "", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())

	stored, err := p.store.GetOrderByID(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPending, stored.Status, "503 不得改状态")
	require.Nil(t, stored.FailReason, "配置坏属于 5xx（可修复），不是可判定的失败信号")
	require.Zero(t, stored.FailCount)
	require.Nil(t, stored.PaidAt)
	require.Nil(t, stored.RedemptionCodeID)
}

// TestPaymentAccountSecretPlaceholderKeepsValue P34/B47：PUT 时 `private_key="***"`
// → 库内**原值不变**，且 `config_revision` 递增（配置确实被写过）。
func TestPaymentAccountSecretPlaceholderKeepsValue(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()
	before, err := p.store.GetPaymentAccountByKey(ctx, paymentMainAccount)
	require.NoError(t, err)
	var oldCfg map[string]string
	require.NoError(t, json.Unmarshal(before.Config, &oldCfg))
	require.NotEqual(t, "***", oldCfg[epay.ConfigFieldPrivateKey])

	body := `{"config":{"base_url":"https://gw.example.com","pid":"1001","private_key":"***","public_key":"` +
		escapeJSON(oldCfg[epay.ConfigFieldPublicKey]) + `"}}`
	rec := p.do(p.adminPay, http.MethodPut, "/api/admin/payment/accounts/"+paymentMainAccount, body, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := p.store.GetPaymentAccountByKey(ctx, paymentMainAccount)
	require.NoError(t, err)
	var newCfg map[string]string
	require.NoError(t, json.Unmarshal(after.Config, &newCfg))
	require.Equal(t, oldCfg[epay.ConfigFieldPrivateKey], newCfg[epay.ConfigFieldPrivateKey],
		"`***` 是占位符，绝不能被写成真实值（否则该账户从此签不出可验的 URL）")
	require.Equal(t, before.ConfigRevision+1, after.ConfigRevision, "config 确实被写过 → revision 递增")

	// 回显仍是掩码（读面绝不漏密钥）。
	reqRec := p.do(p.adminPay, http.MethodGet, "/api/admin/payment/accounts", "", "")
	require.Equal(t, http.StatusOK, reqRec.Code, reqRec.Body.String())
	var accounts []paymentadminapi.PaymentAccount
	require.NoError(t, json.Unmarshal(reqRec.Body.Bytes(), &accounts))
	for _, a := range accounts {
		if a.Key == paymentMainAccount {
			require.Equal(t, "***", a.Config[epay.ConfigFieldPrivateKey])
		}
	}
}

// escapeJSON 把字符串塞进手写 JSON body（PEM 里有换行）。
func escapeJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b[1 : len(b)-1])
}

// TestPaymentSyncUsesOrderSnapshotAccount P37/§9（杀 M27）：订单快照 A、当前主已切到
// B → sync 只许查 A（调用计数 A=1、B=0）。
func TestPaymentSyncUsesOrderSnapshotAccount(t *testing.T) {
	provider := newCountingProvider("counting_v1")
	p := newPaymentHarnessBare(t, provider)
	ctx := t.Context()

	// 先建 A（快照账户）与 B（后来的主账户）；provider 是计数桩，不联网。
	acctA, err := p.store.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key: "acct_a", Provider: "counting_v1", MerchantRef: "a",
		DisplayName: "a", Enabled: true, Priority: 1,
		Config: []byte(`{"merchant_ref":"a"}`),
	})
	require.NoError(t, err)
	_, err = p.store.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key: "acct_b", Provider: "counting_v1", MerchantRef: "b",
		DisplayName: "b", Enabled: true, Priority: 0,
		Config: []byte(`{"merchant_ref":"b"}`),
	})
	require.NoError(t, err)
	for _, acct := range []struct {
		id      int64
		payType string
	}{{acctA.ID, epay.PayTypeAlipay}} {
		_, chErr := p.store.UpsertPaymentChannel(ctx, acct.id, &domain.PaymentChannel{
			PayType: acct.payType, Enabled: true, Currency: "CNY",
			RateMicro: 7_200_000, MinUSD: 1, MaxUSD: 1000,
		})
		require.NoError(t, chErr)
	}

	// 订单快照 = acct_a（即使 acct_b 现在是 priority 0 的主账户）。
	order := seedOrder(t, p, 71, "P20260101000071ABCDEFGHJK", "acct_a")
	rec := p.do(p.admin, http.MethodPost, "/api/admin/orders/71/sync", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, 1, provider.count("a"), "sync 必须查订单快照账户（M27）")
	require.Equal(t, 0, provider.count("b"), "当前主账户一次都不该被查")

	stored, err := p.store.GetOrderByID(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPending, stored.Status, "查单报非 paid：只落 raw_notify，不迁移状态")
	require.NotNil(t, stored.RawNotify)
}

// countingConfig 计数桩账户凭据（密封接口的最小实现：嵌 AccountConfigSeal）。
type countingConfig struct {
	domain.AccountConfigSeal
	ref string
}

func (c *countingConfig) Provider() domain.ProviderName { return "counting_v1" }
func (c *countingConfig) MerchantRef() string          { return c.ref }

// countingProvider 计数型协议实现（P37 的判据是**调用次数**，不是返回值形状）。
type countingProvider struct {
	name string
	mu   sync.Mutex
	hits map[string]int
	// paidMinor > 0 ⇒ Query 回「已付款，实付 = paidMinor」（P29 的关单前置查询）。
	paidMinor int64
}

func newCountingProvider(name string) *countingProvider {
	return &countingProvider{name: name, hits: make(map[string]int, 2)}
}

func (p *countingProvider) Name() domain.ProviderName   { return domain.ProviderName(p.name) }
func (p *countingProvider) Capabilities() []string      { return []string{epay.PayTypeAlipay} }
func (p *countingProvider) SecretFields() []string      { return nil }
func (p *countingProvider) ParseConfig(raw []byte) (domain.AccountConfig, error) {
	var cfg struct {
		MerchantRef string `json:"merchant_ref"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.MerchantRef == "" {
		return nil, domain.ErrConfigMissing
	}
	return &countingConfig{ref: cfg.MerchantRef}, nil
}

func (p *countingProvider) BuildPayURL(domain.AccountConfig, domain.PayRequest) (string, error) {
	return "https://counting.invalid/pay", nil
}

func (p *countingProvider) VerifyNotify(domain.AccountConfig, url.Values) (domain.NotifyEvent, error) {
	return domain.NotifyEvent{}, errCountingUnsupported
}

// Query 计数 + 回「非 paid」：sync 的断言只看打了谁，不看状态迁移。
func (p *countingProvider) Query(_ context.Context, cfg domain.AccountConfig, _ string) (domain.NotifyEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits[cfg.MerchantRef()]++
	event := domain.NotifyEvent{
		Status: domain.NotifyStatusOther,
		Raw:    map[string]string{"trade_status": "WAIT"},
	}
	if p.paidMinor > 0 {
		event.Status = domain.NotifyStatusPaid
		event.PaidMinor = p.paidMinor
		event.TradeNo = "GW-PAID"
		event.Raw = map[string]string{"trade_status": "TRADE_SUCCESS"}
	}
	return event, nil
}

func (p *countingProvider) count(ref string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[ref]
}

var errCountingUnsupported = &countingUnsupportedError{}

type countingUnsupportedError struct{}

func (*countingUnsupportedError) Error() string { return "counting provider: notify unsupported" }

// 编译期断言：计数桩必须同时满足协议面与可选查单能力。
var (
	_ domain.PaymentProvider = (*countingProvider)(nil)
	_ domain.OrderQuerier    = (*countingProvider)(nil)
)
