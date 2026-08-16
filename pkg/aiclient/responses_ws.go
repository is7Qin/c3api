// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

// --- Responses WebSocket（resp-ws 通用传输，coder/websocket） ---
// resp-ws = 通用格式：任何模板 supported_formats 含 resp-ws 即可用，1:1 透传。
// 帧是 JSON 文本事件，本层只做传输（握手/压缩/心跳），零协议解析——帧内容
// 由网关编排层嗅探（response.completed usage），解析面最小化。

// responsesWSPath 上游 WS 端点路径：与 POST /v1/responses 同端点（用户核实
// codex 真实实现 responses_websocket.rs：websocket_url_for_path("responses")
// 仅做 http→ws/https→wss 协议替换，无 /ws 后缀）。模板 base_url 为裸根约定，
// 与 v1/messages 同款自带 v1 前缀，parseFullURL 不再补 /v1。
const responsesWSPath = "v1/responses"

// ResponsesWSBetaHeader 上游握手 beta 头（现役唯一：responses_websockets=
// 2026-02-06，OpenAI 随版本滚动；网关固定最新值，客户端自带旧值被覆盖）。
const ResponsesWSBetaHeader = "2026-02-06"

// AcceptResponsesWS 服务端接受客户端 WS 升级（101）。压缩按真实客户端行为
// 启用 permessage-deflate（ContextTakeover 协商，对端不支持自动降级）；心跳/
// 关闭由编排层负责（本层只做握手）。非升级请求返回错误（Accept 已写出 4xx）。
func AcceptResponsesWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
}

// ResponsesWSDial 上游 WS 拨号（aiclient 懒缓存 URL 复用，同 rawPost 惯例）：
// 注入账号鉴权（Bearer）与 beta 头；header 为编排层透传的客户端头（hop-by-hop
// 与网关 key 已在编排层剔除），本函数就地 Set 改写——header is consumed，
// 调用方须传私有副本（库内 dial 会再 Clone 一次，防御边界在库，此处不再
// Clone）。返回 resp 仅在握手失败时非 nil（含上游拒绝 body，调用方按状态码
// 分类 429/4xx）。压缩同 Accept 侧协商。
func (f *Factory) ResponsesWSDial(ctx context.Context, templateID int64, baseURL, key string, header http.Header) (*websocket.Conn, *http.Response, error) {
	full, err := f.fullURLOf(templateID, baseURL, responsesWSPath)
	if err != nil {
		return nil, nil, err
	}
	header.Set("Authorization", "Bearer "+key)
	header.Set("Responses-Websockets", ResponsesWSBetaHeader)
	return websocket.Dial(ctx, full.String(), &websocket.DialOptions{
		HTTPClient:      f.hc,
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionContextTakeover,
	})
}

// ResponsesWSURL 取模板 resp-ws 完整端点 URL 字符串（T4 codex Dial
// WithBaseURL 用——SDK 完整端点语义 client.go:137-144：覆盖值按完整端点直用，
// 传裸根打 /v1 静默 404；复用 fullURLOf 的组装与懒缓存，零新代码）。
func (f *Factory) ResponsesWSURL(templateID int64, baseURL string) (string, error) {
	full, err := f.fullURLOf(templateID, baseURL, responsesWSPath)
	if err != nil {
		return "", err
	}
	return full.String(), nil
}
