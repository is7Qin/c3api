// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "github.com/is7qin/c3api/internal/credential"

// CodexResponsesTransport 原生 codex responses 车道的**出站传输**选择
// （spec-codex-outbound-transport §3）：模板级开关，落在
// template_ext.codex_responses_transport。
//
// 网关只存/校验/透传——帧构造、读模型、close 语义、错误面全在 codex-sdk。
// 用它而不是裸 string：该值出现在 TemplateExt/Template/Selection/planKey/
// sdkbridge 五处，裸 string 会让"任意字符串都能塞进来"成为编译期合法行为。
//
// 不做大小写/前缀归一（`"WS"`/`"websocket"`/`" "` 一律 400）：回落会让两条
// 实验臂都落 http，读数"一致地看不出差别"，是事后最难定位的故障。
type CodexResponsesTransport string

const (
	// CodexResponsesTransportDefault 未配置 = 默认 http（零值；库里落 ""）。
	CodexResponsesTransportDefault CodexResponsesTransport = ""
	// CodexResponsesTransportHTTP 显式 HTTP/SSE（与 Default 效果相同，仅表达意图）。
	CodexResponsesTransportHTTP CodexResponsesTransport = "http"
	// CodexResponsesTransportWS 上游 WebSocket（每次 attempt 一条连接，无池）。
	CodexResponsesTransportWS CodexResponsesTransport = "ws"
)

// Valid 值域判定（写面 400 的唯一判据）：只接受 ""/"http"/"ws"。
func (t CodexResponsesTransport) Valid() bool {
	switch t {
	case CodexResponsesTransportDefault, CodexResponsesTransportHTTP, CodexResponsesTransportWS:
		return true
	}
	return false
}

// AllowedFor 类型面判定：只有 codex 号池类型（oauth/pat）经过 responses
// 出站面，非 codex 只接受 ""（"http" 与 "ws" 一样是永不生效的配置）。
//
// 非法字面量对任何类型都不放行（service 的第 1 步值域校验先行；这里保持
// fail-closed，避免"两步都只做了一半"时漏放）。
func (t CodexResponsesTransport) AllowedFor(ct credential.Type) bool {
	if !t.Valid() {
		return false
	}
	return ct.IsCodex() || t == CodexResponsesTransportDefault
}

// Key 计划/缓存键维度的归一："" 与 "http" 同键（否则一次"留空改成显式
// http"的空改动就换叶换计划）；非法值 → ""（默认面）。
//
// 与 Name 的分工写死在这里：**三份归一 = 三份静默分歧**，sdkbridge 的
// codexTransport 与 proxy 的响应头都只调这两个方法。
func (t CodexResponsesTransport) Key() string {
	if t == CodexResponsesTransportWS {
		return string(CodexResponsesTransportWS)
	}
	return ""
}

// Name 对外观测（响应头 x-c3api-upstream-transport）的归一："http" | "ws"；
// 非法值 → "http"（与 Key 的默认面一致，不把脏值透给实验仪器）。
func (t CodexResponsesTransport) Name() string {
	if t == CodexResponsesTransportWS {
		return string(CodexResponsesTransportWS)
	}
	return string(CodexResponsesTransportHTTP)
}
