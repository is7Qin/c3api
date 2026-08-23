// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"time"
)

// PricingSource 价格行来源：litellm = 自动拉取；manual = 管理端手动设置。
// 行级互斥优先级 manual > litellm：拉取 upsert（DO UPDATE 带
// WHERE source != 'manual'）永不覆盖手动价；删除手动行后下轮拉取补回。
type PricingSource string

const (
	PricingSourceLitellm PricingSource = "litellm"
	PricingSourceManual  PricingSource = "manual"
)

func (s PricingSource) Valid() bool {
	switch s {
	case PricingSourceLitellm, PricingSourceManual:
		return true
	}
	return false
}

// Pricing 模型价格行（Phase 5 计费价格来源；一行 = 最终生效价，计费读快照
// 不关心来源）。价格单位：毫分 / 1M tokens（1 USD = 100,000 毫分 = 10⁻⁵ USD
// 精度；litellm per-token USD 换算 ×1e11 四舍五入取整）。
// 矩阵价（Phase 5，挡位归属定稿）：Priority*/Flex* 为 **OpenAI 专属**单价替换档
// （gpt-5 系列 priority 价、gpt-5.6-sol flex 价；缺失 = 无该档价，计费回退
// 基础价）；FastMultiplier 为 **Anthropic 专属**（claude 系列 Fast Mode 整单
// 倍率，万分数 20000 = ×2.0，fast 挡计费 = 基础价 × 此倍率，源自
// provider_specific_entry.fast）；Above* 为上下文超阈值分段价（AboveThreshold
// = 阈值 tokens，litellm _above_{N}k 动态提取；Above* 基础组 / AbovePriority* /
// AboveFlex* 对齐官方表 tier 变体，如 gpt-5.6-sol 的 _above_272k_tokens_flex、
// azure 的 _above_272k_tokens_priority）；基础 4 价与 above 三组 12 价通用。
// 全部矩阵价缺失（nil）不参与行有效性判定，计费回退基础价/不涨价。
type Pricing struct {
	ID                                        int64
	Model                                     string
	PromptPricePerMillion                     int64
	CompletionPricePerMillion                 int64
	MaxInputTokens                            *int64 // litellm 自带上下文窗口；nil = 未知
	MaxOutputTokens                           *int64
	CacheReadPricePerMillion                  *int64 // 缓存读取价（litellm cache_read_input_token_cost）；nil = 无缓存价
	CacheCreationPricePerMillion              *int64 // 缓存写入价（litellm cache_creation_input_token_cost）；nil = 无缓存价
	PriorityPromptPricePerMillion             *int64 // service_tier=priority 单价替换档（OpenAI 专属：gpt-5 系列 priority 价）
	PriorityCompletionPricePerMillion         *int64
	PriorityCacheReadPricePerMillion          *int64
	PriorityCacheCreationPricePerMillion      *int64
	FlexPromptPricePerMillion                 *int64 // service_tier=flex 单价替换档（OpenAI 专属：gpt-5.6-sol flex 价）
	FlexCompletionPricePerMillion             *int64
	FlexCacheReadPricePerMillion              *int64
	FlexCacheCreationPricePerMillion          *int64
	AboveThreshold                            *int64 // 分段阈值（tokens）；nil = 无分段
	AbovePromptPricePerMillion                *int64 // 超阈值分段价（基础组）
	AboveCompletionPricePerMillion            *int64
	AboveCacheReadPricePerMillion             *int64
	AboveCacheCreationPricePerMillion         *int64
	AbovePriorityPromptPricePerMillion        *int64 // 超阈值分段价（priority 组，azure 形态；OpenAI 专属）
	AbovePriorityCompletionPricePerMillion    *int64
	AbovePriorityCacheReadPricePerMillion     *int64
	AbovePriorityCacheCreationPricePerMillion *int64
	AboveFlexPromptPricePerMillion            *int64 // 超阈值分段价（flex 组，gpt-5.6-sol 形态；OpenAI 专属）
	AboveFlexCompletionPricePerMillion        *int64
	AboveFlexCacheReadPricePerMillion         *int64
	AboveFlexCacheCreationPricePerMillion     *int64
	FastMultiplier                            *int64          // Anthropic 专属：claude 系列 Fast Mode 整单倍率（万分数，20000 = ×2.0；fast 挡计费 = 基础价 × 此倍率）
	Provider                                  *string         // litellm_provider（litellm 行才有；manual 行 nil）
	Mode                                      *string         // litellm mode（chat/completion/embedding 等）
	SupportsPromptCaching                     *bool           // litellm supports_prompt_caching
	Raw                                       json.RawMessage // litellm 原始条目完整镜像（含未映射字段）；nil = 无（manual 行）
	Source                                    PricingSource
	CreatedAt                                 time.Time
	UpdatedAt                                 time.Time
}
