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
