// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// payment_routes_test.go 支付网关 B 批的**路由层**单测（spec v2 §5/§6/§8/§10）：
// 真实 service + fake store（含内存账户/渠道）+ 真实 epay 协议实现，装配方式与
// cmd/server 同序（settings 快照 → 协议 → service → 三条子路由）。
//
// 覆盖的都是"接错线就资损/雪崩"的形状：
//   - 回调按**账户 key** 路由：key 不存在 → 404（不是 500、更不是入账）
//   - 账户 config 坏 → 503（让网关重试）；enabled=false 仍照常入账
//   - notify 缺必填 → 200 fail 且不动订单行
//   - 管理面账户 CRUD：掩码、config_error、capabilities 400、软停幂等
//   - 端到端：选路 → 建单 → 用账户私钥签回调 → 入账 → 余额
//   - 订单管理面：未知订单 404、resolve 幂等 409、查单失败 502 + force 关单

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	paymentapi "github.com/is7qin/c3api/internal/handler/payment"
	paymentadminapi "github.com/is7qin/c3api/internal/handler/paymentadmin"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/service/epay"
	"github.com/is7qin/c3api/internal/settingssnap"
)

// testOrderNo 形状合法的订单号（P + 14 位 UTC + 10 位 base32，字符集 A-Z 去 I/O
// + 2-9）：domain.ValidOutTradeNo 的锚点用例。
const testOrderNo = "P20260101000000ABCDEFGHJK"

// epayKeyPair 测试用 2048-bit 商户密钥对（PEM）。
type epayKeyPair struct{ privatePEM, publicPEM string }

var (
	epayKeyOnce sync.Once
	epayKeys    epayKeyPair
)

func epayTestKeys(t *testing.T) epayKeyPair {
	t.Helper()
	epayKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate rsa key: %v", err)
		}
		privDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal private key: %v", err)
		}
		pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		epayKeys = epayKeyPair{
			privatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
			publicPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		}
	})
	return epayKeys
}

// epayAccountConfig 生成一份合法的账户 config（base_url 故意指向不可达地址：
// notify 面不联网，查单面用它造"查询本身失败 → 502"）。
func epayAccountConfig(t *testing.T, baseURL string) []byte {
	t.Helper()
	return epayAccountConfigPID(t, baseURL, "1001")
}

// epayAccountConfigPID 同上，商户号可指定（唯一索引 (provider, merchant_ref) 的用例需要）。
func epayAccountConfigPID(t *testing.T, baseURL, pid string) []byte {
	t.Helper()
	keys := epayTestKeys(t)
	raw, err := json.Marshal(map[string]string{
		epay.ConfigFieldBaseURL:    baseURL,
		epay.ConfigFieldPID:        pid,
		epay.ConfigFieldPrivateKey: keys.privatePEM,
		epay.ConfigFieldPublicKey:  keys.publicPEM,
	})
	require.NoError(t, err)
	return raw
}

// paymentHarness 支付相关路由的测试接线。
type paymentHarness struct {
	store *fakeStore
	svc   *service.Service
	snap  *settingssnap.Snapshot
	// pay /api/payment/*（公开面，无鉴权）
	pay http.Handler
	// admin /api/admin/*（管理面订单；本测试不装 adminAuth，直接打路由）
	admin http.Handler
	// adminPay /api/admin/payment/*（账户/渠道管理面）
	adminPay http.Handler
	// user /api/user/*（身份鉴权由 RequireIdentity 完成）
	user  http.Handler
	token string
	// userID 支付入账的收款人。
	userID int64
}

const (
	// paymentMainAccount 正常账户（回调/建单/端到端都走它）。
	paymentMainAccount = "epay_main"
	// paymentBrokenAccount config 坏掉的账户（种行造：创建接口会 400）。
	paymentBrokenAccount = "epay_broken"
)

func newPaymentHarness(t *testing.T) *paymentHarness {
	t.Helper()
	p := newPaymentHarnessBare(t, epay.New(&http.Client{Timeout: 2 * time.Second}))
	seedDefaultPaymentAccounts(t, p)
	return p
}

// seedDefaultPaymentAccounts 默认双账户 fixture：正常账户（两个渠道）+ 坏配置账户。
func seedDefaultPaymentAccounts(t *testing.T, p *paymentHarness) {
	t.Helper()
	ctx := t.Context()
	store := p.store
	mainAcct, err := store.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key: paymentMainAccount, Provider: domain.ProviderEpayV2, MerchantRef: "1001",
		DisplayName: "epay main", Enabled: true, Priority: 0,
		Config: epayAccountConfig(t, "http://127.0.0.1:1"),
	})
	require.NoError(t, err)
	for _, ch := range []*domain.PaymentChannel{
		{PayType: epay.PayTypeAlipay, Enabled: true, Currency: "CNY", RateMicro: 7_200_000, MinUSD: 1, MaxUSD: 1000},
		{PayType: epay.PayTypeWxpay, Enabled: true, Currency: "CNY", RateMicro: 7_200_000, MinUSD: 1, MaxUSD: 1000},
	} {
		_, chErr := store.UpsertPaymentChannel(ctx, mainAcct.ID, ch)
		require.NoError(t, chErr)
	}
	_, err = store.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key: paymentBrokenAccount, Provider: domain.ProviderEpayV2, MerchantRef: "1002",
		DisplayName: "epay broken", Enabled: true, Priority: 1,
		Config: []byte("{not-a-json"),
	})
	require.NoError(t, err)
}

// newPaymentHarnessBare 只接线 store / settings / service / 四条子路由 + 一个可登录
// 用户，**不种任何支付账户**：多上游用例要自己控制账户行（priority / 汇率 / 坏配置
// 的形状本身就是断言对象）。
func newPaymentHarnessBare(t *testing.T, providers ...domain.PaymentProvider) *paymentHarness {
	t.Helper()
	ctx := t.Context()
	store := newFakeStore()
	// main 装配同序：先共享 settings 快照（首载失败仅 Warn）→ 协议 → service。
	snap := settingssnap.New(store, nil)
	require.NoError(t, snap.Load(ctx))
	// 支付开关 + 回调/门户基址（v2 只剩这 4 键，凭据在账户行里）。
	for key, value := range map[string]struct {
		typ domain.SettingType
		val string
	}{
		domain.SettingPaymentEnabled:  {domain.SettingTypeSwitch, "true"},
		domain.SettingNotifyBaseURL:   {domain.SettingTypeString, "https://api.example.com"},
		domain.SettingOrderTTLMinutes: {domain.SettingTypeNumber, "30"},
	} {
		_, err := store.SetSetting(ctx, key, value.typ, value.val)
		require.NoError(t, err)
	}
	require.NoError(t, snap.Reload(ctx))
	paymentRegistry, err := domain.NewProviderRegistry(providers)
	require.NoError(t, err)
	svc := service.New(service.Deps{
		Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Auth: &fakeKeys{}, EmailCodeStore: store, SettingsSnapshot: snap,
		PaymentRegistry: paymentRegistry,
	})

	iss := auth.NewIssuer("test-secret")
	u, err := store.CreateUser(ctx, &domain.User{
		Email: "payer@example.com", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.UserStatusActive,
	})
	require.NoError(t, err)
	token, err := iss.Issue(u.ID, u.Email, string(u.Role), u.TokenVersion)
	require.NoError(t, err)
	return &paymentHarness{
		store: store, svc: svc, snap: snap,
		pay:      paymentapi.Router(svc, nil),
		admin:    New(svc).Router(),
		adminPay: paymentadminapi.New(svc, nil).Router(),
		user:     userapi.Router(svc, iss, fakeUserStatus{store: store}, nil, nil, userapi.Features{}),
		token:    token,
		userID:   u.ID,
	}
}

// do 打一条路由（h 为四条子路由之一；token 非空时带 Authorization）。
func (p *paymentHarness) do(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// signedNotify 用商户私钥签一组通知参数（sign_type 已设，签名剔除它）。
func signedNotify(t *testing.T, params url.Values) url.Values {
	t.Helper()
	sign, err := epay.SignValues(params, epayTestKeys(t).privatePEM)
	require.NoError(t, err)
	params.Set("sign", sign)
	params.Set("sign_type", "RSA")
	return params
}

// notifyQuery 一组通过验签的最小通知参数（trade_status 成功、金额 7.20）。
func notifyQuery(outTradeNo string) url.Values {
	params := url.Values{}
	params.Set("pid", "1001")
	params.Set("out_trade_no", outTradeNo)
	params.Set("trade_no", "GW-9")
	params.Set("money", "7.20")
	params.Set("trade_status", "TRADE_SUCCESS")
	params.Set("type", epay.PayTypeAlipay)
	params.Set("timestamp", "1762900000")
	return params
}

// TestPaymentNotifyUnknownAccount 账户 key 不存在 → 404（B52/P43）：这个 URL 从来
// 不属于任何账户，回 200 fail 会让网关误以为"已处理"。
func TestPaymentNotifyUnknownAccount(t *testing.T) {
	p := newPaymentHarness(t)
	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/nope/notify?pid=1&out_trade_no="+testOrderNo+"&money=1.00&trade_status=TRADE_SUCCESS", "", "")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())
	require.Empty(t, p.store.orders, "未知账户不得触碰订单表")
}

// TestPaymentNotifyMissingRequired 缺必填 → 200 fail，不入账、不 panic、不动订单行。
func TestPaymentNotifyMissingRequired(t *testing.T) {
	p := newPaymentHarness(t)
	for _, tc := range []struct{ name, query string }{
		{"全空", ""},
		{"只有 pid", "?pid=1001"},
		{"缺 money", "?pid=1001&out_trade_no=" + testOrderNo + "&trade_status=TRADE_SUCCESS"},
		{"缺 out_trade_no", "?pid=1001&money=1.00&trade_status=TRADE_SUCCESS"},
		{"缺 trade_status", "?pid=1001&out_trade_no=" + testOrderNo + "&money=1.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := p.do(p.pay, http.MethodGet, "/api/payment/"+paymentMainAccount+"/notify"+tc.query, "", "")
			require.Equal(t, http.StatusOK, rec.Code, "输入错误必须是 200 fail（网关不重试）: %s", rec.Body.String())
			require.Equal(t, "fail", rec.Body.String())
		})
	}
	require.Empty(t, p.store.orders, "缺必填不得落库")
}

// TestPaymentNotifyBrokenConfigIs503 账户 config 坏 → **503**（B51/P42）：让网关
// 重试而不是判定失败后放弃；订单不动。
func TestPaymentNotifyBrokenConfigIs503(t *testing.T) {
	p := newPaymentHarness(t)
	rec := p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentBrokenAccount+"/notify?pid=1002&out_trade_no="+testOrderNo+
			"&money=1.00&trade_status=TRADE_SUCCESS&sign=deadbeef&sign_type=RSA", "", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())
	require.Empty(t, p.store.orders)
}

// TestPaymentReturnRedirect 同步跳转（§6-1）：只校验单号形状；portal_base_url
// 空 → 绝对路径，非空 → 绝对 URL；非法单号 → 400；未知账户 → 404。
func TestPaymentReturnRedirect(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()

	rec := p.do(p.pay, http.MethodGet, "/api/payment/"+paymentMainAccount+"/return?order="+testOrderNo, "", "")
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	require.Equal(t, "/user/wallet?order="+testOrderNo, rec.Header().Get("Location"))

	// 非空 portal_base_url → 前缀拼接（尾斜杠被去掉，不产生 //user）。
	_, err := p.store.SetSetting(ctx, domain.SettingPortalBaseURL, domain.SettingTypeString, "https://portal.example.com/")
	require.NoError(t, err)
	require.NoError(t, p.snap.Reload(ctx))
	rec = p.do(p.pay, http.MethodGet, "/api/payment/"+paymentMainAccount+"/return?order="+testOrderNo, "", "")
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://portal.example.com/user/wallet?order="+testOrderNo, rec.Header().Get("Location"))

	// 非法单号：不改状态、不落账，直接 400（不跳到一个假单页）。
	rec = p.do(p.pay, http.MethodGet, "/api/payment/"+paymentMainAccount+"/return?order=not-an-order", "", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rec.Header().Get("Location"))

	rec = p.do(p.pay, http.MethodGet, "/api/payment/nope/return?order="+testOrderNo, "", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestPaymentMethodsHonorsGlobalSwitch §5-1b「销售面 vs 结算面」：`payment.enabled=false`
// → methods 返回**空列表**（不给用户看不可用的入口）；但**已收的钱照常结算**——同一开关
// 关掉后，先前建好的 pending 单收到回调仍然入账。开关管"能不能卖"，不管"能不能结算"。
func TestPaymentMethodsHonorsGlobalSwitch(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()

	// 开关还开着时先建一张 pending 单。
	rec := p.do(p.user, http.MethodPost, "/api/user/orders",
		`{"amount_usd":"1.00","pay_type":"alipay"}`, p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created userapi.CreateOrderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// 关开关（读路径走内存快照，必须 Reload 才生效）。
	_, err := p.store.SetSetting(ctx, domain.SettingPaymentEnabled, domain.SettingTypeSwitch, "false")
	require.NoError(t, err)
	require.NoError(t, p.snap.Reload(ctx))

	rec = p.do(p.user, http.MethodGet, "/api/user/payment/methods", "", p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, "[]", rec.Body.String(), "payment.enabled=false → methods 空列表（不是错误）")

	// 结算面不看开关：回调照常入账（F16）。
	rec = p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String(), "结算面不得被总开关掐断")
	user, err := p.store.GetUser(ctx, p.userID)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), user.Balance, "开关关闭不影响已建单入账")
}

// TestUserOrdersUnknownPayType 未知 pay_type → 400 + 订单表 0 行（B26/P24）。
func TestUserOrdersUnknownPayType(t *testing.T) {
	p := newPaymentHarness(t)
	rec := p.do(p.user, http.MethodPost, "/api/user/orders",
		`{"amount_usd":"10.00","pay_type":"nope"}`, p.token)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Contains(t, body, "error")
	require.Empty(t, p.store.orders, "400 之后订单表必须 0 行")
}

// TestPaymentHappyPath 端到端：methods（投影 ResolveChannel）→ 建单（冻结账户 +
// 汇率）→ 用**该账户**私钥签回调 → 单事务入账 → 余额 +1.00 USD。
func TestPaymentHappyPath(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()

	// methods：alipay/wxpay 各出现一次，rate 是参考值。
	rec := p.do(p.user, http.MethodGet, "/api/user/payment/methods", "", p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var methods []userapi.PaymentMethod
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &methods))
	require.Len(t, methods, 2)
	for _, m := range methods {
		require.Equal(t, "7.200000", m.Rate)
		require.Equal(t, int64(1), m.MinUsd)
	}

	// 建单：1.00 USD @7.2 → pay_amount 7.20；account 必须是 epay_main（快照）。
	rec = p.do(p.user, http.MethodPost, "/api/user/orders",
		`{"amount_usd":"1.00","pay_type":"alipay"}`, p.token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created userapi.CreateOrderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, paymentMainAccount, created.Account, "订单快照必须含上游账户 key")
	require.Equal(t, "7.20", created.PayAmount)
	payURL, err := url.Parse(created.PayUrl)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/api/payment/"+paymentMainAccount+"/notify",
		payURL.Query().Get("notify_url"), "回调地址按订单快照的 account_key 拼")
	require.Equal(t, "RSA", payURL.Query().Get("sign_type"))

	// 回调：金额与订单 pay_minor 等值（720 分）。
	rec = p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String(), "入账成功必须回裸文本 success")

	orders, _, err := p.store.ListOrders(ctx, repository.OrderQuery{})
	require.NoError(t, err)
	require.Len(t, orders, 1)
	require.Equal(t, domain.OrderStatusPaid, orders[0].Status)
	require.Equal(t, paymentMainAccount, orders[0].AccountKey)
	require.NotNil(t, orders[0].RedemptionCodeID)

	user, err := p.store.GetUser(ctx, p.userID)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), user.Balance, "1.00 USD = 100,000 毫分")

	// 重复通知：幂等 success（行锁 + 条件迁移），余额只加一次。
	rec = p.do(p.pay, http.MethodGet,
		"/api/payment/"+paymentMainAccount+"/notify?"+signedNotify(t, notifyQuery(created.OrderNo)).Encode(), "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "success", rec.Body.String())
	user, err = p.store.GetUser(ctx, p.userID)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), user.Balance, "重复通知不得重复入账")
}

// TestPaymentAccountAdminSurface 管理面账户/渠道（§10-3）：创建 → 列表掩码 →
// config_error → capabilities 400 → 软停幂等。
func TestPaymentAccountAdminSurface(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()

	// 列表：正常账户 config 掩码（private_key → "***"），坏账户 config_error 非空。
	rec := p.do(p.adminPay, http.MethodGet, "/api/admin/payment/accounts", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var accounts []paymentadminapi.PaymentAccount
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accounts))
	require.Len(t, accounts, 2)
	byKey := make(map[string]paymentadminapi.PaymentAccount, len(accounts))
	for _, a := range accounts {
		byKey[a.Key] = a
	}
	main := byKey[paymentMainAccount]
	require.Equal(t, "***", main.Config[epay.ConfigFieldPrivateKey], "秘密字段必须掩码")
	require.Nil(t, main.ConfigError)
	require.Len(t, main.Channels, 2)
	require.Equal(t, int64(1), main.ConfigRevision, "创建即 revision 1")
	broken := byKey[paymentBrokenAccount]
	require.NotNil(t, broken.ConfigError, "解析失败的账户必须暴露 config_error（P33）")
	require.Empty(t, broken.Config, "协议字段未知时宁可空表也不回显原文")

	// 创建：合法 config → 201；同一 (provider, merchant_ref) 再来一次 → 409。
	body := `{"key":"epay_new","provider":"epay_v2","config":` + string(epayAccountConfigPID(t, "https://gw.example.com", "2001")) + `}`
	rec = p.do(p.adminPay, http.MethodPost, "/api/admin/payment/accounts", body, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created paymentadminapi.PaymentAccount
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, "2001", created.MerchantRef)
	require.True(t, created.Enabled, "缺省 enabled = true")

	dup := `{"key":"epay_dup","provider":"epay_v2","config":` + string(epayAccountConfigPID(t, "https://gw.example.com", "2001")) + `}`
	rec = p.do(p.adminPay, http.MethodPost, "/api/admin/payment/accounts", dup, "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// 非法 config（缺 private_key）→ 400 且不落行。
	bad := `{"key":"epay_bad","provider":"epay_v2","config":{"base_url":"https://gw.example.com","pid":"9","public_key":"x"}}`
	rec = p.do(p.adminPay, http.MethodPost, "/api/admin/payment/accounts", bad, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	if _, err := p.store.GetPaymentAccountByKey(ctx, "epay_bad"); err == nil {
		t.Fatal("非法配置不得落库（P33①）")
	}

	// 渠道：pay_type 不在 capabilities → 400；合法 → 200。
	rec = p.do(p.adminPay, http.MethodPut, "/api/admin/payment/accounts/epay_new/channels/card",
		`{"enabled":true,"currency":"USD","rate_micro":1000000,"min_usd":1,"max_usd":100}`, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = p.do(p.adminPay, http.MethodPut, "/api/admin/payment/accounts/epay_new/channels/alipay",
		`{"enabled":true,"currency":"CNY","rate_micro":7200000,"min_usd":1,"max_usd":100}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 软停：200 + 行仍在 + enabled=false；重复 DELETE 幂等 200。
	for range 2 {
		rec = p.do(p.adminPay, http.MethodDelete, "/api/admin/payment/accounts/epay_new", "", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.False(t, byKeyEnabled(t, p, "epay_new"), "DELETE 恒为软停（行仍在、enabled=false）")
	}
	// PUT 改 display_name 不动渠道、不动 revision（B55/P44）。
	rec = p.do(p.adminPay, http.MethodPut, "/api/admin/payment/accounts/epay_new",
		`{"display_name":"renamed","priority":3,"enabled":true}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var renamed paymentadminapi.PaymentAccount
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &renamed))
	require.Equal(t, "renamed", renamed.DisplayName)
	require.Equal(t, int64(1), renamed.ConfigRevision, "只改展示名不得推进 config_revision")
	require.Len(t, renamed.Channels, 1, "改展示名不得删光渠道")
}

// byKeyEnabled 读账户当前 enabled（软停断言用）。
func byKeyEnabled(t *testing.T, p *paymentHarness, key string) bool {
	t.Helper()
	acct, err := p.store.GetPaymentAccountByKey(t.Context(), key)
	require.NoError(t, err)
	return acct.Enabled
}

// TestPaymentAccountMerchantRefImmutable PUT 改 config 里的 pid（改变 MerchantRef）
// → 400 且配置不变（N29/P45）。
func TestPaymentAccountMerchantRefImmutable(t *testing.T) {
	p := newPaymentHarness(t)
	ctx := t.Context()
	before, err := p.store.GetPaymentAccountByKey(ctx, paymentMainAccount)
	require.NoError(t, err)

	body := `{"display_name":"x","priority":0,"enabled":true,"config":{"base_url":"https://gw.example.com","pid":"9999","private_key":"***","public_key":"x"}}`
	rec := p.do(p.adminPay, http.MethodPut, "/api/admin/payment/accounts/"+paymentMainAccount, body, "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	after, err := p.store.GetPaymentAccountByKey(ctx, paymentMainAccount)
	require.NoError(t, err)
	require.Equal(t, before.MerchantRef, after.MerchantRef)
	require.Equal(t, before.ConfigRevision, after.ConfigRevision, "被拒的更新不得推进 revision")
	require.JSONEq(t, string(before.Config), string(after.Config))
}

// TestAdminOrdersSurface 管理面订单面（§7-3/§9）：空列表、未知 id 404、resolve 的
// 「只许一次」（B41）、查单失败 → 502 + force 关单。
func TestAdminOrdersSurface(t *testing.T) {
	p := newPaymentHarness(t)

	rec := p.do(p.admin, http.MethodGet, "/api/admin/orders", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list OrderListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Zero(t, list.Total)
	require.NotNil(t, list.Rows, "空列表必须是 [] 而不是 null")
	require.Empty(t, list.Rows)

	// 非法 status 枚举 → 400（生成器不校验 enum，handler 必须自己挡）。
	rec = p.do(p.admin, http.MethodGet, "/api/admin/orders?status=bogus", "", "")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	rec = p.do(p.admin, http.MethodGet, "/api/admin/orders/999", "", "")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// 种一行 pending 订单（不经 CreateOrder：这里只验路由与状态机边界）。
	order := &domain.Order{
		ID: 1, OutTradeNo: testOrderNo, UserID: p.userID, Provider: domain.ProviderEpayV2,
		AccountKey: paymentMainAccount,
		Status:     domain.OrderStatusPending, CreditMilli: 1_000_000, PayMinor: 7200,
		Currency: "CNY", RateMicro: 7_200_000, PayType: epay.PayTypeAlipay,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	}
	p.store.mu.Lock()
	p.store.orders[order.ID] = order
	p.store.mu.Unlock()

	rec = p.do(p.admin, http.MethodGet, "/api/admin/orders/1", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var detail OrderDetail
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	require.Equal(t, testOrderNo, detail.OrderNo)
	require.Equal(t, paymentMainAccount, detail.AccountKey)
	require.Equal(t, "10.00", detail.AmountUsd, "金额必须走定点字符串（毫分 → USD）")
	require.Equal(t, "72.00", detail.PayAmount)
	require.Equal(t, "7.200000", detail.Rate)

	// resolve 幂等：第一次 200，第二次 409（同一订单不得被补两次）。
	rec = p.do(p.admin, http.MethodPost, "/api/admin/orders/1/resolve", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resolved OrderResolveResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
	require.True(t, resolved.Resolved)
	require.NotNil(t, resolved.Order.ResolvedAt)

	rec = p.do(p.admin, http.MethodPost, "/api/admin/orders/1/resolve", "", "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// close：账户 base_url 不可达 → 查询本身失败（§9 第三态）→ 无 force 必须 502
	// （网关故障可重试），**绝不**悄悄关单；force 才允许越过查询失败。
	rec = p.do(p.admin, http.MethodPost, "/api/admin/orders/1/close", "", "")
	require.Equal(t, http.StatusBadGateway, rec.Code, "无 force 且查询失败 → 502 且不关单: %s", rec.Body.String())
	stored, err := p.store.GetOrderByID(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, domain.OrderStatusPending, stored.Status, "入账/查询失败绝不关单")
	require.NotNil(t, stored.FailReason)
	require.Equal(t, domain.OrderFailReasonInternal, *stored.FailReason)

	rec = p.do(p.admin, http.MethodPost, "/api/admin/orders/1/close", `{"force":true}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var closed OrderCloseResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &closed))
	require.Equal(t, OrderStatus("closed"), closed.Status)
	require.False(t, closed.Credited)
	require.NotNil(t, closed.Order.FailReason)
	require.Equal(t, domain.OrderFailReasonForced, *closed.Order.FailReason)

	// 关单后再关 → 409（只有 pending 可关）。
	rec = p.do(p.admin, http.MethodPost, "/api/admin/orders/1/close", `{"force":true}`, "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// seedOrder 种一行 pending 订单（不经 CreateOrder：本用例只验 §9 的关单前置判定）。
func seedOrder(t *testing.T, p *paymentHarness, id int64, outTradeNo, accountKey string) *domain.Order {
	t.Helper()
	o := &domain.Order{
		ID: id, OutTradeNo: outTradeNo, UserID: p.userID, Provider: domain.ProviderEpayV2,
		AccountKey: accountKey, Status: domain.OrderStatusPending,
		CreditMilli: 1_000_000, PayMinor: 7200, Currency: "CNY",
		RateMicro: 7_200_000, PayType: epay.PayTypeAlipay,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	}
	p.store.mu.Lock()
	p.store.orders[o.ID] = o
	p.store.mu.Unlock()
	return o
}

// TestCloseOrderNoQueryFaceIs409 §9 的 **409 族**：账户行缺失 / 账户 cfg 不可解析 /
// 协议未注册——这三种在「发起查询」之前就没有查单面，故必须 409（提示 force 或先修
// 配置），**不是 502**；502 只留给"查询请求本身失败"（上一个用例已钉住）。
//
// 这是资损口径：把"配置坏了"说成 502（网关故障，可重试）会让运维一直点重试而不去
// 修账户；说成 409 才是"你这条命令现在执行不了，要么 force，要么先修配置"。
func TestCloseOrderNoQueryFaceIs409(t *testing.T) {
	p := newPaymentHarness(t)
	// 协议未注册的账户（种行造：创建接口会 400）。
	ghostProvider, err := p.store.CreatePaymentAccount(t.Context(), &domain.PaymentAccount{
		Key: "epay_ghost_provider", Provider: domain.ProviderName("nope_v9"), MerchantRef: "9001",
		DisplayName: "unregistered provider", Enabled: true,
		Config: epayAccountConfig(t, "http://127.0.0.1:1"),
	})
	require.NoError(t, err)
	require.NotZero(t, ghostProvider.ID)

	cases := []struct {
		name    string
		id      int64
		orderNo string
		account string
	}{
		{"账户行缺失", 11, "P20260101000001ABCDEFGHJK", "epay_missing"},
		{"账户 cfg 不可解析", 12, "P20260101000002ABCDEFGHJK", paymentBrokenAccount},
		{"协议未注册", 13, "P20260101000003ABCDEFGHJK", "epay_ghost_provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := seedOrder(t, p, tc.id, tc.orderNo, tc.account)
			rec := p.do(p.admin, http.MethodPost, "/api/admin/orders/"+strconv.FormatInt(tc.id, 10)+"/close", "", "")
			require.Equal(t, http.StatusConflict, rec.Code, "必须是 409（不是 502）: %s", rec.Body.String())
			stored, getErr := p.store.GetOrderByID(t.Context(), o.ID)
			require.NoError(t, getErr)
			require.Equal(t, domain.OrderStatusPending, stored.Status, "409 不关单")

			// force 才是出口：无查单面时允许强制关单，fail_reason=forced。
			rec = p.do(p.admin, http.MethodPost, "/api/admin/orders/"+strconv.FormatInt(tc.id, 10)+"/close", `{"force":true}`, "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var closed OrderCloseResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &closed))
			require.Equal(t, OrderStatus("closed"), closed.Status)
			require.NotNil(t, closed.Order.FailReason)
			require.Equal(t, domain.OrderFailReasonForced, *closed.Order.FailReason)
		})
	}
}

// TestPaymentAccountPartialUpdate openapi 契约：`PaymentAccountUpdate` 四字段**全可选**
// （缺省 = 不变）。前端只想切启停时不得被迫回传 display_name——否则一次误传就把展示名
// 覆盖成空串。这里钉住"只发 enabled"的三种后果：展示名/优先级/渠道与 config_revision
// 全部不动。
func TestPaymentAccountPartialUpdate(t *testing.T) {
	p := newPaymentHarness(t)
	before, err := p.store.GetPaymentAccountByKey(t.Context(), paymentMainAccount)
	require.NoError(t, err)

	rec := p.do(p.adminPay, http.MethodPut, "/api/admin/payment/accounts/"+paymentMainAccount, `{"enabled":false}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out paymentadminapi.PaymentAccount
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.False(t, out.Enabled)
	require.Equal(t, before.DisplayName, out.DisplayName, "缺省字段必须保持原值")
	require.Equal(t, before.Priority, out.Priority, "缺省字段必须保持原值")
	require.Equal(t, before.ConfigRevision, out.ConfigRevision, "缺省 config 不得推进 revision")
	require.Len(t, out.Channels, 2, "缺省字段更新不得动渠道")
	require.Equal(t, before.MerchantRef, out.MerchantRef, "merchant_ref 只读")
}
