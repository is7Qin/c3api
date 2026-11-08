// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// 端到端（进程内）：客户端说 Messages / Responses，组的唯一上游只说 chat。
// 这些用例钉住 spec §4/§5 表里写死的**字面量**（路径、字段名、停因、净额口径），
// 而不是"转换函数自己和自己一致"。

// TestConvertedMessToChatNonStreaming 非流式：mess 客户端 → chat 上游。
func TestConvertedMessToChatNonStreaming(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":64,"system":"sb","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/chat/completions", path, "chat 目标必须打 chat 路径")
	// 上游请求形态：max_tokens → max_completion_tokens；tools 嵌套；system 落首条消息。
	require.Nil(t, body["max_tokens"], "chat 上游不得收到 max_tokens")
	require.EqualValues(t, 64, body["max_completion_tokens"])
	msgs := body["messages"].([]any)
	require.Equal(t, "system", msgs[0].(map[string]any)["role"])
	require.Equal(t, "sb", msgs[0].(map[string]any)["content"])
	tools := body["tools"].([]any)
	require.NotNil(t, tools[0].(map[string]any)["function"], "tools 必须嵌套在 function 下")
	require.Equal(t, "function", tools[0].(map[string]any)["type"])

	// 客户端响应形态：Messages 对象。
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "message", out["type"])
	require.Equal(t, "end_turn", out["stop_reason"], "chat stop → Messages end_turn")
	require.Nil(t, out["stop_sequence"])
	content := out["content"].([]any)
	require.Equal(t, "text", content[0].(map[string]any)["type"])
	require.Equal(t, "hi", content[0].(map[string]any)["text"])
	usage := out["usage"].(map[string]any)
	// chat prompt_tokens=3 且无缓存 ⇒ net=3；三项恒写（含 0）。
	require.EqualValues(t, 3, usage["input_tokens"])
	require.EqualValues(t, 0, usage["cache_read_input_tokens"])
	require.EqualValues(t, 0, usage["cache_creation_input_tokens"])
	require.EqualValues(t, 5, usage["output_tokens"])
}

// TestConvertedMessToChatStreaming 流式：客户端收到 Messages 帧序列。
func TestConvertedMessToChatStreaming(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code)
	_, body, _ := up.last(t)
	so := body["stream_options"].(map[string]any)
	require.Equal(t, true, so["include_usage"], "必须强制注入 include_usage，否则客户端终态用量恒 0")

	got := rec.Body.String()
	require.Contains(t, got, `event: message_start`)
	require.Contains(t, got, `"delta":{"text":"hi","type":"text_delta"}`)
	require.Contains(t, got, `event: message_delta`)
	require.Contains(t, got, `"stop_reason":"end_turn"`)
	require.Contains(t, got, `event: message_stop`)
	require.NotContains(t, got, "chat.completion.chunk", "chat 原始帧不得外泄")
}

// TestConvertedRespToChatNonStreaming 非流式：resp 客户端 → chat 上游。
func TestConvertedRespToChatNonStreaming(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertRespToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"max_output_tokens":64,"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code)
	path, body, _ := up.last(t)
	require.Equal(t, "/v1/chat/completions", path)
	require.EqualValues(t, 64, body["max_completion_tokens"])
	require.Nil(t, body["max_output_tokens"])
	tools := body["tools"].([]any)
	require.NotNil(t, tools[0].(map[string]any)["function"], "resp 的扁平工具必须重排成嵌套")

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "response", out["object"])
	require.Equal(t, "completed", out["status"])
	output := out["output"].([]any)
	item := output[0].(map[string]any)
	require.Equal(t, "message", item["type"])
	part := item["content"].([]any)[0].(map[string]any)
	require.Equal(t, "output_text", part["type"])
	require.Equal(t, "hi", part["text"])
	usage := out["usage"].(map[string]any)
	require.EqualValues(t, 3, usage["input_tokens"])
	require.EqualValues(t, 8, usage["total_tokens"], "total = input + output")
	details := usage["input_tokens_details"].(map[string]any)
	require.Contains(t, details, "cached_tokens")
	require.Contains(t, details, "cache_write_tokens", "两键恒写（官方必填语义）")
}

// TestConvertedRespToChatStreaming 流式：客户端收到 Responses 事件序列。
func TestConvertedRespToChatStreaming(t *testing.T) {
	up := &capturedUpstream{}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertRespToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"m","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code)
	got := rec.Body.String()
	require.Contains(t, got, `event: response.created`)
	require.Contains(t, got, `event: response.output_text.delta`)
	require.Contains(t, got, `event: response.output_item.done`)
	require.Contains(t, got, `event: response.completed`)
	require.Contains(t, got, `"status":"completed"`)
	require.NotContains(t, got, "chat.completion.chunk")
}

// TestConvertedChatTargetBillingNonZero spec §8(i)：计费非零且不做二次扣减。
// chat 上游给 gross=100 / cached=40 / write=10 ⇒ 记账口径 it=50, cr=40, cc=10。
func TestConvertedChatTargetBillingNonZero(t *testing.T) {
	up := &capturedUpstream{usage: map[string]any{
		"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120,
		"prompt_tokens_details": map[string]any{"cached_tokens": 40, "cache_write_tokens": 10},
	}}
	srv := up.srv(t)
	defer srv.Close()
	logs := &captureLogStore{}
	p := newConvertedTestProxyLogs(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat}, logs, 30*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code)

	// 客户端看到的用量与账务口径必须同源（禁二次扣减：100→50→0）。
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	usage := out["usage"].(map[string]any)
	require.EqualValues(t, 50, usage["input_tokens"])
	require.EqualValues(t, 40, usage["cache_read_input_tokens"])
	require.EqualValues(t, 10, usage["cache_creation_input_tokens"])

	require.NoError(t, p.rec.Close(context.Background()))
	logs.mu.Lock()
	defer logs.mu.Unlock()
	require.Len(t, logs.logs, 1, "用量必须落账")
	l := logs.logs[0]
	require.EqualValues(t, 50, l.InputTokens, "记账用净额（不得再用 gross 二次扣减）")
	require.EqualValues(t, 40, l.CacheReadTokens)
	require.EqualValues(t, 10, l.CacheCreationTokens)
	require.EqualValues(t, 20, l.OutputTokens)
}

// --- C7：spec §8(f) 写死的字面量（parameters.type / 多轮 tool_call_id / 停因） ---

// sseFrames 把 SSE 响应体切成每帧的 data 载荷。只收 `data: ` 行（chat 上游是
// data-only 形态，客户端侧带 event: 名），并强制每帧是合法 JSON。
func sseFrames(t *testing.T, body string) []gjson.Result {
	t.Helper()
	var out []gjson.Result
	for _, blk := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(blk, "\n") {
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok || payload == "[DONE]" {
				continue
			}
			require.True(t, gjson.Valid(payload), "SSE 帧必须是合法 JSON：%s", payload)
			out = append(out, gjson.Parse(payload))
		}
	}
	return out
}

// frameOfType 取第一个 type 字段等于 want 的帧。
func frameOfType(t *testing.T, frames []gjson.Result, want string) gjson.Result {
	t.Helper()
	for _, f := range frames {
		if f.Get("type").Str == want {
			return f
		}
	}
	require.FailNowf(t, "缺帧", "帧序列里没有 type=%q 的帧", want)
	return gjson.Result{}
}

// TestConvertedMessToChatMultiTurnToolPairing 多轮工具配对：assistant.tool_calls
// 与紧随其后的 role:"tool" 必须同一个 id，且停因映射成 tool_use。
func TestConvertedMessToChatMultiTurnToolPairing(t *testing.T) {
	up := &capturedUpstream{chatChoices: []any{map[string]any{
		"index": 0,
		"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_2", "type": "function",
				"function": map[string]any{"name": "f", "arguments": `{"a":1}`}},
		}},
		"finish_reason": "tool_calls",
	}}}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":64,"messages":[`+
			`{"role":"user","content":"hi"},`+
			`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"f","input":{"a":1}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code)

	// 上游侧：最后一个 tool 消息必须紧邻它的 assistant 且 id 相同。
	_, body, _ := up.last(t)
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	require.Equal(t, "tool", last["role"])
	require.Equal(t, "call_1", last["tool_call_id"], "messages[last].tool_call_id 必须是 assistant 声明的那个 id")
	asst := msgs[len(msgs)-2].(map[string]any)
	require.Equal(t, "assistant", asst["role"])
	tcs := asst["tool_calls"].([]any)
	require.Equal(t, "call_1", tcs[0].(map[string]any)["id"])

	// 客户端侧：finish_reason=tool_calls → stop_reason=tool_use，且带 tool_use 块。
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "tool_use", out["stop_reason"])
	var found bool
	for _, c := range out["content"].([]any) {
		blk := c.(map[string]any)
		if blk["type"] != "tool_use" {
			continue
		}
		found = true
		require.Equal(t, "call_2", blk["id"])
		require.Equal(t, "f", blk["name"])
		require.Equal(t, map[string]any{"a": float64(1)}, blk["input"])
	}
	require.True(t, found, "content 里必须有 tool_use 块")
}

// TestConvertedRespToChatMultiTurnToolPairing resp 侧同一不变量。
func TestConvertedRespToChatMultiTurnToolPairing(t *testing.T) {
	up := &capturedUpstream{chatChoices: []any{map[string]any{
		"index": 0,
		"message": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_2", "type": "function",
				"function": map[string]any{"name": "f", "arguments": `{"a":1}`}},
		}},
		"finish_reason": "tool_calls",
	}}}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertRespToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"m","max_output_tokens":64,"input":[`+
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},`+
			`{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"a\":1}"},`+
			`{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code)

	_, body, _ := up.last(t)
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	require.Equal(t, "tool", last["role"])
	require.Equal(t, "call_1", last["tool_call_id"])

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	var fc map[string]any
	for _, it := range out["output"].([]any) {
		if m := it.(map[string]any); m["type"] == "function_call" {
			fc = m
		}
	}
	require.NotNil(t, fc, "output[] 里必须有 function_call item")
	require.Equal(t, "call_2", fc["call_id"])
	require.Equal(t, "f", fc["name"])
	require.Equal(t, `{"a":1}`, fc["arguments"], "arguments 是字符串且原样")
}

// TestConvertedToolSchemaParametersAreObject spec §8(f)：两层工具表的
// `function.parameters.type` 必须落在 "object"（扁平→嵌套重排不能丢 schema 本体）。
func TestConvertedToolSchemaParametersAreObject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pc      domain.ProtocolConvert
		handler func(*Proxy, http.ResponseWriter, *http.Request)
		body    string
	}{
		{
			name:    "mess_to_chat",
			pc:      domain.ProtocolConvertMessToChat,
			handler: (*Proxy).HandleAnthropic,
			body: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}]}`,
		},
		{
			name:    "resp_to_chat",
			pc:      domain.ProtocolConvertRespToChat,
			handler: (*Proxy).HandleResponses,
			body: `{"model":"m","max_output_tokens":8,"input":"hi",` +
				`"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &capturedUpstream{}
			srv := up.srv(t)
			defer srv.Close()
			p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
				[]domain.ProtocolConvert{tc.pc})
			req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			tc.handler(p, rec, req)
			require.Equal(t, 200, rec.Code)

			_, body, _ := up.last(t)
			tools, ok := body["tools"].([]any)
			require.True(t, ok, "上游必须收到 tools[]")
			fn := tools[0].(map[string]any)["function"].(map[string]any)
			require.Equal(t, "f", fn["name"])
			params := fn["parameters"].(map[string]any)
			require.Equal(t, "object", params["type"], "function.parameters.type 必须是 object")
			require.Contains(t, params["properties"].(map[string]any), "a", "schema 本体不得丢")
		})
	}
}

// --- C8：spec §8(i) 流式账务（与非流式同源，禁二次扣减） ---

func TestConvertedChatTargetBillingStreaming(t *testing.T) {
	up := &capturedUpstream{usage: map[string]any{
		"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120,
		"prompt_tokens_details": map[string]any{"cached_tokens": 40, "cache_write_tokens": 10},
	}}
	srv := up.srv(t)
	defer srv.Close()
	logs := &captureLogStore{}
	p := newConvertedTestProxyLogs(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat}, logs, 30*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code)

	// 客户端侧的用量与账务口径必须同源（100 净额 → 50，不得二次扣减）。
	// 注意：message_start 的 usage 是占位 0（chat 的用量只在末帧出现，见
	// chat_stream.go 的 chatStart 注释），真实口径在终止的 message_delta 帧上。
	frames := sseFrames(t, rec.Body.String())
	delta := frameOfType(t, frames, "message_delta")
	require.EqualValues(t, 50, delta.Get("usage.input_tokens").Int())
	require.EqualValues(t, 40, delta.Get("usage.cache_read_input_tokens").Int())
	require.EqualValues(t, 10, delta.Get("usage.cache_creation_input_tokens").Int())
	require.EqualValues(t, 20, delta.Get("usage.output_tokens").Int())
	require.NotContains(t, rec.Body.String(), `"input_tokens":100`, "gross 数不得外泄（净额才是对外口径）")

	require.NoError(t, p.rec.Close(context.Background()))
	logs.mu.Lock()
	defer logs.mu.Unlock()
	require.Len(t, logs.logs, 1, "流式用量必须落账")
	l := logs.logs[0]
	require.EqualValues(t, 50, l.InputTokens)
	require.EqualValues(t, 40, l.CacheReadTokens)
	require.EqualValues(t, 10, l.CacheCreationTokens)
	require.EqualValues(t, 20, l.OutputTokens)
}

// TestConvertedRespToChatStreamEOFWithoutDone 上游**不发 [DONE]** 的正常 EOF +
// **隐式模型映射**（客户端 model ≠ 上游 model）：收尾必须走 sserelay 的 Terminal
// （恰好一次），且收尾帧里的 model 也要改写成客户端模型（spec §5.7 + §13 I1）。
//
// 为什么要映射：`Selection.ClientResponseModel` 只在隐式映射模式下返回客户端模型；
// 没有映射时上游与客户端同名，改写与不改写无法区分。
func TestConvertedRespToChatStreamEOFWithoutDone(t *testing.T) {
	up := &capturedUpstream{
		noDone: true,
		chatStreamFrames: []string{
			`{"id":"c_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","content":"par"},"finish_reason":null}]}`,
			`{"id":"c_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"content":"tial"},"finish_reason":null}]}`,
			`{"id":"c_1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
		},
	}
	srv := up.srv(t)
	defer srv.Close()
	tpl := newImplicitTpl(srv.URL, domain.FormatOpenAIChat, domain.ModelMappingModeImplicit)
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
	}}}
	p := newConvertedTestProxyAccsLogs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertRespToChat},
		noopLogStore{}, 30*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"client-model","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code)

	got := rec.Body.String()
	frames := sseFrames(t, got)
	done := frameOfType(t, frames, "response.completed")
	require.Equal(t, "client-model", done.Get("response.model").Str,
		"EOF 收尾帧的 model 必须改写成客户端模型")
	// 终态 output 必须是**累积后的完整内容**（不是最后一帧的片段），usage 也要齐。
	require.Equal(t, "partial", done.Get("response.output.0.content.0.text").Str,
		"EOF 收尾的 output 必须带上累积文本")
	require.Equal(t, "output_text", done.Get("response.output.0.content.0.type").Str)
	require.EqualValues(t, 3, done.Get("response.usage.input_tokens").Int())
	require.EqualValues(t, 5, done.Get("response.usage.output_tokens").Int())
	require.EqualValues(t, 8, done.Get("response.usage.total_tokens").Int())
	require.Contains(t, got, `"type":"response.created"`, "首帧仍要发 response.created")
	require.NotContains(t, got, "upstream-model", "上游模型名不得外泄（含收尾帧）")
	require.NotContains(t, got, "chat.completion.chunk", "chat 原始帧不得外泄")
	// 收尾恰好一次。
	var n int
	for _, f := range frames {
		if f.Get("type").Str == "response.completed" {
			n++
		}
	}
	require.Equal(t, 1, n, "response.completed 必须恰好一次")
}

// TestConvertedMessToChatStreamEOFWithoutDone 上游**不发 [DONE]** 时的 Mess 目标：
// 收尾必须走 sserelay 的 Terminal（恰好一次 message_stop），且这条 EOF 路径的用量
// 同样要落账（spec §5.7 + §8(b)(i)：不能只有 [DONE] 路径记账）。
func TestConvertedMessToChatStreamEOFWithoutDone(t *testing.T) {
	up := &capturedUpstream{
		noDone: true,
		usage: map[string]any{
			"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120,
			"prompt_tokens_details": map[string]any{"cached_tokens": 40, "cache_write_tokens": 10},
		},
		chatStreamFrames: []string{
			`{"id":"c_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"tail"},"finish_reason":null}]}`,
			`{"id":"c_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`,
		},
	}
	srv := up.srv(t)
	defer srv.Close()
	logs := &captureLogStore{}
	p := newConvertedTestProxyLogs(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat}, logs, 30*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code)

	got := rec.Body.String()
	require.NotContains(t, got, "chat.completion.chunk", "chat 原始帧不得外泄")
	// EOF 收尾恰好一次，且停因按 length → max_tokens。
	require.Equal(t, 1, strings.Count(got, "event: message_stop"), "无 [DONE] 的 EOF 必须由 Terminal 补一次收尾")
	frames := sseFrames(t, got)
	delta := frameOfType(t, frames, "message_delta")
	require.Equal(t, "max_tokens", delta.Get("delta.stop_reason").Str)
	require.EqualValues(t, 50, delta.Get("usage.input_tokens").Int(), "净额口径")
	// 内容块必须闭合（否则客户端停在半截块），文本要送到。
	require.Contains(t, got, `"delta":{"text":"tail","type":"text_delta"}`, "文本增量必须送达")
	require.Equal(t, 1, strings.Count(got, "event: content_block_stop"), "内容块恰好关闭一次")
	require.Less(t,
		strings.Index(got, "event: content_block_stop"),
		strings.Index(got, "event: message_delta"),
		"关块必须早于 message_delta（否则客户端先看到终态再收到内容）")

	require.NoError(t, p.rec.Close(context.Background()))
	logs.mu.Lock()
	defer logs.mu.Unlock()
	require.Len(t, logs.logs, 1, "EOF 路径的用量同样必须落账")
	require.EqualValues(t, 50, logs.logs[0].InputTokens)
	require.EqualValues(t, 40, logs.logs[0].CacheReadTokens)
	require.EqualValues(t, 10, logs.logs[0].CacheCreationTokens)
	require.EqualValues(t, 20, logs.logs[0].OutputTokens)
}

// TestConvertedMessToChatStreamErrorOrdering spec §8(b)：mess 目标的上游 error
// 必须**先关掉已开的内容块再报错**（客户端才不会停在半截块），并且错误帧之后
// 不再有正常完成帧。
func TestConvertedMessToChatStreamErrorOrdering(t *testing.T) {
	up := &capturedUpstream{
		noDone: true,
		chatStreamFrames: []string{
			`{"id":"c_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"half"},"finish_reason":null}]}`,
			`{"error":{"type":"server_error","message":"upstream boom"}}`,
		},
	}
	srv := up.srv(t)
	defer srv.Close()
	p := newConvertedTestProxy(t, srv.URL, []domain.RequestFormat{domain.FormatOpenAIChat},
		[]domain.ProtocolConvert{domain.ProtocolConvertMessToChat})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code)

	got := rec.Body.String()
	stop := strings.Index(got, "event: content_block_stop")
	errAt := strings.Index(got, "event: error")
	require.Positive(t, stop, "错误前必须关闭已开的内容块")
	require.Positive(t, errAt, "错误必须转成 error 帧")
	require.Less(t, stop, errAt, "content_block_stop 必须在 error 之前")
	require.Contains(t, got, "upstream boom", "上游错误文本必须原样带给客户端")
	require.Equal(t, 1, strings.Count(got, "event: error"), "错误帧恰好一次")
	require.NotContains(t, got, `"stop_reason":"end_turn"`, "错误结局不得声明正常完成")
}
