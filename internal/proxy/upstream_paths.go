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
