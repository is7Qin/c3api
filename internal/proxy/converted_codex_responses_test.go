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

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- 协议转换 → codex 上游（chat/mess → resp）接线测试 ---
// 现象：分组 protocol_convert=[chat_to_resp] + 账号模板 codex（BaseURL==""）时，
// 客户端发 chat → 协议转换路径 convertedCaller 拿空 base 走通用 aiclient →
// Post "/v1/responses": unsupported protocol scheme ""。修复：转换路径命中 codex
// 凭证且目标协议 = resp 时，上游走 codex SDK 适配层，再把 responses 输出反向映射
// 回客户端协议。本文件覆盖 spec §5.1-5.9。

// 转换路径 codex 上游事件 fixture：在既有 t6 事件（created / output_item.done /
// completed）基础上新增 response.output_text.delta——mapRespToChat 仅对 delta /
// created / completed 等事件出帧，纯 output_item.done 会被丢弃（chat_resp.go:
// 704-707），故 delta 是 chat 文本 chunk 的必要面。
const (
	convCodexRespCreated = `{"type":"response.created","response":{"id":"resp_cc","object":"response","status":"in_progress","model":"gpt-5.6"}}`
	convCodexRespDelta   = `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}`
	convCodexRespItem    = `{"id":"msg_1","status":"completed","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}`
	convCodexRespItemEv  = `{"type":"response.output_item.done","item":` + convCodexRespItem + `}`
	convCodexRespUsage   = `{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":4}}`
	convCodexRespDone    = `{"type":"response.completed","response":{"id":"resp_cc","object":"response","status":"completed","usage":` + convCodexRespUsage + `}}`
)

// newConvertedCodexTestProxy 构造「协议转换 → codex 上游」测试代理：模板为
// credType 类型 + openai-responses 格式 + BaseURL=""（codex 端点归 SDK 官方默认）
// + 携带 Ext 的账号（同组 10）；KeyMeta 携带组级 protocol_convert 方向集合 pcs。
// 装配 codex 适配层（official-rewrite transport 重写到 mock 上游）——与
// newTestCodexRespProxy 同款骨架，差异在 pcs 与 SupportedFormats。
func newConvertedCodexTestProxy(t *testing.T, credType credential.Type, accounts map[int64]*domain.AccountExt, upstream string, mapping map[string]domain.ModelMappingEntry, pcs []domain.ProtocolConvert, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "",
		CredentialType:   credType,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
		ModelMapping:     mapping,
	}
	accs := make(map[int64][]*domain.Account, 1)
	for id, ext := range accounts {
		accs[10] = append(accs[10], &domain.Account{
			ID: id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4, Ext: ext,
		})
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture:          true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	key := activeKey(1, 1, 10)
	key.ProtocolConverts = pcs
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": key,
	}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	store := &fakeFailureStore{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: store, Failer: sched, Log: nil})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize: 4096, BatchSize: 100,
		FlushInterval: 20 * time.Millisecond,
	}, logs, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	codex := sdkbridge.NewCodex(failure, newProxyOfficialRewriteTransportWithAssert(t, upstream), sdkbridge.RotationDeps{})
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{Codex: codex})
	return p, store
}

// doChat 向网关发 chat 请求（Bearer ck-1），返回 recorder。
func postChatConv(t *testing.T, p *Proxy, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	return rec
}

// doAnthropic 向网关发 anthropic messages 请求（Bearer ck-1），返回 recorder。
func postAnthropicConv(t *testing.T, p *Proxy, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	return rec
}

// codexOfficialRespPath SDK 官方 codex responses 端点路径（official-rewrite
// transport 断言目标——上游 mock 收到的 r.URL.Path）。
const codexOfficialRespPath = "/backend-api/codex/responses"

// TestConvertedCodexChatToRespStreaming spec §5.1/§5.2：分组 chat_to_resp +
// codex 账号，客户端发 chat 流式 → 200 + 客户端收 chat SSE；上游收官方
// /backend-api/codex/responses POST 且带 SDK 伪装（client_metadata）；恰好一个
// data: [DONE]（计数断言）；delta → chat chunk；usage 正确。
func TestConvertedCodexChatToRespStreaming(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
	}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store)

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	// 先红后绿证据（历史）：修复前此处 502 且 err detail =
	// `Post "/v1/responses": unsupported protocol scheme ""`（空 base 走通用
	// aiclient）。修复后恒 200，本诊断分支不触发（保留以固化回归语义）。
	if rec.Code != http.StatusOK {
		time.Sleep(200 * time.Millisecond)
		store.mu.Lock()
		for _, lg := range store.logs {
			if lg.ErrorMessage != nil {
				t.Logf("先红证据（修复前 err detail）: %s", *lg.ErrorMessage)
			}
		}
		store.mu.Unlock()
	}
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	got := rec.Body.String()

	// 客户端收 chat 格式（非 responses）。
	require.Contains(t, got, `"object":"chat.completion.chunk"`, "客户端收 chat chunk 流")
	require.Contains(t, got, `"delta":{"content":"Hello"}`, "response.output_text.delta → chat chunk")
	require.NotContains(t, got, `"type":"response.`, "上游 responses 事件不外泄")

	// 终止帧：映射分支恰产一个 data: [DONE]（不得重复补发）。
	require.Equal(t, 1, strings.Count(got, "data: [DONE]"), "恰好一个 data: [DONE]")

	// usage = codex 直连口径（completed 帧内联 usage）。
	require.Contains(t, got, `"completion_tokens":20`)
	require.Contains(t, got, `"prompt_tokens":10`)
	require.Contains(t, got, `"total_tokens":30`)

	// 上游断言：官方 codex responses 端点 + SDK 伪装。
	require.Equal(t, 1, upc.callsN())
	require.Equal(t, codexOfficialRespPath, upc.path(0), "请求打到 SDK 官方 codex responses 端点")
	require.Equal(t, "Bearer at-10", upc.auth(0), "codex 凭据经适配层传递")
	cm := gjson.GetBytes(upc.body(0), "client_metadata")
	require.True(t, cm.Get("session_id").Exists(), "SDK 伪装：client_metadata 注入")
	require.True(t, isUUIDv7(cm.Get("session_id").String()), "槽身份 UUIDv7")
}

// TestConvertedCodexChatToRespNonStreaming spec §5.3：客户端 chat 非流式 → 上游
// resp 合成体 → 客户端收 chat completion JSON。
func TestConvertedCodexChatToRespNonStreaming(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
	}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store)

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "chat.completion", out["object"])
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "Hello", msg["content"], "合成体 message 文本 → chat choices.message")
	require.Equal(t, "stop", choices[0].(map[string]any)["finish_reason"])
	usage := out["usage"].(map[string]any)
	require.Equal(t, float64(10), usage["prompt_tokens"])
	require.Equal(t, float64(20), usage["completion_tokens"])
	require.Equal(t, float64(30), usage["total_tokens"])

	require.Equal(t, 1, upc.callsN())
	require.Equal(t, codexOfficialRespPath, upc.path(0))

	// 日志口径（§4.3）：转换路由按客户端格式记录（openai-chat），非 codex 口径。
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.FormatOpenAIChat, store.logs[0].Format, "转换路由日志 format = 客户端格式")
}

// TestConvertedCodexResponseModelBackfill spec §5.5：客户端模型回填——隐式映射
// 下客户端响应 model = 请求模型（非上游模型名），流式与非流式皆然。
func TestConvertedCodexResponseModelBackfill(t *testing.T) {
	implicit := map[string]domain.ModelMappingEntry{
		"gpt-4o": {MappedModel: "gpt-5.6", Mode: domain.ModelMappingModeImplicit},
	}
	t.Run("streaming", func(t *testing.T) {
		up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
			convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
		}})
		defer up.Close()
		p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
			map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
			up.URL, implicit, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})

		rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		got := rec.Body.String()
		require.Contains(t, got, `"model":"gpt-4o"`, "客户端模型 = 请求模型（回填）")
		require.NotContains(t, got, `"model":"gpt-5.6"`, "上游映射模型名不外泄")
		require.Equal(t, "gpt-5.6", gjson.GetBytes(upc.body(0), "model").String(), "上游 wire model = 映射目标")
	})
	t.Run("nonstreaming", func(t *testing.T) {
		up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
			convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
		}})
		defer up.Close()
		p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
			map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
			up.URL, implicit, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})

		rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, "gpt-4o", out["model"], "客户端响应 model = 请求模型（回填）")
		require.Equal(t, "gpt-5.6", gjson.GetBytes(upc.body(0), "model").String(), "上游 wire model = 映射目标")
	})
}

// TestConvertedCodexMessToRespStreaming spec §5.4：mess_to_resp + codex → 客户端
// 收 anthropic messages；流式以 message_stop 收尾且流内无 data: [DONE]。
func TestConvertedCodexMessToRespStreaming(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
	}})
	defer up.Close()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertMessToResp}, &captureLogStore{})

	rec := postAnthropicConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got := rec.Body.String()

	require.Contains(t, got, `event: message_start`, "客户端收 anthropic messages 流")
	require.Contains(t, got, `"delta":{"text":"Hello","type":"text_delta"}`, "resp delta → anthropic content_block_delta")
	require.Contains(t, got, `event: message_delta`)
	require.Contains(t, got, `event: message_stop`, "以 message_stop 收尾")
	require.NotContains(t, got, "[DONE]", "anthropic 流内无 data: [DONE]")

	require.Equal(t, 1, upc.callsN())
	require.Equal(t, codexOfficialRespPath, upc.path(0))
}

// TestConvertedCodexMessToRespNonStreaming spec §5.4：mess_to_resp + codex 非流式
// → 客户端收 anthropic message JSON（content[].text）。
func TestConvertedCodexMessToRespNonStreaming(t *testing.T) {
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
	}})
	defer up.Close()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertMessToResp}, &captureLogStore{})

	rec := postAnthropicConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "message", out["type"])
	content := out["content"].([]any)
	require.Equal(t, "Hello", content[0].(map[string]any)["text"])
}

// TestConvertedCodexAdapterMissing501 spec §5.8：适配层未装配 → 501 显式拒绝
// （与直连一致，绝不触达上游 / 不误走凭据缺失 502）。
func TestConvertedCodexAdapterMissing501(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
	}})
	defer up.Close()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})
	p.codex = nil // 适配层未装配模拟（main 未 SetCodex）

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusNotImplemented, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "adapter not wired")
	require.Equal(t, 0, upc.callsN(), "未装配不触达上游")
}

// TestConvertedCodexExtMissing spec §5.8：codex 账号缺 ext 快照 → 连接级错误转移
// （耗尽 502 + 不触达上游），与直连 errCodexExtMissing 同语义。
func TestConvertedCodexExtMissing(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
	}})
	defer up.Close()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: nil}, // ext 快照缺失（配置损坏）
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, "连接级错误耗尽 → 502，body=%s", rec.Body.String())
	require.Equal(t, 0, upc.callsN(), "配置错误不触达上游")
}

// TestConvertedCodexTurnStateCarryAndClear spec §5.9：转换路径 turn-state 透传优先
// + 同轮续传 + 轮结束清除（与直连 TestCodexResponsesTurnStateCarryAndClear 同语义
// 序列：轮首无头 → 同轮续传 → 轮结束清除）。
func TestConvertedCodexTurnStateCarryAndClear(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t,
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespCallEv, t6RespDone}, turnState: "ts-1"},
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}},
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})

	// 轮首：客户端未带 turn-state；响应签发 ts-1 + 工具调用（轮继续）。
	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// 同轮后续：自动注入 held（ts-1）；响应无工具调用 → 轮结束清除。
	rec = postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// 跨轮：清除后不再回传。
	rec = postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	require.Equal(t, 3, upc.callsN())
	require.Equal(t, "", upc.turnState(0), "轮首请求不带头")
	require.Equal(t, "ts-1", upc.turnState(1), "同轮续传（注入 held）")
	require.Equal(t, "", upc.turnState(2), "轮结束清除——跨轮不回传")

	// 透传优先：客户端自带 x-codex-turn-state → 原值透传不覆盖。
	up2, upc2 := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up2.Close()
	p2, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up2.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})
	rec = postChatConv(t, p2, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-codex-turn-state": "client-ts"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "client-ts", upc2.turnState(0), "客户端自带 → 透传优先不覆盖")
}

// TestConvertedCodexAdvanceIdentityDrivesWindow spec §5.9：转换路径每次成功取到
// usage 都调用 sel.AdvanceIdentity（与直连同一调用点）→ 单槽（MaxConcurrency=1）
// 上连续完成轮数驱动 window_id 递增。断言上游 client_metadata.x-codex-window-id
// 出现 ≥2 个不同值——若未推进身份，window 恒为 ":0"。
func TestConvertedCodexAdvanceIdentityDrivesWindow(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone,
	}})
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 10, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 1, // 单槽
		Ext: codexOAuthExt(10, "at-10", "rt-10"),
	}}}
	key := activeKey(1, 1, 10)
	key.ProtocolConverts = []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}
	p := newConvertedCodexTestProxyAccs(t, accs, key, up.URL)

	// windowSpan ∈ [48,96]（codexsdk/windowSpan），100 次成功推进必跨至少一窗。
	const n = 100
	for i := 0; i < n; i++ {
		rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
		require.Equal(t, http.StatusOK, rec.Code, "第 %d 次请求 body=%s", i, rec.Body.String())
	}
	require.Equal(t, n, upc.callsN())
	seen := map[string]struct{}{}
	for i := 0; i < upc.callsN(); i++ {
		seen[gjson.GetBytes(upc.body(i), "client_metadata").Get("x-codex-window-id").String()] = struct{}{}
	}
	require.GreaterOrEqual(t, len(seen), 2, "AdvanceIdentity 推进 → window_id 递增（未推进则恒为同一值）")
}

// newConvertedCodexTestProxyAccs 单槽/自定义并发变体（AdvanceIdentity 用例用）：
// 直接注入账号集 + KeyMeta，其余装配同 newConvertedCodexTestProxy。
func newConvertedCodexTestProxyAccs(t *testing.T, accs map[int64][]*domain.Account, key domain.KeyMeta, upstream string) *Proxy {
	t.Helper()
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, noopLogStore{}, nil)
	cfg := Config{MaxBodySize: 1 << 20, FailoverAttempts: 2, UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second, UsageCapture: true}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": key}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	store := &fakeFailureStore{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: store, Failer: sched, Log: nil})
	codex := sdkbridge.NewCodex(failure, newProxyOfficialRewriteTransportWithAssert(t, upstream), sdkbridge.RotationDeps{})
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, nil, Deps{Codex: codex})
}
