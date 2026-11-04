// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// upstreamSpy 记录非流上游收到的请求事实（路径、鉴权头、body、UA），并回放指定
// 状态码与响应体。M1 门禁需要「逐调用点」验证线上事实：路径未变、鉴权头未变、
// body 除 model 改写外原样（尤其**不得注入 stream**）。
type upstreamSpy struct {
	mu     sync.Mutex
	hits   int
	path   string
	query  string
	auth   string
	apiKey string
	ua     string
	accept string
	body   []byte
}

func (s *upstreamSpy) snapshot() upstreamSpy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return upstreamSpy{
		hits: s.hits, path: s.path, query: s.query,
		auth: s.auth, apiKey: s.apiKey, ua: s.ua, accept: s.accept, body: s.body,
	}
}

// newUpstreamSpy 起一个记录型上游：固定回放 status + respBody。
func newUpstreamSpy(t *testing.T, status int, respBody string) (*httptest.Server, *upstreamSpy) {
	t.Helper()
	spy := &upstreamSpy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		spy.mu.Lock()
		spy.hits++
		spy.path = r.URL.Path
		spy.query = r.URL.RawQuery
		spy.auth = r.Header.Get("Authorization")
		spy.apiKey = r.Header.Get("x-api-key")
		spy.ua = r.Header.Get("User-Agent")
		spy.accept = r.Header.Get("Accept")
		spy.body = raw
		spy.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, spy
}

// TestNonStreamCallSitesPathAuthBody 五处非流改道的逐点判据（chat / responses /
// anthropic）：上游收到的路径与格式默认一致、鉴权头口径不变、body 的 model 已
// 按映射改写、**未注入 stream**、未知请求字段原样透传；客户端收到的响应保真
// （未知响应字段与嵌套值都不被丢弃——这是去 SDK 的核心理由）。
func TestNonStreamCallSitesPathAuthBody(t *testing.T) {
	const okBody = `{"id":"c1","unknown_field":{"k":1},"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`

	cases := []struct {
		name       string
		format     domain.RequestFormat
		clientPath string
		body       string
		wantPath   string
		wantAuth   string
		wantAPIKey string
	}{
		{
			name:       "chat 非流",
			format:     domain.FormatOpenAIChat,
			clientPath: "/v1/chat/completions",
			body:       `{"model":"gpt-4o","messages":[],"unknown_request_field":1}`,
			wantPath:   "/v1/chat/completions",
			wantAuth:   "Bearer sk-upstream",
		},
		{
			name:       "responses 非流",
			format:     domain.FormatOpenAIResponses,
			clientPath: "/v1/responses",
			body:       `{"model":"gpt-4o","input":"hi","unknown_request_field":1}`,
			wantPath:   "/v1/responses",
			wantAuth:   "Bearer sk-upstream",
		},
		{
			name:       "anthropic 非流",
			format:     domain.FormatAnthropic,
			clientPath: "/v1/messages",
			body:       `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"unknown_request_field":1}`,
			wantPath:   "/v1/messages",
			wantAPIKey: "sk-upstream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, spy := newUpstreamSpy(t, http.StatusOK, okBody)
			tpl := mappingTpl(up.URL, nil)
			tpl.SupportedFormats = []domain.RequestFormat{tc.format}
			p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)

			req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			req.Header.Set("x-api-key", "ck-1")
			rec := httptest.NewRecorder()
			switch tc.format {
			case domain.FormatOpenAIChat:
				p.HandleChat(rec, req)
			case domain.FormatOpenAIResponses:
				p.HandleResponses(rec, req)
			default:
				p.HandleAnthropic(rec, req)
			}
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

			got := spy.snapshot()
			require.Equal(t, 1, got.hits, "应恰好一次上游调用")
			require.Equal(t, tc.wantPath, got.path, "上游路径必须与格式默认一致")
			require.Empty(t, got.query, "默认端点不带 query")
			require.Equal(t, tc.wantAuth, got.auth, "openai 系鉴权头口径")
			require.Equal(t, tc.wantAPIKey, got.apiKey, "anthropic 鉴权头口径")
			require.Equal(t, "application/json", got.accept, "SDK 默认 Accept 必须补回（客户端没带时）")

			var sent map[string]any
			require.NoError(t, json.Unmarshal(got.body, &sent), "上游收到合法 JSON：%s", got.body)
			require.Equal(t, "gpt-4o", sent["model"], "模型改写后送达")
			_, hasStream := sent["stream"]
			require.False(t, hasStream, "非流请求不得被注入 stream 字段")
			require.Equal(t, float64(1), sent["unknown_request_field"], "未知请求字段透传上游")

			// 客户端收到的响应保真：未知响应字段必须原样出现（SDK 时代会被丢弃）
			require.Equal(t, float64(1), gjson.Get(rec.Body.String(), "unknown_field.k").Num,
				"未知响应字段与嵌套值透传客户端：%s", rec.Body.String())
		})
	}
}

// TestNonStreamStatusMatrix 非流「成功判定」集中化的可观测行为：只有 200 视为
// 成功；201/204/299/101 在 rawJSON 层即归为失败（不把空 body 当成功、不让
// 1xx 悬挂），最终统一收敛 502。4xx/5xx 的客户端投影由规则引擎决定（seed：
// 400 原文透传、429 码透文不透、401/403/404/409 归一 502 —— 见
// TestErrClassify4xxNormalizeMatrix），与本层改动正交，这里只钉 400 与 5xx
// 两个代表档以免重复覆盖。
func TestNonStreamStatusMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantStatus int
		wantBody   string
	}{
		{"200 成功且 body 原样回灌", http.StatusOK, http.StatusOK, "upstream said no"},
		{"201 非 200 2xx", http.StatusCreated, http.StatusBadGateway, ""},
		{"204 空 body", http.StatusNoContent, http.StatusBadGateway, ""},
		{"299 非标准 2xx", 299, http.StatusBadGateway, ""},
		{"101 升级", 101, http.StatusBadGateway, ""},
		{"302 残余重定向", http.StatusFound, http.StatusBadGateway, ""},
		{"400 规则透传", http.StatusBadRequest, http.StatusBadRequest, "upstream said no"},
		{"500 归一", http.StatusInternalServerError, http.StatusBadGateway, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := newUpstreamSpy(t, tc.status, `{"error":{"message":"upstream said no"}}`)
			p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, &captureLogStore{}, nil)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)

			require.Equal(t, tc.wantStatus, rec.Code, "上游 %d 的客户端投影", tc.status)
			if tc.wantBody != "" {
				require.Contains(t, rec.Body.String(), tc.wantBody, "该档上游 body 必须可见")
			} else {
				require.NotContains(t, rec.Body.String(), "upstream said no", "非透传档不得泄漏上游 body")
			}
		})
	}
}

// TestNonStreamRejectsMalformedBody 本地校验只剩「JSON object + model 字符串」
// （SDK 反序列化时代的 schema 校验随 typed 入口一起退场）；拒绝必须发生在
// 触达上游之前。无 model 的请求在路由层就 fail-closed 成 404（既有语义，
// 见 TestHandleChat_ModellessRequest_FailsClosedWithoutDispatch），故此处
// 只钉「model 可路由、body 形状非法」这一档。
func TestNonStreamRejectsMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"截断 object", `{"model":"gpt-4o","messages":[`, http.StatusBadRequest},
		{"object 后跟垃圾", `{"model":"gpt-4o","messages":[]} trailing`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, spy := newUpstreamSpy(t, http.StatusOK, `{}`)
			p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, &captureLogStore{}, nil)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, tc.want, rec.Code, "body=%s", tc.body)
			require.Zero(t, spy.snapshot().hits, "本地拒绝不得触达上游（body=%s）", tc.body)
		})
	}
}

// TestNonStreamConvertedCallSites 转换面两条非流改道（caller_converted.go 的
// rawJSON 双分支：目标 resp / 目标 anthropic）逐点判据：上游路径 = 目标格式
// 默认端点、鉴权头按目标格式、模型已改写、**未携带 stream=true**（转换面在
// 非流分支必须产出非流上游请求），客户端拿到 200 且已转成客户端协议。
func TestNonStreamConvertedCallSites(t *testing.T) {
	const respBody = `{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`
	const messBody = `{"id":"msg_1","type":"message","role":"assistant","model":"gpt-4o","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":5}}`
	const chatBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	const respReqBody = `{"model":"gpt-4o","input":"hi"}`

	cases := []struct {
		name       string
		dir        domain.ProtocolConvert
		tplFormat  domain.RequestFormat
		clientPath string
		body       string
		upBody     string
		wantPath   string
		wantAuth   string
		wantAPIKey string
	}{
		{
			name: "chat → resp 非流", dir: domain.ProtocolConvertChatToResp,
			tplFormat: domain.FormatOpenAIResponses, clientPath: "/v1/chat/completions", body: chatBody,
			upBody: respBody, wantPath: "/v1/responses", wantAuth: "Bearer sk-upstream",
		},
		{
			name: "resp → mess 非流", dir: domain.ProtocolConvertRespToMess,
			tplFormat: domain.FormatAnthropic, clientPath: "/v1/responses", body: respReqBody,
			upBody: messBody, wantPath: "/v1/messages", wantAPIKey: "sk-upstream",
		},
		{
			name: "chat → mess 非流", dir: domain.ProtocolConvertChatToMess,
			tplFormat: domain.FormatAnthropic, clientPath: "/v1/chat/completions", body: chatBody,
			upBody: messBody, wantPath: "/v1/messages", wantAPIKey: "sk-upstream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, spy := newUpstreamSpy(t, http.StatusOK, tc.upBody)
			p := newConvertedTestProxyLogs(t, up.URL,
				[]domain.RequestFormat{tc.tplFormat}, []domain.ProtocolConvert{tc.dir},
				&captureLogStore{}, 30*time.Second)

			req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			req.Header.Set("x-api-key", "ck-1")
			rec := httptest.NewRecorder()
			if tc.clientPath == "/v1/responses" {
				p.HandleResponses(rec, req)
			} else {
				p.HandleChat(rec, req)
			}
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

			got := spy.snapshot()
			require.Equal(t, 1, got.hits, "应恰好一次上游调用")
			require.Equal(t, tc.wantPath, got.path, "转换面必须打目标格式默认端点")
			require.Equal(t, tc.wantAuth, got.auth, "目标 resp 用 Bearer")
			require.Equal(t, tc.wantAPIKey, got.apiKey, "目标 anthropic 用 x-api-key")
			require.Equal(t, "gpt-4o", gjson.GetBytes(got.body, "model").String(), "转换面模型改写后送达")
			require.NotContains(t, string(got.body), `"stream"`, "非流转换不得携带 stream 字段（值判 false 会放过 false 注入）")

			// 客户端拿到的是转换后的自身协议（chat 响应 / resp 响应）
			require.True(t, gjson.Valid(rec.Body.String()), "客户端协议响应必须是合法 JSON：%s", rec.Body.String())
		})
	}
}

// TestNonStreamErrorBodyBounded 错误体读取有界（§3.3.8）：上游 4xx 回 >1 MiB
// 错误体时，网关按 1 MiB 截断后原样写客户端，状态码仍为 400，不 panic、不
// 误判成功（截断后的 body 可能不是合法 JSON，这是有意的既有取舍）。
func TestNonStreamErrorBodyBounded(t *testing.T) {
	big := `{"error":{"message":"` + strings.Repeat("x", 1<<20) + `"}}`
	up, _ := newUpstreamSpy(t, http.StatusBadRequest, big)
	p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, &captureLogStore{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "截断不得改变状态码")
	require.Len(t, rec.Body.Bytes(), 1<<20, "错误体按 1 MiB 有界截断")
}

// TestNonStreamModelRewriteReachesUpstream 逐调用点断言「映射后的模型真的到达
// 上游」：无映射时 model 不变，`setModel` 短路会让「改写丢失」的回归逃过检查，
// 所以必须用显式映射（mapped ≠ client）覆盖三种非流格式。响应侧按映射模式分叉：
// 显式映射保留上游模型（客户端看得见上游模型名），隐式映射改写回客户端模型。
func TestNonStreamModelRewriteReachesUpstream(t *testing.T) {
	const clientModel, upstreamModel = "client-model", "upstream-model"
	cases := []struct {
		name       string
		format     domain.RequestFormat
		clientPath string
		body       string
		wantAuth   string
		wantAPIKey string
	}{
		{
			name: "chat 非流", format: domain.FormatOpenAIChat, clientPath: "/v1/chat/completions",
			body: `{"model":"client-model","messages":[]}`, wantAuth: "Bearer sk-upstream",
		},
		{
			name: "responses 非流", format: domain.FormatOpenAIResponses, clientPath: "/v1/responses",
			body: `{"model":"client-model","input":"hi"}`, wantAuth: "Bearer sk-upstream",
		},
		{
			name: "anthropic 非流", format: domain.FormatAnthropic, clientPath: "/v1/messages",
			body:       `{"model":"client-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			wantAPIKey: "sk-upstream",
		},
	}
	modes := []struct {
		name         string
		mode         domain.ModelMappingMode
		wantRespBody string
	}{
		{"explicit", domain.ModelMappingModeExplicit, upstreamModel},
		{"implicit", domain.ModelMappingModeImplicit, clientModel},
	}
	for _, tc := range cases {
		for _, m := range modes {
			t.Run(tc.name+"/"+m.name, func(t *testing.T) {
				up, spy := newUpstreamSpy(t, http.StatusOK,
					`{"id":"x1","model":"`+upstreamModel+`","usage":{"input_tokens":1,"output_tokens":2,"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
				tpl := mappingTpl(up.URL, map[string]domain.ModelMappingEntry{
					clientModel: {MappedModel: upstreamModel, Mode: m.mode},
				})
				tpl.SupportedFormats = []domain.RequestFormat{tc.format}
				tpl.Models = []string{clientModel}
				p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)

				req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.body))
				req.Header.Set("Authorization", "Bearer ck-1")
				req.Header.Set("x-api-key", "ck-1")
				rec := httptest.NewRecorder()
				switch tc.format {
				case domain.FormatOpenAIChat:
					p.HandleChat(rec, req)
				case domain.FormatOpenAIResponses:
					p.HandleResponses(rec, req)
				default:
					p.HandleAnthropic(rec, req)
				}
				require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

				got := spy.snapshot()
				require.Equal(t, upstreamModel, gjson.GetBytes(got.body, "model").String(),
					"映射后的模型必须到达上游（mapping 丢失时这里还是 client-model）")
				require.Equal(t, m.wantRespBody, gjson.Get(rec.Body.String(), "model").String(),
					"响应模型按映射模式分叉：explicit 保留上游名，implicit 改写回客户端名")
			})
		}
	}
}

// TestNonStreamRejectsDuplicateSensitiveKeys 顶层 model/stream 重复必须拒：
// 路由与改写读首个值，而不少上游按末个生效——`{"model":"gpt-4o",
// "model":"other"}` 会被批准成 gpt-4o、setModel 因首值已达标而短路，上游却
// 执行 other，模型白名单与计价被绕过。SDK 时代的 typed 出栈天然只写一个
// model 键，改原始出栈后这条由显式校验接管。
func TestNonStreamRejectsDuplicateSensitiveKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"重复 model", `{"model":"gpt-4o","model":"other-model","messages":[]}`},
		{"重复 stream", `{"model":"gpt-4o","stream":false,"stream":true,"messages":[]}`},
		// 转义形态的键名在 JSON 语义上是同一个键：上游按末个生效，裸字节比较
		// 会放过这类重复（外部评审实测的绕过点）。
		{"转义重复 model", `{"model":"gpt-4o","\u006dodel":"other-model","messages":[]}`},
		{"转义重复 stream", `{"model":"gpt-4o","stream":false,"\u0073tream":true,"messages":[]}`},
		// 流式分支同款洞：改写同样按首个值短路，重复键必须在入口就拒（覆盖两条分支）。
		{"流式重复 model", `{"model":"gpt-4o","model":"other-model","stream":true,"messages":[]}`},
		{"model 出现在后段仍算重复", `{"messages":[],"temperature":1,"model":"gpt-4o","model":"gpt-4o"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, spy := newUpstreamSpy(t, http.StatusOK, `{}`)
			p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, &captureLogStore{}, nil)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", tc.body)
			require.Contains(t, rec.Body.String(), "duplicate", "错误文案要点明重复键：%s", rec.Body.String())
			require.Zero(t, spy.snapshot().hits, "本地拒绝不得触达上游")
		})
	}

	// 嵌套对象里的同名键不算顶层重复（别把正常请求误拒）。
	t.Run("嵌套同名键放行", func(t *testing.T) {
		up, spy := newUpstreamSpy(t, http.StatusOK, `{"id":"c1"}`)
		p := newTestProxyTplTimeoutLogs(t, mappingTpl(up.URL, nil), 1, true, 30*time.Second, &captureLogStore{}, nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"gpt-4o","messages":[],"metadata":{"model":"inner","stream":true}}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleChat(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Equal(t, 1, spy.snapshot().hits)
	})
}
