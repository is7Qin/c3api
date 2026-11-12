// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// payment.go 支付网关编排（spec 支付网关 v2 §4/§5/§6/§7/§9/§10）。
//
// 与 v1 的形状差别（本文件是本批的主战场）：
//
//  1. **账户是数据**：协议实现无状态、凭据在 payment_accounts.config（jsonb），
//     渠道/汇率/限额在 payment_channels。service 里**不出现任何协议字段名**——
//     掩码靠 `ConfigSchema().SecretNames()`，能力靠 `Capabilities()`。
//  2. **选路唯一入口** `ResolveChannel`（§5-1）：`methods` 与 `CreateOrder` 共用
//     它，禁止各写一套过滤（否则会出现"展示 A、下单 B"）。
//  3. **cfg 随选路结果返回**（N3）：建单时用同一份快照签名，账户在本次下单内被
//     禁用/改配置都不影响结果。
//  4. **回调按账户路由**：`account_key` 是订单快照，同步/关单/重签都按它取账户与
//     配置，而不是"当前主账户"（M27）。
//
// 三条铁律不变：
//   - 余额只经兑换 applier 变动（同事务建码 + 立即应用）；
//   - 契约可替换（本文件只见 domain.PaymentProvider / domain.OrderQuerier）；
//   - 金额全程整数（credit_milli = usd_cents*1000，
//     pay_minor = (credit_milli*rate_micro + 5e8)/1e9，溢出判据用除法）。

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// PaymentStore 支付持久化（非事务面）：订单读写 + **账户/渠道 CRUD**。
//
// 完成订单的唯一入账路径走 Store.WithTx + repository.TxStore（行锁 + 条件迁移）；
// 账户/渠道永远不参与支付事务（一次账户编辑不该把入账事务拖成锁竞争）。
type PaymentStore interface {
	CreateOrder(ctx context.Context, o *domain.Order) (*domain.Order, error)
	GetOrderByID(ctx context.Context, id int64) (*domain.Order, error)
	ListOrders(ctx context.Context, q repository.OrderQuery) ([]*domain.Order, int64, error)
	// MarkOrderNotifyFailure 失败审计（非事务；WHERE status <> 'paid'）。**验签/
	// 重复键/未知单号失败不得调用**——单号未验证，会被伪造签名污染任意订单。
	MarkOrderNotifyFailure(ctx context.Context, outTradeNo, reason string) error
	// CloseOrder 单语句原子关单（WHERE id=? AND status='pending'）；0 行 → (false,nil)。
	CloseOrder(ctx context.Context, id int64, failReason *string) (bool, error)
	// ResolveOrder 写 resolved_at（防同一订单被补两次）；已标记 → (false,nil)。
	ResolveOrder(ctx context.Context, id int64) (bool, error)
	// UpdateOrderRawNotify 只更新 raw_notify（sync 遇到非 paid 状态时记录，不迁移状态）。
	UpdateOrderRawNotify(ctx context.Context, id int64, rawNotify *string) error

	// 账户/渠道（spec §3-1/§3-2/§10-3）。命名带 Payment 前缀：Go 不能重载，
	// 且不与既有上游账号的 CreateAccount/GetAccount 冲突（§17-9）。
	CreatePaymentAccount(ctx context.Context, a *domain.PaymentAccount) (*domain.PaymentAccount, error)
	GetPaymentAccountByKey(ctx context.Context, key string) (*domain.PaymentAccount, error)
	ListPaymentAccounts(ctx context.Context) ([]*domain.PaymentAccount, error)
	UpdatePaymentAccount(ctx context.Context, key string, upd repository.AccountUpdate) (*domain.PaymentAccount, error)
	// SoftDeletePaymentAccount **软停**（enabled=false，行永不删除，幂等）。
	SoftDeletePaymentAccount(ctx context.Context, key string) error
	UpsertPaymentChannel(ctx context.Context, accountID int64, ch *domain.PaymentChannel) (*domain.PaymentChannel, error)
	DeletePaymentChannel(ctx context.Context, accountID int64, payType string) error
	ListPaymentChannels(ctx context.Context, accountID int64) ([]*domain.PaymentChannel, error)
}

// 支付编排专用哨兵（domain 层哨兵之外的**归因**信号，供 notify 独立错误
// 映射表按 §8 落 fail_reason；不承担 HTTP 语义）。
var (
	// ErrProviderMismatch 回调协议与订单快照不符（跨协议回调）→ fail + provider。
	ErrProviderMismatch = errors.New("payment: provider mismatch")
	// ErrAmountMismatch 金额或币种不等值 → fail + amount。
	ErrAmountMismatch = errors.New("payment: amount mismatch")
	// ErrOrderClosed 已关单收到成功通知 → fail + closed + ERROR 日志（必须人工介入）。
	ErrOrderClosed = errors.New("payment: order closed")
	// ErrStatusNotPaid 非成功状态进入完成流程（纵深防御：handler 已按 trade_status
	// 分流，走到这里说明调用面写错了，必须 fail-closed 不入账）→ fail + status。
	ErrStatusNotPaid = errors.New("payment: notify status is not paid")
	// ErrPaymentProviderMissing 账户声明的协议没有注册（spec §4：管理面红标 +
	// ERROR + 指标，选路跳过，回调 503 让网关重试）。**不是** ErrConflict：管理面
	// 把它当 503（可修复的装配问题），不是 409。
	ErrPaymentProviderMissing = errors.New("payment: provider is not registered")
)

// rawNotifyMaxBytes raw_notify 上限（§4-1：清洗后 ≤4 KiB；B18）。
const rawNotifyMaxBytes = 4 << 10

// orderNoAttempts 单号冲突换号重试次数（§4-8：≤3）。
const orderNoAttempts = 3

// completeOrderAttempts 完成订单的最大尝试次数（§4-4：3 次，仅可重试错误）。
const completeOrderAttempts = 3

// secretPlaceholder 管理面回显的秘密字段占位（PUT 收到它 = 保持原值，§3-1/B47）。
const secretPlaceholder = "***"

// 金额换算常量（§4-3，全整数）：
//
//	credit_milli = usd_cents * 1000
//	pay_minor    = (credit_milli*rate_micro + 5e8) / 1e9   // half-up
const (
	milliPerCent = int64(1000)
	// feeHalfUnit half-up 的进位偏移 = 1e9/2。
	feeHalfUnit      = int64(500_000_000)
	rateMicroPerUnit = int64(1_000_000_000)
	// milliPerUSD 1 USD = 100,000 毫分（= 100 分 × 1000）。
	milliPerUSD = int64(100_000)
	// maxInt64 溢出判据用（除法形式，绝不做乘法比较）。
	maxInt64 = int64(^uint64(0) >> 1)
)

// usdAmountPattern amount_usd 值域：`^\d{1,7}(\.\d{1,2})?$`（§4-3/B1）。
var usdAmountPattern = regexp.MustCompile(`^\d{1,7}(\.\d{1,2})?$`)

// paymentAccountKeyPattern 账户 key 值域（§3-1）：它是 URL 段 + 订单快照，
// 也是网关后台填过的回调地址——形状必须在写入时就定死。
var paymentAccountKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)

// newOrderCode 订单到账码生成入口（P15 注入点：测试替换后 defer 还原——码冲突
// 重试路径要用「先诱饵后新码」编排）。
var newOrderCode = randomCode

// cachedPaymentCfg 账户配置缓存项（N19）：键 = account_key，值里带
// config_revision——revision 变了就当 miss（写面还会主动删，双保险）。
type cachedPaymentCfg struct {
	revision int64
	cfg      domain.AccountConfig
}

// PaymentMethod 一条可售渠道的展示面（GET /api/user/payment/methods）。
//
// `RateMicro` 是**参考值**（"当前汇率"），不构成报价承诺；下单返回的 pay_amount
// 才是权威值（§11-11/§5-3）。
type PaymentMethod struct {
	PayType   string
	Provider  domain.ProviderName
	Currency  string
	RateMicro int64
	MinUSD    int64
	MaxUSD    int64 // **生效**上限（已按汇率可表示范围收敛）
}

// CreateOrderInput 创建订单输入（POST /api/user/orders）。
type CreateOrderInput struct {
	AmountUSD string // 字符串金额（禁 float：解析全程整数）
	PayType   string
}

// CreateOrderResult 创建订单结果（含已签名的跳转 URL 与订单快照）。
type CreateOrderResult struct {
	Order  *domain.Order
	PayURL string
}

// CompleteInput 完成订单输入（§6-2 第 7–10 步）。
//
// AccountKey / Provider 来自**回调 URL 段 + 账户行**，绝不从订单取——不一致必须
// 在 paid 幂等分支之前被拦住（M23/P32），否则错账户的通知会被幂等分支吞掉。
type CompleteInput struct {
	Provider     domain.ProviderName
	AccountKey   string
	OutTradeNo   string
	TradeNo      string
	MoneyMinor   int64  // 实付最小单位（渠道币种分）
	PaidCurrency string // 空 = 上游未提供：以订单快照为币种（epay notify 不带币种）
	Status       domain.NotifyStatus
	Timestamp    string
	RawNotify    *string // 清洗+截断后的 JSON（≤4 KiB）
}

// SyncResult 查单结果（管理面 POST /orders/{id}/sync）。
type SyncResult struct {
	Order       *domain.Order
	Credited    bool // 本次调用完成入账（false = 早已 paid 或非 paid 状态）
	AlreadyPaid bool // 查单时订单已 paid（幂等重放）
}

// CloseResult 关单结果（管理面 POST /orders/{id}/close）。Status 是**处置后**
// 的状态：paid（查单发现已付款并转入账）或 closed（真的关掉了）。
type CloseResult struct {
	Order    *domain.Order
	Status   domain.OrderStatus
	Credited bool // Status=paid 时：本次调用是否完成入账
}

// Resolved 选路结果（§5-1）。`Config` 是**选路当时**取出的配置快照（N3）：
// 建单签名只用它，账户在本请求内被禁用/改配置都不影响结果。
type Resolved struct {
	AccountKey string
	Provider   domain.ProviderName
	Channel    domain.ChannelConfig
	Config     domain.AccountConfig
}

// PaymentAccountView 管理面账户视图（§10-3）：config **恒掩码** + config_error +
// 渠道行。秘密字段一律 `"***"`（绝不回显密钥）。
type PaymentAccountView struct {
	Account     *domain.PaymentAccount
	Channels    []*domain.PaymentChannel
	Config      map[string]any
	ConfigError string
}

// PaymentAccountInput 创建账户输入（POST /payment/accounts）。
//
// key/provider/merchant_ref 都是**只读身份**：key 由请求给（URL 段），provider
// 由请求给（只读），merchant_ref 由 `ParseConfig` 从 config 推导（N29）。
type PaymentAccountInput struct {
	Key         string
	Provider    domain.ProviderName
	DisplayName string
	Enabled     bool
	Priority    int
	Config      json.RawMessage
}

// PaymentAccountUpdateInput 更新账户输入（PUT /payment/accounts/{key}）。
//
// 指针 nil = 保持不变；Config nil = 不改配置（**不动** config_revision）。
type PaymentAccountUpdateInput struct {
	DisplayName *string
	Priority    *int
	Enabled     *bool
	Config      json.RawMessage
}

// PaymentChannelInput 渠道 upsert 输入（PUT /payment/accounts/{key}/channels/{pay_type}）。
type PaymentChannelInput struct {
	PayType   string
	Enabled   bool
	Currency  string
	RateMicro int64
	MinUSD    int64
	MaxUSD    int64
}

// isRetryableTxErr 可重试的事务级冲突：唯一/条件迁移冲突（repository.ErrConflict）
// 或 PG 死锁/锁不可用（40P01/55P03）。判据与 supplier/retry.go 同源（本地复制
// 以断开 service→supplier 依赖；两处均有单测钉住）。
func isRetryableTxErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, repository.ErrConflict) {
		return true
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code == "40P01" || pgErr.Code == "55P03"
	}
	return false
}

// OrderFailReason 把完成订单的错误归类为 fail_reason（§8 表）。返回空串 =
// **不写审计**（未知单号/验签类输入错误：单号未验证，不得污染订单行）。
func OrderFailReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrOrderNotFound):
		return "" // 未知单号：不写（§8）
	case errors.Is(err, domain.ErrAccountMismatch):
		return domain.OrderFailReasonAccount
	case errors.Is(err, ErrProviderMismatch):
		return domain.OrderFailReasonProvider
	case errors.Is(err, ErrAmountMismatch):
		return domain.OrderFailReasonAmount
	case errors.Is(err, ErrOrderClosed):
		return domain.OrderFailReasonClosed
	case errors.Is(err, ErrStatusNotPaid):
		return domain.OrderFailReasonStatus
	case errors.Is(err, domain.ErrUserUnavailable):
		return domain.OrderFailReasonUser
	case errors.Is(err, repository.ErrConflict),
		errors.Is(err, domain.ErrInternal),
		errors.Is(err, ErrUpstream):
		return domain.OrderFailReasonInternal
	}
	return ""
}

// ---------- 选路（唯一入口，§5-1） ----------

// ResolveChannel **唯一**选路入口：methods 与 CreateOrder 都必须调它
// （`PaymentMethods` 只投影结果，`CreateOrder` 用返回的 cfg 签名）。
//
// 过滤顺序**严格**按 §5-1：
//
//	① 账户 enabled → ② 协议已注册 → ③ config 可解析（缓存）→
//	④ 渠道 enabled 且 pay_type 匹配 → ⑤ 币种白名单 → ⑥ rate_micro > 0 →
//	⑦ amountUSD ∈ [min_usd, max_usd]（N18）→ ⑧ priority ASC, id ASC 取第一个
//
// 两个类型化错误（§10-4）：
//   - ErrUnknownPayType：任何账户都没有该 pay_type 的渠道行 → 400
//   - ErrNoEligibleAccount：有渠道行但全被过滤（停用/协议未注册/配置坏/超限额）→ 503
func (s *Service) ResolveChannel(ctx context.Context, payType string, amountUSD int64) (Resolved, error) {
	return s.resolveChannel(ctx, payType, amountUSD*100)
}

// resolveChannel 选路的**唯一实现**（单位：USD 分）。
//
// 为什么内部用分：`amount_usd` 允许两位小数，而 `min_usd/max_usd` 是整数 USD；
// 用分做比较才不会被 1.99 > max=1 这类边界骗过（公开入口把整数 USD 乘 100 后
// 走同一条路径，禁止第二份过滤代码）。
func (s *Service) resolveChannel(ctx context.Context, payType string, amountCents int64) (Resolved, error) {
	accounts, err := s.store.ListPaymentAccounts(ctx)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: list payment accounts: %w", domain.ErrInternal, err)
	}
	// ⑧ 的排序先做：priority ASC, id ASC（M21/M33 的判据）。
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority < accounts[j].Priority
		}
		return accounts[i].ID < accounts[j].ID
	})

	sawPayType := false
	for _, acct := range accounts {
		channels, chErr := s.store.ListPaymentChannels(ctx, acct.ID)
		if chErr != nil {
			return Resolved{}, fmt.Errorf("%w: list payment channels (account %q): %w",
				domain.ErrInternal, acct.Key, chErr)
		}
		// pay_type 的"存在性"必须在所有过滤**之前**统计：它决定 400（未知方式）
		// 还是 503（有这种方式但当前不可用）。
		var channel *domain.PaymentChannel
		for _, ch := range channels {
			if ch.PayType != payType {
				continue
			}
			sawPayType = true
			if channel == nil {
				channel = ch
			}
		}
		if !acct.Enabled {
			continue // ①
		}
		p, ok := s.paymentRegistry.Get(acct.Provider)
		if !ok {
			// ② 协议未注册：跳过 + 可观测（管理面 config_error 会标红）。
			s.warnPaymentAccount(acct, fmt.Errorf("%w: %q", ErrPaymentProviderMissing, acct.Provider))
			continue
		}
		cfg, cfgErr := s.paymentAccountConfig(ctx, acct, p)
		if cfgErr != nil {
			continue // ③ 配置不可解析：跳过 + 可观测
		}
		if channel == nil || !channel.Enabled {
			continue // ④
		}
		if !domain.PaymentCurrencySupported(channel.Currency) {
			continue // ⑤
		}
		if channel.RateMicro <= 0 {
			continue // ⑥
		}
		maxUSD := effectiveMaxUSD(channel.MaxUSD, channel.RateMicro)
		if maxUSD <= 0 {
			continue // 汇率不可表示 → 与 ⑥ 同口径不可售
		}
		if amountCents < channel.MinUSD*100 || amountCents > maxUSD*100 {
			continue // ⑦
		}
		return Resolved{
			AccountKey: acct.Key,
			Provider:   acct.Provider,
			Channel:    channel.ChannelConfig(),
			Config:     cfg,
		}, nil
	}
	if !sawPayType {
		return Resolved{}, fmt.Errorf("%w: %q", domain.ErrUnknownPayType, payType)
	}
	return Resolved{}, fmt.Errorf("%w: pay_type %q amount %d cents", domain.ErrNoEligibleAccount, payType, amountCents)
}

// PaymentMethods 可售方式列表（GET /api/user/payment/methods，§5-2）：
// `distinct(pay_type)` → 逐项调**同一个** ResolveChannel → 投影。
//
// **不得另写过滤**（M26）：展示面与下单面必须是同一次判定的两个投影，否则会出现
// "展示 A、下单 B"（价格承诺失信）。
//
// **看**总开关（§5-1b 销售面）：`payment.enabled=false` → 返回**空列表**（不给用户
// 看不可用的入口）。结算面（notify / sync / return / CompleteOrder / CloseOrder）
// 一律**不看**该开关——钱已收的路径不能被开关掐断（F16）：开关管"能不能卖"，
// 不管"能不能结算"。
func (s *Service) PaymentMethods(ctx context.Context) ([]PaymentMethod, error) {
	if !s.settingBool(domain.SettingPaymentEnabled, false) {
		// 空可售集：用户面渲染"暂无可用方式"即可，不是错误（§5-1b）。
		return []PaymentMethod{}, nil
	}
	accounts, err := s.store.ListPaymentAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: list payment accounts: %w", domain.ErrInternal, err)
	}
	// distinct(pay_type) + 该方式的**探针金额候选集**（§5-2）。
	//
	// 候选集 = 该 pay_type 在**所有**渠道行上的 min_usd（升序去重），**不做过滤**：
	// 过滤是 ResolveChannel 的职责（M26：methods 内不得另写过滤条件），这里只决定
	// "拿哪些金额去问它"。收集停用账户/停用渠道的 min_usd 是**故意**的——
	//
	//   - 每个渠道的 min_usd 都在候选集里，所以"最小的那个**可用**渠道的 min_usd"
	//     必然被试到，不会漏；
	//   - 候选金额只用来问 ResolveChannel "这个金额能不能卖"，命中后投影的是它的
	//     返回结果（含真实 min_usd），探针金额不参与任何计算，所以试大一点的金额
	//     不会把展示价抬高。
	//
	// 为什么不能只留最小值：min_usd 是"该渠道能收多少起"，而人工切流（§5-4）的
	// 手段就是 enabled=false（或调小备用 priority）。主账户 alipay min_usd=1 被软
	// 停、备用 min_usd=10 时，唯一下限探针停在 1 → ResolveChannel(1) 只剩备用、
	// 备用要求 ≥10 → ErrNoEligibleAccount → 整个 alipay 从列表消失（备用明明可卖
	// ≥10）：照 spec 切流反而把支付方式卖没了。
	minUSDByType := make(map[string][]int64, 4)
	for _, acct := range accounts {
		channels, chErr := s.store.ListPaymentChannels(ctx, acct.ID)
		if chErr != nil {
			return nil, fmt.Errorf("%w: list payment channels (account %q): %w",
				domain.ErrInternal, acct.Key, chErr)
		}
		for _, ch := range channels {
			minUSDByType[ch.PayType] = append(minUSDByType[ch.PayType], ch.MinUSD)
		}
	}
	payTypes := make([]string, 0, len(minUSDByType))
	for payType := range minUSDByType {
		payTypes = append(payTypes, payType)
	}
	sort.Strings(payTypes)

	out := make([]PaymentMethod, 0, len(payTypes))
	for _, payType := range payTypes {
		// 升序逐个试：小的下限被停用行占住时，换下一个更大的下限再问一次。
		probes := minUSDByType[payType]
		slices.Sort(probes)
		probes = slices.Compact(probes)
		var (
			res    Resolved
			onSale bool
		)
		for _, probe := range probes {
			amountUSD := probe
			if amountUSD < 1 {
				amountUSD = 1
			}
			got, rErr := s.ResolveChannel(ctx, payType, amountUSD)
			if rErr == nil {
				res, onSale = got, true
				break
			}
			if errors.Is(rErr, domain.ErrNoEligibleAccount) {
				continue // 该下限只挂在不可用账户/渠道上 → 试下一个更大的下限
			}
			if errors.Is(rErr, domain.ErrUnknownPayType) {
				break // 候选集来自渠道行，理论上不会发生；保持原语义（不出现）
			}
			return nil, rErr
		}
		if !onSale {
			continue // 当前不可售：不出现在列表里（不是错误）
		}
		maxUSD := effectiveMaxUSD(res.Channel.MaxUSD, res.Channel.RateMicro)
		if maxUSD <= 0 {
			continue
		}
		out = append(out, PaymentMethod{
			PayType:   res.Channel.PayType,
			Provider:  res.Provider,
			Currency:  res.Channel.Currency,
			RateMicro: res.Channel.RateMicro,
			MinUSD:    res.Channel.MinUSD,
			MaxUSD:    maxUSD,
		})
	}
	return out, nil
}

// ---------- 配置缓存（N19） ----------

// paymentAccountConfig 取账户的配置快照（缓存键 = account_key + config_revision）。
//
// 解析失败**不缓存**（坏行不值得占缓存位），但每次都会包 domain.ErrConfigMissing
// ——回调据此 503（让网关重试），选路据此跳过，管理面据此标红。
func (s *Service) paymentAccountConfig(ctx context.Context, acct *domain.PaymentAccount, p domain.PaymentProvider) (domain.AccountConfig, error) {
	s.paymentCfgMu.RLock()
	cached, ok := s.paymentCfgs[acct.Key]
	s.paymentCfgMu.RUnlock()
	if ok && cached.cfg != nil && cached.revision == acct.ConfigRevision {
		return cached.cfg, nil
	}
	cfg, err := p.ParseConfig(acct.Config)
	if err != nil {
		s.warnPaymentAccount(acct, err)
		return nil, fmt.Errorf("%w: account %q: %w", domain.ErrConfigMissing, acct.Key, err)
	}
	s.paymentCfgMu.Lock()
	if s.paymentCfgs == nil {
		s.paymentCfgs = make(map[string]cachedPaymentCfg, 4)
	}
	s.paymentCfgs[acct.Key] = cachedPaymentCfg{revision: acct.ConfigRevision, cfg: cfg}
	s.paymentCfgMu.Unlock()
	return cfg, nil
}

// storePaymentConfig 写面（创建后）顺手种缓存：创建即 config_revision = 1。
func (s *Service) storePaymentConfig(acct *domain.PaymentAccount, cfg domain.AccountConfig) {
	if acct == nil || cfg == nil {
		return
	}
	s.paymentCfgMu.Lock()
	if s.paymentCfgs == nil {
		s.paymentCfgs = make(map[string]cachedPaymentCfg, 4)
	}
	s.paymentCfgs[acct.Key] = cachedPaymentCfg{revision: acct.ConfigRevision, cfg: cfg}
	s.paymentCfgMu.Unlock()
}

// invalidatePaymentConfig 配置变更后失效缓存（§4：UpdateAccount 提交后删除该键）。
func (s *Service) invalidatePaymentConfig(key string) {
	s.paymentCfgMu.Lock()
	delete(s.paymentCfgs, key)
	s.paymentCfgMu.Unlock()
}

// warnPaymentAccount 配置/装配问题必须**可观测**（§4：静默只允许存在于选路内部）：
// ERROR 日志 + 选路跳过。日志里不含 config 内容（密钥不落日志）。
func (s *Service) warnPaymentAccount(acct *domain.PaymentAccount, err error) {
	if s.log == nil || acct == nil {
		return
	}
	s.log.Error("payment: account unusable",
		logx.String("account_key", acct.Key),
		logx.String("provider", string(acct.Provider)),
		logx.Int64("config_revision", acct.ConfigRevision),
		logx.Error(err))
}

// ---------- 订单 ----------

// CreateOrder 创建充值订单（§5-3/§4-8）：
//
//	① 总开关（只管创建，F16）→ ② 金额整数解析 → ③ ResolveChannel（同一入口）
//	→ ④ 生效上限/换算（禁 float）→ ⑤ 事务外生成单号 → ⑥ 插库（冲突换号 ≤3）
//	→ ⑦ 用**选路快照的 cfg** 签名（N3）
func (s *Service) CreateOrder(ctx context.Context, userID int64, in CreateOrderInput) (*CreateOrderResult, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("%w: missing user", ErrInvalidInput)
	}
	if !s.settingBool(domain.SettingPaymentEnabled, false) {
		return nil, fmt.Errorf("%w: payment disabled (%s)", ErrUnavailable, domain.SettingPaymentEnabled)
	}
	usdCents, err := parseUSDAmount(in.AmountUSD)
	if err != nil {
		return nil, err
	}
	// ③ 唯一入口：金额按**分**传入（同一份过滤实现），错误类型决定 400/503。
	resolved, err := s.resolveChannel(ctx, in.PayType, usdCents)
	if err != nil {
		if errors.Is(err, domain.ErrUnknownPayType) {
			return nil, fmt.Errorf("%w: unknown pay_type %q", ErrInvalidInput, in.PayType)
		}
		if errors.Is(err, domain.ErrNoEligibleAccount) {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return nil, err
	}
	ch := resolved.Channel
	// ④ 生效上限（F46：汇率可表示范围）+ 整数换算。
	maxUSD := effectiveMaxUSD(ch.MaxUSD, ch.RateMicro)
	if maxUSD <= 0 || usdCents > maxUSD*100 {
		return nil, fmt.Errorf("%w: amount_usd exceeds the channel representable range (max %d USD)",
			ErrInvalidInput, maxUSD)
	}
	creditMilli := quoteCreditMilli(usdCents)
	payMinor, err := quotePayMinor(creditMilli, ch.RateMicro)
	if err != nil {
		return nil, fmt.Errorf("%w: amount_usd %q (%v)", ErrInvalidInput, in.AmountUSD, err)
	}
	if payMinor < 1 {
		return nil, fmt.Errorf("%w: pay amount rounds down to 0 (B4)", ErrInvalidInput)
	}
	// 回调基址是 service 级配置：缺失在**建单前**就 503（不留 closed 单，也不留
	// pending 挂单——一次配置错误不该在库里留痕迹）。
	if s.notifyBaseURL() == "" {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, domain.SettingNotifyBaseURL)
	}

	now := time.Now().UTC()
	ttl := s.settingIntOr(domain.SettingOrderTTLMinutes, 30)
	if ttl <= 0 {
		ttl = 30
	}
	var created *domain.Order
	for attempt := 0; attempt < orderNoAttempts; attempt++ {
		outTradeNo, noErr := randomOrderNo(now)
		if noErr != nil {
			return nil, fmt.Errorf("%w: generate out_trade_no: %w", domain.ErrInternal, noErr)
		}
		o := &domain.Order{
			OutTradeNo:  outTradeNo,
			UserID:      userID,
			Provider:    resolved.Provider,
			AccountKey:  resolved.AccountKey, // 上游快照（§3-3）
			Status:      domain.OrderStatusPending,
			CreditMilli: creditMilli,
			PayMinor:    payMinor,
			Currency:    ch.Currency,
			RateMicro:   ch.RateMicro,
			PayType:     ch.PayType,
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Duration(ttl) * time.Minute),
		}
		created, err = s.store.CreateOrder(ctx, o)
		if err != nil {
			if errors.Is(err, repository.ErrConflict) && attempt < orderNoAttempts-1 {
				continue // 单号冲突 → 换号重试
			}
			return nil, fmt.Errorf("%w: create order: %w", domain.ErrInternal, err)
		}
		break
	}
	if created == nil {
		return nil, fmt.Errorf("%w: create order: out of attempts", domain.ErrInternal)
	}

	// ⑦ 用**选路时那份 cfg** 签名（TOCTOU：账户随后被禁用/改配置不影响本单）。
	payURL, err := s.buildPayURL(created, resolved.Config)
	if err != nil {
		reason := domain.OrderFailReasonInternal
		if closeErr := s.closeOrderAfterPayURLFailure(ctx, created.ID, &reason); closeErr != nil {
			// 关单也失败 → 订单留 pending（TTL 兜底，不 panic、不回滚）。
			if s.log != nil {
				s.log.Error("payment: close order after pay url failure",
					logx.String("out_trade_no", created.OutTradeNo),
					logx.Bool("pay_url_close_failed", true), logx.Error(closeErr))
			}
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return &CreateOrderResult{Order: created, PayURL: payURL}, nil
}

// payURLCleanupAttempts BuildPayURL 失败后关单的尝试次数（M3：有界 2 次）。
const payURLCleanupAttempts = 2

// payURLCleanupBackoff 两次关单之间的退避：极短（建单请求不该被关单拖住）。
// 用 time.After 而非 time.Sleep——等待必须可被 ctx 取消打断。
const payURLCleanupBackoff = 20 * time.Millisecond

// closeOrderAfterPayURLFailure 关掉「签名失败」的订单（M3）：CloseOrder 是单语句
// 原子更新，连接抖动/死锁这类瞬时故障值得有界重试——不重试就会把一张
// BuildPayURL 已失败的 pending 单留在库里（用户收到 503，订单却显示待支付）。
func (s *Service) closeOrderAfterPayURLFailure(ctx context.Context, id int64, reason *string) error {
	var err error
	for attempt := 0; attempt < payURLCleanupAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err // 客户端已断开：不再占用连接，把上次错误交回调用方
			case <-time.After(payURLCleanupBackoff):
			}
		}
		if _, err = s.store.CloseOrder(ctx, id, reason); err == nil {
			return nil
		}
	}
	return err
}

// CompleteOrder 完成订单——**唯一入账路径**（单事务，§6-2 第 7–10 步）：
//
//	FOR UPDATE 行锁 → **账户一致（8）→ 协议一致（9）** → 状态 switch →
//	金额与币种等值 → 用户状态 → 建单次码 → applyInTx → CreateUse →
//	IncrementUsed（检查 bool）→ MarkOrderPaid
//
// 8/9 必须在 paid 幂等分支**之前**：否则已 paid 的订单收到错误账户的通知会被
// 幂等分支吞掉、回 success（M23 的资损形态）。
func (s *Service) CompleteOrder(ctx context.Context, in CompleteInput) (alreadyPaid bool, err error) {
	// 纵深防御：非成功状态绝不入账（handler 已按 trade_status 分流，M4 的第二道锁）。
	if in.Status != domain.NotifyStatusPaid {
		return false, fmt.Errorf("%w: %q", ErrStatusNotPaid, in.Status)
	}
	for attempt := 0; attempt < completeOrderAttempts; attempt++ {
		code, codeErr := newOrderCode()
		if codeErr != nil {
			return false, fmt.Errorf("%w: generate redemption code: %w", domain.ErrInternal, codeErr)
		}
		err = s.store.WithTx(ctx, func(tx repository.TxStore) error {
			if s.testBeforeOrderLock != nil {
				s.testBeforeOrderLock(ctx)
			}
			o, lockErr := tx.LockOrderByOutTradeNo(ctx, in.OutTradeNo)
			if lockErr != nil {
				if errors.Is(lockErr, repository.ErrNotFound) {
					// 未知单号：fail + 日志，不写 fail_reason（单号未验证，§8）。
					return fmt.Errorf("%w: %s", domain.ErrOrderNotFound, in.OutTradeNo)
				}
				return fmt.Errorf("%w: lock order: %w", domain.ErrInternal, lockErr)
			}
			if s.testAfterOrderLock != nil {
				s.testAfterOrderLock(ctx, o)
			}
			// ⑧ 账户一致（回调 URL 段 vs 订单快照）。
			if o.AccountKey != in.AccountKey {
				return fmt.Errorf("%w: order %s account %q, callback %q",
					domain.ErrAccountMismatch, o.OutTradeNo, o.AccountKey, in.AccountKey)
			}
			// ⑨ 协议一致（订单快照 vs 回调账户的协议）。
			if o.Provider != in.Provider {
				return fmt.Errorf("%w: order %s provider %s, callback %s",
					ErrProviderMismatch, o.OutTradeNo, o.Provider, in.Provider)
			}
			switch o.Status {
			case domain.OrderStatusPaid:
				alreadyPaid = true // 幂等：重复通知也回 success
				return nil
			case domain.OrderStatusClosed:
				return fmt.Errorf("%w: order %s", ErrOrderClosed, o.OutTradeNo)
			default:
				if o.Status != domain.OrderStatusPending {
					// 未知状态禁止入账（F43）。
					return fmt.Errorf("%w: unknown status %q", domain.ErrInternal, o.Status)
				}
			}
			// ⑩ 金额与币种都必须等值（P21：跨协议等额同币由 ⑧/⑨ 拦，这里拦金额/币种
			// 漂移）。上游不带币种时（epay notify 只有 money）以订单快照为币种——
			// 币种是渠道行的属性，不是通知的属性。
			paidCurrency := in.PaidCurrency
			if paidCurrency == "" {
				paidCurrency = o.Currency
			}
			if in.MoneyMinor != o.PayMinor || paidCurrency != o.Currency {
				return fmt.Errorf("%w: order %s expects %d/%s, notified %d/%s",
					ErrAmountMismatch, o.OutTradeNo, o.PayMinor, o.Currency, in.MoneyMinor, paidCurrency)
			}
			u, userErr := tx.GetUser(ctx, o.UserID)
			if userErr != nil {
				if errors.Is(userErr, repository.ErrNotFound) {
					// 真·被删（F35）：其余 DB 错误 → 5xx 让网关重试（F53）。
					return fmt.Errorf("%w: user %d missing", domain.ErrUserUnavailable, o.UserID)
				}
				return fmt.Errorf("%w: get user: %w", domain.ErrInternal, userErr)
			}
			if u.Status != domain.UserStatusActive {
				// 被禁 → 不入账（既有 applier 只看 id，必须在此显式判状态）。
				return fmt.Errorf("%w: user %d status %s", domain.ErrUserUnavailable, u.ID, u.Status)
			}
			remark := "order " + o.OutTradeNo
			c := &domain.RedemptionCode{
				Code: code, Type: domain.RedemptionTypeBalance, Value: o.CreditMilli,
				Remark: &remark, MaxUses: 1, UsedCount: 0,
				Status: domain.RedemptionStatusActive, CreatedBy: 0,
			}
			if err := tx.CreateCodes(ctx, []*domain.RedemptionCode{c}); err != nil {
				return err
			}
			if err := s.applyInTx(ctx, tx, o.UserID, c); err != nil {
				return err
			}
			if err := tx.CreateUse(ctx, &domain.RedemptionUse{
				CodeID: c.ID, UserID: o.UserID, Value: c.Value,
			}); err != nil {
				return err
			}
			ok, incErr := tx.IncrementUsed(ctx, c.ID)
			if incErr != nil {
				return incErr
			}
			if !ok {
				// 码用尽（max_uses=1 已用）→ 冲突回滚（F44：既有 Redeem 也检查 bool）。
				return fmt.Errorf("%w: code exhausted", repository.ErrConflict)
			}
			return tx.MarkOrderPaid(ctx, o.ID, c.ID, in.TradeNo, in.RawNotify)
		})
		if err != nil && attempt < completeOrderAttempts-1 && isRetryableTxErr(err) {
			continue // 新事务 + 新码（多造码无副作用，F51）
		}
		break
	}
	if err != nil {
		return alreadyPaid, err
	}
	if !alreadyPaid {
		s.inv.Users()                              // 余额已变更 → auth + 余额快照刷新
		s.publish(ctx, notify.Change{Users: true}) // 其余实例同步
	}
	return alreadyPaid, nil
}

// SyncOrder 管理面查单（§7-3/M27）：非 pending → 409；无查单能力 → 409；查单失败
// → 502（订单不变）；paid → 走 CompleteOrder（与 notify 同一把行锁）。
//
// **按订单快照的 account_key 取账户与配置**（不是当前主账户）：订单在 A 上创建、
// 之后主切到 B，查单/回调仍必须打到 A（P37：A.Query 1 次、B 0 次）。
func (s *Service) SyncOrder(ctx context.Context, id int64) (*SyncResult, error) {
	o, err := s.store.GetOrderByID(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if o.Status != domain.OrderStatusPending {
		return nil, fmt.Errorf("%w: order %s is %s", ErrConflict, o.OutTradeNo, o.Status)
	}
	querier, acctCfg, err := s.orderQuerier(ctx, o)
	if err != nil {
		return nil, err
	}
	ev, err := querier.Query(ctx, acctCfg, o.OutTradeNo)
	if err != nil {
		// 上游故障（§7-4 502）：订单不变，只落 internal 信号（B32）。
		s.markNotifyFailure(ctx, o.OutTradeNo, domain.OrderFailReasonInternal)
		return nil, fmt.Errorf("%w: query order %s: %w", ErrUpstream, o.OutTradeNo, err)
	}
	rawNotify := SanitizeRawNotify(ev.Raw)
	if ev.Status != domain.NotifyStatusPaid {
		// 非 paid：只写 raw_notify 与审计，不迁移状态（B14）。
		if err := s.store.UpdateOrderRawNotify(ctx, o.ID, rawNotify); err != nil {
			return nil, fmt.Errorf("%w: record raw notify: %w", domain.ErrInternal, err)
		}
		fresh, err := s.store.GetOrderByID(ctx, id)
		if err != nil {
			return nil, mapRepoErr(err)
		}
		return &SyncResult{Order: fresh}, nil
	}
	currency := ev.PaidCurrency
	if currency == "" {
		// 协议 V2 查单响应通常不含币种：以订单快照为准（notify 面仍严格比对）。
		currency = o.Currency
	}
	alreadyPaid, err := s.CompleteOrder(ctx, CompleteInput{
		Provider:     o.Provider,   // 历史订单走**订单快照**的协议（B25/P20）
		AccountKey:   o.AccountKey, // 历史订单走**订单快照**的账户（M27）
		OutTradeNo:   o.OutTradeNo,
		TradeNo:      ev.TradeNo,
		MoneyMinor:   ev.PaidMinor,
		PaidCurrency: currency,
		Status:       ev.Status,
		Timestamp:    ev.Timestamp,
		RawNotify:    rawNotify,
	})
	if err != nil {
		if reason := OrderFailReason(err); reason != "" {
			s.markNotifyFailure(ctx, o.OutTradeNo, reason)
		}
		return nil, err
	}
	fresh, err := s.store.GetOrderByID(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return &SyncResult{Order: fresh, Credited: !alreadyPaid, AlreadyPaid: alreadyPaid}, nil
}

// orderQuerier 取订单快照账户的查单面（§9：sync/close 的"前置查询"入口）。
//
// 三层可失败点：账户行（已删/不存在）、协议未注册、配置不可解析——都归到
// ErrUnavailable/ErrConflict 的**可判定**错误上（管理面 409/502/503，不是 500）。
func (s *Service) orderQuerier(ctx context.Context, o *domain.Order) (domain.OrderQuerier, domain.AccountConfig, error) {
	acct, err := s.store.GetPaymentAccountByKey(ctx, o.AccountKey)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// 账户行消失（数据损坏/人工清理）：订单仍可读，但查单面不可用 → 503。
			return nil, nil, fmt.Errorf("%w: order %s account %q no longer exists",
				ErrUnavailable, o.OutTradeNo, o.AccountKey)
		}
		return nil, nil, fmt.Errorf("%w: get payment account %q: %w", domain.ErrInternal, o.AccountKey, err)
	}
	p, ok := s.paymentRegistry.Get(acct.Provider)
	if !ok {
		return nil, nil, fmt.Errorf("%w: provider %q of order %s", ErrUnavailable, acct.Provider, o.OutTradeNo)
	}
	cfg, err := s.paymentAccountConfig(ctx, acct, p)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	querier, ok := p.(domain.OrderQuerier)
	if !ok {
		return nil, nil, fmt.Errorf("%w: provider %s does not support order query", ErrConflict, acct.Provider)
	}
	return querier, cfg, nil
}

// closeQueryTarget 关单的「前置查询目标」（§9 的 409 族唯一判定点）：
//
//   - querier != nil：有查单面 → 调用方正常发起查询；
//   - querier == nil && err == nil：**无查单面**，noQuery 说明原因（账户行缺失 /
//     协议未注册 / cfg 不可解析 / 协议无查单能力）→ 调用方返 **409**（提示 force
//     或先修配置）。这四种都在"发起查询"之前就查不了，故不是 502；
//   - err != nil：仅账户读取本身的**非 NotFound** 故障（DB 不可用）→ 500。502 只
//     留给"查询请求本身失败"（网络 / 非 JSON / code!=0）。
func (s *Service) closeQueryTarget(ctx context.Context, o *domain.Order) (domain.OrderQuerier, domain.AccountConfig, string, error) {
	acct, err := s.store.GetPaymentAccountByKey(ctx, o.AccountKey)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// 账户行消失（数据损坏/人工清理）：订单仍可读，但查单面没了 → 409。
			return nil, nil, fmt.Sprintf("account %q no longer exists", o.AccountKey), nil
		}
		// 账户读取失败不是"无查单面"，是真内部错误：别把 500 说成 409。
		return nil, nil, "", fmt.Errorf("%w: get payment account %q: %w", domain.ErrInternal, o.AccountKey, err)
	}
	p, ok := s.paymentRegistry.Get(acct.Provider)
	if !ok {
		// 协议未注册：可观测（与管理面 config_error / 选路跳过同一口径，§17-12）。
		s.warnPaymentAccount(acct, fmt.Errorf("%w: %q", ErrPaymentProviderMissing, acct.Provider))
		return nil, nil, fmt.Sprintf("provider %q is not registered", acct.Provider), nil
	}
	cfg, cfgErr := s.paymentAccountConfig(ctx, acct, p)
	if cfgErr != nil {
		return nil, nil, fmt.Sprintf("account %q config is not parseable", acct.Key), nil
	}
	q, ok := p.(domain.OrderQuerier)
	if !ok {
		return nil, nil, fmt.Sprintf("provider %s does not support order query", acct.Provider), nil
	}
	return q, cfg, "", nil
}

// CloseOrder 关单（§9 完整语义）：有查单能力时**必须先查**
//
//	paid + 入账成功 → 200 {status: paid, credited: true}
//	paid + 入账失败 → **绝不关单**，502 + 同一 fail_reason 落库（订单保持 pending）
//	非 paid → 正常关单 → 200 {status: closed}（0 行 → 409）
//
// force 只在「无查单能力 / 查询本身失败」时生效；查询明确报 paid 时 force 无效。
// 账户/配置一律取**订单快照的 account_key**（M27）。
//
// **409 族**（§9 例外）：账户行缺失 / 账户 cfg 不可解析 / 协议未注册（以及协议无
// 查单能力）在"发起查询"之前就没有查单面 → 409 + "重试加 force 或先修配置"，
// **不是 502**。502 只留给"查询请求本身失败"（网络 / 非 JSON / code!=0）。
func (s *Service) CloseOrder(ctx context.Context, id int64, force bool) (*CloseResult, error) {
	o, err := s.store.GetOrderByID(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if o.Status != domain.OrderStatusPending {
		return nil, fmt.Errorf("%w: order %s is %s", ErrConflict, o.OutTradeNo, o.Status)
	}
	forced := false
	querier, acctCfg, noQuery, qErr := s.closeQueryTarget(ctx, o)
	if qErr != nil {
		return nil, qErr
	}
	if querier == nil {
		if !force {
			return nil, fmt.Errorf("%w: order %s has no usable order query (%s); retry with force or fix the payment account",
				ErrConflict, o.OutTradeNo, noQuery)
		}
		forced = true
	} else {
		ev, queryErr := querier.Query(ctx, acctCfg, o.OutTradeNo)
		switch {
		case queryErr != nil:
			if !force {
				s.markNotifyFailure(ctx, o.OutTradeNo, domain.OrderFailReasonInternal)
				return nil, fmt.Errorf("%w: query order %s: %w", ErrUpstream, o.OutTradeNo, queryErr)
			}
			forced = true
		case ev.Status == domain.NotifyStatusPaid:
			// 已付款：转入账（force 无效）。入账失败绝不关单。
			currency := ev.PaidCurrency
			if currency == "" {
				currency = o.Currency
			}
			alreadyPaid, cerr := s.CompleteOrder(ctx, CompleteInput{
				Provider: o.Provider, AccountKey: o.AccountKey, OutTradeNo: o.OutTradeNo,
				TradeNo: ev.TradeNo, MoneyMinor: ev.PaidMinor, PaidCurrency: currency,
				Status: ev.Status, Timestamp: ev.Timestamp, RawNotify: SanitizeRawNotify(ev.Raw),
			})
			if cerr != nil {
				if reason := OrderFailReason(cerr); reason != "" {
					s.markNotifyFailure(ctx, o.OutTradeNo, reason)
				}
				if s.log != nil {
					s.log.Error("payment: close refused, order is paid but credit failed",
						logx.String("out_trade_no", o.OutTradeNo), logx.String("trade_no", ev.TradeNo),
						logx.Error(cerr))
				}
				return nil, fmt.Errorf("%w: order %s is paid but credit failed: %w", ErrUpstream, o.OutTradeNo, cerr)
			}
			fresh, err := s.store.GetOrderByID(ctx, id)
			if err != nil {
				return nil, mapRepoErr(err)
			}
			return &CloseResult{Order: fresh, Status: domain.OrderStatusPaid, Credited: !alreadyPaid}, nil
		}
	}
	var failReason *string
	if forced {
		reason := domain.OrderFailReasonForced
		failReason = &reason
	}
	closed, err := s.store.CloseOrder(ctx, o.ID, failReason)
	if err != nil {
		return nil, fmt.Errorf("%w: close order: %w", domain.ErrInternal, err)
	}
	if !closed {
		// close ∥ notify 互斥：行已不是 pending（notify 赢）→ 409。
		return nil, fmt.Errorf("%w: order %s is no longer pending", ErrConflict, o.OutTradeNo)
	}
	fresh, err := s.store.GetOrderByID(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return &CloseResult{Order: fresh, Status: domain.OrderStatusClosed}, nil
}

// ListOrders 管理面订单列表（§7-3）：limit 默认 20 / 上限 100；status 非法枚举 →
// 400。用户面由 handler 强制 q.UserID = 本人（service 不猜身份）。
func (s *Service) ListOrders(ctx context.Context, q repository.OrderQuery) ([]*domain.Order, int64, error) {
	if q.Status != nil && !q.Status.Valid() {
		return nil, 0, ErrInvalidInput
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 100 {
		q.Limit = 100
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	return s.store.ListOrders(ctx, q)
}

// GetOrder 订单详情（管理面/用户面共用：归属校验在 handler，service 不猜身份）。
func (s *Service) GetOrder(ctx context.Context, id int64) (*domain.Order, error) {
	o, err := s.store.GetOrderByID(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return o, nil
}

// OrderPayURL 订单的**实时重签**跳转 URL（§10-1 详情面：pending 且未过期才返回
// pay_url）。
//
// 账户与配置按订单快照的 account_key 取（详情页访问时主账户可能已经切换）；
// 签名入参与建单逐字段一致，详情里再点支付才会得到与首次语义相同的签名串。
func (s *Service) OrderPayURL(ctx context.Context, o *domain.Order) (string, error) {
	if o == nil {
		return "", fmt.Errorf("%w: nil order", ErrInvalidInput)
	}
	acct, err := s.store.GetPaymentAccountByKey(ctx, o.AccountKey)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", fmt.Errorf("%w: order %s account %q no longer exists",
				ErrUnavailable, o.OutTradeNo, o.AccountKey)
		}
		return "", fmt.Errorf("%w: get payment account %q: %w", domain.ErrInternal, o.AccountKey, err)
	}
	p, ok := s.paymentRegistry.Get(acct.Provider)
	if !ok {
		return "", fmt.Errorf("%w: provider %q", ErrUnavailable, acct.Provider)
	}
	cfg, err := s.paymentAccountConfig(ctx, acct, p)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return s.buildPayURL(o, cfg)
}

// RecordNotifyFailure 通知失败的审计入口（§4-4/F21：**事务外**副作用；审计失败
// 只告警，绝不回滚或阻断入账）。notify handler 按 §8 表决定是否/用什么 reason
// 调用——验签、重复键、必填缺失、未知单号**不得**调用（单号未验证，会让伪造
// 签名污染任意订单行）。
func (s *Service) RecordNotifyFailure(ctx context.Context, outTradeNo, reason string) {
	s.markNotifyFailure(ctx, outTradeNo, reason)
}

// ResolveOrder 人工处置标记（§7-3 / F48/F20）：写 resolved_at，返回处置后的订单。
// 已标记过 → ErrConflict（管理面 409，B41：同一订单不得被补两次）；订单缺失 →
// ErrNotFound（404）。
func (s *Service) ResolveOrder(ctx context.Context, id int64) (*domain.Order, error) {
	ok, err := s.store.ResolveOrder(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	fresh, err := s.store.GetOrderByID(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if !ok {
		// 区分「订单不存在」（上面 Get 已 404）与「已处置过」：只有后者到这里。
		return nil, fmt.Errorf("%w: order %s is already resolved", ErrConflict, fresh.OutTradeNo)
	}
	return fresh, nil
}

// notifyBaseURL 回调/跳转基址（只填 scheme+host；去尾斜杠）；空 = 未配置。
func (s *Service) notifyBaseURL() string {
	return strings.TrimRight(s.settingValue(domain.SettingNotifyBaseURL), "/")
}

// PortalBaseURL 门户基址（`payment.portal_base_url`；空 = 用相对路径回退）。
// return 端点 302 到 `{portal}/user/wallet?order=…`（§6-1）——路径由本方法拼，
// 运维只配 scheme+host。
func (s *Service) PortalBaseURL() string {
	return strings.TrimRight(s.settingValue(domain.SettingPortalBaseURL), "/")
}

// payRequest 组装协议入参（建单与详情重签的唯一形状来源）。
//
// 回调/跳转地址按**订单快照的 account_key** 拼（§6-1："路径即账户"），协议实现
// 只把这两个 URL 原样放进待签串——它不知道也不该知道账户有几家。
func (s *Service) payRequest(o *domain.Order) (domain.PayRequest, error) {
	notifyBase := s.notifyBaseURL()
	if notifyBase == "" {
		return domain.PayRequest{}, fmt.Errorf("%w: %s", ErrUnavailable, domain.SettingNotifyBaseURL)
	}
	if o.AccountKey == "" {
		return domain.PayRequest{}, fmt.Errorf("%w: order %s has empty account_key",
			domain.ErrConfigMissing, o.OutTradeNo)
	}
	prefix := notifyBase + "/api/payment/" + o.AccountKey
	return domain.PayRequest{
		PayType:    o.PayType,
		Currency:   o.Currency,
		OutTradeNo: o.OutTradeNo,
		Subject:    "Balance_topup_" + o.OutTradeNo,
		Money:      formatMinor(o.PayMinor),
		NotifyURL:  prefix + "/notify",
		ReturnURL:  prefix + "/return",
	}, nil
}

// buildPayURL 用**调用方给的 cfg 快照**签名跳转 URL（N3：建单后账户被改动不影响
// 本单）。纵深防御：cfg 的协议必须与订单快照的协议一致（N31 的服务层那一层）。
func (s *Service) buildPayURL(o *domain.Order, cfg domain.AccountConfig) (string, error) {
	p, ok := s.paymentRegistry.Get(o.Provider)
	if !ok {
		return "", fmt.Errorf("%w: provider %q is not registered", ErrUnavailable, o.Provider)
	}
	if cfg == nil {
		return "", fmt.Errorf("%w: order %s has no config snapshot", domain.ErrConfigMissing, o.OutTradeNo)
	}
	if cfg.Provider() != p.Name() {
		return "", fmt.Errorf("%w: order %s config is %q, provider is %q",
			domain.ErrConfigProviderMismatch, o.OutTradeNo, cfg.Provider(), p.Name())
	}
	in, err := s.payRequest(o)
	if err != nil {
		return "", err
	}
	return p.BuildPayURL(cfg, in)
}

// markNotifyFailure 尽力记录一次失败审计（失败只告警：审计不得回滚/阻断入账）。
func (s *Service) markNotifyFailure(ctx context.Context, outTradeNo, reason string) {
	if reason == "" {
		return
	}
	if err := s.store.MarkOrderNotifyFailure(ctx, outTradeNo, reason); err != nil && s.log != nil {
		s.log.Warn("payment: mark notify failure",
			logx.String("out_trade_no", outTradeNo), logx.String("reason", reason), logx.Error(err))
	}
}

// ---------- 协议注册表 ----------

// PaymentProviderView 协议元数据（§18-2 的响应体）：只有形状与标志，**不含任何
// 配置值**——账户级的值只出现在账户读面的 config 里（那里按 secret 掩码）。
type PaymentProviderView struct {
	Name         domain.ProviderName
	Capabilities []string
	Schema       domain.ConfigSchema
}

// ListPaymentProviders 列出本二进制注册的全部协议及其配置形状（§18-2）。
//
// 顺序直接取注册表的 All()（name 升序，构造期已定死）——这里再排序就会制造第二个
// 定序点。Capabilities 复制一份：协议实现返回的是共享切片，调用方（handler 序列化
// 之外的代码）就地改动会污染后续响应。
func (s *Service) ListPaymentProviders() []PaymentProviderView {
	providers := s.paymentRegistry.All()
	out := make([]PaymentProviderView, 0, len(providers))
	for _, p := range providers {
		out = append(out, PaymentProviderView{
			Name:         p.Name(),
			Capabilities: append([]string(nil), p.Capabilities()...),
			Schema:       p.ConfigSchema(),
		})
	}
	return out
}

// PaymentProvider 按名字取协议实现（回调 handler 第 3 步；未注册 → false）。
func (s *Service) PaymentProvider(name domain.ProviderName) (domain.PaymentProvider, bool) {
	p, ok := s.paymentRegistry.Get(name)
	return p, ok
}

// providerFor 内部取协议实现；未注册 → 错误（回调面按 503 处理）。
func (s *Service) providerFor(name domain.ProviderName) (domain.PaymentProvider, error) {
	if p, ok := s.paymentRegistry.Get(name); ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrPaymentProviderMissing, name)
}

// ---------- 账户 / 渠道（管理面 CRUD，§10-3） ----------

// GetPaymentAccountConfig 取账户的**已解析配置**（回调 handler 第 2 步；走缓存）。
// 协议未注册 → ErrPaymentProviderMissing（第 3 步的语义，503）；解析失败 →
// domain.ErrConfigMissing（503，让网关重试）。
func (s *Service) GetPaymentAccountConfig(ctx context.Context, acct *domain.PaymentAccount) (domain.AccountConfig, error) {
	if acct == nil {
		return nil, fmt.Errorf("%w: nil payment account", ErrInvalidInput)
	}
	p, err := s.providerFor(acct.Provider)
	if err != nil {
		return nil, err
	}
	return s.paymentAccountConfig(ctx, acct, p)
}

// GetPaymentAccount 按 key 取账户（回调 handler 第 1 步；缺失 → ErrNotFound）。
func (s *Service) GetPaymentAccount(ctx context.Context, key string) (*domain.PaymentAccount, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: empty payment account key", ErrInvalidInput)
	}
	acct, err := s.store.GetPaymentAccountByKey(ctx, key)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return acct, nil
}

// ListPaymentAccounts 管理面账户列表（含渠道 / config_error / 掩码后的 config）。
func (s *Service) ListPaymentAccounts(ctx context.Context) ([]PaymentAccountView, error) {
	accounts, err := s.store.ListPaymentAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: list payment accounts: %w", domain.ErrInternal, err)
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority < accounts[j].Priority
		}
		return accounts[i].ID < accounts[j].ID
	})
	out := make([]PaymentAccountView, 0, len(accounts))
	for _, acct := range accounts {
		view, err := s.describePaymentAccount(ctx, acct)
		if err != nil {
			return nil, err
		}
		out = append(out, *view)
	}
	return out, nil
}

// CreatePaymentAccount 创建账户：`ParseConfig` 校验 + 推导 merchant_ref；
// 非法配置 → 400（ErrInvalidInput），`(provider, merchant_ref)` 或 key 冲突 → 409。
func (s *Service) CreatePaymentAccount(ctx context.Context, in PaymentAccountInput) (*PaymentAccountView, error) {
	if !paymentAccountKeyPattern.MatchString(in.Key) {
		return nil, fmt.Errorf("%w: payment account key must match %s", ErrInvalidInput,
			paymentAccountKeyPattern.String())
	}
	p, ok := s.paymentRegistry.Get(in.Provider)
	if !ok {
		return nil, fmt.Errorf("%w: provider %q is not registered", ErrInvalidInput, in.Provider)
	}
	cfg, err := p.ParseConfig(in.Config)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	merchantRef := cfg.MerchantRef()
	if merchantRef == "" {
		return nil, fmt.Errorf("%w: config does not declare a merchant reference", ErrInvalidInput)
	}
	displayName := in.DisplayName
	if displayName == "" {
		displayName = in.Key
	}
	acct, err := s.store.CreatePaymentAccount(ctx, &domain.PaymentAccount{
		Key:         in.Key,
		Provider:    in.Provider,
		MerchantRef: merchantRef,
		DisplayName: displayName,
		Enabled:     in.Enabled,
		Priority:    in.Priority,
		Config:      in.Config,
	})
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, fmt.Errorf("%w: create payment account: %w", domain.ErrInternal, err)
	}
	s.storePaymentConfig(acct, cfg) // 创建即 revision 1：顺手种缓存
	return s.describePaymentAccount(ctx, acct)
}

// UpdatePaymentAccount 更新账户（只改 display_name/priority/enabled/config）：
//
//   - config 里秘密字段值为 `"***"` 的用旧值替换（B47/P34，通用实现靠
//     `ConfigSchema().SecretNames()`）；
//   - 新 config 的 `MerchantRef()` 必须等于现列，否则 **400**（N29/P45：
//     `repository.ErrMerchantRefImmutable` 是 ErrConflict 的细分，handler 单独映射 400）；
//   - 成功 `config_revision++`（repo 做）+ **失效缓存**。
func (s *Service) UpdatePaymentAccount(ctx context.Context, key string, in PaymentAccountUpdateInput) (*PaymentAccountView, error) {
	cur, err := s.GetPaymentAccount(ctx, key)
	if err != nil {
		return nil, err
	}
	upd := repository.AccountUpdate{
		DisplayName: in.DisplayName,
		Priority:    in.Priority,
		Enabled:     in.Enabled,
	}
	if in.Config != nil {
		p, ok := s.paymentRegistry.Get(cur.Provider)
		if !ok {
			return nil, fmt.Errorf("%w: provider %q is not registered", ErrInvalidInput, cur.Provider)
		}
		merged, mergeErr := mergeSecretPlaceholders(p.ConfigSchema().SecretNames(), cur.Config, in.Config)
		if mergeErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, mergeErr)
		}
		cfg, parseErr := p.ParseConfig(merged)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, parseErr)
		}
		if cfg.MerchantRef() != cur.MerchantRef {
			// 商户身份漂移：唯一索引只保护"创建那一刻"，所以这里必须硬拦（N29）。
			// 语义是"你写了错误的配置"→ 400，不是并发冲突（§17-10）。
			return nil, fmt.Errorf("%w: payment account key=%q: merchant_ref is read-only (current=%q new=%q)",
				repository.ErrMerchantRefImmutable, key, cur.MerchantRef, cfg.MerchantRef())
		}
		upd.Config = merged
		upd.MerchantRef = cfg.MerchantRef()
	}
	acct, err := s.store.UpdatePaymentAccount(ctx, key, upd)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrMerchantRefImmutable):
			return nil, err // handler 映射 400
		case errors.Is(err, repository.ErrNotFound):
			return nil, mapRepoErr(err)
		case errors.Is(err, repository.ErrInvalidInput):
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		return nil, fmt.Errorf("%w: update payment account: %w", domain.ErrInternal, err)
	}
	s.invalidatePaymentConfig(key)
	return s.describePaymentAccount(ctx, acct)
}

// SoftDeletePaymentAccount **软停**账户（§10-3 / N30）：置 enabled=false，行永不
// 删除，幂等 200。重复 DELETE 也返回账户体（运维看到的是"现在确实停着"）。
func (s *Service) SoftDeletePaymentAccount(ctx context.Context, key string) (*PaymentAccountView, error) {
	acct, err := s.GetPaymentAccount(ctx, key)
	if err != nil {
		return nil, err
	}
	if err := s.store.SoftDeletePaymentAccount(ctx, key); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, mapRepoErr(err)
		}
		return nil, fmt.Errorf("%w: soft delete payment account: %w", domain.ErrInternal, err)
	}
	acct.Enabled = false
	return s.describePaymentAccount(ctx, acct)
}

// UpsertPaymentChannel 写入/覆盖单渠道（§3-2）：`pay_type ∈ capabilities(provider)`
// （B54/P39）、币种白名单、汇率 > 0、`1 ≤ min ≤ max` —— 任一不满足 400。
func (s *Service) UpsertPaymentChannel(ctx context.Context, accountKey string, in PaymentChannelInput) (*PaymentAccountView, error) {
	acct, err := s.GetPaymentAccount(ctx, accountKey)
	if err != nil {
		return nil, err
	}
	p, ok := s.paymentRegistry.Get(acct.Provider)
	if !ok {
		return nil, fmt.Errorf("%w: provider %q is not registered", ErrInvalidInput, acct.Provider)
	}
	if !capabilityContains(p.Capabilities(), in.PayType) {
		return nil, fmt.Errorf("%w: pay_type %q is not supported by provider %q", ErrInvalidInput,
			in.PayType, acct.Provider)
	}
	if !domain.PaymentCurrencySupported(in.Currency) {
		return nil, fmt.Errorf("%w: currency %q is not supported", ErrInvalidInput, in.Currency)
	}
	if in.RateMicro <= 0 {
		return nil, fmt.Errorf("%w: rate_micro must be positive", ErrInvalidInput)
	}
	if in.MinUSD < 1 || in.MaxUSD < in.MinUSD {
		return nil, fmt.Errorf("%w: require 1 <= min_usd <= max_usd", ErrInvalidInput)
	}
	ch := &domain.PaymentChannel{
		AccountID: acct.ID,
		PayType:   in.PayType,
		Enabled:   in.Enabled,
		Currency:  in.Currency,
		RateMicro: in.RateMicro,
		MinUSD:    in.MinUSD,
		MaxUSD:    in.MaxUSD,
	}
	if _, err := s.store.UpsertPaymentChannel(ctx, acct.ID, ch); err != nil {
		if errors.Is(err, repository.ErrInvalidInput) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		return nil, fmt.Errorf("%w: upsert payment channel: %w", domain.ErrInternal, err)
	}
	return s.describePaymentAccount(ctx, acct)
}

// DeletePaymentChannel 删除单渠道（幂等；账户不存在 → 404）。
func (s *Service) DeletePaymentChannel(ctx context.Context, accountKey, payType string) error {
	acct, err := s.GetPaymentAccount(ctx, accountKey)
	if err != nil {
		return err
	}
	if payType == "" {
		return fmt.Errorf("%w: empty pay_type", ErrInvalidInput)
	}
	if err := s.store.DeletePaymentChannel(ctx, acct.ID, payType); err != nil {
		return fmt.Errorf("%w: delete payment channel: %w", domain.ErrInternal, err)
	}
	return nil
}

// PaymentAccountTest 账户自检结果（`POST /payment/accounts/{key}/test`）：
// ok=false 时 Error 带脱敏后的失败原因（不回显密钥）。
type PaymentAccountTest struct {
	OK    bool
	Error *string
}

// ProbePaymentAccount 签名自检（§5-4：上线前对备用账户跑一次，是唯一探活手段）。
//
// 只验**配置与签名**：用账户 cfg 构造一个形状合法的签名 URL，不发网络请求、
// 不落库、不改状态。查单探活需要真实订单号，属于 `/orders/{id}/sync` 的职责。
func (s *Service) ProbePaymentAccount(ctx context.Context, key string) (PaymentAccountTest, error) {
	acct, err := s.GetPaymentAccount(ctx, key)
	if err != nil {
		return PaymentAccountTest{}, err
	}
	fail := func(err error) (PaymentAccountTest, error) {
		msg := err.Error()
		return PaymentAccountTest{OK: false, Error: &msg}, nil
	}
	p, ok := s.paymentRegistry.Get(acct.Provider)
	if !ok {
		return fail(fmt.Errorf("%w: %q", ErrPaymentProviderMissing, acct.Provider))
	}
	cfg, err := s.paymentAccountConfig(ctx, acct, p)
	if err != nil {
		return fail(err)
	}
	capabilities := p.Capabilities()
	payType := ""
	if len(capabilities) > 0 {
		payType = capabilities[0]
	}
	const probeOrderNo = "P19700101000000AAAAAAAAAA" // 形状合法即可（不落库）
	probeURL := "https://example.invalid/api/payment/" + acct.Key
	if _, err := p.BuildPayURL(cfg, domain.PayRequest{
		PayType:    payType,
		Currency:   "CNY",
		OutTradeNo: probeOrderNo,
		Subject:    "Balance_topup_" + probeOrderNo,
		Money:      "1.00",
		NotifyURL:  probeURL + "/notify",
		ReturnURL:  probeURL + "/return",
	}); err != nil {
		return fail(err)
	}
	return PaymentAccountTest{OK: true}, nil
}

// describePaymentAccount 组装管理面视图：渠道 + **掩码后**的 config + config_error。
//
// 协议未注册时不回显任何 config 内容（不知道哪些字段是 secret 就不知道该掩谁——
// 宁可空表，也不赌"这个协议没有秘密字段"）。
func (s *Service) describePaymentAccount(ctx context.Context, acct *domain.PaymentAccount) (*PaymentAccountView, error) {
	channels, err := s.store.ListPaymentChannels(ctx, acct.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: list payment channels (account %q): %w",
			domain.ErrInternal, acct.Key, err)
	}
	view := &PaymentAccountView{Account: acct, Channels: channels, Config: map[string]any{}}
	p, ok := s.paymentRegistry.Get(acct.Provider)
	if !ok {
		view.ConfigError = fmt.Sprintf("%v: %q", ErrPaymentProviderMissing, acct.Provider)
		s.warnPaymentAccount(acct, ErrPaymentProviderMissing)
		return view, nil
	}
	view.Config = maskSecretFields(p.ConfigSchema().SecretNames(), acct.Config)
	if _, cfgErr := s.paymentAccountConfig(ctx, acct, p); cfgErr != nil {
		view.ConfigError = cfgErr.Error() // 只含字段名/格式原因，不含密钥内容
	}
	return view, nil
}

// capabilityContains pay_type 是否在协议自述的能力里（N23）。
func capabilityContains(capabilities []string, payType string) bool {
	for _, c := range capabilities {
		if c == payType {
			return true
		}
	}
	return false
}

// maskSecretFields 把 config 里 `secretFields` 声明的字段替换成 `"***"`（读面
// 永不回显密钥）；解析不出对象 → 空表（不把原文漏给浏览器）。
func maskSecretFields(secretFields []string, raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	fields := map[string]any{}
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return map[string]any{}
	}
	for _, name := range secretFields {
		if _, ok := fields[name]; ok {
			fields[name] = secretPlaceholder
		}
	}
	return fields
}

// mergeSecretPlaceholders 把新 config 里值为 `"***"` 的秘密字段替换成旧值（B47/P34）。
//
// **通用实现**：只认 `secretFields` 列表，不认任何协议字段名——"新增上游零代码"
// 在掩码路径上也成立（N24）。
func mergeSecretPlaceholders(secretFields []string, oldRaw, newRaw json.RawMessage) (json.RawMessage, error) {
	var next map[string]any
	if err := json.Unmarshal(newRaw, &next); err != nil || next == nil {
		return nil, fmt.Errorf("config must be a JSON object: %w", err)
	}
	needsOld := false
	for _, name := range secretFields {
		if v, ok := next[name]; ok && v == secretPlaceholder {
			needsOld = true
			break
		}
	}
	if !needsOld {
		return newRaw, nil
	}
	var old map[string]any
	if len(oldRaw) > 0 {
		_ = json.Unmarshal(oldRaw, &old) // 旧值解析失败 → 没有可保留的原值
	}
	for _, name := range secretFields {
		if v, ok := next[name]; ok && v == secretPlaceholder {
			if old == nil {
				return nil, fmt.Errorf("config field %q cannot be preserved: current config is unreadable", name)
			}
			prev, ok := old[name]
			if !ok {
				return nil, fmt.Errorf("config field %q cannot be preserved: no previous value", name)
			}
			next[name] = prev
		}
	}
	merged, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	return merged, nil
}

// ---------- 金额与纯函数（§7） ----------

// effectiveMaxUSD 生效上限（纯函数，便于单测）：min(配置上限,
// (MaxInt64/2)/rate_micro/100000)。除法形式，绝不做乘法比较。
func effectiveMaxUSD(maxUSD, rateMicro int64) int64 {
	if rateMicro <= 0 || maxUSD <= 0 {
		return 0
	}
	arithmeticCap := (maxInt64 / 2) / rateMicro / milliPerUSD
	if maxUSD > arithmeticCap {
		return arithmeticCap
	}
	return maxUSD
}

// settingBool 读开关：缺失/非法 → fallback（配置面只接受 true/false）。
func (s *Service) settingBool(key string, fallback bool) bool {
	switch s.settingValue(key) {
	case "true":
		return true
	case "false":
		return false
	}
	return fallback
}

// settingIntOr 读整型设置：缺失/0 → fallback（0 在这些键上不可能是合法值）。
func (s *Service) settingIntOr(key string, fallback int64) int64 {
	if v := s.settingInt(key); v != 0 {
		return v
	}
	return fallback
}

// parseUSDAmount 解析 USD 金额为**分**（整数，禁 float）：
// `^\d{1,7}(\.\d{1,2})?$` 且 > 0。前导零/缺小数位都合法（等值比）。
func parseUSDAmount(amount string) (int64, error) {
	if !usdAmountPattern.MatchString(amount) {
		return 0, fmt.Errorf("%w: amount_usd %q is not a valid USD amount", ErrInvalidInput, amount)
	}
	major, frac, hasDot := strings.Cut(amount, ".")
	dollars, err := strconv.ParseInt(major, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: amount_usd %q", ErrInvalidInput, amount)
	}
	cents := int64(0)
	if hasDot {
		padded := frac + strings.Repeat("0", 2-len(frac))
		cents, err = strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: amount_usd %q", ErrInvalidInput, amount)
		}
	}
	usdCents := dollars*100 + cents
	if usdCents <= 0 {
		return 0, fmt.Errorf("%w: amount_usd must be positive", ErrInvalidInput)
	}
	return usdCents, nil
}

// errAmountOverflow 金额在该渠道汇率下不可表示（→ 400；B19）。
var errAmountOverflow = errors.New("payment: amount overflows channel rate range")

// quoteCreditMilli USD 分 → 入账毫分：credit_milli = usd_cents * 1000
// （1 USD = 100,000 毫分；§4-3）。
func quoteCreditMilli(usdCents int64) int64 { return usdCents * milliPerCent }

// quotePayMinor 实付最小单位（half-up，**全整数**，M8 的判别点）：
//
//	pay_minor = (credit_milli*rate_micro + 500_000_000) / 1_000_000_000
//
// 溢出判据用**除法**（F14：乘法判据自身会溢出）；rate_micro <= 0 → 渠道不可用。
func quotePayMinor(creditMilli, rateMicro int64) (int64, error) {
	if rateMicro <= 0 {
		return 0, errAmountOverflow
	}
	if creditMilli > (maxInt64/2)/rateMicro {
		return 0, errAmountOverflow
	}
	return (creditMilli*rateMicro + feeHalfUnit) / rateMicroPerUnit, nil
}

// formatMinor 最小单位 → 两位小数（`%d.%02d`，不经 float）。
func formatMinor(minor int64) string {
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// randomOrderNo 订单号：P{UTC yyyyMMddHHmmss}{10 位 base32}。字符集与兑换码
// 同源（A-Z 去 I/O + 2-9），满足 domain.ValidOutTradeNo 的形状。
func randomOrderNo(now time.Time) (string, error) {
	b := make([]byte, 10)
	max := big.NewInt(int64(len(codeCharset)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = codeCharset[n.Int64()]
	}
	return "P" + now.UTC().Format("20060102150405") + string(b), nil
}

// SanitizeRawNotify 清洗审计载荷并序列化为 JSON（≤4 KiB，合法 UTF-8）：
// 去掉 NUL 与非法 UTF-8 字节（B35：审计不阻断入账），超长则保留合法 JSON
// 形状（`{"truncated":true,"prefix":"..."}`），rune 回退截断（F9/B18）。
// 空载荷 → nil（不写该列）。
func SanitizeRawNotify(raw map[string]string) *string {
	if len(raw) == 0 {
		return nil
	}
	cleaned := make(map[string]string, len(raw))
	for k, v := range raw {
		cleaned[sanitizeText(k)] = sanitizeText(v)
	}
	body, err := json.Marshal(cleaned)
	if err != nil {
		return nil // map[string]string 的 Marshal 实际不可能失败；防御性返回 nil
	}
	if len(body) <= rawNotifyMaxBytes {
		out := string(body)
		return &out
	}
	// 超长：保留可解析的 JSON 形状，只带截断前缀（长度按 rune 边界回退）。
	for cut := len(body); cut > 0; cut -= 256 {
		wrapped, err := json.Marshal(map[string]any{
			"truncated": true,
			"prefix":    string(truncateUTF8(body, cut)),
		})
		if err != nil {
			continue
		}
		if len(wrapped) <= rawNotifyMaxBytes {
			out := string(wrapped)
			return &out
		}
	}
	return nil
}

// sanitizeText 去掉 NUL 与非 UTF-8 字节（网关可能塞二进制）。
func sanitizeText(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "")
}

// truncateUTF8 截断到 ≤max 字节且不切断多字节字符（逐字节回退到合法前缀）。
func truncateUTF8(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	b = b[:max]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return b
}
