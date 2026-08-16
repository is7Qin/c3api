// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicstream "github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- 假上游：openai /v1/responses（Responses API） ---
// failMode: "" = 正常；"429" = 非流式 429（测 failover）；"400" = 非流式 400（测透传）；
// "400-stream" = 流式 400（测流式 4xx 透传）。
func fakeResponses(t *testing.T, failMode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if failMode == "429" && !stream {
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if failMode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if failMode == "400-stream" && stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			// "slow-stream"：第二个事件前停 50ms，模拟上游长生成——客户端
			// 可在流中途断开，代理 relay 写失败感知断开（断开记录测试用）。
			// 真实 Responses API 的 SSE 带 event: 行（openai SDK 只按 data JSON
			// 的 type 分发，event 行不影响 SDK 消费）；代理 relay 后原样保留。
			fmt.Fprint(w, `event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
			fl.Flush()
			if failMode == "slow-stream" {
				time.Sleep(50 * time.Millisecond)
			}
			fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "rsp_1", "object": "response", "created_at": 1750000000,
			"status": "completed", "model": body["model"], "output": []any{},
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
		})
	}))
	return srv
}

// --- 假上游：anthropic /v1/messages ---
func fakeAnthropic(t *testing.T, failMode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("x-api-key") != "sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if failMode == "429" && !stream {
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if failMode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if failMode == "400-stream" && stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			// "slow-stream"：事件间隔 50ms，模拟上游长生成——客户端可在流
			// 中途断开，代理 relay 写失败感知断开（客户端断开记录测试用）。
			if failMode == "slow-stream" {
				time.Sleep(50 * time.Millisecond)
			}
			// anthropic SSE 带 event: 行；SDK 按 event 类型分发事件（无 event 行的事件被跳过）。
			// 用量按真实 API 分布：input_tokens 在 message_start.message.usage，
			// output_tokens 在 message_delta.usage（message_delta 不带 input_tokens）。
			fmt.Fprint(w, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"gpt-4o","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`+"\n\n")
			fl.Flush()
			if failMode == "slow-stream" {
				time.Sleep(50 * time.Millisecond)
			}
			fmt.Fprint(w, `event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":20}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"model": body["model"], "content": []any{map[string]any{"type": "text", "text": "hi"}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 5},
		})
	}))
	return srv
}

// captureLogStore 捕获落库的用量明细（用量值断言用）。
type captureLogStore struct {
	mu   sync.Mutex
	logs []*domain.UsageLog
}

func (c *captureLogStore) InsertBatch(ctx context.Context, l []*domain.UsageLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, l...)
	return nil
}

// newTestProxyFormat 构造指定模板格式的测试代理（调度器按模板 FormatSupports 做格式硬过滤）。
func newTestProxyFormat(t *testing.T, upstream string, format domain.RequestFormat) *Proxy {
	t.Helper()
	return newTestProxyFormatLogs(t, upstream, format, noopLogStore{})
}

// newTestProxyFormatLogs 同 newTestProxyFormat，但允许注入 LogInserter（用量断言用捕获实现）。
func newTestProxyFormatLogs(t *testing.T, upstream string, format domain.RequestFormat, logs usage.LogInserter) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{format}, Models: []string{"gpt-4o"},
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
	require.NoError(t, re.Reload(context.Background())) // 空表写种子
	sched := scheduler.New(scheduler.Config{
		DefaultMaxConcurrency: 4, SyncInterval: time.Hour,
	}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"gk-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background())) // 构造不再自载——测试显式首刷（快照注册表单一入口）
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	// errlog worker（分表设计）：错误明细与 usage_logs 共用捕获 store（错误路径
	// 断言经 p.errlog.Close 显式排空）；成功路径不投递。
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errLogStoreFrom(logs), nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW)
}

func TestProxyResponsesNonStreaming(t *testing.T) {
	up := fakeResponses(t, "")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatOpenAIResponses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "rsp_1", resp["id"])
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
}

func TestProxyResponsesStreaming(t *testing.T) {
	up := fakeResponses(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIResponses, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, `"type":"response.output_text.delta"`)
	require.Contains(t, body, `"input_tokens":3`, "usage captured from response.completed event")
	require.Contains(t, body, "data: [DONE]")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")

	// 用量值断言：response.completed 事件的 response.usage 字段（relay observer 提取）。
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	lg := store.logs[0]
	require.Equal(t, int64(3), lg.InputTokens, "input_tokens from response.completed.response.usage")
	require.Equal(t, int64(5), lg.OutputTokens, "output_tokens from response.completed.response.usage")
	require.Equal(t, int64(8), lg.TotalTokens, "total_tokens from response.completed.response.usage")
	require.Equal(t, "gpt-4o", lg.Model, "成功流式：Model = 客户端请求模型")
	require.Equal(t, "", lg.MappedModel, "无映射 → MappedModel 空")
}

// TestProxyResponsesStreamingDataOnly P3：上游 resp 流缺 event: 名（只发
// data: 行，同仓库 fakeupstream /v1/responses）→ 直接 resp 路径不得丢帧、
// 用量提取不得静默缺失——字节原样透传 + Observer 按 data.type 推断
// response.completed 提取 usage。
func TestProxyResponsesStreamingDataOnly(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`+"\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIResponses, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.NotEmpty(t, body, "缺名帧不得静默全丢（P3）")
	require.Contains(t, body, `"type":"response.output_text.delta"`, "字节原样透传")
	require.Contains(t, body, `"type":"response.completed"`)
	require.Contains(t, body, "data: [DONE]")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")

	// 用量断言：缺名 completed 帧按 data.type 推断 → usage 不静默缺失
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	lg := store.logs[0]
	require.Equal(t, int64(3), lg.InputTokens, "input_tokens from data-only response.completed")
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(8), lg.TotalTokens)
}

func TestProxyAnthropicNonStreaming(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	// anthropic SDK 的路径自带 v1 前缀（v1/messages），base 不能含 /v1
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "msg_1", resp["id"])
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")
}

// 流式 + 客户端提前断开：上游已消费请求（成功），仍必须记录一条用量
// （修复：此前 finish(nil) 只释放并发槽不落日志，成功请求丢日志）。
func TestProxyStreamClientAbortStillLogs(t *testing.T) {
	up := fakeResponses(t, "slow-stream")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIResponses, store)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponses))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer gk-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	// 读到第一个 SSE 事件后主动断开（模拟 SDK 迭代完/工具退出关闭连接）
	buf := make([]byte, 512)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()

	// relay 感知断开是异步的：先等记录进队列（Pending>0 或已自动 flush），再 Close 兜底 flush
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.logs) == 1 || p.rec.Pending() > 0
	}, 3*time.Second, 10*time.Millisecond, "relay 必须感知客户端断开并记录用量")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "客户端断开后上游已消费，必须记录一条用量")
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, http.StatusOK, store.logs[0].StatusCode)
	require.Equal(t, "gpt-4o", store.logs[0].Model, "客户端断开：Model = 客户端请求模型")
	require.Equal(t, "", store.logs[0].MappedModel, "无映射 → MappedModel 空")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "客户端断开后并发槽必须释放")
}

// anthropic 同款：流式 + 客户端提前断开也必须记录用量（此前 finish(nil) 同样丢日志）。
func TestProxyAnthropicStreamClientAbortStillLogs(t *testing.T) {
	up := fakeAnthropic(t, "slow-stream")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatAnthropic, store)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleAnthropic))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer gk-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	// 读到第一个 SSE 事件后主动断开
	buf := make([]byte, 512)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()

	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.logs) == 1 || p.rec.Pending() > 0
	}, 3*time.Second, 10*time.Millisecond, "relay 必须感知客户端断开并记录用量")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "客户端断开后上游已消费，必须记录一条用量")
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, http.StatusOK, store.logs[0].StatusCode)
	require.Equal(t, "gpt-4o", store.logs[0].Model, "客户端断开：Model = 客户端请求模型")
	require.Equal(t, "", store.logs[0].MappedModel, "无映射 → MappedModel 空")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "客户端断开后并发槽必须释放")
}

func TestProxyResponsesFailoverExhausted429(t *testing.T) {
	up := fakeResponses(t, "429")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatOpenAIResponses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "失败转移耗尽后并发槽必须释放")
	require.Zero(t, p.rec.Pending(), "耗尽路径失败行不产生明细 pending（err_logs 承载）")
}

// responses 端点 4xx：与 chat 同构——透传上游状态码与原始 body、不转移。
func TestProxyResponsesPassthrough4xx(t *testing.T) {
	up := fakeResponses(t, "400")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatOpenAIResponses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"bad request"`, "4xx 必须透传上游原始 body")
	require.NotContains(t, rec.Body.String(), "upstream rejected request", "透传 body 时不得回退网关文案")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "4xx 透传后并发槽必须释放")
	require.Zero(t, p.rec.Pending(), "4xx 透传不产生明细 pending（err_logs 承载）")
}

// 回归（评审 Minor）：responses 流式 4xx 透传（上游非 200 在 relay 前检出）。
func TestProxyResponsesStreamingPassthrough4xx(t *testing.T) {
	up := fakeResponses(t, "400-stream")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatOpenAIResponses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"bad request"`, "4xx 必须透传上游原始 body")
	require.NotContains(t, rec.Body.String(), "upstream rejected request", "透传 body 时不得回退网关文案")
	// 未 MarkResult：状态保持 active，不冷却
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
	require.Zero(t, ri.Concurrency, "4xx 透传后并发槽必须释放")
	require.Zero(t, p.rec.Pending(), "4xx 透传不产生明细 pending（err_logs 承载）")
}

func TestProxyAnthropicPassthrough4xx(t *testing.T) {
	up := fakeAnthropic(t, "400")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"bad request"`, "4xx 必须透传上游原始 body")
	require.NotContains(t, rec.Body.String(), "upstream rejected request", "透传 body 时不得回退网关文案")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "4xx 透传后并发槽必须释放")
	require.Zero(t, p.rec.Pending(), "4xx 透传不产生明细 pending（err_logs 承载）")
}

// 回归（评审 Minor）：anthropic 流式 4xx 透传（上游非 200 在 relay 前检出）。
func TestProxyAnthropicStreamingPassthrough4xx(t *testing.T) {
	up := fakeAnthropic(t, "400-stream")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"bad request"`, "4xx 必须透传上游原始 body")
	require.NotContains(t, rec.Body.String(), "upstream rejected request", "透传 body 时不得回退网关文案")
	// 未 MarkResult：状态保持 active，不冷却
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Nil(t, ri.CooldownUntil)
	require.Zero(t, ri.Concurrency, "4xx 透传后并发槽必须释放")
	require.Zero(t, p.rec.Pending(), "4xx 透传不产生明细 pending（err_logs 承载）")
}

// 裸根约定的 anthropic 流式回归：base_url 为裸根（模板校验拒绝尾 /v1，
// 见 service.validateBaseURL），anthropic SDK 自带 v1 前缀，流式原始请求
// 必须发到 /v1/messages——不得再拼出 /v1/v1/messages 404（旧约定曾双拼）。
func TestProxyAnthropicStreamingBaseBareRoot(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	// 裸根 base：fakeAnthropic 只认 /v1/messages，若拼出双 v1 会 404
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"type":"content_block_delta"`)
}

func TestProxyAnthropicStreaming(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatAnthropic, store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, `"type":"content_block_delta"`)
	require.Contains(t, body, `"input_tokens":10`, "input_tokens passthrough from message_start event")
	require.Contains(t, body, `"output_tokens":20`, "output_tokens passthrough from message_delta event")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
	require.Equal(t, 1, p.rec.Pending(), "成功路径必须记录一条用量")

	// 用量值断言（评审发现修复）：prompt_tokens 来自 message_start.message.usage，
	// completion_tokens 来自 message_delta.usage，total 为两者之和——此前只累计
	// message_delta 导致 prompt_tokens 恒为 0。
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "must capture exactly one usage log")
	lg := store.logs[0]
	require.Equal(t, int64(10), lg.InputTokens, "input_tokens from message_start.message.usage")
	require.Equal(t, int64(20), lg.OutputTokens, "output_tokens from message_delta.usage")
	require.Equal(t, int64(30), lg.TotalTokens, "total = input + output")
	require.Equal(t, "gpt-4o", lg.Model, "成功流式：Model = 客户端请求模型")
	require.Equal(t, "", lg.MappedModel, "无映射 → MappedModel 空")
}

// 兼容性钉（Task 3）：anthropic 流式转发必须保留上游原始字节——event: 行与
// 用量字段（input_tokens/output_tokens）原样透传。
func TestProxyAnthropicStreamingPreservesEventLines(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, "event: message_start")
	require.Contains(t, body, "event: message_stop")
	require.Contains(t, body, `"input_tokens":10`)
	require.Contains(t, body, `"output_tokens":20`)
}

// parseAnthropicSSE 按 anthropic 协议解析输出：每个 data 块前必须有 event: 行，
// 且 event 类型与 data JSON 的 "type" 字段一致（data-only 事件官方 SDK 静默跳过）。
func parseAnthropicSSE(t *testing.T, body string) []string {
	t.Helper()
	var (
		types []string
		cur   string
	)
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			cur = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" { // 代理收尾标记，非 anthropic 协议事件
				continue
			}
			var ev struct {
				Type string `json:"type"`
			}
			require.NoError(t, json.Unmarshal([]byte(payload), &ev))
			require.Equal(t, ev.Type, cur, "event: 行与 JSON type 不一致: %q", line)
			types = append(types, ev.Type)
			cur = ""
		}
	}
	return types
}

// 回归（评审 Important）：/v1/messages 流式输出必须带 event: <type> 行——
// anthropic 官方 SDK 按 event: 行类型分发，data-only 事件被静默跳过 → 官方客户端拿到空流。
func TestProxyAnthropicStreamingSSEFraming(t *testing.T) {
	up := fakeAnthropic(t, "")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())

	// 1) 文本级解析：event: 行存在且与 JSON type 一致
	want := []string{"message_start", "content_block_delta", "message_delta", "message_stop"}
	require.Equal(t, want, parseAnthropicSSE(t, rec.Body.String()))

	// 2) 官方 SDK 客户端消费代理输出：event: 行缺失时流为空（修复前即空流）
	stream := anthropicstream.NewStream[anthropic.MessageStreamEventUnion](
		anthropicstream.NewDecoder(&http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(rec.Body.String())),
			Request:    httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		}), nil)
	var got []string
	for stream.Next() {
		got = append(got, stream.Current().Type)
	}
	require.NoError(t, stream.Err())
	require.Equal(t, want, got)
}
