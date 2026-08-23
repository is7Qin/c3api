// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"time"
)

// FunctionPrice 按单元计费功能类价格行（search 起，audio/video 等未来 per-unit
// 端点复用；对齐 image_price 形态）。价格单位（与 pricings 表头同款 1 USD =
// 100,000 毫分 = 10⁻⁵ USD 精度）：PricePerCall = 毫分/单元（litellm
// input_cost_per_query USD/次 ×1e5 四舍五入取整；codex-search 手动行
// 1000 毫分 = $0.01/次）。nil = 无按单元价（litellm 行恒非 nil——无价或
// 非正跳过；manual 行由应用层拒绝全 nil 落行）。Source 复用 PricingSource
// （litellm|manual，与 pricings 同款行级互斥优先级 manual > litellm）。
type FunctionPrice struct {
	ID           int64
	Model        string
	PricePerCall *int64          // 毫分/单元（次/秒等）；nil = 无按单元价
	Provider     *string         // litellm_provider（litellm 行才有；manual 行 nil）
	Raw          json.RawMessage // litellm 原始条目完整镜像（manual 行恒 nil）
	Source       PricingSource
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// HasAnyPrice 行有效性 = 按单元价非 nil（全 nil 行应用层拒绝落行/设价；DB
// 无法约束，应用层判定——对齐 ImagePrice.HasAnyPrice 语义）。
func (p *FunctionPrice) HasAnyPrice() bool {
	return p.PricePerCall != nil
}

// CodexSearchModel codex-search 固定功能标识：function_price 表初始化行
// （bootstrap 幂等种子）与 GetFunctionPrice 默认兜底共用同一常量，防两处
// 魔法字符串漂移。
const CodexSearchModel = "codex-search"

// DefaultCodexSearchPricePerCall codex-search 默认按次价（1000 毫分 = $0.01/次）：
// 初始化种子行写入值（source=manual）与快照兜底常量同值——表删/初始化失败时
// GetFunctionPrice("codex-search") 仍返回该默认行（防御语义，注释见 service）。
const DefaultCodexSearchPricePerCall int64 = 1000
