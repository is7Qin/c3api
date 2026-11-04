// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import "net/http"

// anthropicDefaultVersion 是 anthropic-sdk-go v1.76.0 的 DefaultVersion：SDK 在
// 客户端未显式提供时写入 `Anthropic-Version: 2023-06-01`。非流改原始出栈后这条
// 默认值必须由网关自己补——原生 anthropic 客户端通常带，但**转换方向**
// （openai 客户端 → anthropic 上游）从来不带，缺了它上游直接 400。
const anthropicDefaultVersion = "2023-06-01"

// withProtocolDefaults 给非流原始出栈补回「SDK 会替我们加、原始出栈不会加」
// 的协议默认头：
//   - anthropic 目标：Anthropic-Version（必需，见上）；
//   - 所有目标：Accept: application/json（openai-go 默认头；部分上游据此选响应
//     表示）。
//
// 语义与 SDK 一致：默认先写、客户端头覆盖，因此客户端带了就原样沿用。只在缺失
// 时复制一份 map——不能就地改入站 r.Header（RelayHeaders 的调用方契约：入站
// 只读）。
func withProtocolDefaults(in http.Header, anthropic bool) http.Header {
	needVersion := anthropic && in.Get("Anthropic-Version") == ""
	needAccept := in.Get("Accept") == ""
	if !needVersion && !needAccept {
		return in
	}
	out := make(http.Header, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	if needAccept {
		out.Set("Accept", "application/json")
	}
	if needVersion {
		out.Set("Anthropic-Version", anthropicDefaultVersion)
	}
	return out
}
