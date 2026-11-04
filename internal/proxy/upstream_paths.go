// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

// 自定义上游路径的键集合与默认尾段（spec §4.3，键名必须与 service 层校验一致）。
//
// 默认值只是**端点尾段**，不含 /v1：base_url 是裸根约定（模板校验禁止以 /v1
// 结尾），/v1 是协议段，由 targetPath 在「走内置端点」时拼一次。默认值里不出现
// 协议段，是因为它不属于配置面——custom 路径写什么就打什么（见 targetPath），
// 两者互不影响。
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
	defaultPathResponsesWS = "responses"
	defaultPathAnthropic   = "messages"
	defaultPathSearch      = "alpha/search"
	defaultPathImagesGen   = "images/generations"
	defaultPathImagesEdits = "images/edits"
)

// targetPath 取该格式最终拼进 URL 的路径（热路径：一次 map 读 + 常量回落，
// 不查 DB、不加锁、不复制 map；配置 map 在快照内不可变，此处只读）。
//
//   - custom 模板配置了该键 → **原样返回**：自定义路径就是用户对上游路径的完整
//     表达，这里不做任何补全/改写（"补 /v1" 是内置端点的协议适配，与 custom
//     无关；要打 /v1/x 就写 v1/x，要打 /x 就写 x）。
//   - 其余（非 custom，或 custom 未配置该键）→ 内置默认端点："/v1/" + 尾段。
//     裸根 base_url 下得 base/v1/chat/completions 等，与去 SDK 前逐字节一致。
//
// 仅 custom 模板可覆盖：类型被改回 api_key 等时，即使快照里残留配置也必须回落
// 默认，避免请求静默打到旧的自定义端点。
func targetPath(sel *scheduler.Selection, key, tail string) string {
	if sel != nil && sel.CredentialType == credential.TypeCustom && sel.UpstreamPaths != nil {
		if v := sel.UpstreamPaths[key]; v != "" {
			return v
		}
	}
	return "/v1/" + tail
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
