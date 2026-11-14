// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package epay

// sign_test.go 签名规则与协议面的判别性用例（spec §5-1/§5-2/§5-4/§5-3）：
// base64 四试、sign 排除值字符检查（B39）、`+` 被解成空格的还原（F60）、
// sign_type 硬校验（F41/M21）、必填存在性、金额整数解析（B34）、三层开关、
// 查单响应验签。

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
)

// testOrderNo 任意合法形状的订单号（P + 14 位时间 + 10 位 base32）。
const testOrderNo = "P20261112010101ABCDEFGHJK"

// testKeyPEM 固定 2048-bit 商户密钥对（PEM）。2048 是硬要求：RSA-2048 签名
// 256 字节 → base64 344 字符、尾部 `==`，B39 的判别力依赖这个长度。
type testKeyPEM struct {
	privatePEM string
	publicPEM  string
}

var (
	testKeyOnce sync.Once
	testKey     testKeyPEM
)

func testKeys(t *testing.T) testKeyPEM {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic("epay test: generate RSA key: " + err.Error())
		}
		privDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			panic("epay test: marshal private key: " + err.Error())
		}
		pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			panic("epay test: marshal public key: " + err.Error())
		}
		testKey = testKeyPEM{
			privatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
			publicPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
		}
	})
	return testKey
}

// signedParams 用商户私钥签一组通知参数（sign_type 已设，签名剔除它）。
func signedParams(t *testing.T, keys testKeyPEM, params url.Values) url.Values {
	t.Helper()
	sign, err := SignValues(params, keys.privatePEM)
	if err != nil {
		t.Fatalf("SignValues: %v", err)
	}
	params.Set(signField, sign)
	params.Set(signTypeField, signTypeRSA)
	return params
}

// notifyParams 一组通过验签的最小通知参数。
func notifyParams() url.Values {
	params := url.Values{}
	params.Set("pid", "1001")
	params.Set("out_trade_no", testOrderNo)
	params.Set("trade_no", "GW-9")
	params.Set("money", "1.15")
	params.Set("trade_status", tradeStatusSuccess)
	params.Set("type", PayTypeAlipay)
	params.Set("timestamp", "1762900000")
	return params
}

// testConfig 构造一份合法的账户凭据（v2：凭据随 cfg 传入，provider 无状态）。
func testConfig(t *testing.T, baseURL string) *Config {
	t.Helper()
	keys := testKeys(t)
	return &Config{
		BaseURL:    baseURL,
		PID:        "1001",
		PrivateKey: keys.privatePEM,
		PublicKey:  keys.publicPEM,
	}
}

// otherConfig 伪造一份"别的协议"的 cfg（同名字段但 Provider() 不同）：N31 的判据。
type otherConfig struct {
	domain.AccountConfigSeal
}

func (otherConfig) Provider() domain.ProviderName { return domain.ProviderName("stripe_v2") }
func (otherConfig) MerchantRef() string           { return "acct_1" }

func TestSignVerifyRoundTrip(t *testing.T) {
	keys := testKeys(t)
	params := signedParams(t, keys, notifyParams())

	// sign 是 RSA-2048 base64：344 字符且尾部恒为 `==`——值字符检查必须排除它，
	// 否则每条合法通知都会被拒（B39）。
	require(t, strings.HasSuffix(params.Get(signField), "=="),
		"RSA-2048 base64 签名尾部必须是 ==（B39 判据前提）")
	require(t, len(params.Get(signField)) == 344, "base64 签名长度应为 344")

	if err := VerifyValues(params, keys.publicPEM); err != nil {
		t.Fatalf("合法签名验签失败: %v", err)
	}
}

func TestVerifyAcceptsBase64Variants(t *testing.T) {
	keys := testKeys(t)
	base := notifyParams()
	rawSign, err := SignValues(base, keys.privatePEM)
	if err != nil {
		t.Fatalf("SignValues: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(rawSign)
	if err != nil {
		t.Fatalf("decode std base64: %v", err)
	}
	cases := map[string]string{
		"std":    rawSign,
		"rawstd": base64.RawStdEncoding.EncodeToString(raw),
		"url":    base64.URLEncoding.EncodeToString(raw),
		"rawurl": base64.RawURLEncoding.EncodeToString(raw),
	}
	for name, sign := range cases {
		t.Run(name, func(t *testing.T) {
			params := notifyParams()
			params.Set(signField, sign)
			params.Set(signTypeField, signTypeRSA)
			if err := VerifyValues(params, keys.publicPEM); err != nil {
				t.Fatalf("base64 变体 %s 验签失败: %v", name, err)
			}
		})
	}
}

// TestVerifyRestoresPlusFromSpace F60：url.Values 面丢失 RawQuery，未做
// percent-encoding 的 `+` 会被解成空格 → 必须能还原后再验签。
func TestVerifyRestoresPlusFromSpace(t *testing.T) {
	keys := testKeys(t)
	base := notifyParams()
	sign, err := SignValues(base, keys.privatePEM)
	if err != nil {
		t.Fatalf("SignValues: %v", err)
	}
	// 只替换成空格的形状才有判别力：签名里必须真的含 `+`。
	for i := 0; i < 8 && !strings.Contains(sign, "+"); i++ {
		base.Set("timestamp", strconv.Itoa(1762900000+i))
		sign, err = SignValues(base, keys.privatePEM)
		if err != nil {
			t.Fatalf("SignValues: %v", err)
		}
	}
	mangled := strings.ReplaceAll(sign, "+", " ")

	params := notifyParams()
	params.Set(signField, mangled)
	params.Set(signTypeField, signTypeRSA)
	if err := VerifyValues(params, keys.publicPEM); err != nil {
		t.Fatalf("空格还原 + 后应验签通过: %v", err)
	}
}

func TestVerifyRejectsTamperedAndMalformed(t *testing.T) {
	keys := testKeys(t)

	tampered := signedParams(t, keys, notifyParams())
	tampered.Set("money", "1.16")
	requireErr(t, VerifyValues(tampered, keys.publicPEM), "篡改金额必须验签失败")

	// 扩展字段参与签名（不做参数白名单）：改它也必须失败（B12/M10）。
	extended := signedParams(t, keys, notifyParams())
	extended.Set("extra", "x")
	requireErr(t, VerifyValues(extended, keys.publicPEM), "篡改扩展字段必须验签失败")

	// 值含 `&`/`=` → 拒绝（签名串重建歧义，B39）。
	for _, bad := range []string{"a&b", "a=b"} {
		params := notifyParams()
		params.Set("attach", bad)
		params = signedParams(t, keys, params)
		requireErr(t, VerifyValues(params, keys.publicPEM), "值含 &/= 必须拒绝: %q", bad)
	}

	// 重复键 → 拒绝（B33）。
	dup := signedParams(t, keys, notifyParams())
	dup.Add("money", "1.15")
	requireErr(t, VerifyValues(dup, keys.publicPEM), "重复键必须拒绝")

	// 缺 sign → 拒绝（不 panic）。
	noSign := notifyParams()
	requireErr(t, VerifyValues(noSign, keys.publicPEM), "缺 sign 必须拒绝")
}

// TestVerifyMissingPublicKeyIsConfigMissing B40：验签配置缺失必须可被识别为
// 配置问题（调用方回 503 让网关重试，而不是 200 fail 让它放弃）。
func TestVerifyMissingPublicKeyIsConfigMissing(t *testing.T) {
	keys := testKeys(t)
	params := signedParams(t, keys, notifyParams())
	err := VerifyValues(params, "")
	if !errors.Is(err, domain.ErrConfigMissing) {
		t.Fatalf("空公钥应包 ErrConfigMissing，实际: %v", err)
	}

	// v2：公钥缺失在 ParseConfig 就被拦住（写面 400 / 列表 config_error），
	// 不会拖到回调路径才 503。
	provider := New(nil)
	if _, err := provider.ParseConfig([]byte(`{"base_url":"https://gw.example.com","pid":"1001","private_key":"x"}`)); !errors.Is(err, domain.ErrConfigMissing) {
		t.Fatalf("缺 public_key 的 config 应被 ParseConfig 拒绝，实际: %v", err)
	}
}

func TestParseMoney(t *testing.T) {
	ok := map[string]int64{
		"1.15":   115,
		"0.01":   1,
		"10":     1000,
		"010.00": 1000, // 前导零合法（解析为最小单位等值比，B34）
		"0.00":   0,    // 零值本身合法：是否可付由 pay_minor >= 1 判（B4）
	}
	for in, want := range ok {
		got, err := parseMoney(in)
		if err != nil {
			t.Fatalf("parseMoney(%q) 失败: %v", in, err)
		}
		if got != want {
			t.Fatalf("parseMoney(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "1e2", "-0.00", "10.000", "10.000000001", "1.2.3", ".5", "1.", "abc"} {
		if _, err := parseMoney(bad); err == nil {
			t.Fatalf("parseMoney(%q) 应失败（>2 位小数/非数字不四舍五入）", bad)
		}
	}
}

// TestProviderVerifyNotify 覆盖 §5-4 的成功路径与必填/算法前置校验。
func TestProviderVerifyNotify(t *testing.T) {
	keys := testKeys(t)
	provider := New(nil)
	cfg := testConfig(t, "https://gw.example.com")

	ev, err := provider.VerifyNotify(cfg, signedParams(t, keys, notifyParams()))
	if err != nil {
		t.Fatalf("VerifyNotify: %v", err)
	}
	if ev.Status != domain.NotifyStatusPaid {
		t.Fatalf("status = %q, want paid", ev.Status)
	}
	if ev.PaidMinor != 115 {
		t.Fatalf("PaidMinor = %d, want 115（1.15 元）", ev.PaidMinor)
	}
	if ev.PaidCurrency != "" {
		// v2：币种是渠道行（= 订单快照）的属性，协议只搬上游原文。service 用订单
		// 快照兜底，因此这里必须是空串——非空就说明有人在协议层猜币种。
		t.Fatalf("PaidCurrency = %q, want empty（由订单快照兜底）", ev.PaidCurrency)
	}
	if ev.OutTradeNo != testOrderNo || ev.TradeNo != "GW-9" {
		t.Fatalf("单号/流水解析错误: %+v", ev)
	}

	// 非成功状态不报错，回 other（调用方落 fail_reason=status 并审计）。
	wait := notifyParams()
	wait.Set("trade_status", "WAIT")
	ev, err = provider.VerifyNotify(cfg, signedParams(t, keys, wait))
	if err != nil {
		t.Fatalf("WAIT 不应报错（由调用方落 status）: %v", err)
	}
	if ev.Status != domain.NotifyStatusOther {
		t.Fatalf("WAIT status = %q, want other", ev.Status)
	}

	// sign_type 非 RSA → 即便签名合法也必须在验签前拒绝（F41/M21）。
	md5Params := notifyParams()
	md5Params.Set("pid", "1001")
	sign, err := SignValues(md5Params, keys.privatePEM)
	if err != nil {
		t.Fatalf("SignValues: %v", err)
	}
	md5Params.Set(signField, sign)
	md5Params.Set(signTypeField, "MD5")
	requireErr(t, ignoreEvent(provider.VerifyNotify(cfg, md5Params)), "sign_type=MD5 必须拒绝（验签前硬校验）")

	// 缺必填参数 → fail（不得跳过校验进验签/入账，F42/B38）。
	missingMoney := notifyParams()
	missingMoney.Del("money")
	requireErr(t, ignoreEvent(provider.VerifyNotify(cfg, signedParams(t, keys, missingMoney))), "缺 money 必须 fail")

	// pid 不符 → fail（M3/P7）。
	otherPID := notifyParams()
	otherPID.Set("pid", "2002")
	requireErr(t, ignoreEvent(provider.VerifyNotify(cfg, signedParams(t, keys, otherPID))), "pid 不符必须 fail")

	// 公开面无鉴权：单号形状与参数个数由协议兜底（§7-2）。
	badNo := notifyParams()
	badNo.Set("out_trade_no", "P1")
	requireErr(t, ignoreEvent(provider.VerifyNotify(cfg, signedParams(t, keys, badNo))), "单号形状不符必须 fail")

	tooMany := notifyParams()
	for i := range 40 {
		tooMany.Set("extra_"+strconv.Itoa(i), "v")
	}
	requireErr(t, ignoreEvent(provider.VerifyNotify(cfg, signedParams(t, keys, tooMany))), "参数个数超限必须 fail")
}

// TestProviderBuildPayURL §5-2：参数齐全、金额快照两位小数、签名可用商户
// 公钥验过（P1 的核心断言）。
func TestProviderBuildPayURL(t *testing.T) {
	keys := testKeys(t)
	provider := New(nil)
	cfg := testConfig(t, "https://gw.example.com/")
	payURL, err := provider.BuildPayURL(cfg, domain.PayRequest{
		PayType:    PayTypeAlipay,
		Currency:   "CNY",
		OutTradeNo: testOrderNo,
		Subject:    "Balance_topup_" + testOrderNo,
		Money:      "1.15",
		NotifyURL:  "https://api.example.com/api/payment/epay_v2/notify",
		ReturnURL:  "https://api.example.com/api/payment/epay_v2/return",
	})
	if err != nil {
		t.Fatalf("BuildPayURL: %v", err)
	}
	parsed, err := url.Parse(payURL)
	if err != nil {
		t.Fatalf("pay_url 不是合法 URL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "gw.example.com" || parsed.Path != submitPath {
		t.Fatalf("pay_url 形状错误: %s", payURL)
	}
	query := parsed.Query()
	for k, want := range map[string]string{
		"pid": "1001", "type": PayTypeAlipay, "out_trade_no": testOrderNo,
		"money": "1.15", "name": "Balance_topup_" + testOrderNo, "param": testOrderNo,
		"sign_type":  signTypeRSA,
		"notify_url": "https://api.example.com/api/payment/epay_v2/notify",
		"return_url": "https://api.example.com/api/payment/epay_v2/return",
	} {
		if got := query.Get(k); got != want {
			t.Fatalf("pay_url 参数 %s = %q, want %q", k, got, want)
		}
	}
	// 用**商户密钥对的公钥**验签（P1：证明签名串与私钥同源）。
	if err := VerifyValues(query, keys.publicPEM); err != nil {
		t.Fatalf("pay_url 签名验不过: %v", err)
	}

	// 构造期值域：name 含空格/中文、金额非两位小数、单号形状不符 → 直接拒绝（B27）。
	bad := domain.PayRequest{
		PayType:    PayTypeAlipay,
		OutTradeNo: testOrderNo,
		Subject:    "Balance topup 充值",
		Money:      "1.15",
		NotifyURL:  "https://api.example.com/n", ReturnURL: "https://api.example.com/r",
	}
	requireErr(t, ignoreStr(provider.BuildPayURL(cfg, bad)), "name 含空格/中文必须构造期拒绝")
	bad.Subject = "Balance_topup"
	bad.Money = "1.1"
	requireErr(t, ignoreStr(provider.BuildPayURL(cfg, bad)), "金额非两位小数必须拒绝")
	bad.Money = "1.15"
	bad.OutTradeNo = "P1"
	requireErr(t, ignoreStr(provider.BuildPayURL(cfg, bad)), "单号形状不符必须拒绝")

	// 跨协议误用（N31）：密封接口防不住"epayCfg 被传给别的 provider"，所以每个
	// 入口都必须自己校验——这里是 BuildPayURL 的入口。
	_, err = provider.BuildPayURL(otherConfig{}, domain.PayRequest{PayType: PayTypeAlipay})
	if !errors.Is(err, domain.ErrConfigProviderMismatch) {
		t.Fatalf("跨协议 cfg 必须回 ErrConfigProviderMismatch（不 panic），实际: %v", err)
	}
}

// TestProviderCapabilitiesAndConfigSchema §4/N23/N35/N39：协议自述的能力与配置
// 形状是"新增上游零代码"的两根支柱——service 里不出现任何协议字段名。
//
// 字段顺序是表单顺序（§18-2：服务端不得重排），秘密集合必须只有 private_key：
// 掩码与 PUT 的 "***" 合并都从它派生，多标一个是密钥泄漏，少标一个是不可用。
func TestProviderCapabilitiesAndConfigSchema(t *testing.T) {
	provider := New(nil)
	if got := provider.Capabilities(); len(got) != 2 || got[0] != PayTypeAlipay || got[1] != PayTypeWxpay {
		t.Fatalf("Capabilities() = %v, want [alipay wxpay]", got)
	}
	schema := provider.ConfigSchema()
	wantNames := []string{ConfigFieldBaseURL, ConfigFieldPID, ConfigFieldPrivateKey, ConfigFieldPublicKey}
	if len(schema.Fields) != len(wantNames) {
		t.Fatalf("ConfigSchema().Fields has %d fields, want %d: %v", len(schema.Fields), len(wantNames), schema.Fields)
	}
	for i, want := range wantNames {
		if got := schema.Fields[i].Name; got != want {
			t.Fatalf("ConfigSchema().Fields[%d].Name = %q, want %q (声明顺序即表单顺序)", i, got, want)
		}
	}
	if got := schema.SecretNames(); len(got) != 1 || got[0] != ConfigFieldPrivateKey {
		t.Fatalf("ConfigSchema().SecretNames() = %v, want [private_key]", got)
	}
	if provider.Name() != domain.ProviderEpayV2 {
		t.Fatalf("Name() = %q", provider.Name())
	}
}

// TestProviderConfigProviderMismatch N31：三个入口都必须拒绝"别的协议的 cfg"，
// 且**返回错误而不是 panic**（回调路径上 panic = 每个陌生请求都能打挂进程）。
func TestProviderConfigProviderMismatch(t *testing.T) {
	provider := New(nil)
	keys := testKeys(t)
	params := signedParams(t, keys, notifyParams())

	if _, err := provider.VerifyNotify(otherConfig{}, params); !errors.Is(err, domain.ErrConfigProviderMismatch) {
		t.Fatalf("VerifyNotify 跨协议: %v", err)
	}
	if _, err := provider.BuildPayURL(otherConfig{}, domain.PayRequest{PayType: PayTypeAlipay}); !errors.Is(err, domain.ErrConfigProviderMismatch) {
		t.Fatalf("BuildPayURL 跨协议: %v", err)
	}
	if _, err := provider.Query(context.Background(), otherConfig{}, testOrderNo); !errors.Is(err, domain.ErrConfigProviderMismatch) {
		t.Fatalf("Query 跨协议: %v", err)
	}
	if _, err := provider.VerifyNotify(nil, params); !errors.Is(err, domain.ErrConfigMissing) {
		t.Fatalf("nil cfg 应回 ErrConfigMissing: %v", err)
	}
}

// queryFor 打一次查单（srv 的 cfg + 客户端同源）。
func queryFor(t *testing.T, providerFor func(*httptest.Server) (*Provider, *Config), srv *httptest.Server) error {
	t.Helper()
	p, cfg := providerFor(srv)
	_, err := p.Query(context.Background(), cfg, testOrderNo)
	return err
}

// TestQueryVerifiesResponse §5-3：查单响应必须验签，code != 0 失败，仅 status==1
// 归为 paid。
func TestQueryVerifiesResponse(t *testing.T) {
	keys := testKeys(t)

	respond := func(t *testing.T, payload func() map[string]string) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != queryPath {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// 请求侧也必须带 RSA 签名（网关鉴权面）。
			if r.PostForm.Get(signField) == "" || r.PostForm.Get(signTypeField) != signTypeRSA {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, _ := json.Marshal(payload())
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))
	}

	// providerFor 用**当前** httptest 地址装配 cfg（base_url 是账户凭据的一部分，
	// 由调用方随 cfg 传入——provider 不再读全局设置）。
	providerFor := func(srv *httptest.Server) (*Provider, *Config) {
		cfg := testConfig(t, srv.URL)
		return New(srv.Client()), cfg
	}

	signedPayload := func(t *testing.T, fields map[string]string) map[string]string {
		t.Helper()
		params := url.Values{}
		for k, v := range fields {
			params.Set(k, v)
		}
		sign, err := SignValues(params, keys.privatePEM)
		if err != nil {
			t.Fatalf("SignValues: %v", err)
		}
		out := map[string]string{}
		for k, v := range fields {
			out[k] = v
		}
		out[signField] = sign
		out[signTypeField] = signTypeRSA
		return out
	}

	t.Run("paid", func(t *testing.T) {
		srv := respond(t, func() map[string]string {
			return signedPayload(t, map[string]string{
				"code": "0", "msg": "ok", "status": "1",
				"out_trade_no": testOrderNo, "trade_no": "GW-9",
				"money": "1.15", "type": PayTypeAlipay, "timestamp": "1762900000",
			})
		})
		defer srv.Close()
		p, cfg := providerFor(srv)
		ev, err := p.Query(context.Background(), cfg, testOrderNo)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if ev.Status != domain.NotifyStatusPaid || ev.PaidMinor != 115 || ev.PaidCurrency != "" {
			t.Fatalf("查单归一错误: %+v", ev)
		}
	})

	t.Run("status not paid", func(t *testing.T) {
		srv := respond(t, func() map[string]string {
			return signedPayload(t, map[string]string{
				"code": "0", "status": "2", "out_trade_no": testOrderNo,
				"trade_no": "GW-9", "money": "1.15", "type": PayTypeAlipay,
			})
		})
		defer srv.Close()
		p, cfg := providerFor(srv)
		ev, err := p.Query(context.Background(), cfg, testOrderNo)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if ev.Status != domain.NotifyStatusOther {
			t.Fatalf("status=2 应归为 other，实际 %q", ev.Status)
		}
	})

	t.Run("tampered response", func(t *testing.T) {
		srv := respond(t, func() map[string]string {
			payload := signedPayload(t, map[string]string{
				"code": "0", "status": "1", "out_trade_no": testOrderNo,
				"money": "1.15", "type": PayTypeAlipay,
			})
			payload["money"] = "0.01" // 验签后篡改
			return payload
		})
		defer srv.Close()
		requireErr(t, queryFor(t, providerFor, srv),
			"查单响应验签失败必须报错（B13）")
	})

	t.Run("code not zero", func(t *testing.T) {
		srv := respond(t, func() map[string]string {
			return signedPayload(t, map[string]string{
				"code": "10001", "msg": "order not found", "status": "0",
				"out_trade_no": testOrderNo, "money": "0.00", "type": PayTypeAlipay,
			})
		})
		defer srv.Close()
		requireErr(t, queryFor(t, providerFor, srv),
			"code != 0 必须失败（§5-3）")
	})

	t.Run("missing sign_type", func(t *testing.T) {
		srv := respond(t, func() map[string]string {
			return map[string]string{"code": "0", "status": "1", "money": "1.15"}
		})
		defer srv.Close()
		requireErr(t, queryFor(t, providerFor, srv),
			"缺 sign_type 的响应必须拒绝")
	})
}

// TestVerifyNotifyBackfillsOutTradeNoAfterVerification §16/L8：验签（+ pid）通过
// 之后的失败必须回填单号，否则「已付款无痕」；验签之前的失败**不得**回填——单号
// 未验证时回填等于让伪造签名写脏任意订单行（§8 的「不写 fail_reason」前提）。
func TestVerifyNotifyBackfillsOutTradeNoAfterVerification(t *testing.T) {
	keys := testKeys(t)
	provider := New(nil)
	cfg := testConfig(t, "https://gw.example.com")

	// ① 验签通过 + money 三位小数（§5-4 第 8 步）：回填单号 + ErrMoneyMalformed +
	// fail_reason=amount。状态已是 paid——失败只发生在金额解析这一步。
	badMoney := notifyParams()
	badMoney.Set("money", "1.234")
	ev, err := provider.VerifyNotify(cfg, signedParams(t, keys, badMoney))
	require(t, errors.Is(err, ErrMoneyMalformed), "money 非法应包 ErrMoneyMalformed，实际: %v", err)
	require(t, ev.OutTradeNo == testOrderNo, "验签通过后必须回填单号，实际 %q", ev.OutTradeNo)
	require(t, ev.Status == domain.NotifyStatusPaid, "状态应为 paid（仅金额不可解析），实际 %q", ev.Status)
	require(t, ev.Raw["money"] == "1.234", "raw 应保留通知原文，实际 %q", ev.Raw["money"])
	var rejected *notifyRejection
	require(t, errors.As(err, &rejected), "错误应可取出 *notifyRejection（携带审计原因），实际 %T", err)
	require(t, rejected.FailReason() == domain.OrderFailReasonAmount,
		"fail_reason 应为 amount，实际 %q", rejected.FailReason())

	// ② 验签失败（签名后篡改 money）：回零值事件——单号未被证明，不得落审计。
	tampered := signedParams(t, keys, notifyParams())
	tampered.Set("money", "1.234")
	ev, err = provider.VerifyNotify(cfg, tampered)
	requireErr(t, err, "篡改金额必须验签失败")
	require(t, ev.OutTradeNo == "", "验签失败不得回填单号，实际 %q", ev.OutTradeNo)

	// ③ 其余失败（缺必填/重复键/sign_type 非 RSA/缺 sign/pid 不符）一律不回填。
	// before 在签名前改（保证签名与参数一致，打到目标校验点）；after 在签名后改（伪造形态）。
	for _, tc := range []struct {
		name   string
		before func(url.Values)
		after  func(url.Values)
	}{
		{name: "缺必填(money)", after: func(p url.Values) { p.Del("money") }},
		{name: "重复键", after: func(p url.Values) { p.Add("money", "9.99") }},
		{name: "sign_type 非 RSA", after: func(p url.Values) { p.Set(signTypeField, "MD5") }},
		{name: "缺 sign", after: func(p url.Values) { p.Del(signField) }},
		{name: "pid 不符", before: func(p url.Values) { p.Set("pid", "2002") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := notifyParams()
			if tc.before != nil {
				tc.before(params)
			}
			params = signedParams(t, keys, params)
			if tc.after != nil {
				tc.after(params)
			}
			ev, err := provider.VerifyNotify(cfg, params)
			requireErr(t, err, "%s 必须被拒绝", tc.name)
			require(t, ev.OutTradeNo == "", "%s 不得回填单号（单号未验证），实际 %q", tc.name, ev.OutTradeNo)
		})
	}
}

// --- 小工具：把 (值, error) / (值, 值2, error) 压成 error 便于 requireErr ---

func ignoreStr(_ string, err error) error               { return err }
func ignoreEvent(_ domain.NotifyEvent, err error) error { return err }

func requireErr(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if err == nil {
		t.Fatalf(format, args...)
	}
}

func require(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

// ---------- 密钥材料形态（线上事故：`private key is not PEM`）----------
//
// 彩虹易支付后台给商户的私钥/公钥常常是**裸 base64**（PKCS#8 / PKIX DER），没有
// PEM 头；本仓原先只认 PEM，于是创建账户必然 400、列表 config_error 非空、选路
// 直接跳过该账户。这一组用例把"三种形态 × 编解码变体"逐条钉住，并**用签名/验签
// 证明解析出来的确实是一对可用密钥**（只断言"能解析"会被"解析成错的 key"骗过）。

// keyForms 返回同一把私钥/公钥的多种粘贴形态（name → 文本）。
func keyForms(t *testing.T) map[string][2]string {
	t.Helper()
	keys := testKeys(t)

	privBlock, _ := pem.Decode([]byte(keys.privatePEM))
	pubBlock, _ := pem.Decode([]byte(keys.publicPEM))
	if privBlock == nil || pubBlock == nil {
		t.Fatalf("test fixture PEM 解不开")
	}
	priv, err := x509.ParsePKCS8PrivateKey(privBlock.Bytes)
	if err != nil {
		t.Fatalf("fixture: parse pkcs8: %v", err)
	}
	rsaPriv, ok := priv.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("fixture: not RSA")
	}
	pkcs1PrivDER := x509.MarshalPKCS1PrivateKey(rsaPriv)
	pkcs1PubDER := x509.MarshalPKCS1PublicKey(&rsaPriv.PublicKey)

	wrap := func(b64 string) string { // 按 64 列折行（后台/编辑器常见）
		var sb strings.Builder
		for i := 0; i < len(b64); i += 64 {
			end := min(i+64, len(b64))
			sb.WriteString(b64[i:end])
			sb.WriteString("\n")
		}
		return sb.String()
	}
	escaped := func(b64 string) string { // 字面量 \n（JSON 往返后没被还原的形态）
		var sb strings.Builder
		for i := 0; i < len(b64); i += 64 {
			end := min(i+64, len(b64))
			sb.WriteString(b64[i:end])
			sb.WriteString(`\n`)
		}
		return sb.String()
	}

	stdPriv := base64.StdEncoding.EncodeToString(privBlock.Bytes)
	stdPub := base64.StdEncoding.EncodeToString(pubBlock.Bytes)
	stdPrivPKCS1 := base64.StdEncoding.EncodeToString(pkcs1PrivDER)
	stdPubPKCS1 := base64.StdEncoding.EncodeToString(pkcs1PubDER)

	return map[string][2]string{
		"pem":                {keys.privatePEM, keys.publicPEM},
		"pem-crlf":           {strings.ReplaceAll(keys.privatePEM, "\n", "\r\n"), strings.ReplaceAll(keys.publicPEM, "\n", "\r\n")},
		"pem-no-trailing-nl": {strings.TrimSpace(keys.privatePEM), strings.TrimSpace(keys.publicPEM)},
		"pem-single-line":    {strings.ReplaceAll(keys.privatePEM, "\n", ""), strings.ReplaceAll(keys.publicPEM, "\n", "")},
		// 单行 input 在部分平台把换行换成空格（也是"头还在但报 not PEM"的成因）
		"pem-space-joined":     {strings.ReplaceAll(keys.privatePEM, "\n", " "), strings.ReplaceAll(keys.publicPEM, "\n", " ")},
		"bare-pkcs8-pkix":      {stdPriv, stdPub},
		"bare-pkcs1":           {stdPrivPKCS1, stdPubPKCS1},
		"bare-wrapped-64":      {wrap(stdPriv), wrap(stdPub)},
		"bare-escaped-newline": {escaped(stdPriv), escaped(stdPub)},
		"bare-quoted":          {`"` + stdPriv + `"`, `'` + stdPub + `'`},
		"bare-url-safe":        {base64.URLEncoding.EncodeToString(privBlock.Bytes), base64.URLEncoding.EncodeToString(pubBlock.Bytes)},
		"bare-raw-std":         {base64.RawStdEncoding.EncodeToString(privBlock.Bytes), base64.RawStdEncoding.EncodeToString(pubBlock.Bytes)},
	}
}

// TestParseKeysAcceptRealWorldForms 三种材料形态都必须解析成功，且**签出来的值能被
// 解出来的公钥验过**（证明 pair 正确，不是"解析到了别的 key"）。
func TestParseKeysAcceptRealWorldForms(t *testing.T) {
	forms := keyForms(t)
	params := notifyParams()
	for name, pair := range forms {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRSAPrivateKey(pair[0]); err != nil {
				t.Fatalf("parseRSAPrivateKey(%s): %v", name, err)
			}
			if _, err := parseRSAPublicKey(pair[1]); err != nil {
				t.Fatalf("parseRSAPublicKey(%s): %v", name, err)
			}
			signed := url.Values{}
			for k, vs := range params {
				signed[k] = append([]string(nil), vs...)
			}
			sign, err := SignValues(signed, pair[0])
			if err != nil {
				t.Fatalf("SignValues(%s): %v", name, err)
			}
			signed.Set(signField, sign)
			signed.Set(signTypeField, signTypeRSA)
			if err := VerifyValues(signed, pair[1]); err != nil {
				t.Fatalf("VerifyValues(%s): %v（解析出的私钥/公钥不是同一对）", name, err)
			}
		})
	}
}

// TestParseKeysRejectGarbage 反向判据：垃圾输入必须报 config missing，且错误里
// **同时说清接受的两种形态**（运维照着日志就能改，不必猜）。
func TestParseKeysRejectGarbage(t *testing.T) {
	cases := map[string]string{
		"empty":          "   ",
		"prose":          "not a key at all", // 含空格：必须报 not base64（不是 ASN.1 噪声）
		"broken-base64":  "-----BEGIN PRIVATE KEY-----!!!-----END PRIVATE KEY-----",
		"base64-garbage": base64.StdEncoding.EncodeToString([]byte("definitely not DER")),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseRSAPrivateKey(in)
			if !errors.Is(err, domain.ErrConfigMissing) {
				t.Fatalf("parseRSAPrivateKey(%s) err = %v, want ErrConfigMissing", name, err)
			}
			if !strings.Contains(err.Error(), "PEM or bare base64") {
				t.Fatalf("parseRSAPrivateKey(%s) err = %v, 必须说清接受的形态", name, err)
			}
			if _, err := parseRSAPublicKey(in); !errors.Is(err, domain.ErrConfigMissing) {
				t.Fatalf("parseRSAPublicKey(%s) err = %v, want ErrConfigMissing", name, err)
			}
		})
	}
}

// TestParseConfigAcceptsEveryKeyForm 用户侧的真实形状（**创建账户必须成功**）：
// 管理面表单里粘进来的可能是易支付后台给的裸 base64，也可能是**换行被控件吃掉**的
// PEM（带 `-----BEGIN-----` 头、body 全挤在一行）——后者正是线上
// `epay config private_key: private key is not PEM` 的现场。
func TestParseConfigAcceptsEveryKeyForm(t *testing.T) {
	for name, pair := range keyForms(t) {
		t.Run(name, func(t *testing.T) {
			raw := `{"base_url":"https://pay.example.com","pid":"1001","private_key":` +
				strconv.Quote(pair[0]) + `,"public_key":` + strconv.Quote(pair[1]) + `}`
			cfg, err := New(nil).ParseConfig([]byte(raw))
			if err != nil {
				t.Fatalf("凭据形态 %s 必须被接受（线上事故形状）：%v", name, err)
			}
			if got := cfg.MerchantRef(); got != "1001" {
				t.Fatalf("MerchantRef() = %q, want 1001", got)
			}
		})
	}

	// 反向判据：垃圾 base64 仍必须被拒——兼容形态不等于放弃校验。
	if _, err := New(nil).ParseConfig([]byte(`{"base_url":"https://pay.example.com","pid":"1001","private_key":"AAAA","public_key":"AAAA"}`)); err == nil {
		t.Fatalf("垃圾 base64 仍必须被拒")
	}
}
