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

// PriceResolver 统一价格解析（零 DB 快照读，首中即停变体解析）。
type PriceResolver interface {
	ResolvePrices(model string, promptTokens int64, tier string, at time.Time) (domain.ResolvedPrices, bool)
}

// BillingHooks 计费钩子（proxy.New 参数；nil = 计费全关：不查价、不记
// BillingTier、不处理 service_tier 转发策略、不做余额预检）。
type BillingHooks struct {
	Resolver PriceResolver
	Balances *billing.Balances // 余额只读快照（预检 + 扣费后定向刷新）
	Flusher  *billing.Flusher  // 批量扣费落库（billed 路由终点）
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
