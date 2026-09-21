// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/is7qin/c3api/internal/domain"
)

// AIRouter 挂载 AI 端点（规格 §6.1/§9）：路径决定请求格式，全部走通用转发
// 骨架 handleFormat（UpstreamCaller 注册表分发）；resp-ws 与 HTTP
// responses 同路径（真实客户端无 /ws 后缀）——/v1/responses 带 upgrade 头
// → 按 resp-ws 处理，走专用编排 HandleResponsesWS（caller_responses_ws.go）。
func AIRouter(p *Proxy) http.Handler {
	r := chi.NewRouter()
	r.Post("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatOpenAIChat, w, req)
	})
	r.Post("/v1/responses", func(w http.ResponseWriter, req *http.Request) {
		if isWebSocketUpgrade(req) {
			p.HandleResponsesWS(w, req)
			return
		}
		p.handleFormat(domain.FormatOpenAIResponses, w, req)
	})
	// WS 升级请求是 GET（RFC 6455），HTTP responses 是 POST——GET 路由只放行
	// upgrade，普通 GET 保持 405（API 语义不变）。
	r.Get("/v1/responses", func(w http.ResponseWriter, req *http.Request) {
		if !isWebSocketUpgrade(req) {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
			return
		}
		p.HandleResponsesWS(w, req)
	})
	r.Post("/v1/messages", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatAnthropic, w, req)
	})
	// 图片生成双端点（§5.1）：同一格式 openai-images，上游子路径由
	// handleFormat 内 imagesCallerFor 按路径区分（generations/edits）。
	r.Post("/v1/images/generations", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatOpenAIImages, w, req)
	})
	r.Post("/v1/images/edits", func(w http.ResponseWriter, req *http.Request) {
		p.handleFormat(domain.FormatOpenAIImages, w, req)
	})
	// codex /v1/alpha/search（spec 2026-08-13）：codex CLI web search 独立 unary
	// 端点——透传路由（专用编排 HandleSearch：独立选号 + 四类型分派 + 按次计费
	// 落账，forward_search.go）。
	r.Post("/v1/alpha/search", p.HandleSearch)
	// GET /v1/models（OpenAI 兼容模型列表）：user API key 鉴权；端点冷面——
	// 快照读零 DB（models.go）。
	r.Get("/v1/models", p.HandleModels)
	return r
}
