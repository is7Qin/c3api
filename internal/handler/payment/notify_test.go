// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package payment

// notify_test.go 公开回调面的**审计闭环**单测（spec v2 §6-2/§8 + §16/L8）：
//
//  1. 验签通过 + money 非法 → 200 fail，且**恰好一次** RecordNotifyFailure(no,"amount")
//     （repository 侧即 fail_reason=amount + fail_count+1）——否则已付款无痕；
//  2. 验签失败（签名后篡改 money）→ 200 fail 且**零次**审计：单号未被证明属于我们，
//     写 fail_reason 等于让伪造签名污染任意订单行；
//  3. 账户 key 不存在 → 404（B52），一步都不进入验签。
//
// 用真实 epay 协议实现（签名/验签整条链）+ 只接线"账户 + 审计缝"的 store 桩：
// 判别点就是「有没有调用 MarkOrderNotifyFailure、reason 是什么」以及「404 有没有
// 抢先于验签」。

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/service/epay"
	"github.com/is7qin/c3api/internal/settingssnap"
)

// notifyOrderNo 形状合法的订单号（P + 14 位 UTC + 10 位 base32）。
const notifyOrderNo = "P20261112010101ABCDEFGHJK"

// notifyAccount / notifyAccountKey 测试账户（= 回调 URL 段）。
const (
	notifyAccountKey = "epay_main"
	notifyMerchantID = "1001"
)

// auditCall 一次失败审计（reason = §8 表里的 fail_reason）。
type auditCall struct{ outTradeNo, reason string }

// auditStore 只接线「账户读取 + 审计缝」：其余 Store 面未装配（本文件不触达；
// 误触达即 nil panic，起到"没走不该走的分支"的哨兵作用）。
type auditStore struct {
	service.Store
	mu       sync.Mutex
	calls    []auditCall
	accounts map[string]*domain.PaymentAccount
}

func (s *auditStore) GetPaymentAccountByKey(_ context.Context, key string) (*domain.PaymentAccount, error) {
	acct, ok := s.accounts[key]
	if !ok {
		return nil, repository.ErrNotFound
	}
	c := *acct
	return &c, nil
}

func (s *auditStore) ListPaymentAccounts(context.Context) ([]*domain.PaymentAccount, error) {
	out := make([]*domain.PaymentAccount, 0, len(s.accounts))
	for _, acct := range s.accounts {
		c := *acct
		out = append(out, &c)
	}
	return out, nil
}

func (s *auditStore) ListPaymentChannels(context.Context, int64) ([]*domain.PaymentChannel, error) {
	return nil, nil
}

func (s *auditStore) MarkOrderNotifyFailure(_ context.Context, outTradeNo, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, auditCall{outTradeNo: outTradeNo, reason: reason})
	return nil
}

// recorded 返回已记录的审计副本（并发安全只在测试内部需要）。
func (s *auditStore) recorded() []auditCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]auditCall(nil), s.calls...)
}

// emailCodeStub service.New 要求非 nil 的验证码存储（本文件不触达该面）。
type emailCodeStub struct{ service.EmailCodeStore }

// testKeyPair 固定 2048-bit 密钥对（与 epay 包同一判据：签名长度依赖 RSA-2048）。
type testKeyPair struct{ privatePEM, publicPEM string }

var (
	testKeyOnce sync.Once
	testKey     testKeyPair
)

func testKeys(t *testing.T) testKeyPair {
	t.Helper()
	testKeyOnce.Do(func() {
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
		testKey = testKeyPair{
			privatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
			publicPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		}
	})
	return testKey
}

// newNotifyHandler 装配真实协议 + 真实 service + 账户/审计桩 store。
func newNotifyHandler(t *testing.T) (http.Handler, *auditStore) {
	t.Helper()
	keys := testKeys(t)
	cfg, err := json.Marshal(map[string]string{
		epay.ConfigFieldBaseURL:    "https://gw.example.com",
		epay.ConfigFieldPID:        notifyMerchantID,
		epay.ConfigFieldPrivateKey: keys.privatePEM,
		epay.ConfigFieldPublicKey:  keys.publicPEM,
	})
	require.NoError(t, err)
	store := &auditStore{accounts: map[string]*domain.PaymentAccount{
		notifyAccountKey: {
			ID: 1, Key: notifyAccountKey, Provider: domain.ProviderEpayV2,
			MerchantRef: notifyMerchantID, Enabled: true, Config: cfg, ConfigRevision: 1,
		},
	}}
	registry, err := domain.NewProviderRegistry([]domain.PaymentProvider{epay.New(nil)})
	require.NoError(t, err)
	svc := service.New(service.Deps{
		Store:            store,
		EmailCodeStore:   emailCodeStub{},
		SettingsSnapshot: settingssnap.New(nil, nil),
		PaymentRegistry:  registry,
	})
	return Mount(New(svc, nil)), store
}

// signedNotify 一组通过验签的最小通知参数（mutate 在签名前应用）。
func signedNotify(t *testing.T, keys testKeyPair, mutate func(url.Values)) url.Values {
	t.Helper()
	params := url.Values{}
	params.Set("pid", notifyMerchantID)
	params.Set("out_trade_no", notifyOrderNo)
	params.Set("trade_no", "GW-9")
	params.Set("money", "1.15")
	params.Set("trade_status", "TRADE_SUCCESS")
	params.Set("type", epay.PayTypeAlipay)
	params.Set("timestamp", "1762900000")
	if mutate != nil {
		mutate(params)
	}
	sign, err := epay.SignValues(params, keys.privatePEM)
	if err != nil {
		t.Fatalf("sign notify: %v", err)
	}
	params.Set("sign", sign)
	params.Set("sign_type", "RSA")
	return params
}

// getNotify 打一次回调（query 走 Encode，`+` 会被正确转义再由解析还原）。
func getNotify(h http.Handler, account string, params url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/payment/"+account+"/notify?"+params.Encode(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNotifyPostVerifyFailureIsAudited §16/L8：验签通过后的失败必须落审计；验签
// 失败的同一形状**不得**落审计。
func TestNotifyPostVerifyFailureIsAudited(t *testing.T) {
	h, store := newNotifyHandler(t)
	keys := testKeys(t)

	// ① 验签通过 + money 三位小数：200 fail + 一次 reason=amount 的审计
	// （store 侧即 fail_reason=amount、fail_count+1、last_notify_at 刷新）。
	rec := getNotify(h, notifyAccountKey, signedNotify(t, keys, func(p url.Values) { p.Set("money", "1.234") }))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String(), "网关只认裸文本 fail（不是 JSON 信封）")
	require.Equal(t, []auditCall{{outTradeNo: notifyOrderNo, reason: domain.OrderFailReasonAmount}}, store.recorded(),
		"money 非法必须落 fail_reason=amount（一次）")

	// ② 验签失败（签名后篡改 money）：同样 200 fail，但**零**审计。
	store.mu.Lock()
	store.calls = nil
	store.mu.Unlock()
	tampered := signedNotify(t, keys, nil)
	tampered.Set("money", "1.234")
	rec = getNotify(h, notifyAccountKey, tampered)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())
	require.Empty(t, store.recorded(),
		"验签失败不得写 fail_reason（单号未验证，可被伪造签名污染任意订单行）")

	// ③ 非成功状态（验签过）仍按 §8 落 reason=status —— 与本批改动共用同一条审计缝。
	store.mu.Lock()
	store.calls = nil
	store.mu.Unlock()
	waiting := signedNotify(t, keys, func(p url.Values) { p.Set("trade_status", "WAIT") })
	rec = getNotify(h, notifyAccountKey, waiting)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())
	require.Equal(t, []auditCall{{outTradeNo: notifyOrderNo, reason: domain.OrderFailReasonStatus}}, store.recorded())
}

// TestNotifyUnknownAccountIs404 第 1 步就 404（B52）：未知账户没有任何可落库的
// 单号，也不该进入验签——回 200 fail 会让网关误以为"已处理"。
func TestNotifyUnknownAccountIs404(t *testing.T) {
	h, store := newNotifyHandler(t)
	keys := testKeys(t)
	rec := getNotify(h, "nope", signedNotify(t, keys, nil))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, "fail", rec.Body.String())
	require.Empty(t, store.recorded(), "404 不得写审计")
}
