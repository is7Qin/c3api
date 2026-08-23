// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"
	"time"

	"github.com/tidwall/sjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
)

// PriceLookup 价格快照读取（service.Service 实现，零 DB 快照读）。任何错误
// = 该模型无可用价格（计费方拒绝计费而非按 0 计价）。GetImagePrice 为图片
// 生成价快照（Task A 数据面；resp/resp-ws 检测旁路按 model 查价用）。
type PriceLookup interface {
	GetPrice(model string) (*domain.Pricing, error)
	GetImagePrice(model string) (*domain.ImagePrice, error)
}

// PriceResolver 统一价格解析（新入口）。
type PriceResolver interface {
	ResolvePrices(model string, promptTokens int64, tier string, at time.Time) (domain.ResolvedPrices, bool)
}

// ImagePriceLookup 图片价格快照读取（service.Service 实现，零 DB 快照读）。
// Task B images 端点预检用（P1-1 预检按格式切换：images 格式查 GetImagePrice、
// 跳过 chat 价预检 GetPrice）。任何错误 = 该模型无图片价格（402 拒绝计费而
// 非按 0 计价——空行语义 = 端点定生死）。
type ImagePriceLookup interface {
	GetImagePrice(model string) (*domain.ImagePrice, error)
}

// FunctionPriceLookup 按单元计费功能类价格快照读取（service.Service 实现，
// 零 DB 快照读）：search 等 per-unit 端点计费价。语义（价格表三件套裁决）：
// 查无 + model == codex-search → 返回默认价行（$0.01/次常量兜底，防御语义）；
// 查无其他 → 错误（计费方拒绝计费而非按 0 计价）。
type FunctionPriceLookup interface {
	GetFunctionPrice(model string) (*domain.FunctionPrice, error)
}

// BillingHooks 计费钩子（proxy.New 参数；nil = 计费全关：不查价、不记
// BillingTier、不处理 service_tier 转发策略、不做余额预检）。
// T3 填满（中间态清理终点）：Balances/Flusher 为真实类型直接接线，无 nil
// 容忍分支——装配方（main）保证 bill 非 nil 时六字段齐备（计费开关 =
// config.Billing.Enabled，与 hooks 装配同一判定）。
type BillingHooks struct {
	Prices   PriceLookup
	Resolver PriceResolver
	Balances *billing.Balances // 余额只读快照（预检 + 扣费后定向刷新）
	Flusher  *billing.Flusher  // 批量扣费落库（billed 路由终点）
	// ImagePrices 图片价格快照（Task B：images 端点预检专用；nil = 未装配
	// ——images 端点缺价预检跳过，等价计费全关）。
	ImagePrices ImagePriceLookup
	// FunctionPrices 按单元价快照（价格表三件套：search 等 per-unit 端点计费
	// 用；nil = 未装配——按单元计费端点缺价预检跳过，等价计费全关）。
	FunctionPrices FunctionPriceLookup
	// TierPolicy 读取 service_tier 转发策略（nil = 恒透传）：priority/flex/fast
	// 分别按 settings service_tier_policy_priority / service_tier_policy_flex /
	// service_tier_policy_fast 快照取值（装配方注入闭包，零 DB）。
	TierPolicy func(tier billing.Tier) billing.TierPolicyMode
}

var (
	// errNoPrice 402：模型缺价（计费启用后未设价/未同步；空价格表 = 全模型 402）。
	errNoPrice = &formatError{status: http.StatusPaymentRequired, msg: "no price configured for this model"}
	// errInsufficientBalance 402：余额预检拒绝（快照缺失或 <0；免费放行路径
	// T3.5 价格倍率扩展）。
	errInsufficientBalance = &formatError{status: http.StatusPaymentRequired, msg: "insufficient balance"}
	// errServiceTierRejected 400：service_tier 策略 reject（不转发，记 ErrBilling）。
	errServiceTierRejected = &formatError{status: http.StatusBadRequest, msg: "service_tier rejected by gateway policy"}
)

// stripServiceTier 删除请求体 service_tier 字段（strip 策略）：sjson 字节级
// 删除（与 WS 面 relayWS 首帧预处理同库同风格，非 map 往返——精度/键序/转义
// 保真同 setModel）。字段缺失时 sjson 删除不存在键 = 无操作返回原字节（与
// map 版 delete 缺失键语义一致）。顶层形状守卫同 setModel（sjson DeleteBytes
// 对标量/数组根无操作返回原字节，map 版报错——调用前 extractTier 已保证
// 对象体，仅防御性一致）。
func stripServiceTier(body []byte) ([]byte, error) {
	if !isJSONObjectRoot(body) {
		return nil, errBodyNotObject
	}
	return sjson.DeleteBytes(body, "service_tier")
}

// applyMultiplier 价格倍率应用（T3.5 用户拍板：整单 round-half-up）：
// (cost×m + 5000)/10000。m==10000（×1 未设置/组默认）恒等短路——默认路径
// 逐指令等价零 round 偏差（函数可内联）。m==0 → cost 0（免费，不扣费）。
// 溢出安全：cost 毫分正常 ≤1e7 量级（恶意 token 经 cost.go 溢出钳制后仍
// ≤ MaxInt64/1e6）× 倍率上限 1e5 → 远小于 MaxInt64。
func applyMultiplier(cost int64, m int) int64 {
	if m == 10000 {
		return cost
	}
	return (cost*int64(m) + 5000) / 10000
}
