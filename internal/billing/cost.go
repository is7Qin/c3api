// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package billing 计费核心：service_tier 归一化 + 价格矩阵纯函数计算。
// 纯函数零分配零锁；扣费落库（T3）与请求路径分离。
package billing

import (
	"math"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

// Tier service_tier 归一化档位。与 sub2api 的 fast→priority 归一不同：fast
// 为独立档位（Anthropic Fast Mode 整单倍率，Anthropic 官方语义；用户裁决，
// 见 Phase 5 计划）。
type Tier int

const (
	TierAuto Tier = iota // 默认档（auto/default/scale/空/未知，恒透传）
	TierFast             // Anthropic Fast Mode（fast_multiplier 整单倍率）
	TierPriority
	TierFlex
)

// String 归一化字符串（UsageLog.BillingTier 落库值）。
func (t Tier) String() string {
	switch t {
	case TierFast:
		return "fast"
	case TierPriority:
		return "priority"
	case TierFlex:
		return "flex"
	default:
		return "auto"
	}
}

// NormalizeTier 归一化请求 service_tier：小写去空格；fast/priority/flex 精确
// 匹配；空/auto/default/scale/未知 → TierAuto（默认档）。
func NormalizeTier(raw string) Tier {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "fast":
		return TierFast
	case "priority":
		return TierPriority
	case "flex":
		return TierFlex
	default:
		return TierAuto
	}
}

// TierPolicyMode service_tier 转发策略（settings 键 service_tier_policy_priority
// / service_tier_policy_flex / service_tier_policy_fast；缺失/未知值 →
// passthrough 默认）。auto/空恒透传（proxy 侧短路，不查策略）。
type TierPolicyMode string

const (
	TierPolicyPassthrough TierPolicyMode = "passthrough" // 原样转发（默认）
	TierPolicyStrip       TierPolicyMode = "strip"       // 转发体删除 service_tier 字段
	TierPolicyReject      TierPolicyMode = "reject"      // 直接 400 拒绝（不转发）
)

// 溢出预算（评审 I-2）：合法域单段 t×p ≤ 1e13（毫分×1e6 原始单位），四分量
// 合计 ≤ 4e13；fast 万分数 ≤ 1e5（实测 ×6.0 = 60000，留余量）→ 4e13×1e5
// = 4e18 < MaxInt64。fast 万分数上界由钳制强制（A-P2-4 双保险：本函数 fast
// 分支 + pricing fetch assign 同钳 > 1e5 → 1e5——快照可经任何途径进入超界
// 值含手动 DB 操作，钳后本预算先决条件恢复成立）。价格列已由写路径校验 ≥ 0
// （service 校验/fetcher validCost）；负数 token 钳 0。除 1e6 在求和后一次
// 完成（毫分/1M tokens → 毫分）。
const (
	milliPerMillion = 1_000_000 // 毫分/1M tokens → 毫分的除数
)

// segBudget 单乘积上界（评审 I-1 溢出钳制）：每分量至多 2 个乘法（阈值内/
// 超额），四分量共 ≤ 8 个；各乘积钳制后总和 ≤ 8×segBudget ≤ MaxInt64 -
// milliPerMillion/2，末次 (raw + 5e5) 四舍五入不回绕。
const segBudget = (math.MaxInt64 - milliPerMillion/2) / 8

// clampToken 恶意防护：恶意/异常上游可报超大 token 数（如 9e15），t×p 会
// 回绕成负 cost（T3 扣费变反向入账）。乘法前把 token 钳到 segBudget/p，乘积
// 恒 ≤ segBudget；合法输入（t ≤ 1e6、p ≤ 1e7 → 乘积 ≤ 1e13）远低于上界，
// 正常路径仅一次除法一次比较，零分配。负数 token 已在 Cost 入口钳 0。
func clampToken(t, p int64) int64 {
	if p > 0 {
		if lim := segBudget / p; t > lim {
			return lim
		}
	}
	return t
}

const imageTotalCap = math.MaxInt64 / 100_000

// CostFromResolved pure arithmetic on resolved prices (new unified path).
func CostFromResolved(rp domain.ResolvedPrices, pt, ct, cr, cc int64) int64 {
	clamp := func(t int64) int64 {
		if t < 0 {
			return 0
		}
		return t
	}
	pt, ct, cr, cc = clamp(pt), clamp(ct), clamp(cr), clamp(cc)
	orInt := func(v *int64) int64 {
		if v == nil {
			return 0
		}
		return *v
	}
	raw := clampToken(pt, orInt(rp.InputPerM))*orInt(rp.InputPerM) +
		clampToken(ct, orInt(rp.OutputPerM))*orInt(rp.OutputPerM) +
		clampToken(cr, orInt(rp.CacheReadPerM))*orInt(rp.CacheReadPerM) +
		clampToken(cc, orInt(rp.CacheWritePerM))*orInt(rp.CacheWritePerM)
	return (raw + milliPerMillion/2) / milliPerMillion
}

// ImageCostFromResolved image branch via unified ResolvedPrices.
func ImageCostFromResolved(rp domain.ResolvedPrices, inTok, outTok, count int64) int64 {
	clamp := func(t int64) int64 {
		if t < 0 {
			return 0
		}
		return t
	}
	inTok, outTok, count = clamp(inTok), clamp(outTok), clamp(count)
	var inP, outP, perP int64
	if rp.ImgInTokPerM != nil {
		inP = *rp.ImgInTokPerM
	}
	if rp.ImgOutTokPerM != nil {
		outP = *rp.ImgOutTokPerM
	}
	if rp.PricePerImage != nil {
		perP = *rp.PricePerImage
	}
	raw := clampToken(inTok, inP)*inP + clampToken(outTok, outP)*outP
	tokenCost := (raw + milliPerMillion/2) / milliPerMillion
	var perImage int64
	if perP > 0 && count > 0 {
		if lim := imageTotalCap / perP; count > lim {
			count = lim
		}
		perImage = count * perP
	}
	total := tokenCost + perImage
	if total > imageTotalCap {
		total = imageTotalCap
	}
	return total
}

// CallCostFromResolved per-call branch.
func CallCostFromResolved(rp domain.ResolvedPrices, count int64) int64 {
	if count < 0 {
		count = 0
	}
	if rp.PricePerCall == nil {
		return 0
	}
	if p := *rp.PricePerCall; p > 0 {
		if count > imageTotalCap/p {
			count = imageTotalCap / p
		}
		return count * p
	}
	return 0
}
