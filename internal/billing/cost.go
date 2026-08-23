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

// Cost 计费纯函数：按 tier 选价 → per 分量独立分段（above）→ fast 整单倍率 →
// 求和毫分（1 USD = 100,000 毫分；零换算零取整误差，与 balance 直接相减）。
//
// 选价：priority → priority 列 ?? 基础列；flex → flex 列 ?? 基础列；fast/auto
// → 基础列（不碰 tier 列——无 tier 价不涨价，回退基础）。分量价缺失（如无缓存
// 价）→ 该分量 0（不发明价格）。
//
// 分段：pt/ct/cr/cc 各自独立拆两段，tokens > AboveThreshold 才拆段；above 组
// 按请求 tier 选（priority → above_priority ?? above；flex → above_flex ??
// above；fast/auto → above），该分量 above 缺失 → 不拆段（全按组内价）。
// aboveHit = 任一分量实际拆段（t > 阈值且该分量 above 价存在）。
//
// fast：tier == TierFast 且 FastMultiplier 有效（> 0）→ 整单 ×（万分数，
// 四舍五入）——total = (total×m+5000)/10000；m 先钳 ≤ 1e5（A-P2-4，见下方
// 分支注释与溢出预算注释）。
func Cost(p *domain.Pricing, tier Tier, pt, ct, cr, cc int64) (int64, bool) {
	clamp := func(t int64) int64 {
		if t < 0 {
			return 0
		}
		return t
	}
	pt, ct, cr, cc = clamp(pt), clamp(ct), clamp(cr), clamp(cc)

	orPtr := func(a, b *int64) *int64 {
		if a != nil {
			return a
		}
		return b
	}
	tierOr := func(tierV *int64, base int64) int64 {
		if tierV != nil {
			return *tierV
		}
		return base
	}
	// 单价档：priority/flex 列 ?? 基础列（基础 prompt/completion 恒存在，
	// 缓存价可缺失）；fast/auto 保持基础列。
	prompt, completion := p.PromptPricePerMillion, p.CompletionPricePerMillion
	cacheRead, cacheCreation := p.CacheReadPricePerMillion, p.CacheCreationPricePerMillion
	switch tier {
	case TierPriority:
		prompt = tierOr(p.PriorityPromptPricePerMillion, prompt)
		completion = tierOr(p.PriorityCompletionPricePerMillion, completion)
		cacheRead = orPtr(p.PriorityCacheReadPricePerMillion, cacheRead)
		cacheCreation = orPtr(p.PriorityCacheCreationPricePerMillion, cacheCreation)
	case TierFlex:
		prompt = tierOr(p.FlexPromptPricePerMillion, prompt)
		completion = tierOr(p.FlexCompletionPricePerMillion, completion)
		cacheRead = orPtr(p.FlexCacheReadPricePerMillion, cacheRead)
		cacheCreation = orPtr(p.FlexCacheCreationPricePerMillion, cacheCreation)
	}
	// above 组按 tier 选（组内缺失分量 → 基础 above 组回退；再缺失 → 不拆段）。
	var above [4]*int64
	if p.AboveThreshold != nil {
		ap, ac, ar, aw := p.AbovePromptPricePerMillion, p.AboveCompletionPricePerMillion,
			p.AboveCacheReadPricePerMillion, p.AboveCacheCreationPricePerMillion
		switch tier {
		case TierPriority:
			ap = orPtr(p.AbovePriorityPromptPricePerMillion, ap)
			ac = orPtr(p.AbovePriorityCompletionPricePerMillion, ac)
			ar = orPtr(p.AbovePriorityCacheReadPricePerMillion, ar)
			aw = orPtr(p.AbovePriorityCacheCreationPricePerMillion, aw)
		case TierFlex:
			ap = orPtr(p.AboveFlexPromptPricePerMillion, ap)
			ac = orPtr(p.AboveFlexCompletionPricePerMillion, ac)
			ar = orPtr(p.AboveFlexCacheReadPricePerMillion, ar)
			aw = orPtr(p.AboveFlexCacheCreationPricePerMillion, aw)
		}
		above = [4]*int64{ap, ac, ar, aw}
	}
	orInt := func(v *int64) int64 {
		if v == nil {
			return 0
		}
		return *v
	}
	units := [4]int64{prompt, completion, orInt(cacheRead), orInt(cacheCreation)}
	toks := [4]int64{pt, ct, cr, cc}
	thr := p.AboveThreshold

	raw := int64(0)
	aboveHit := false
	for i := 0; i < 4; i++ {
		t, base, ov := toks[i], units[i], above[i]
		if thr != nil && ov != nil && t > *thr {
			raw += clampToken(*thr, base)*base + clampToken(t-*thr, *ov)**ov // 钳制后单乘积 ≤ segBudget
			aboveHit = true
		} else {
			raw += clampToken(t, base) * base
		}
	}
	cost := (raw + milliPerMillion/2) / milliPerMillion // 四舍五入 → 毫分
	if tier == TierFast && p.FastMultiplier != nil && *p.FastMultiplier > 0 {
		// A-P2-4 fast 倍率钳制（必要层）：快照可经任何途径进入超界值（含手动
		// DB 操作；fetch assign 双保险防的是脏数据再入快照，本层兜住快照已脏
		// 的情形）。钳制而非拒绝（拒绝致该模型整条无价全 402）；钳后 cost ≤
		// 9.22e13 毫分 ≪ MaxInt64，溢出预算注释先决条件恢复成立；纯算术零分配。
		m := *p.FastMultiplier
		if m > 100000 {
			m = 100000
		}
		cost = (cost*m + 5000) / 10000
	}
	return cost, aboveHit
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
	p := *rp.PricePerCall
	if count > imageTotalCap/p {
		count = imageTotalCap / p
	}
	return count * p
}

// ImageCost legacy shim (old ImagePrice path).
func ImageCost(p *domain.ImagePrice, inTok, outTok, count int64) int64 {
	if p == nil {
		return 0
	}
	rp := domain.ResolvedPrices{ImgInTokPerM: p.InputImageTokenPricePerMillion, ImgOutTokPerM: p.OutputImageTokenPricePerMillion, PricePerImage: p.OutputCostPerImageMilli}
	return ImageCostFromResolved(rp, inTok, outTok, count)
}
