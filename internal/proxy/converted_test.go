// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/redisx"
)

// --- 协议转换路径（W5）接线测试 ---

// capturedUpstream 记录最近一次请求的路径与体（上游协议断言），并按路径/stream
// 返回对应协议的非流式 JSON 或 SSE 流。
type capturedUpstream struct {
	mu       sync.Mutex
	path     string
	body     map[string]any
	stream   bool
	dataOnly bool // /v1/responses 流式不产 event: 行（P3：非规范上游形态，同 fakeupstream）
}

func (c *capturedUpstream) last(t *testing.T) (string, map[string]any, bool) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.body, c.stream
}

// srv 按路径返回协议响应：/v1/responses → resp 流/JSON；/v1/messages → anthropic
// 流/JSON；/v1/chat/completions → chat 流/JSON。
func (c *capturedUpstream) srv(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		c.mu.Lock()
		c.path, c.body, c.stream = r.URL.Path, body, body["stream"] == true
		c.mu.Unlock()
		stream, _ := body["stream"].(bool)
		switch r.URL.Path {
		case "/v1/responses":
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				c.mu.Lock()
				only := c.dataOnly
				c.mu.Unlock()
				if only {
					// P3 形态：只发 data: 行（缺 event: 名），帧自带 type 字段
					fmt.Fprint(w, `data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}`+"\n\n")
					fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`+"\n\n")
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				fmt.Fprint(w, `event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"in_progress","model":"gpt-4o","output":[],"usage":null}}`+"\n\n")
				fmt.Fprint(w, `event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}`+"\n\n")
				fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`+"\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "rsp_1", "object": "response", "created_at": 1750000000,
				"status": "completed", "model": body["model"],
				"output": []any{map[string]any{
					"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "hi", "annotations": []any{}}},
				}},
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
			})
		case "/v1/messages":
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":3}}}`+"\n\n")
				fmt.Fprint(w, `event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
				fmt.Fprint(w, `event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":20}}`+"\n\n")
				fmt.Fprint(w, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant",
				"model": body["model"], "content": []any{map[string]any{"type": "text", "text": "hi"}},
				"stop_reason": "end_turn", "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 5},
			})
		case "/v1/chat/completions":
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `data: {"id":"c_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n")
				fmt.Fprint(w, `data: {"id":"c_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
				fmt.Fprint(w, `data: {"id":"c_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`+"\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c_1", "object": "chat.completion", "model": body["model"],
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
			})
		default:
			w.WriteHeader(404)
		}
	}))
}

// newConvertedTestProxy 构造转换路径测试代理：模板支持 tplFormats（组内全部
// 账号同模板），KeyMeta 携带组级 protocol_convert 方向集合（空 = off）。
func newConvertedTestProxy(t *testing.T, upstream string, tplFormats []domain.RequestFormat, pcs []domain.ProtocolConvert) *Proxy {
	t.Helper()
	return newConvertedTestProxyLogs(t, upstream, tplFormats, pcs, noopLogStore{}, 30*time.Second)
}

// newConvertedTestProxyLogs 同 newConvertedTestProxy，但允许注入 LogInserter
// （用量断言用捕获实现）与上游流超时（中止路径用例缩短触发）。模板为全模型
// 账号（无模型空间）：转换路径测试覆盖解析/转换/失败语义，不测模型白名单
// 路由——无 model 字段请求在 plan-only 契约下 fail-closed 404（无 canonical
// 身份即无派生，见 scheduler buildRoutes 与 reservePlanAttempt）。
func newConvertedTestProxyLogs(t *testing.T, upstream string, tplFormats []domain.RequestFormat, pcs []domain.ProtocolConvert, logs usage.LogInserter, streamTimeout time.Duration) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: tplFormats,
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, MaxConcurrency: 4,
	}}}
	return newConvertedTestProxyAccsLogs(t, accs, pcs, logs, streamTimeout)
}

// newConvertedTestProxyAccs 同 newConvertedTestProxyLogs，但账号直接注入
// （自定义模板/状态/并发上限——用户场景与全忙变体需多模板组）。
func newConvertedTestProxyAccs(t *testing.T, accs map[int64][]*domain.Account, pcs []domain.ProtocolConvert) *Proxy {
	t.Helper()
	return newConvertedTestProxyAccsLogs(t, accs, pcs, noopLogStore{}, 30*time.Second)
}

// newConvertedTestProxyAccsLogs 构造转换路径测试代理（共享内核）：账号按组
// 注入（noopLoader 直供调度器快照），KeyMeta 携带组级 protocol_convert 方向
// 集合（空 = off）。
func newConvertedTestProxyAccsLogs(t *testing.T, accs map[int64][]*domain.Account, pcs []domain.ProtocolConvert, logs usage.LogInserter, streamTimeout time.Duration) *Proxy {
	t.Helper()
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: streamTimeout,
		UsageCapture:          true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, re, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour,
	}, logs, nil)
	key := activeKey(1, 1, 10)
	key.ProtocolConverts = pcs
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": key,
	}}, noopUserLoader{}, nil, true)
	require.NoError(t, auth.Reload(context.Background())) // 构造不再自载——测试显式首刷（快照注册表单一入口）
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, nil)
}

// TestConvertedChatToRespStreaming 客户端 chat 流式 → 上游 resp 流 →
// 客户端收到 chat chunk 流（[DONE] 收尾）。
func TestConvertedChatToRespStreaming(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/responses", path, "转换后走模板 resp 协议路由")
	_, hasMessages := body["messages"]
	require.False(t, hasMessages, "上游请求体为 resp 形态（无 messages）")
	require.NotNil(t, body["input"], "messages → input")
	require.Equal(t, true, body["stream"], "stream 标志转换透传")

	got := rec.Body.String()
	require.Contains(t, got, `"object":"chat.completion.chunk"`, "客户端收到 chat chunk 流")
	require.Contains(t, got, `"delta":{"content":"hi"}`, "文本 delta 映射")
	require.Contains(t, got, `"usage":{"completion_tokens":5,"prompt_tokens":3,"total_tokens":8}`, "收尾 chunk 内联用量")
	require.Contains(t, got, "data: [DONE]")
	require.NotContains(t, got, "response.completed", "上游事件不外泄")
}

// TestConvertedChatToRespNonStreaming 客户端 chat 非流式 → 上游 resp JSON →
// 客户端收到 chat completion JSON。
func TestConvertedChatToRespNonStreaming(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code)
	path, _, _ := up.last(t)
	require.Equal(t, "/v1/responses", path)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "chat.completion", out["object"])
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "hi", msg["content"])
	require.Equal(t, "stop", choices[0].(map[string]any)["finish_reason"])
	usage := out["usage"].(map[string]any)
	require.Equal(t, float64(3), usage["prompt_tokens"])
	require.Equal(t, float64(5), usage["completion_tokens"])
}

// TestConvertedMessToResp 客户端 anthropic messages 流式 → 上游 resp 流 →
// 客户端收到 anthropic message 流（message_start/message_stop）。
func TestConvertedMessToResp(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, []domain.ProtocolConvert{domain.ProtocolConvertMessToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/responses", path)
	require.NotNil(t, body["input"], "messages → input")
	require.Equal(t, float64(100), body["max_output_tokens"], "max_tokens → max_output_tokens")

	got := rec.Body.String()
	require.Contains(t, got, `event: message_start`, "客户端收到 anthropic message 流")
	require.Contains(t, got, `"delta":{"text":"hi","type":"text_delta"}`)
	require.Contains(t, got, `event: message_delta`)
	require.Contains(t, got, `event: message_stop`)
}

// TestConvertedRespToMess 客户端 resp 流式 → 上游 anthropic 流 →
// 客户端收到 resp 事件流（response.created/response.completed）。
func TestConvertedRespToMess(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatAnthropic}, []domain.ProtocolConvert{domain.ProtocolConvertRespToMess})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi","max_output_tokens":100,"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/messages", path)
	require.NotNil(t, body["messages"], "input → messages")
	require.Equal(t, float64(100), body["max_tokens"], "max_output_tokens → max_tokens")

	got := rec.Body.String()
	require.Contains(t, got, `event: response.created`, "客户端收到 resp 事件流")
	require.Contains(t, got, `"type":"response.output_text.delta"`, "文本 delta 映射")
	require.Contains(t, got, `event: response.completed`)
	require.Contains(t, got, `"status":"completed"`)
}

// TestConvertedChatToMess 客户端 chat 流式 → 上游 anthropic 流 →
// 客户端收到 chat chunk 流。
func TestConvertedChatToMess(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatAnthropic}, []domain.ProtocolConvert{domain.ProtocolConvertChatToMess})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/messages", path)
	require.NotNil(t, body["messages"])
	require.Equal(t, true, body["stream"])

	got := rec.Body.String()
	require.Contains(t, got, `"object":"chat.completion.chunk"`)
	require.Contains(t, got, `"delta":{"content":"hi"}`)
	require.Contains(t, got, `"finish_reason":"stop"`)
	require.Contains(t, got, `"usage":{"completion_tokens":20,"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":3},"total_tokens":30}`, "input 来自 message_start + output 来自 message_delta")
	require.Contains(t, got, "data: [DONE]")
}

// TestConvertedChatToRespStreamingDataOnly P3：上游 resp 流缺 event: 名（只发
// data: 行，同仓库 fakeupstream /v1/responses）→ 转换路径不得整帧丢弃——
// 客户端仍收到 chat chunk 流（修复前 200 + 空流，Content-Length 0）。
func TestConvertedChatToRespStreamingDataOnly(t *testing.T) {
	up := &capturedUpstream{dataOnly: true}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code)
	got := rec.Body.String()
	require.NotEmpty(t, got, "缺名帧不得静默全丢（P3）")
	require.Contains(t, got, `"delta":{"content":"hi"}`, "缺名 delta 帧按 data.type 推断 → content chunk")
	require.Contains(t, got, `"usage":{"completion_tokens":5,"prompt_tokens":3,"total_tokens":8}`, "缺名 completed 帧推断 → 收尾 chunk 内联用量")
	require.Contains(t, got, "data: [DONE]", "completed 推断 → [DONE] 收尾")
	require.NotContains(t, got, "response.completed", "上游事件不外泄")
}

// TestConvertedDirectForwardZeroConversion 补差语义：组内模板已支持客户端协议
// → 直接转发零转换（上游收到 chat 形态请求体，不经过转换器）。
func TestConvertedDirectForwardZeroConversion(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL,
		[]domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses},
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/chat/completions", path, "模板已支持 chat → 直连零转换")
	require.NotNil(t, body["messages"], "上游收到 chat 形态请求体")
	_, hasInput := body["input"]
	require.False(t, hasInput, "无 input 字段（未转换）")
}

// TestConvertedOff 默认 off：客户端协议无路由 → 404（不转换，与现状一致）。
func TestConvertedOff(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "off：无 chat 路由 → 404（现状语义）")
}

// TestConvertedDirectionMismatch 组配置转换方向与请求格式不匹配 → 不转换
// （resp 请求不受 chat_to_resp 配置影响，无路由 404）。
func TestConvertedDirectionMismatch(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "chat_to_resp 不影响 resp 请求（方向不匹配不转换）")
}

// TestConvertedMultiDirection 多值配置：chat_to_resp + mess_to_resp 并存——
// chat 请求走 chat_to_resp、anthropic 请求走 mess_to_resp（按客户端格式命中，
// 互不干扰）。
func TestConvertedMultiDirection(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses},
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp})

	// chat 请求 → 上游 resp（chat_to_resp 命中）
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/responses", path, "chat 请求按 chat_to_resp 命中")
	require.NotNil(t, body["input"], "上游请求体为 resp 形态（messages → input）")

	// anthropic 请求 → 上游 resp（mess_to_resp 命中）
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	req2.Header.Set("Authorization", "Bearer ck-1")
	rec2 := httptest.NewRecorder()
	p.HandleAnthropic(rec2, req2)
	require.Equal(t, 200, rec2.Code)
	path2, body2, _ := up.last(t)
	require.Equal(t, "/v1/responses", path2, "anthropic 请求按 mess_to_resp 命中")
	require.NotNil(t, body2["input"])

	// resp 请求：无匹配方向 + 模板已支持 resp → 直连零转换（补差语义，不受
	// chat/mess 方向配置影响）
	req3 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi"}`))
	req3.Header.Set("Authorization", "Bearer ck-1")
	rec3 := httptest.NewRecorder()
	p.HandleResponses(rec3, req3)
	require.Equal(t, 200, rec3.Code)
	path3, body3, _ := up.last(t)
	require.Equal(t, "/v1/responses", path3, "resp 请求直连零转换")
	require.Equal(t, "hi", body3["input"], "上游收到 resp 形态请求体（未转换）")
}

// TestConvertedUnconvertibleBodyFailsClosedNoSlot 顶层数组 JSON 合法
// （json.Valid 通过）但不可转换——且不含可提取 model：plan-only 契约下选号
// 先失败关闭（404，无 canonical 身份即无派生），转换面不再持有目标并发槽。
func TestConvertedUnconvertibleBodyFailsClosedNoSlot(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`[1,2,3]`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "无 model 身份 → fail-closed 404（不派生、不转换）")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "fail-closed 路径零并发槽占用")
}

// TestConvertedChatToMessStreamingLogTotalTokens 转换流（chat→anthropic）流式
// 成功路径 tt 断言（spec 2026-08-16）：message_start（it/cr）+ message_delta
// （ot）→ usage_logs.TotalTokens = it + ot（修复前恒 0 → 额度恒不扣）。
func TestConvertedChatToMessStreamingLogTotalTokens(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	store := &captureLogStore{}
	p := newConvertedTestProxyLogs(t, srv.URL, []domain.RequestFormat{domain.FormatAnthropic}, []domain.ProtocolConvert{domain.ProtocolConvertChatToMess}, store, 30*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, int64(10), lg.InputTokens, "it 来自 message_start")
	require.Equal(t, int64(20), lg.OutputTokens, "ot 来自 message_delta")
	require.Equal(t, int64(30), lg.TotalTokens, "流式成功 tt = it + ot（native 同式）")
	require.Equal(t, int64(3), lg.CacheReadTokens, "cr 来自 message_start")
}

// TestConvertedChatToMessStreamingAbortNoDelta 转换流缺 message_delta 中止路径
// （message_start 后上游停滞 → UpstreamStreamTimeout → recordStreamAbort）：
// TotalTokens = it（native 同式——不是 0，delta 处赋值会欠扣残留）。
func TestConvertedChatToMessStreamingAbortNoDelta(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprint(w, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":3}}}`+"\n\n")
		fl.Flush()
		<-r.Context().Done() // 首帧后停滞 → 超时中止（缺 message_delta）
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newConvertedTestProxyLogs(t, up.URL, []domain.RequestFormat{domain.FormatAnthropic}, []domain.ProtocolConvert{domain.ProtocolConvertChatToMess}, store, 100*time.Millisecond)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code, "流已开始 → 200 已写出")

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrAbort, lg.ErrorType, "停滞超时 → recordStreamAbort")
	require.Equal(t, int64(10), lg.InputTokens, "中止前已收到的 message_start 不丢")
	require.Zero(t, lg.OutputTokens, "缺 message_delta → ot 0")
	require.Equal(t, int64(10), lg.TotalTokens, "缺 delta 中止 → tt = it（native 同式，非 0）")
}

// TestConvertedChatToMessNonStreamingLogTotalTokens 转换非流式（chat→anthropic）
// tt 验证：anthropicUsageFromResponse 自带 tt = it + ot（改动范围外，回归验证）。
func TestConvertedChatToMessNonStreamingLogTotalTokens(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	store := &captureLogStore{}
	p := newConvertedTestProxyLogs(t, srv.URL, []domain.RequestFormat{domain.FormatAnthropic}, []domain.ProtocolConvert{domain.ProtocolConvertChatToMess}, store, 30*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, int64(3), lg.InputTokens)
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(8), lg.TotalTokens, "非流式 tt 由 anthropicUsageFromResponse 自带（it + ot）")
}

// --- 429 回退扩展（A-1：ErrNoAvailable 也触发转换）测试 ---

// userScenarioAccs 用户场景组账号（2026-08-18 用户报告）：模板 A 全协议
// full-model（无模型空间）+ 模板 B 仅 openai-responses（models 白名单
// gpt-4o）；fullEnabled 控制模板 A 账号启用位（false = 转换回退场景，
// true = 直连对照组）。
func userScenarioAccs(srvURL string, fullEnabled bool) map[int64][]*domain.Account {
	tplFull := &domain.Template{ID: 1, Name: "full-t", BaseURL: srvURL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses}}
	tplResp := &domain.Template{ID: 2, Name: "resp-t", BaseURL: srvURL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	return map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplFull, UpstreamKey: "sk-upstream", Enabled: fullEnabled, LifecycleRevision: 1, MaxConcurrency: 4},
		{ID: 2, TemplateID: 2, Template: tplResp, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 4},
	}}
}

// TestConvertedUserScenarioDisabledFullModel 用户报告场景（2026-08-18）规格化：
// 模板 A 全协议 full-model 账号 disabled（无模型空间 → 任意模型都建路由，但
// pickFrom 跳过 disabled → ErrNoAvailable）+ 模板 B resp 白名单 active +
// chat_to_resp → 修复前 429 "no available account" 上游零命中；修复后 200
// 转换生效（上游命中 /v1/responses）。
func TestConvertedUserScenarioDisabledFullModel(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxyAccs(t, userScenarioAccs(srv.URL, false),
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "全协议账号 disabled → 转换回退到 resp 白名单账号（修复前 429）")
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/responses", path, "上游命中转换目标模板协议路由")
	require.NotNil(t, body["input"], "上游收到 resp 形态请求体（messages → input）")
}

// TestConvertedUserScenarioFullModelActive 用户场景对照组：同组全协议
// full-model 账号 active → 直连 chat 零转换（客户端协议直连优先，补差语义
// 不变）。
func TestConvertedUserScenarioFullModelActive(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxyAccs(t, userScenarioAccs(srv.URL, true),
		[]domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/chat/completions", path, "账号可用 → 直连零转换")
	require.NotNil(t, body["messages"], "上游收到 chat 形态请求体（未转换）")
}

// TestConvertedChatBusyFallback 全忙变体（429 语义扩展边界）：客户端路由存在
// 但账号并发满（非 disabled）→ ErrNoAvailable → 同样转换回退。先经
// sched.Select 直接占满 chat 账号唯一并发槽再发请求；断言转换目标账号槽位
// 随请求结束释放（复用 TestConvertedUnconvertibleBodyFailsClosedNoSlot 的
// Runtime 断言模式）。
func TestConvertedChatBusyFallback(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	tplChat := &domain.Template{ID: 1, Name: "chat-t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}
	tplResp := &domain.Template{ID: 2, Name: "resp-t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	accs := map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplChat, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 1},
		{ID: 2, TemplateID: 2, Template: tplResp, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 4},
	}}
	p := newConvertedTestProxyAccs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	// 占满 chat 账号唯一并发槽（直接经调度器抢占——并发满而非 disabled）
	sel, err := p.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	defer p.sched.Release(1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, 200, rec.Code, "chat 账号并发满 → 转换回退")
	path, _, _ := up.last(t)
	require.Equal(t, "/v1/responses", path, "上游命中转换目标模板协议路由")

	// 槽释放断言：转换目标槽随请求结束归零（无泄漏）；chat 槽仍由测试持有
	ri, ok := p.sched.Runtime(2)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "转换目标账号槽位已释放")
	ri, ok = p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, int64(1), ri.Concurrency, "chat 槽仍由测试持有（未误释放）")
}

// TestConvertedTargetAlsoBusy429 目标也全忙：客户端 429（ErrNoAvailable）→
// 转换目标 Select ErrNoAvailable → 响应 429 "no available account" +
// Retry-After: 1 原样（错误分流与 P-1 目标 404 成对覆盖）。
func TestConvertedTargetAlsoBusy429(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	tplChat := &domain.Template{ID: 1, Name: "chat-t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}
	tplResp := &domain.Template{ID: 2, Name: "resp-t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	accs := map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplChat, UpstreamKey: "sk-upstream", Enabled: false, MaxConcurrency: 4},
		{ID: 2, TemplateID: 2, Template: tplResp, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 1},
	}}
	p := newConvertedTestProxyAccs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	// 占满 resp 账号唯一并发槽（目标全忙）
	sel, err := p.sched.Select(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID)
	defer p.sched.Release(2)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code, "目标也全忙 → 429 原样")
	require.Equal(t, "1", rec.Header().Get("Retry-After"), "Retry-After: 1 保留")
	require.Contains(t, rec.Body.String(), "no available account")
}

// TestConvertedTargetFormatUnavailable404 P-1 分流钉死（行为漂移声明）：客户端
// 429（ErrNoAvailable）进入转换分支后目标 Select ErrFormatUnavailable（组配了
// chat_to_resp 但组内无 resp 模板——配置错误）→ 404 "no account supports this
// request format" 且无 Retry-After（修复前该场景是 429 + Retry-After——404 是
// 配置错误的准确信号，且转换分支仅在方向已配置时进入）。
func TestConvertedTargetFormatUnavailable404(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	tplChat := &domain.Template{ID: 1, Name: "chat-t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tplChat, UpstreamKey: "sk-upstream",
		Enabled: false, MaxConcurrency: 4,
	}}}
	p := newConvertedTestProxyAccs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, "转换方向指向不存在的格式 → 404（配置错误信号）")
	require.Contains(t, rec.Body.String(), "no account supports this request format")
	require.Empty(t, rec.Header().Get("Retry-After"), "404 无 Retry-After（漂移声明）")
}

// TestConvertedGroupNotFoundNoConvert 组不存在 → ErrGroupNotFound 不转换 →
// 404 "group not found"（即使配置了转换方向）。
func TestConvertedGroupNotFoundNoConvert(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxyAccs(t, map[int64][]*domain.Account{}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "group not found")
}

// --- 硬续接绑定（resp_to_mess 转换路径）---

// convContUpstream anthropic 假上游：/v1/messages 非流式 JSON / SSE 流均携带
// 给定 message id，命中计数。
func convContUpstream(t *testing.T, msgID string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if b, _ := body["stream"].(bool); b {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n", msgID)
			fl.Flush()
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": "claude",
			"content": []any{map[string]any{"type": "text", "text": "hi"}},
			"usage":   map[string]any{"input_tokens": 3, "output_tokens": 5},
		})
	}))
}

// convAcc anthropic 全模型账号（模板 BaseURL 即上游）。
func convAcc(id int64, tpl *domain.Template) *domain.Account {
	bu := tpl.BaseURL
	return &domain.Account{ID: id, TemplateID: tpl.ID, Template: tpl, BaseURL: &bu, UpstreamKey: "sk-upstream", Enabled: true, LifecycleRevision: 1, MaxConcurrency: 4}
}

func convTpl(id int64, url string) *domain.Template {
	return &domain.Template{ID: id, Name: "ct", BaseURL: url, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic}}
}

// convContProxy resp_to_mess 转换代理 + 续接 store 装配（nil = 未装配）。
func convContProxy(t *testing.T, accs map[int64][]*domain.Account, store *continuation.Store) *Proxy {
	t.Helper()
	p := newConvertedTestProxyAccs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertRespToMess})
	if store != nil {
		p.SetContinuationStore(store)
	}
	return p
}

// convContLookup 转换路径绑定键：客户端协议是 Responses（tag=responses），但
// 计划规范 route class 是转换目标（anthropic）身份。
func convContLookup(t *testing.T, s *continuation.Store, contID string) (*continuation.Binding, bool) {
	t.Helper()
	b, ok, err := s.Lookup(context.Background(), 1, 10, contRouteID(t, domain.FormatAnthropic, "", domain.OpAnthropicMessages), contProtocolREST, contID)
	require.NoError(t, err)
	return b, ok
}

func TestConvertedRespToMessJSONBindsContinuation(t *testing.T) {
	_, s, hook := contFixture(t)
	var hits atomic.Int32
	up := convContUpstream(t, "msg_cv_1", &hits)
	defer up.Close()
	tpl := convTpl(1, up.URL)
	p := convContProxy(t, map[int64][]*domain.Account{10: {convAcc(1, tpl)}}, s)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleResponses(w, req)

	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "msg_cv_1")
	require.EqualValues(t, 1, hook.n.Load(), "one bind command per converted Responses request")
	b, ok := convContLookup(t, s, "msg_cv_1")
	require.True(t, ok, "converted response id must be bound before visibility")
	require.Equal(t, int64(1), b.AccountID)
	require.Equal(t, int64(1), b.Revision)
}

func TestConvertedRespToMessStreamACKBeforeVisible(t *testing.T) {
	_, s, hook := contFixture(t)
	var hits atomic.Int32
	up := convContUpstream(t, "msg_cv_stream", &hits)
	defer up.Close()
	tpl := convTpl(1, up.URL)
	p := convContProxy(t, map[int64][]*domain.Account{10: {convAcc(1, tpl)}}, s)

	gate := make(chan struct{})
	hook.setGate(gate)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		p.HandleResponses(w, req)
	}()

	<-hook.started // 转换后 response.created 帧的 id 绑定 EVALSHA 在途…
	require.Empty(t, w.Body.String(), "映射帧不得先于 Redis ACK 可见")
	close(gate)
	<-done
	require.Contains(t, w.Body.String(), "msg_cv_stream", "ACK 后全流放出")
	require.Contains(t, w.Body.String(), "response.completed")
	_, ok := convContLookup(t, s, "msg_cv_stream")
	require.True(t, ok)
}

func TestConvertedRespToMessJSONBindFailClosed(t *testing.T) {
	mr, s, _ := contFixture(t)
	var hits atomic.Int32
	up := convContUpstream(t, "msg_cv_dead", &hits)
	defer up.Close()
	tpl := convTpl(1, up.URL)
	p := convContProxy(t, map[int64][]*domain.Account{10: {convAcc(1, tpl)}}, s)

	oldTO := contOpTimeout
	contOpTimeout = 300 * time.Millisecond
	t.Cleanup(func() { contOpTimeout = oldTO })
	mr.Close() // Redis 故障：绑定 fail-closed，转换响应弃置

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleResponses(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "msg_cv_dead", "未绑定 id 不得泄漏")
	require.EqualValues(t, 1, hits.Load(), "绑定失败终态不迁移、不重播")
}

func TestConvertedRespToMessStreamBindFailClosed(t *testing.T) {
	mr, s, _ := contFixture(t)
	var hits atomic.Int32
	up := convContUpstream(t, "msg_cv_sdead", &hits)
	defer up.Close()
	tpl := convTpl(1, up.URL)
	p := convContProxy(t, map[int64][]*domain.Account{10: {convAcc(1, tpl)}}, s)

	oldTO := contOpTimeout
	contOpTimeout = 300 * time.Millisecond
	t.Cleanup(func() { contOpTimeout = oldTO })
	mr.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleResponses(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code, "闸门未放 → 归一错误可达")
	require.NotContains(t, w.Body.String(), "msg_cv_sdead", "缓冲帧必须弃置")
	require.NotContains(t, w.Body.String(), "response.created")
}

func TestConvertedNonResponsesClientZeroRedis(t *testing.T) {
	_, s, hook := contFixture(t)
	var hits atomic.Int32
	up := convContUpstream(t, "msg_cv_chat", &hits)
	defer up.Close()
	tpl := convTpl(1, up.URL)
	p := newConvertedTestProxyAccs(t, map[int64][]*domain.Account{10: {convAcc(1, tpl)}}, []domain.ProtocolConvert{domain.ProtocolConvertChatToMess})
	p.SetContinuationStore(s)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p.HandleChat(w, req)

	require.Equal(t, 200, w.Code)
	require.Zero(t, hook.n.Load(), "非 Responses 客户端转换路径零 Redis、零闸门")
}

func TestConvertedRespToMessContinuationPinsBoundAccount(t *testing.T) {
	mr, s1, _ := contFixture(t)
	c2, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c2) })
	s2, err := continuation.New(c2, "cont-test-secret-0123456789")
	require.NoError(t, err)

	var hits1, hits2 atomic.Int32
	up1 := convContUpstream(t, "msg_gen_1", &hits1)
	defer up1.Close()
	up2 := convContUpstream(t, "msg_other_2", &hits2)
	defer up2.Close()
	tpl1, tpl2 := convTpl(1, up1.URL), convTpl(2, up2.URL)
	acc1, acc2 := convAcc(1, tpl1), convAcc(2, tpl2)

	p1 := convContProxy(t, map[int64][]*domain.Account{10: {acc1, acc2}}, s1)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	w := httptest.NewRecorder()
	p1.HandleResponses(w, req)
	require.Equal(t, 200, w.Code)
	require.EqualValues(t, 1, hits1.Load())

	// 续接（另一实例、acc2 优先计划）：必须钉回绑定账号 acc1，绝不迁移。
	p2 := convContProxy(t, map[int64][]*domain.Account{10: {acc2, acc1}}, s2)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"again","previous_response_id":"msg_gen_1"}`))
	req2.Header.Set("Authorization", "Bearer ck-1")
	w2 := httptest.NewRecorder()
	p2.HandleResponses(w2, req2)

	require.Equal(t, 200, w2.Code, "body=%s", w2.Body.String())
	require.EqualValues(t, 0, hits2.Load(), "钉选续接不得触碰未绑定偏好账号")
	require.EqualValues(t, 2, hits1.Load(), "续接必须派发到绑定账号")
}
