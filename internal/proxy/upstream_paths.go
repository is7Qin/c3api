// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// 自定义上游路径的键集合与默认尾段：**唯一实现已下沉到 internal/domain**
// （spec account-model-test §4.2——proxy 热路径、service 写面校验与探测执行共用
// 同一份键表/默认端点）。这里的常量只是 domain 常量的别名，调用点保持不动。
const (
	pathKeyChat        = string(domain.UpstreamPathChat)
	pathKeyResponses   = string(domain.UpstreamPathResponses)
	pathKeyResponsesWS = string(domain.UpstreamPathResponsesWS)
	pathKeyAnthropic   = string(domain.UpstreamPathAnthropic)
	pathKeySearch      = string(domain.UpstreamPathSearch)
	pathKeyImagesGen   = string(domain.UpstreamPathImagesGen)
	pathKeyImagesEdits = string(domain.UpstreamPathImagesEdits)

	defaultPathChat        = domain.UpstreamDefaultTailChat
	defaultPathResponses   = domain.UpstreamDefaultTailResponses
	defaultPathResponsesWS = domain.UpstreamDefaultTailResponsesWS
	defaultPathAnthropic   = domain.UpstreamDefaultTailAnthropic
	defaultPathSearch      = domain.UpstreamDefaultTailSearch
	defaultPathImagesGen   = domain.UpstreamDefaultTailImagesGen
	defaultPathImagesEdits = domain.UpstreamDefaultTailImagesEdits
)

// targetPath 取该格式最终拼进 URL 的路径（热路径：一次 map 读 + 常量回落，
// 不查 DB、不加锁、不复制 map；配置 map 在快照内不可变，此处只读）。语义与
// 判定全部在 domain.ResolveUpstreamPath（含 custom 原样透传/非 custom 回落默认
// 的唯一实现），本函数只做 Selection → 参数的搬运。
func targetPath(sel *scheduler.Selection, key, tail string) string {
	if sel == nil {
		return domain.ResolveUpstreamPath("", nil, domain.UpstreamPathKey(key), tail)
	}
	return domain.ResolveUpstreamPath(sel.CredentialType, sel.UpstreamPaths, domain.UpstreamPathKey(key), tail)
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
