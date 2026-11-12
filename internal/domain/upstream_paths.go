// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"github.com/is7qin/c3api/internal/credential"
)

// 自定义上游路径的键集合与默认尾段（spec account-model-test §4.2/§5.1）：
// **全仓唯一一份**——proxy 热路径、service 写面校验、探测执行三方共用本文件，
// 不得再抄第二份键表或默认端点。
//
// 默认值只是**端点尾段**，不含 /v1：base_url 是裸根约定（模板校验禁止以 /v1
// 结尾），/v1 是协议段，由 ResolveUpstreamPath 在「走内置端点」时拼一次。默认
// 值里不出现协议段，是因为它不属于配置面——custom 路径写什么就打什么，两者
// 互不影响。
const (
	UpstreamPathChat        UpstreamPathKey = "openai-chat"
	UpstreamPathResponses   UpstreamPathKey = "openai-responses"
	UpstreamPathResponsesWS UpstreamPathKey = "openai-responses-ws"
	UpstreamPathAnthropic   UpstreamPathKey = "anthropic"
	UpstreamPathSearch      UpstreamPathKey = "openai-search"
	UpstreamPathImagesGen   UpstreamPathKey = "openai-images.generations"
	UpstreamPathImagesEdits UpstreamPathKey = "openai-images.edits"
)

// UpstreamPathKey 是 template_ext.upstream_paths 的键类型（落库仍是原字符串）。
type UpstreamPathKey string

// 默认端点尾段（不含 /v1、不带前导斜杠；见上方注释）。
const (
	UpstreamDefaultTailChat        = "chat/completions"
	UpstreamDefaultTailResponses   = "responses"
	UpstreamDefaultTailResponsesWS = "responses"
	UpstreamDefaultTailAnthropic   = "messages"
	UpstreamDefaultTailSearch      = "alpha/search"
	UpstreamDefaultTailImagesGen   = "images/generations"
	UpstreamDefaultTailImagesEdits = "images/edits"
)

// upstreamDefaultTails 键 → 默认尾段（探测执行与 proxy 常量别名的同源表）。
var upstreamDefaultTails = map[UpstreamPathKey]string{
	UpstreamPathChat:        UpstreamDefaultTailChat,
	UpstreamPathResponses:   UpstreamDefaultTailResponses,
	UpstreamPathResponsesWS: UpstreamDefaultTailResponsesWS,
	UpstreamPathAnthropic:   UpstreamDefaultTailAnthropic,
	UpstreamPathSearch:      UpstreamDefaultTailSearch,
	UpstreamPathImagesGen:   UpstreamDefaultTailImagesGen,
	UpstreamPathImagesEdits: UpstreamDefaultTailImagesEdits,
}

// upstreamPathFormats 键 → 需要的格式（service 写面校验与 ext 读取共用的唯一
// 映射；两个 images 键都归 FormatOpenAIImages）。
var upstreamPathFormats = map[string]RequestFormat{
	string(UpstreamPathChat):        FormatOpenAIChat,
	string(UpstreamPathResponses):   FormatOpenAIResponses,
	string(UpstreamPathResponsesWS): FormatOpenAIResponsesWS,
	string(UpstreamPathAnthropic):   FormatAnthropic,
	string(UpstreamPathSearch):      FormatOpenAISearch,
	string(UpstreamPathImagesGen):   FormatOpenAIImages,
	string(UpstreamPathImagesEdits): FormatOpenAIImages,
}

// UpstreamPathFormat 键是否属于固定 7 键全集；返回该键绑定的请求格式。
func UpstreamPathFormat(key string) (RequestFormat, bool) {
	f, ok := upstreamPathFormats[key]
	return f, ok
}

// UpstreamDefaultTail 键的默认端点尾段（未知键返回空串）。
func UpstreamDefaultTail(key UpstreamPathKey) string {
	return upstreamDefaultTails[key]
}

// UpstreamPathKeyForFormat 文本四格式 → 自定义路径键（探测执行用）。images 按
// ImageMode 直接取 UpstreamPathImagesGen/Edits，search 直接取 UpstreamPathSearch，
// 都不经过本表（同一格式两端点，不能共用一个键）。
func UpstreamPathKeyForFormat(f RequestFormat) (UpstreamPathKey, bool) {
	switch f {
	case FormatOpenAIChat:
		return UpstreamPathChat, true
	case FormatOpenAIResponses:
		return UpstreamPathResponses, true
	case FormatOpenAIResponsesWS:
		return UpstreamPathResponsesWS, true
	case FormatAnthropic:
		return UpstreamPathAnthropic, true
	}
	return "", false
}

// ResolveUpstreamPath 取该格式最终拼进 URL 的路径（单一实现：proxy 热路径与
// 探测执行都调用这里）。
//
//   - custom 模板配置了该键 → **原样返回**：自定义路径就是用户对上游路径的完整
//     表达，这里不做任何补全/改写（"补 /v1" 是内置端点的协议适配，与 custom
//     无关；要打 /v1/x 就写 v1/x，要打 /x 就写 x）。
//   - 其余（非 custom，或 custom 未配置该键）→ 内置默认端点："/v1/" + 尾段。
//     裸根 base_url 下得 base/v1/chat/completions 等，与去 SDK 前逐字节一致。
//
// defaultTail 是调用方持有的默认尾段（proxy 以常量传入，探测以
// UpstreamDefaultTail(key) 传入）：保留该形参使 proxy 既有调用点与测试零改动。
func ResolveUpstreamPath(ct credential.Type, paths map[string]string, key UpstreamPathKey, defaultTail string) string {
	if ct == credential.TypeCustom && paths != nil {
		if v := paths[string(key)]; v != "" {
			return v
		}
	}
	return "/v1/" + defaultTail
}
