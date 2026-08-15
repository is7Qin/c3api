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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
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
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: tplFormats, Models: []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		GroupKeyRPM:           0, UsageCapture: true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour,
	}, noopLogStore{}, nil)
	key := activeKey(1, 1, 10)
	key.ProtocolConverts = pcs
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"gk-1": key,
	}}, noopUserLoader{}, nil)
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/responses", path, "chat 请求按 chat_to_resp 命中")
	require.NotNil(t, body["input"], "上游请求体为 resp 形态（messages → input）")

	// anthropic 请求 → 上游 resp（mess_to_resp 命中）
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	req2.Header.Set("Authorization", "Bearer gk-1")
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
	req3.Header.Set("Authorization", "Bearer gk-1")
	rec3 := httptest.NewRecorder()
	p.HandleResponses(rec3, req3)
	require.Equal(t, 200, rec3.Code)
	path3, body3, _ := up.last(t)
	require.Equal(t, "/v1/responses", path3, "resp 请求直连零转换")
	require.Equal(t, "hi", body3["input"], "上游收到 resp 形态请求体（未转换）")
}

// TestConvertedRequestConvertFailReleasesSlot 请求体转换失败 → 本地 400，且
// 目标选号已占的并发槽必须释放（防槽位泄漏）。
func TestConvertedRequestConvertFailReleasesSlot(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIResponses}, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp})

	// 顶层数组 JSON 合法（json.Valid 通过）但不可转换 → ConvertRequest 报错
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`[1,2,3]`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "转换失败 → 本地 400")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "转换失败路径释放目标选号并发槽（无泄漏）")
}
