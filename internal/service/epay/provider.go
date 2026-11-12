// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package epay

// provider.go 易支付 V2 的 domain.PaymentProvider 实现（spec 支付网关 v2 §4）。
//
// v2 与 v1 的**形状差别**（这是"新增一家上游 = 插一行"的前提）：
//
//   - **无状态**：Provider 不持 any 配置，也不再读 settings。凭据由调用方以
//     密封的 domain.AccountConfig 传进来（每个账户一份），provider 只在本次
//     调用内解析/使用。
//   - **三个入口都校验 cfg 的归属**（N31）：`BuildPayURL` / `VerifyNotify` /
//     `Query` 一旦拿到"别的协议的 cfg"就返回 ErrConfigProviderMismatch，**不
//     panic**——密封接口只防包外伪造，防不住 epayCfg 被传给别的 provider。
//   - **notify_url / return_url 一律由调用方给**（`PayRequest.NotifyURL/
//     ReturnURL`）：路径里带账户 key（§6-1），协议不可能自己拼出来。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

const (
	// PayTypeAlipay/PayTypeWxpay 协议能力（capabilities）：渠道表能写哪些
	// pay_type 由协议自述，service 不再硬编码（N23）。
	PayTypeAlipay = "alipay"
	PayTypeWxpay  = "wxpay"

	// tradeStatusSuccess 网关的成功状态原文（§5-4 第 7 步）。
	tradeStatusSuccess = "TRADE_SUCCESS"
)

// ConfigField* 账户 config（jsonb）里的字段名。它们只在本包出现——service
// 只认 `ConfigSchema().SecretNames()` 返回的字符串，不认具体协议字段（"零代码"
// 在掩码路径上也成立）。
const (
	ConfigFieldBaseURL    = "base_url"
	ConfigFieldPID        = "pid"
	ConfigFieldPrivateKey = "private_key"
	ConfigFieldPublicKey  = "public_key"
)

var (
	// subjectPattern 网关 name 参数值域（F19：含空格/中文的通知验签不稳，构造期
	// 即不可能）。
	subjectPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// moneyPattern 实付金额快照形状：两位小数（service 用 %d.%02d 生成）。
	moneyPattern = regexp.MustCompile(`^\d+\.\d{2}$`)
	// maxNotifyParams 通知参数个数上限（公开面防滥用；正常网关 ~15 个）。
	maxNotifyParams = 32
)

// ErrMoneyMalformed 验签通过后 money 解析失败（§5-4 第 8 步）。
//
// 为什么需要专用 sentinel（§16/L8）：此刻验签与 pid 都已过，单号已被证明属于
// 我们；静默丢弃等于「已付款无痕」。调用方据此把单号落进 fail_reason=amount。
var ErrMoneyMalformed = errors.New("epay: notify money malformed")

// notifyRejection 验签通过后的拒绝：同时携带**可分类的 sentinel**与**审计原因**。
//
// FailReason 是消费者侧接口（handler 断言 `interface{ FailReason() string }`），
// 因此 handler 不必 import 本包——协议实现可替换（§14）而回调面不变。
type notifyRejection struct {
	sentinel error  // errors.Is 分类用（如 ErrMoneyMalformed）
	reason   string // §8 表的 fail_reason（domain.OrderFailReason*）
	err      error  // 人类可读原因（含小数点位数等细节）
}

func (e *notifyRejection) Error() string { return e.err.Error() }

// Unwrap 让 errors.Is/As 同时穿透到 sentinel 与内层原因（Go 1.20+ 多值形态）。
func (e *notifyRejection) Unwrap() []error { return []error{e.sentinel, e.err} }

// FailReason §8 的 fail_reason 取值（空串 = 不写审计）。
func (e *notifyRejection) FailReason() string { return e.reason }

// Config 易支付 V2 的账户凭据（密封：嵌入 domain.AccountConfigSeal）。
//
// 一行 = 一家上游的一个商户号：`pid` 是网关自述的商户标识（= MerchantRef，
// 承载 (provider, merchant_ref) 唯一索引），`base_url` 是网关地址，两把 PEM
// 是本商户的签名/验签密钥。
type Config struct {
	domain.AccountConfigSeal

	BaseURL    string `json:"base_url"`
	PID        string `json:"pid"`
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// 编译期断言：*Config 是合法的密封配置。
var _ domain.AccountConfig = (*Config)(nil)

// Provider 账户凭据所属的协议（= 账户 provider 列）。
func (c *Config) Provider() domain.ProviderName { return domain.ProviderEpayV2 }

// MerchantRef 商户唯一标识（epay = pid）。只用于账户唯一性，**不参与回调校验**
// （回调的 pid 比对走 cfg.PID，见 VerifyNotify）。
func (c *Config) MerchantRef() string { return c.PID }

// Provider 易支付 V2 协议实现（**无状态**：可被所有账户共享一个实例）。
type Provider struct {
	client *Client
	now    func() time.Time
}

// 编译期断言：协议面 + 可选查单能力。
var (
	_ domain.PaymentProvider = (*Provider)(nil)
	_ domain.OrderQuerier    = (*Provider)(nil)
)

// New 构造协议实现。
//
// **hc 可为 nil（内部兜默认客户端）——这是契约，不是巧合**（spec v2 §4 / N67）：
// provider 表驱动契约测试（P51）与装配面门禁派生协议名，一律用
// `paymentregistry.Registry(nil)` 构造本实现，故任何协议构造函数都必须容忍
// nil http client；生产 `main` 显式传带 10s 超时的客户端。
// **不接受 settings**——凭据随 cfg 传入（spec §15-5）。
func New(hc *http.Client) *Provider {
	return &Provider{client: NewClient(hc), now: time.Now}
}

// Name 注册表键 = 账户 provider 列的取值（决策 D6）。
func (p *Provider) Name() domain.ProviderName { return domain.ProviderEpayV2 }

// Capabilities 协议支持的 pay_type（静态能力，不含汇率/限额）：渠道写入时
// service 用它校验 `pay_type ∈ capabilities`（N23/B54）。
func (p *Provider) Capabilities() []string {
	return []string{PayTypeAlipay, PayTypeWxpay}
}

// ConfigSchema 本协议的配置形状（§4/§18-2）：4 个键、顺序即表单顺序，**只有
// private_key 是秘密**。掩码、PUT 的 `"***"` 合并与管理面表单都从这里派生，
// service/前端因此不出现任何协议字段名（N24/N35）。
//
// Kind/Required/Example 只是呈现提示，服务端不据此校验——校验权威只有 ParseConfig。
func (p *Provider) ConfigSchema() domain.ConfigSchema {
	return domain.ConfigSchema{Fields: []domain.ConfigField{
		{Name: ConfigFieldBaseURL, Kind: domain.ConfigFieldURL, Required: true,
			Example: "https://pay.example.com"},
		{Name: ConfigFieldPID, Kind: domain.ConfigFieldText, Required: true, Example: "1001"},
		{Name: ConfigFieldPrivateKey, Kind: domain.ConfigFieldPassword, Secret: true, Required: true,
			Example: "-----BEGIN PRIVATE KEY-----"},
		{Name: ConfigFieldPublicKey, Kind: domain.ConfigFieldMultiline, Required: true,
			Example: "-----BEGIN PUBLIC KEY-----"},
	}}
}

// ParseConfig 解析并校验账户凭据（创建/更新写面与管理面列表共用）。
//
// 失败**必须**让写面 400、让列表 config_error 非空、让选路跳过该账户（§4：
// 解析失败静默只允许存在于选路内部）。校验面刻意包含"密钥可解析"：PEM 坏掉
// 的账户在选路阶段看不出问题，却会在签名/验签时把每一笔订单变成 503。
func (p *Provider) ParseConfig(raw []byte) (domain.AccountConfig, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty epay config", domain.ErrConfigMissing)
	}
	var cfg Config
	if err := jsonUnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%w: invalid epay config json: %w", domain.ErrConfigMissing, err)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("%w: epay config requires %s", domain.ErrConfigMissing, ConfigFieldBaseURL)
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("%w: epay config %s %q is not an absolute URL",
			domain.ErrConfigMissing, ConfigFieldBaseURL, cfg.BaseURL)
	}
	if cfg.PID == "" {
		return nil, fmt.Errorf("%w: epay config requires %s", domain.ErrConfigMissing, ConfigFieldPID)
	}
	if cfg.PrivateKey == "" {
		return nil, fmt.Errorf("%w: epay config requires %s", domain.ErrConfigMissing, ConfigFieldPrivateKey)
	}
	if cfg.PublicKey == "" {
		return nil, fmt.Errorf("%w: epay config requires %s", domain.ErrConfigMissing, ConfigFieldPublicKey)
	}
	if _, err := parseRSAPrivateKey(cfg.PrivateKey); err != nil {
		return nil, fmt.Errorf("%w: epay config %s: %w", domain.ErrConfigMissing, ConfigFieldPrivateKey, err)
	}
	if _, err := parseRSAPublicKey(cfg.PublicKey); err != nil {
		return nil, fmt.Errorf("%w: epay config %s: %w", domain.ErrConfigMissing, ConfigFieldPublicKey, err)
	}
	return &cfg, nil
}

// configOf 取出本协议的 cfg（N31 的唯一落地点）：跨协议误用 → 错误，**不 panic**。
func (p *Provider) configOf(cfg domain.AccountConfig) (*Config, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: nil config", domain.ErrConfigMissing)
	}
	if cfg.Provider() != p.Name() {
		return nil, fmt.Errorf("%w: config is %q, provider is %q",
			domain.ErrConfigProviderMismatch, cfg.Provider(), p.Name())
	}
	typed, ok := cfg.(*Config)
	if !ok {
		// 密封接口保证包外无法伪造实现；走到这里只可能是有人把 *Config 装箱进了
		// 别的载体（如自定义 wrapper）——仍按跨协议误用处理。
		return nil, fmt.Errorf("%w: unexpected epay config type %T",
			domain.ErrConfigProviderMismatch, cfg)
	}
	return typed, nil
}

// BuildPayURL 构造 `GET {base_url}/api/pay/submit?...&sign=...&sign_type=RSA`
// （§5-2）。凭据缺失/非法 → 包 domain.ErrConfigMissing（配置在写入时已校验，
// 正常路径不可达：它是纵深防御）。
func (p *Provider) BuildPayURL(cfg domain.AccountConfig, in domain.PayRequest) (string, error) {
	c, err := p.configOf(cfg)
	if err != nil {
		return "", err
	}
	if c.BaseURL == "" {
		return "", fmt.Errorf("%w: epay config %s", domain.ErrConfigMissing, ConfigFieldBaseURL)
	}
	if c.PID == "" {
		return "", fmt.Errorf("%w: epay config %s", domain.ErrConfigMissing, ConfigFieldPID)
	}
	if c.PrivateKey == "" {
		return "", fmt.Errorf("%w: epay config %s", domain.ErrConfigMissing, ConfigFieldPrivateKey)
	}
	if in.PayType == "" {
		return "", errors.New("epay: empty pay type")
	}
	// 构造期值域校验（B27）：name 只允许 [A-Za-z0-9_-]；money 必须两位小数。
	if !subjectPattern.MatchString(in.Subject) {
		return "", fmt.Errorf("epay: invalid subject %q", in.Subject)
	}
	if !moneyPattern.MatchString(in.Money) {
		return "", fmt.Errorf("epay: invalid money %q", in.Money)
	}
	// 回调地址由 service 按账户 key 拼好（§6-1）；空 = 调用面写错了。
	if in.NotifyURL == "" || in.ReturnURL == "" {
		return "", fmt.Errorf("%w: notify_url/return_url required", domain.ErrConfigMissing)
	}
	if !domain.ValidOutTradeNo(in.OutTradeNo) {
		return "", fmt.Errorf("epay: invalid out_trade_no %q", in.OutTradeNo)
	}

	params := url.Values{}
	params.Set("pid", c.PID)
	params.Set("type", in.PayType)
	params.Set("out_trade_no", in.OutTradeNo)
	params.Set("notify_url", in.NotifyURL)
	params.Set("return_url", in.ReturnURL)
	params.Set("name", in.Subject)
	params.Set("money", in.Money)
	params.Set("param", in.OutTradeNo) // 原样回带单号，便于对账
	params.Set("timestamp", strconv.FormatInt(p.now().Unix(), 10))
	sign, err := SignValues(params, c.PrivateKey)
	if err != nil {
		return "", err
	}
	params.Set(signField, sign)
	params.Set(signTypeField, signTypeRSA)
	return strings.TrimRight(c.BaseURL, "/") + submitPath + "?" + params.Encode(), nil
}

// VerifyNotify 校验并归一一条异步通知（§5-4 第 2–8 步；第 1 步解析在 handler，
// 第 9/10 步完成订单在 service）。顺序是硬约束：
//
//	重复键 → 必填存在性 → sign_type 硬校验 → 验签 → pid → trade_status → money
//
// 非成功状态不报错而是回 Status=other（调用方据此落 fail_reason=status，仍要
// 审计）。**币种不在这里回填**：币种是渠道行（= 订单快照）的属性，协议只搬运
// 上游原文；service 在订单行上比对（ev.PaidCurrency 为空 = 上游没说）。
//
// **error 非 nil 时返回值也可能带 OutTradeNo**（§16/L8）：一旦验签与 pid 都过了，
// 单号就被证明属于我们，后续任何失败（当前只有第 8 步 money 解析）都必须把单号
// 交回调用方落审计——否则「已付款无痕」。验签之前与 pid 不符的失败一律回零值
// 事件（单号未验证，回填等于让伪造签名污染任意订单行）。
func (p *Provider) VerifyNotify(cfg domain.AccountConfig, params url.Values) (domain.NotifyEvent, error) {
	c, err := p.configOf(cfg)
	if err != nil {
		return domain.NotifyEvent{}, err
	}
	if err := checkUniqueKeys(params); err != nil {
		return domain.NotifyEvent{}, err
	}
	for _, key := range []string{"pid", "out_trade_no", "money", "trade_status"} {
		if params.Get(key) == "" {
			return domain.NotifyEvent{}, fmt.Errorf("epay: missing required parameter %q", key)
		}
	}
	// 公开暴露面没有鉴权，参数个数与单号形状由协议实现兜底（§7-2）：单号必须是本
	// 系统生成的形状（同时限长 25），参数个数有界避免超大 query 打爆审计。
	if len(params) > maxNotifyParams {
		return domain.NotifyEvent{}, fmt.Errorf("epay: too many parameters (%d)", len(params))
	}
	if !domain.ValidOutTradeNo(params.Get("out_trade_no")) {
		return domain.NotifyEvent{}, fmt.Errorf("epay: invalid out_trade_no %q", params.Get("out_trade_no"))
	}
	// sign_type 不参与签名 ⇒ 必须在验签前硬校验，且不得据它选择算法（F41）。
	if got := params.Get(signTypeField); got != signTypeRSA {
		return domain.NotifyEvent{}, fmt.Errorf("epay: sign_type %q is not %q", got, signTypeRSA)
	}
	if c.PublicKey == "" {
		return domain.NotifyEvent{}, fmt.Errorf("%w: epay config %s", domain.ErrConfigMissing, ConfigFieldPublicKey)
	}
	if c.PID == "" {
		return domain.NotifyEvent{}, fmt.Errorf("%w: epay config %s", domain.ErrConfigMissing, ConfigFieldPID)
	}
	if err := VerifyValues(params, c.PublicKey); err != nil {
		return domain.NotifyEvent{}, err
	}

	// —— 以下单号已被证明属于本系统（验签已过）——
	if got := params.Get("pid"); got != c.PID {
		// 第 6 步：不是给我们的回调（他家同形单号也会走到这里）。§8 明确
		// 「pid 不符 → 不写 fail_reason」⇒ **不得回填**单号。
		return domain.NotifyEvent{}, errors.New("epay: pid mismatch")
	}
	ev := domain.NotifyEvent{
		OutTradeNo: params.Get("out_trade_no"),
		TradeNo:    params.Get("trade_no"),
		Timestamp:  params.Get("timestamp"),
		Status:     domain.NotifyStatusOther,
		Raw:        firstValues(params),
	}
	// 第 7 步：非成功状态不是错误——调用方要落 fail_reason=status 并审计（§8）。
	if params.Get("trade_status") != tradeStatusSuccess {
		return ev, nil
	}
	// 状态已过第 7 步：即便下面金额解析失败，事件里的状态仍是 paid（失败点只有一处）。
	ev.Status = domain.NotifyStatusPaid
	// 第 8 步：金额解析（>2 位小数/非数字不四舍五入，B34）。失败也必须回填单号：
	// 此刻钱可能真收了、只是格式不可解析——静默丢弃等于资损无痕（§16/L8）。
	paidMinor, err := parseMoney(params.Get("money"))
	if err != nil {
		return ev, &notifyRejection{
			sentinel: ErrMoneyMalformed,
			reason:   domain.OrderFailReasonAmount,
			err:      fmt.Errorf("epay: notify money %q: %w", params.Get("money"), err),
		}
	}
	ev.PaidMinor = paidMinor
	return ev, nil
}

// firstValues 取 url.Values 的首值副本（审计原文用：重复键已在入口拒绝）。
func firstValues(params url.Values) map[string]string {
	out := make(map[string]string, len(params))
	for k, vs := range params {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// Query 查单（§5-3，管理面 sync 与关单前置校验用）。**吃账户 cfg**：查的是该
// 账户背后的商户，而不是"当前主账户"（M27/P37 的判据）。
//
// 响应不带币种是常态：`PaidCurrency` 留空，由 service 用订单快照兜底。
func (p *Provider) Query(ctx context.Context, cfg domain.AccountConfig, outTradeNo string) (domain.NotifyEvent, error) {
	c, err := p.configOf(cfg)
	if err != nil {
		return domain.NotifyEvent{}, err
	}
	return p.client.Query(ctx, QueryRequest{
		BaseURL:       c.BaseURL,
		PID:           c.PID,
		OutTradeNo:    outTradeNo,
		PrivateKeyPEM: c.PrivateKey,
		PublicKeyPEM:  c.PublicKey,
	})
}

// jsonUnmarshalStrict 严格解码账户 config：未知字段（拼错的 base_url/pid）必须
// 报错而不是被静默忽略——静默忽略会让"看起来配好了"的账户在运行期 503。
func jsonUnmarshalStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
