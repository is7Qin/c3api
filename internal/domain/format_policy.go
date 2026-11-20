// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"slices"

	"github.com/is7qin/c3api/internal/credential"
)

// allowedFormats 是格式白名单的唯一真值：凭据类型 → 允许的请求格式（规范序：
// chat / responses / responses-ws / images / search / anthropic）。
//
// 一张表，两种视图——不是两份实现：
//   - 管理面（service 的 validateFormatsForType）问「这个类型允许哪些格式」，对
//     全量集合做逐成员判定；
//   - 运行时格式门（repository / scheduler / Template.FormatSupports）问「这个
//     格式是不是 BPS 允许的那两个」，是 BPS 行的子集视图。
//
// 判据是**是否共享同一个数据源**：共享本表 ⇒ 视图；谁把 BPS 行换回格式字面量，
// 谁就把它变回重复实现（TestBPSFormatGateSites 抓调用点，TestFormatPolicyMatrix
// 抓每一格）。
var allowedFormats = map[credential.Type][]RequestFormat{
	credential.TypeAPIKey: {
		FormatOpenAIChat,
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
		FormatAnthropic,
	},
	credential.TypeCustom: {
		FormatOpenAIChat,
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
		FormatAnthropic,
	},
	credential.TypeResponsesSpecial: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
	},
	credential.TypeCodexOAuth: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
	},
	credential.TypeCodexPAT: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
	},
	credential.TypeBPS: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
	},
}

// FormatsAllowedFor 返回该类型允许的格式（规范序）与「类型在表内」。
//
// 返回**副本**：调用方（管理面校验）改不动权威表。ok=false 表示类型不在表内
// （非法/未知/表外历史值），此时返回 nil。调用方必须显式处理 !ok——丢掉它把 nil
// 交给循环就是「零次迭代 = 放行任意格式」。
//
// 调用点在管理面（service 的模板校验：站点 1 逐模板、站点 2 单次），不在请求/
// 路由编译热路径。返回副本是为了让调用方改不动权威表；**不要**为了省这一次复制
// 把它引入 buildRoutes 的三层循环——那里的判定必须走 FormatAllowedFor /
// BPSFormatsOnly（直接读表行，零分配）。
func FormatsAllowedFor(t credential.Type) ([]RequestFormat, bool) {
	allowed, ok := allowedFormats[t]
	if !ok {
		return nil, false
	}
	return slices.Clone(allowed), true
}

// FormatAllowedFor reports whether t may serve format f. Unknown types are
// always false.
//
// 本函数与 BPSFormatsOnly 在 Template.FormatSupports 里被 buildRoutes 逐账号 ×
// 模型 × 格式调用，故**直接读表行、不做任何复制**（也不许内部转调
// FormatsAllowedFor——那会把每次判定变成一次切片分配）。
func FormatAllowedFor(t credential.Type, f RequestFormat) bool {
	return slices.Contains(allowedFormats[t], f)
}

// BPSFormatsOnly reports the format gate for the BPS provider: BPS serves only
// the two standard Responses formats. The implementation is a subset test
// against the BPS row of allowedFormats — 不许出现格式字面量，否则它就是这张表
// 的第 N 份副本。
func BPSFormatsOnly(formats []RequestFormat) bool {
	allowed := allowedFormats[credential.TypeBPS] // 直接读表行：不复制（热路径）
	for _, f := range formats {
		if !slices.Contains(allowed, f) {
			return false
		}
	}
	return true
}

// RoutableFormats 返回 buildRoutes 建路由索引用的 5 个格式（不含 search），每次
// 返回新切片。search 不进路由索引：codex search 走 scheduler.SelectOpaque 复用
// responses 桶（见 internal/scheduler/search_opaque_topology_test.go）。
func RoutableFormats() []RequestFormat {
	return []RequestFormat{
		FormatOpenAIChat,
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatAnthropic,
	}
}
