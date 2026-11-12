// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- 流内错误载荷必须变成失败（spec-inband-error-truth r10 §5） ---
//
// 判据面 = 客户端状态码/文案 + usage_logs 与 err_logs 的成员资格 + 账号 health
// 读回 + 上游尝试数。这些都与"错误帧被当成流内容转发、请求按成功记账"直接对立。
//
// 所有用例都走**真 Proxy 入口**（HandleChat/HandleResponses/HandleAnthropic/
// HandleResponsesWS）+ httptest 假上游，不 mock relay 本身。

// inbandOverloadMsg 上游原文（S1/S1b/S2/S2b/S3/S5 共用；err_logs 必须含它）。
const inbandOverloadMsg = "Our servers are currently overloaded. Please try again later."

// inbandErrFrame 上游错误帧 payload（顶层 error 对象 + event: 行）。
const inbandErrFrame = `event: error` + "\n" + `data: {"type":"error","error":{"type":"server_error","message":"` + inbandOverloadMsg + `"}}` + "\n\n"

// chatErrFrame 同形但只有 data 行（chat 车道的真实形态：无 event: 行，靠针 ②③）。
const chatErrFrame = `data: {"error":{"type":"server_error","message":"` + inbandOverloadMsg + `"}}` + "\n\n"

// chatNormalFrame 一帧正常业务 chunk（S1b 用：业务字节已写）。
const chatNormalFrame = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}` + "\n\n"

// inbandSSEUpstream 按脚本逐段写原始 SSE 字节的假上游（写满即 EOF）。
// hits 记录被请求次数（failover 断言面）。
type inbandSSEUpstream struct {
	mu   sync.Mutex
	hits int
}

func (u *inbandSSEUpstream) attempts() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

// newInbandSSEUpstream 构造按脚本回放的 SSE 假上游：路径与鉴权头按车道定制
// （chat/responses = Bearer sk-upstream；anthropic = x-api-key sk-upstream）。
func newInbandSSEUpstream(t *testing.T, path, authHeader, authValue string, chunks ...string) (*httptest.Server, *inbandSSEUpstream) {
	t.Helper()
	u := &inbandSSEUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get(authHeader) != authValue {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		u.mu.Lock()
		u.hits++
		u.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, c := range chunks {
			if _, err := io.WriteString(w, c); err != nil {
				return
			}
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, u
}

// newInbandChatProxy 构造 chat 模板测试代理：允许注入额外账号（failover 面）+
// 真实 health 读回（testRuleSink/testRuntimeHealth 钩子——与 codex 侧同款）。
func newInbandChatProxy(t *testing.T, upstream string, extraAccounts int, logs *captureLogStore) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
	}}}
	primary := []scheduler.CompiledCandidate{{AccountID: 1}}
	for id := int64(2); id <= int64(1+extraAccounts); id++ {
		tplx := &domain.Template{
			ID: id, Name: "t", BaseURL: upstream,
			CredentialType:   credential.TypeAPIKey,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
		}
		accs[10] = append(accs[10], &domain.Account{
			ID: id, TemplateID: id, Template: tplx, UpstreamKey: "sk-upstream",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
		})
		primary = append(primary, scheduler.CompiledCandidate{AccountID: id})
	}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture:          true,
	}
	ruleSink := rule.HealthSink(testHealthSink)
	if testRuleSink != nil {
		ruleSink = testRuleSink
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, ruleSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, noopLoader{accs: accs}, re, testRuntimeHealth, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)
	sched.PublishDecisionForTest(
		scheduler.RouteRefFor(10, string(domain.FormatOpenAIChat), "gpt-4o"),
		&scheduler.RouteDecision{Primary: primary})

	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	clients := aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport}, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errLogStoreFrom(logs), nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{})
}

// postChatStream 发一条 chat 流式请求（Bearer ck-1）。
func postChatStream(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	return rec
}

// postResponsesStream 发一条 responses 流式请求（Bearer ck-1）。
func postResponsesStream(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	return rec
}

// postAnthropicStream 发一条 messages 流式请求（Bearer ck-1）。
func postAnthropicStream(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)
	return rec
}

// drainLogs 排空 usage_logs 与 err_logs 双队列后返回全部落袋行。
func drainLogs(t *testing.T, p *Proxy, store *captureLogStore) []*domain.UsageLog {
	t.Helper()
	require.NoError(t, p.rec.Close(context.Background()), "Recorder 手动 flush")
	if p.errlog != nil {
		require.NoError(t, p.errlog.Close(context.Background()), "errlog 手动 flush")
	}
	return storeRows(store)
}

// inbandTracks 逻辑行计数（usage_logs / err_logs 分表成员资格，按**行**而非按
// 落库写次数）：abort 双轨——同一个 *domain.UsageLog 会被 usage worker 与 errlog
// worker 各插一次，共享 captureLogStore 时出现两次同指针条目，这里按指针去重，
// 避免把"usage_logs 恰 1 行"算成 2。（routeLog 的成员资格语义见 forward.go:441-467。）
func inbandTracks(rows []*domain.UsageLog) logTracks {
	seen := make(map[*domain.UsageLog]struct{}, len(rows))
	var tr logTracks
	for _, l := range rows {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		switch l.ErrorType {
		case domain.ErrNone:
			tr.usage++
		case domain.ErrAbort:
			tr.usage++ // abort 属放行路径（半异常计费）→ usage_logs
			tr.errs++  // 且是错误明细 → err_logs
			tr.abort++
		default:
			tr.errs++
		}
	}
	return tr
}

// --- S1：chat · 未提交（首帧即错误帧） ---

func TestInbandS1ChatUncommittedErrorFrameIs502(t *testing.T) {
	testHealthSink.reset()
	up, u := newInbandSSEUpstream(t, "/v1/chat/completions", "Authorization", "Bearer sk-upstream", chatErrFrame)
	store := &captureLogStore{}
	p := newTestProxyTimeoutLogs(t, up.URL, 1, store)

	rec := postChatStream(t, p)

	require.Equal(t, http.StatusBadGateway, rec.Code, "错误帧未提交 ⇒ 客户端 502；body=%q", rec.Body.String())
	body := rec.Body.Bytes()
	require.Equal(t, "Upstream request failed", gjsonString(body, "error.message"), "规则文案（seed-network）；body=%q", body)
	require.Equal(t, 1, u.attempts(), "单账号：恰一次上游尝试")

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(rows),
		"未提交失败：usage_logs 0 行、err_logs 恰 1 行；rows=%+v", rows)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
	require.NotNil(t, rows[0].ErrorMessage, "err_logs 必须带错误文本")
	require.Contains(t, *rows[0].ErrorMessage, inbandOverloadMsg, "err_logs 必须含上游原文")
}

// --- S1b：chat · 已提交（业务字节已写） ---

// TestInbandS1bChatCommittedErrorFrameIs5xx 已提交后命中流内错误载荷：客户端仍是
// 200 + 协议 error 帧（
// 观感不变），但归因是 5xx（上游已明确表态，状态码 0：上游没给过码），不是 abort。
func TestInbandS1bChatCommittedErrorFrameIs5xx(t *testing.T) {
	testHealthSink.reset()
	hr := newHealthReadback(t)
	up, u := newInbandSSEUpstream(t, "/v1/chat/completions", "Authorization", "Bearer sk-upstream",
		chatNormalFrame, chatErrFrame)
	store := &captureLogStore{}
	p := newInbandChatProxy(t, up.URL, 1, store) // 两个候选：证明不换号

	rec := postChatStream(t, p)

	require.Equal(t, http.StatusOK, rec.Code, "已提交 ⇒ 客户端 200；body=%q", rec.Body.String())
	body := rec.Body.String()
	require.Contains(t, body, "event: error", "已提交时补写协议 error 帧")
	require.Contains(t, body, inbandOverloadMsg, "客户端文案必须含上游原文（RawJSON 契约）")

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(rows),
		"已交付的失败双轨：usage_logs 恰 1 行 + err_logs 1 行；rows=%+v", rows)
	require.Equal(t, domain.Err5xx, rows[0].ErrorType,
		"上游在流内声明失败 ⇒ 5xx（序 1）")
	require.Equal(t, 0, rows[0].StatusCode, "序 1 不合成状态码")
	require.Equal(t, int64(0), rows[0].InputTokens)
	require.Equal(t, int64(0), rows[0].OutputTokens)

	p.sched.FlushRules()
	events := testHealthSink.eventsFor(1)
	require.Len(t, events, 1, "失败账号恰一条规则事件；events=%+v", events)
	require.Equal(t, rule.Kind5xx, events[0].kind, "健康 kind 与归因一致（5xx）")
	require.Nil(t, events[0].status, "序 1 不合成状态 ⇒ 规则事件无 HTTPStatus")
	states := hr.statesFor(t, 1)
	require.Len(t, states, 1, "失败账号必须有一条 health 记录")
	for key, st := range states {
		// seed-5xx 是 throttle(account, open, 10m) ⇒ StateOPEN（sink.go:44-50）。
		require.Equal(t, scheduler.StateOPEN, st, "5xx 类失败 → 冷却（StateOPEN）")
		require.NotEqual(t, scheduler.StateReady,
			hr.h.EffectiveState(1, key.Quality, key.Identity, key.IdentityRevision),
			"取用准入门必须判定为不可用")
	}
	require.Empty(t, hr.statesFor(t, 2), "另一账号零尝试（不得有 health 记录）")
	require.Equal(t, 1, u.attempts(), "已提交流不换号：上游恰一次尝试")
}

// --- S2：responses（非 codex 凭证） ---

func TestInbandS2ResponsesNativeErrorFrameIs502(t *testing.T) {
	testHealthSink.reset()
	up, u := newInbandSSEUpstream(t, "/v1/responses", "Authorization", "Bearer sk-upstream", inbandErrFrame)
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatOpenAIResponses, store)

	rec := postResponsesStream(t, p)

	require.Equal(t, http.StatusBadGateway, rec.Code, "event: error 首帧 ⇒ 502；body=%q", rec.Body.String())
	require.Equal(t, "Upstream request failed", gjsonString(rec.Body.Bytes(), "error.message"))
	require.Equal(t, 1, u.attempts())

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
	require.NotNil(t, rows[0].ErrorMessage)
	require.Contains(t, *rows[0].ErrorMessage, inbandOverloadMsg)
}

// --- S2b：codex 凭证 + HTTP 直连（plan.mapper == nil） ---

func TestInbandS2bCodexHTTPDirectErrorFrameIs502(t *testing.T) {
	testHealthSink.reset()
	up, u := newInbandSSEUpstream(t, "/backend-api/codex/responses", "Authorization", "Bearer pat-10", inbandErrFrame)
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, up.URL, nil, nil, store)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "codex HTTP 直连同一帧 ⇒ 502；body=%q", body)
	require.Equal(t, "Upstream request failed", gjsonString(body, "error.message"))
	require.Equal(t, 1, u.attempts())

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	rows := storeRows(store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
	require.NotNil(t, rows[0].ErrorMessage)
	require.Contains(t, *rows[0].ErrorMessage, inbandOverloadMsg)
}

// --- S3：codex 凭证 + WS 传输（无 status 的错误帧 → event:error 渲染路径） ---

func TestInbandS3CodexWSTransportErrorFrameIs502(t *testing.T) {
	testHealthSink.reset()
	// 无 status ⇒ SDK errorFrameHTTPError 返回 nil ⇒ 照常渲染 event:error（网关可见）。
	up, mock := newCodexWSScriptUpstream(t,
		`{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, up.URL, nil, nil, store,
		withCodexResponsesTransport(domain.CodexResponsesTransportWS))
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi","stream":true}`)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "WS 无 status 错误帧 ⇒ 502；body=%q", body)
	require.Equal(t, "Upstream request failed", gjsonString(body, "error.message"))
	require.Len(t, mock.framesSnapshot(), 1, "上游 create 恰 1")

	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	rows := storeRows(store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
	require.NotNil(t, rows[0].ErrorMessage)
	require.Contains(t, *rows[0].ErrorMessage, inbandOverloadMsg)
}

// --- S5：Anthropic ---

func TestInbandS5AnthropicErrorFrameIs502(t *testing.T) {
	testHealthSink.reset()
	up, u := newInbandSSEUpstream(t, "/v1/messages", "x-api-key", "sk-upstream", inbandErrFrame)
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatAnthropic, store)

	rec := postAnthropicStream(t, p)

	require.Equal(t, http.StatusBadGateway, rec.Code, "event: error 首帧 ⇒ 502；body=%q", rec.Body.String())
	require.Equal(t, "Upstream request failed", gjsonString(rec.Body.Bytes(), "error.message"))
	require.Equal(t, 1, u.attempts())

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
	require.NotNil(t, rows[0].ErrorMessage)
	require.Contains(t, *rows[0].ErrorMessage, inbandOverloadMsg)
}

// --- S5b：针①的独有定义域（event: error + data 里既无 "type":"error" 也无 error 对象） ---

func TestInbandS5bEventOnlyErrorFrameCarriesMessage(t *testing.T) {
	testHealthSink.reset()
	// ②③ 按定义全不命中（无 "type":"error"、无 "error":{），只有针①命中。
	onlyEvent := `event: error` + "\n" + `data: {"message":"failed"}` + "\n\n"
	up, u := newInbandSSEUpstream(t, "/v1/messages", "x-api-key", "sk-upstream", onlyEvent)
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, up.URL, domain.FormatAnthropic, store)

	rec := postAnthropicStream(t, p)

	require.Equal(t, http.StatusBadGateway, rec.Code, "event: error 也必须是失败；body=%q", rec.Body.String())
	require.Equal(t, 1, u.attempts())

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 0, errs: 1, abort: 0}, inbandTracks(rows), "rows=%+v", rows)
	require.Equal(t, domain.ErrNetwork, rows[0].ErrorType)
	require.NotNil(t, rows[0].ErrorMessage)
	require.Contains(t, *rows[0].ErrorMessage, "failed",
		"① 也必须解析 data 才拿得到上游 message；ErrorMessage=%q", *rows[0].ErrorMessage)
}

// --- S6：入站 WS（客户端升级后上游发错误帧再以 1000 关闭） ---

// fakeResponsesWSScript 入站 WS 车道的假上游：读到首个客户端帧后按脚本下发，
// 随后 1000 关闭（与 fakeResponsesWS 同握手面，脚本化帧序）。
func fakeResponsesWSScript(t *testing.T, frames ...string) (*httptest.Server, *inbandSSEUpstream) {
	t.Helper()
	h := &inbandSSEUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Responses-Websockets") != aiclient.ResponsesWSBetaHeader {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.hits++
		h.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, _, err := c.Read(context.Background())
		if err != nil {
			return
		}
		for _, f := range frames {
			if err := c.Write(context.Background(), typ, []byte(f)); err != nil {
				return
			}
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	return srv, h
}

// wsHealthProxy 入站 WS 模板测试代理（真实 health 读回钩子）。
func wsHealthProxy(t *testing.T, upstream string, logs *captureLogStore) (*Proxy, *httptest.Server) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, Models: []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture:          true,
	}
	ruleSink := rule.HealthSink(testHealthSink)
	if testRuleSink != nil {
		ruleSink = testRuleSink
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, ruleSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, noopLoader{accs: accs}, re, testRuntimeHealth, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	clients := aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport}, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errLogStoreFrom(logs), nil)
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{})
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(srv.Close)
	return p, srv
}

// wsReadFrames 读到 n 个文本帧（错误帧 + 续帧断言面）。
func wsReadFrames(t *testing.T, c *websocket.Conn, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for range n {
		out = append(out, string(readResponsesWSFrame(t, c)))
	}
	return out
}

// wsDialInbound 拨网关 /v1/responses 并发一条 response.create。
func wsDialInbound(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	c := dialResponsesWS(t, srv)
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	return c
}

func TestInbandS6InboundWSErrorFrameIs5xx(t *testing.T) {
	testHealthSink.reset()
	hr := newHealthReadback(t)
	up, u := fakeResponsesWSScript(t, `{"type":"error","error":{"type":"server_error","message":"`+inbandOverloadMsg+`"}}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	got := wsReadFrames(t, c, 1)
	require.Contains(t, got[0], `"type":"error"`, "错误帧照旧 1:1 转发（客户端观感不变）")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure) // 上游确实 1000 关闭 ⇒ 不虚构 1011

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(rows),
		"入站 WS 流内错误载荷记 5xx：usage_logs 恰 1 行 + err_logs 恰 1 行；rows=%+v", rows)
	require.Equal(t, domain.Err5xx, rows[0].ErrorType, "上游已明确表态 ⇒ 5xx（序 1），不是 abort")
	require.Equal(t, 0, rows[0].StatusCode, "序 1 不合成状态码")
	require.Equal(t, int64(0), rows[0].InputTokens)
	require.Equal(t, int64(0), rows[0].OutputTokens)
	require.Equal(t, 1, u.attempts())

	p.sched.FlushRules()
	events := testHealthSink.eventsFor(1)
	require.Len(t, events, 1, "失败账号恰一条规则事件；events=%+v", events)
	require.Equal(t, rule.Kind5xx, events[0].kind, "入站 WS 健康 kind 必须与 chat/codex 一致")
	require.Nil(t, events[0].status, "序 1 不合成状态 ⇒ 规则事件无 HTTPStatus")
	states := hr.statesFor(t, 1)
	require.Len(t, states, 1, "失败账号必须有一条 health 记录")
	for _, st := range states {
		require.Equal(t, scheduler.StateOPEN, st, "seed-5xx: open 10m（StateOPEN）")
	}
}

// --- T-turnreset / T-turnreset2：轮级清零（只在 completed 处清） ---

func TestInbandTurnResetOnCompleted(t *testing.T) {
	testHealthSink.reset()
	hr := newHealthReadback(t)
	// ① 非致命错误帧（invalid_request_error）→ ② **无 usage 的** completed → ③ 1000。
	// 必须用无 usage 帧：带 usage 的 completed 会让"挂 sniffResponsesCompletedUsage
	// 的 ok 分支"这种错实现也绿。
	up, _ := fakeResponsesWSScript(t,
		`{"type":"error","code":"invalid_request_error","message":"x"}`,
		`{"type":"response.completed","response":{"id":"r"}}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	_ = wsReadFrames(t, c, 2)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 0, abort: 0}, inbandTracks(rows),
		"本轮最后是 completed ⇒ 记成功、无冷却；rows=%+v", rows)
	require.Equal(t, domain.ErrNone, rows[0].ErrorType)

	p.sched.FlushRules()
	require.Empty(t, hr.statesFor(t, 1), "清零后不得冷却健康账号")
}

func TestInbandTurnResetRequiresCompleted(t *testing.T) {
	testHealthSink.reset()
	hr := newHealthReadback(t)
	// 错误帧 → **非 completed** 帧 → 1000：清零不得挂到任意上行帧。
	up, _ := fakeResponsesWSScript(t,
		`{"type":"error","code":"invalid_request_error","message":"x"}`,
		`{"type":"response.output_item.done","item":{"type":"message"}}`)
	store := &captureLogStore{}
	p, srv := wsHealthProxy(t, up.URL, store)

	c := wsDialInbound(t, srv)
	defer c.CloseNow()
	_ = wsReadFrames(t, c, 2)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	rows := drainLogs(t, p, store)
	require.Equal(t, logTracks{usage: 1, errs: 1, abort: 0}, inbandTracks(rows),
		"非 completed 帧不清零 ⇒ 仍记失败（5xx）+ 冷却；rows=%+v", rows)
	require.Equal(t, domain.Err5xx, rows[0].ErrorType,
		"轮级未清零 ⇒ 错误载荷仍归因为 5xx（不是 abort）")

	p.sched.FlushRules()
	states := hr.statesFor(t, 1)
	require.Len(t, states, 1)
	for _, st := range states {
		require.Equal(t, scheduler.StateOPEN, st, "非 completed 帧不清零 ⇒ 仍冷却")
	}
}

// gjsonString 取顶层点路径的字符串值（断言辅助——非热路径）。
func gjsonString(body []byte, path string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	cur := m
	parts := strings.Split(path, ".")
	for i, part := range parts {
		if i == len(parts)-1 {
			var s string
			if err := json.Unmarshal(cur[part], &s); err != nil {
				return ""
			}
			return s
		}
		next := map[string]json.RawMessage{}
		if err := json.Unmarshal(cur[part], &next); err != nil {
			return ""
		}
		cur = next
	}
	return ""
}

// --- isResponsesCompleted 单测（轮级清零判据；api 面见 ws_relay.go） ---

func TestIsResponsesCompleted(t *testing.T) {
	// 无 usage 的 completed 必须 true：这正是不能复用 sniffResponsesCompletedUsage
	// （ok = "是 completed 且解析到 usage"）的理由。
	require.True(t, isResponsesCompleted([]byte(`{"type":"response.completed","response":{"id":"r"}}`)),
		"无 usage 的 completed 也是完成帧")
	require.False(t, isResponsesCompleted(
		[]byte(`{"type":"response.output_text.delta","delta":"hi","meta":{"type":"response.completed"}}`)),
		"嵌套 meta.type 不得误判（顶层精确判定）")
	require.False(t, isResponsesCompleted([]byte(`{"type":"response.output_item.done"}`)))
	require.False(t, isResponsesCompleted(nil))
	require.False(t, isResponsesCompleted([]byte(`not-json`)))
	// 非字符串 type 值：守卫必须挡住 data[start+1:end-1]（否则越界 panic）。
	require.False(t, isResponsesCompleted([]byte(`{"type":123}`)))
	require.False(t, isResponsesCompleted([]byte(`{"type":null}`)))
}
