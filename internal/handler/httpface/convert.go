// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpface

import "math"

// 管理面/用户面共享的 API 边界单位换算（spec 2026-10-13 §3.6/I5）：原为管理面包
// （internal/handler/convert.go）私有，用户面模型目录（handler/user）需同口径——
// 下沉到本包唯一一份，管理面包私有实现改为委托本包（签名不变）。
//
// 依赖仍仅 domain + service/errors 叶子集（math 为标准库），依赖图无环。

// MillisToUSD 毫分 → USD float64（1 USD = 100,000 毫分；API 边界展示换算，
// 内部存储恒毫分）。
func MillisToUSD(millis int64) float64 { return float64(millis) / 1e5 }

// MillisToUSDPtr *int64（毫分）→ *float64（USD）；nil 透传（价格矩阵可选列）。
func MillisToUSDPtr(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := MillisToUSD(*v)
	return &f
}

// USDToMillis USD float64 → 毫分（math.Round 消除浮点取整误差；API 输入边界
// 换算，内部存储恒毫分）。
func USDToMillis(usd float64) int64 { return int64(math.Round(usd * 1e5)) }

// MultBPToNormal 万分数 *int → 正常值 *float64（API 展示换算：15000 → 1.5）；
// nil 透传。
func MultBPToNormal(v *int) *float64 {
	if v == nil {
		return nil
	}
	f := MultBPToNormalValue(*v)
	return &f
}

// MultBPToNormalValue 万分数 int → 正常值 float64（API 展示换算：15000 → 1.5）；
// 值语义，供调用方无需指针往返即可换算（MultBPToNormal 的指针版包装）。
func MultBPToNormalValue(v int) float64 { return float64(v) / 10000.0 }

// NormalToMultBP 正常值 → 万分数 int（API 输入换算：1.5 → 15000；math.Round
// 消除浮点取整误差）。
func NormalToMultBP(v float64) int { return int(math.Round(v * 10000)) }
