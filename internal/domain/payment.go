// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"time"
)

// 支付域（spec 支付网关 v2 §4/§5，2026-11-12 r4）。三层职责是硬边界：
//
//	协议 Provider（代码，无状态）：只回答「怎么签 / 怎么验 / 怎么查」。
//	账户 PaymentAccount（数据）  ：一行 = 一家上游的一个商户号（含凭据 jsonb）。
//	渠道 PaymentChannel（数据）  ：一行 = 该账户下可售卖的一种支付方式（含汇率/限额）。
//
// 由此得到的判据：新增一家上游 = 插一行数据，**零代码改动**——凭据形状与哪些字段
// 是秘密由协议自述（ParseConfig / ConfigSchema），渠道能写哪些 pay_type 由
// Capabilities 自述，service 里不出现任何协议字段名。

// ProviderName 支付协议标识 = 注册表键（如 "epay_v2"）。
//
// **绝不从订单反推协议**：回调按账户 key 路由（路径即账户），账户给出 provider，
// 再与订单快照做一致性校验（§6-2 第 9 步）。
type ProviderName string

// ProviderEpayV2 易支付 V2（首个实现）。注册表只增不减（v1 F48/F19）：下线协议
// 也必须保留实现与公钥以完成对账/补单，故本常量不随任何开关消失。
const ProviderEpayV2 ProviderName = "epay_v2"

// NotifyStatus 网关通知的归一状态：paid = 已收款（才进完成流程）；other = 其余
// 一切（调用方按 trade_status 原文落 fail_reason=status，不入账）。
type NotifyStatus string

const (
	NotifyStatusPaid  NotifyStatus = "paid"
	NotifyStatusOther NotifyStatus = "other"
)

// AccountConfig 协议凭据的**密封**接口：仓库内实现，跨协议误用在编译期被拦住
// （不是回调路径上的 panic）。
//
// 密封靠未导出的 isAccountConfig：包外类型要实现它必须嵌入 AccountConfigSeal。
// 密封只防「包外伪造」，防不住「epayCfg 传给 stripe provider」——因此
// PaymentProvider 的**每个方法入口**都必须校验 `cfg.Provider() == p.Name()`，
// 不符返回 ErrConfigProviderMismatch（**不得 panic**），service 调用前也校验
// 一次（纵深防御）。
type AccountConfig interface {
	// Provider 该凭据属于哪个协议（与账户列一致；不一致 = 配置错放）。
	Provider() ProviderName
	// MerchantRef 协议自述的商户唯一标识（epay = pid）。它不参与回调校验，
	// 只承担账户唯一性（(provider, merchant_ref) 唯一索引）。
	MerchantRef() string
	isAccountConfig()
}

// AccountConfigSeal 密封标记的**唯一**合法入口：协议实现把凭据结构体写成
//
//	type Config struct {
//	    domain.AccountConfigSeal
//	    ...
//	}
//
// 即获得 AccountConfig 的实现资格（无字段、无方法冲突；不要复制它的方法）。
type AccountConfigSeal struct{}

func (AccountConfigSeal) isAccountConfig() {}

// PayRequest 发起支付的全部输入（service 组装；协议只做校验、签名、拼 URL）。
//
// **不含定价字段**（account_key / rate_micro / min_usd / max_usd 是 service 的
// 选路与账本职责，协议不需要）：协议要的只是「哪一种支付方式、多少钱、回哪里」。
type PayRequest struct {
	PayType string // 协议需要它构造 format/type 参数（如 alipay/wxpay）
	// Currency 渠道结算币种（ISO-4217；两位小数白名单）。
	Currency string
	// OutTradeNo 订单号（P{UTC yyyyMMddHHmmss}{10 位 base32}）。
	OutTradeNo string
	// Subject 即网关的 name 参数：`[A-Za-z0-9_-]`（含空格/中文的通知验签不稳，
	// 构造期就不可能产生——service 用固定前缀 + 单号）。
	Subject string
	// Money 实付金额快照，两位小数（%d.%02d，全程整数不经 float）。
	Money string
	// NotifyURL 本账户的回调地址（/api/payment/{account}/notify）。
	NotifyURL string
	// ReturnURL 本账户的同步跳转地址（/api/payment/{account}/return）。
	ReturnURL string
}

// NotifyEvent 归一后的通知/查单事件。PaidMinor 是**渠道最小单位**的整数（分），
// 由协议实现解析（>2 位小数/非数字 → 错误，不四舍五入）。
type NotifyEvent struct {
	OutTradeNo   string
	TradeNo      string
	PaidMinor    int64
	PaidCurrency string
	Status       NotifyStatus
	Timestamp    string
	Raw          map[string]string
}

// ConfigFieldKind 配置字段的**呈现**提示（§4/N37）：只决定管理面用哪种输入控件，
// 服务端不据此校验——校验权威始终是 ParseConfig。
type ConfigFieldKind string

const (
	ConfigFieldText      ConfigFieldKind = "text"
	ConfigFieldPassword  ConfigFieldKind = "password"
	ConfigFieldMultiline ConfigFieldKind = "multiline"
	ConfigFieldURL       ConfigFieldKind = "url"
)

// ConfigField 账户 config（jsonb）里的一个键的形状（§4/§18-2）。
//
// 为什么形状也要协议自述：配置表单若靠管理面抄一份镜像，协议加字段时表单不出
// 输入框（严格 JSON 下必然 400），协议把字段改判为秘密时前端仍以明文回显（密钥
// 进浏览器历史）。因此 Name/Kind/Secret 只能来自协议实现本身。
type ConfigField struct {
	Name     string          // config(jsonb) 的键
	Kind     ConfigFieldKind // 仅呈现提示
	Secret   bool            // 掩码 + "***" 合并的唯一声明口（N35）
	Required bool            // 表单必填提示；权威校验在 ParseConfig
	Label    string          // 可选：英文兜底标签（纯展示，不参与校验）
	Example  string          // 可选：输入示例
}

// ConfigSchema 一份配置的形状：Fields 顺序即表单顺序（服务端不得重排）。
type ConfigSchema struct {
	Fields []ConfigField
}

// SecretNames 由 schema 派生出的秘密字段名（按声明顺序，不排序）。
//
// **不是第二份声明**：掩码与 PUT 的 "***" 合并都从这里取，避免"schema 说秘密、
// 掩码表说不是"的必然漂移（N35）。
func (s ConfigSchema) SecretNames() []string {
	var out []string
	for _, f := range s.Fields {
		if f.Secret {
			out = append(out, f.Name)
		}
	}
	return out
}

// PaymentProvider 支付协议面（spec v2 §4）：service 只见本接口，签名算法、参数名、
// URL 形状全部在实现侧；协议实现**无状态**（不持有配置）。
type PaymentProvider interface {
	// Name 注册表键（= 账户 provider 列 = 管理面下拉的取值）。
	Name() ProviderName
	// Capabilities 支持的 pay_type 静态能力（不含汇率/限额）：渠道写入时
	// 必须 ∈ 本列表，否则 400。
	Capabilities() []string
	// ConfigSchema config 有哪些键、哪个是秘密：掩码、PUT 的 "***" 合并与
	// 管理面表单都从它派生 → service 里不出现任何协议字段名。
	ConfigSchema() ConfigSchema
	// ParseConfig 解析并校验账户凭据（写入与读缓存共用）；同时给出
	// MerchantRef（唯一索引键）。失败 → 管理面 config_error + 选路跳过 + 指标。
	ParseConfig(raw []byte) (AccountConfig, error)
	// BuildPayURL 构造已签名的支付跳转 URL。
	BuildPayURL(cfg AccountConfig, in PayRequest) (string, error)
	// VerifyNotify 校验并归一一条异步通知（顺序见 spec §6-2 第 4–6 步）。
	VerifyNotify(cfg AccountConfig, params url.Values) (NotifyEvent, error)
}

// OrderQuerier 可选能力：向网关查单（管理面 sync、关单前置校验）。协议实现不
// 存在该能力时，sync/close 按「不支持查单」处理（409）。cfg 必须来自订单快照的
// account_key（不是「当前最优先账户」）。
type OrderQuerier interface {
	Query(ctx context.Context, cfg AccountConfig, outTradeNo string) (NotifyEvent, error)
}

// ChannelConfig 选路结果的渠道切片（spec §5-1 `Resolved.Channel`）：pay_type /
// currency / rate_micro / min/max 五项——**不含**账户与配置，故可安全地透给
// methods 投影（rate 仅为参考值）。
type ChannelConfig struct {
	PayType   string
	Currency  string
	RateMicro int64 // 1 USD = N 货币 × 1e6
	MinUSD    int64
	MaxUSD    int64
}

// PaymentAccount 支付账户（spec v2 §3-1）：一行 = 一家上游的一个商户号。
//
// Key / Provider / MerchantRef 创建后**只读**（换协议或换商户 = 新建账户 + 停用
// 旧账户）；Config 是协议专属凭据（jsonb），写入由 ParseConfig 校验，读出由
// ConfigSchema().SecretNames() 掩码。
type PaymentAccount struct {
	ID             int64
	Key            string       // `^[a-z][a-z0-9_]{2,31}$`；URL 段 + 订单快照；永不复用
	Provider       ProviderName // 协议快照（创建后只读）
	MerchantRef    string       // 协议自述商户标识（只读；(provider,merchant_ref) UNIQUE）
	DisplayName    string       // 管理面展示名（唯一可改的身份相关字段）
	Enabled        bool         // 可售开关（**不影响回调处理**）
	Priority       int          // 升序选路（0 = 主；同 priority 按 id 升序）
	Config         json.RawMessage
	ConfigRevision int64 // 每次 config 变更 +1（配置缓存键的一部分）
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PaymentChannel 支付渠道（spec v2 §3-2）：一行 = 该账户下可售卖的一种支付方式。
// 同一 pay_type 可出现在多个账户（多上游），选路按 `priority ASC, id ASC` 决定。
type PaymentChannel struct {
	ID        int64
	AccountID int64
	PayType   string // 必须 ∈ provider.Capabilities()（service 校验）
	Enabled   bool
	Currency  string // 两位小数白名单
	RateMicro int64  // 1 USD = N 货币 × 1e6（> 0 才可售）
	MinUSD    int64  // 单笔下限（≥ 1）
	MaxUSD    int64  // 单笔上限（≥ min）
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChannelConfig 投影出选路/methods 需要的五项（PaymentChannel 行是账本面，含
// id/时间戳等与管理面无关的列，故显式投影而非整体传递）。
func (c PaymentChannel) ChannelConfig() ChannelConfig {
	return ChannelConfig{
		PayType:   c.PayType,
		Currency:  c.Currency,
		RateMicro: c.RateMicro,
		MinUSD:    c.MinUSD,
		MaxUSD:    c.MaxUSD,
	}
}

// OrderStatus 订单状态（v1 §4-2）：只有三态，**时间不是状态**——过期是
// `pending && expires_at < now` 的派生视图，过期单照样入账（B10/P9）。
type OrderStatus string

const (
	OrderStatusPending OrderStatus = "pending"
	OrderStatusPaid    OrderStatus = "paid"   // 终态
	OrderStatusClosed  OrderStatus = "closed" // 终态；之后的通知 → fail + closed
)

// Valid 三态枚举校验（未知状态禁止入账，F43）。
func (s OrderStatus) Valid() bool {
	switch s {
	case OrderStatusPending, OrderStatusPaid, OrderStatusClosed:
		return true
	}
	return false
}

// Order 支付订单（spec v2 §3-3）。金额字段全是整数：CreditMilli 入账毫分
// （credit_milli = usd_cents*1000），PayMinor 实付渠道最小单位，
// RateMicro 下单时的汇率快照（×1e6）——下单后改汇率不影响既有订单（B15）。
//
// AccountKey 是上游账户快照（v1 预留下的 Account 已作废并删除）：**不建外键**
// （账户软停/改名后账本仍可读）。回调第 8 步用它比对「路径账户 == 订单账户」。
type Order struct {
	ID               int64
	OutTradeNo       string // P{UTC yyyyMMddHHmmss}{10 位 base32}，UNIQUE
	UserID           int64
	Provider         ProviderName // 协议快照（从账户取）
	AccountKey       string       // 上游账户 key 快照
	Status           OrderStatus
	CreditMilli      int64
	PayMinor         int64
	Currency         string // 限 2 位小数（白名单）
	RateMicro        int64
	PayType          string
	TradeNo          *string // 网关流水
	RedemptionCodeID *int64  // 自动到账凭据（UNIQUE；同一订单只能入账一次）
	RawNotify        *string // 清洗后 ≤4 KiB 的 JSON
	FailReason       *string // 见 OrderFailReason* 常量
	FailCount        int     // 尽力计数，不承担对账职责（F48/F21）
	LastNotifyAt     *time.Time
	ResolvedAt       *time.Time // 人工处置完成标记（防重复补账，F48/F20）
	CreatedAt        time.Time
	PaidAt           *time.Time
	ExpiresAt        time.Time
}

// 订单 fail_reason 枚举（spec v2 §3-3 内联全枚举）：运维闭环的唯一信号载体——任何
// 「已付款未入账」都必须落其中之一并可由管理面 resolve 收口。
//
// **不写审计的三类**（§8）：验签失败、重复键/值字符、必填缺失——单号尚未验证，
// 写 fail_reason 等于允许伪造签名污染任意订单。
const (
	OrderFailReasonSignature = "sig"      // 验签失败/sign_type 非 RSA（不写审计）
	OrderFailReasonPID       = "pid"      // 商户号不符
	OrderFailReasonAmount    = "amount"   // 金额或币种不符
	OrderFailReasonStatus    = "status"   // trade_status 非成功
	OrderFailReasonClosed    = "closed"   // 已关单收到成功通知（必须人工介入）
	OrderFailReasonProvider  = "provider" // 回调账户的协议与订单快照不符（跨协议回调）
	OrderFailReasonAccount   = "account"  // 回调路径账户与订单快照不符（v2 新增，第 8 步）
	OrderFailReasonUser      = "user"     // 用户被禁/被删 → 不入账
	OrderFailReasonInternal  = "internal" // 事务/DB/未知状态；网关应重试
	OrderFailReasonForced    = "forced"   // 管理员强制关单
)

// paymentCurrencies 渠道币种白名单（§6）：两位小数货币，且都是本系统能整数
// 换算的币种。不在表内 → 渠道不可用 503（B20：JPY 必须 503）。
var paymentCurrencies = map[string]struct{}{
	"CNY": {}, "USD": {}, "EUR": {}, "GBP": {}, "HKD": {},
	"AUD": {}, "CAD": {}, "SGD": {}, "THB": {}, "MYR": {},
}

// PaymentCurrencySupported 报告币种是否在白名单内（大小写敏感：配置面只接受
// 大写的 ISO 代码，写错即渠道不可用而不静默纠正）。
func PaymentCurrencySupported(code string) bool {
	if code == "" {
		return false
	}
	_, ok := paymentCurrencies[code]
	return ok
}

// 支付设置键。**v2 只保留 4 键**（spec §15-7）：payment.enabled /
// payment.notify_base_url / payment.portal_base_url / payment.order_ttl_minutes。
//
// 其余（epay 凭据 / min_usd / max_usd / 渠道三件套）随「账户是数据」作废——
// 常量与注册表项在 settings 批（§15-7「删 13 键」）统一移除；A 批保留它们只为让
// domain/settings.go 仍可编译，**任何 v2 代码都不得再读写这些键**。
const (
	SettingPaymentEnabled  = "payment.enabled"
	SettingNotifyBaseURL   = "payment.notify_base_url"
	SettingPortalBaseURL   = "payment.portal_base_url"
	SettingOrderTTLMinutes = "payment.order_ttl_minutes"
)

// 订单号形状（v1 §4-1/§5-5）：P{UTC yyyyMMddHHmmss}{10 位 base32}。生成与校验
// 共用同一模式——return 端点按它过滤外部输入，非法即拒绝（不改状态、不落账）。
var outTradeNoPattern = regexp.MustCompile(`^P\d{14}[A-Z2-9]{10}$`)

// ValidOutTradeNo 报告单号是否符合订单号形状。
func ValidOutTradeNo(outTradeNo string) bool {
	return outTradeNoPattern.MatchString(outTradeNo)
}

// 支付域专用哨兵（`%w` 保留 cause，调用方用 errors.Is 分类）。
//
// ErrNotFound 语义碰撞的处置：订单缺行、用户缺行、DB 错误在 CompleteOrder 里
// 必须能被分别归类（200 fail / 500 / 503 完全不同），因此内部一律换成专用
// 哨兵——「订单不存在」与「用户不可用」绝不共用 repository.ErrNotFound。
var (
	// ErrOrderNotFound 订单缺行（未知 out_trade_no）→ 200 fail，不写 fail_reason
	// （单号未经验签，不得用伪造签名污染任意订单）。
	ErrOrderNotFound = errors.New("payment: order not found")
	// ErrUserUnavailable 用户被禁或被删 → 200 fail + fail_reason=user。
	ErrUserUnavailable = errors.New("payment: user unavailable")
	// ErrInternal DB/事务/未知状态等内部错误 → 5xx，让网关重试（F53）。
	ErrInternal = errors.New("payment: internal")
	// ErrConfigMissing 协议验签/下单所需配置缺失：notify → 503（网关重试而非
	// 放弃，F47/B40），创建订单 → 503。
	ErrConfigMissing = errors.New("payment: config missing")
	// ErrAccountMismatch 回调路径账户与订单快照的 account_key 不符（§6-2 第 8 步）
	// → 200 fail + fail_reason=account。**必须在 paid 幂等分支之前判**，否则已入账
	// 订单收到错误账户的通知会被幂等分支吞掉并回 success。
	ErrAccountMismatch = errors.New("payment: account mismatch")
	// ErrConfigProviderMismatch 账户凭据属于别的协议（跨协议误用，N31）→ 调用方
	// 映射 5xx（配置错放，不当成网关可判定的输入错误）。**返回错误，不 panic**。
	ErrConfigProviderMismatch = errors.New("payment: config provider mismatch")
	// ErrUnknownPayType 任何账户都没有该 pay_type 的渠道行（N16）→ **400**。
	ErrUnknownPayType = errors.New("payment: unknown pay type")
	// ErrNoEligibleAccount 有渠道行但全被过滤（账户停用/协议未注册/配置坏/金额
	// 超出限额，N18）→ **503**（暂时不可用，不是参数错）。
	ErrNoEligibleAccount = errors.New("payment: no eligible account")
)
