// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

// 自定义上游路径的键集合与默认值（spec §4.3，键名必须与 service 层校验一致）。
// 默认值即去 SDK 前各格式硬编码的 path 参数，未配置时逐字节不变。
const (
	pathKeyChat        = "openai-chat"
	pathKeyResponses   = "openai-responses"
	pathKeyResponsesWS = "openai-responses-ws"
	pathKeyAnthropic   = "anthropic"
	pathKeySearch      = "openai-search"
	pathKeyImagesGen   = "openai-images.generations"
	pathKeyImagesEdits = "openai-images.edits"

	defaultPathChat        = "chat/completions"
	defaultPathResponses   = "responses"
	defaultPathResponsesWS = "v1/responses"
	defaultPathAnthropic   = "v1/messages"
	defaultPathSearch      = "alpha/search"
	defaultPathImagesGen   = "images/generations"
	defaultPathImagesEdits = "images/edits"
)

// pathOf 取该键的生效路径：模板配置优先，未配置回落默认常量。
// 热路径约束：一次 map 读 + 常量回落，不查 DB、不加锁、不复制 map；
// 配置 map 在快照内不可变，此处只读。
func pathOf(sel *scheduler.Selection, key, def string) string {
	// 仅 custom 模板可覆盖路径：类型被改回 api_key 等时，即使快照里残留配置也
	// 必须回落默认，避免请求静默打到旧的自定义端点。
	if sel != nil && sel.CredentialType == credential.TypeCustom && sel.UpstreamPaths != nil {
		if v := sel.UpstreamPaths[key]; v != "" {
			return v
		}
	}
	return def
}

// anthropicStreamingRequiredText anthropic-sdk-go v1.76.0 client.go:316 的原文。
const anthropicStreamingRequiredText = "streaming is required for operations that may take longer than 10 minutes"

// anthropicNonStreamModelLimit 复刻 anthropic-sdk-go v1.76.0
// shared/constant/constants.go:9-17 的 ModelNonStreamingTokens 表（升级 SDK 须同步）。
var anthropicNonStreamModelLimit = map[string]int64{
	"claude-opus-4-20250514":                  8192,
	"claude-4-opus-20250514":                  8192,
	"claude-opus-4-0":                         8192,
	"anthropic.claude-opus-4-20250514-v1:0":   8192,
	"claude-opus-4@20250514":                  8192,
	"anthropic.claude-opus-4-1-20250805-v1:0": 8192,
	"claude-opus-4-1@20250805":                8192,
}

// anthropicRequiresStreaming 与 SDK 同判据：expectedTime = 1h*maxTokens/128000
// 超过 10min（等价 maxTokens > 128000/6，即 > 21333）或命中模型上限表。
// 用映射后的上游模型名判定（SDK 用的是 params.Model，等价 sel.Model）。
func anthropicRequiresStreaming(model string, maxTokens int64) bool {
	if maxTokens > 21333 { // 1h*maxTokens/128000 > 10min
		return true
	}
	if lim, ok := anthropicNonStreamModelLimit[model]; ok && maxTokens > lim {
		return true
	}
	return false
}
