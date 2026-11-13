// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- Codex 不做续接：直连/转换均不入队；首个预留为 Codex 时不解析续接 ---

// newCodexContProxy 构造装配了异步续接 store + worker 的 codex resp 代理（镜像
// newTestCodexRespProxy，额外下发 Deps{Continuation, ContBind}）。pcs = KeyMeta
// 组级 protocol_convert 方向集合（converted 用例用）。
func newCodexContProxy(t *testing.T, credType credential.Type, accounts map[int64]*domain.AccountExt, upstream string, pcs []domain.ProtocolConvert, s *continuation.Store, bind *ContBindWorker) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "",
		CredentialType:   credType,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
	}
	accs := make(map[int64][]*domain.Account, 1)
	for id, ext := range accounts {
		accs[10] = append(accs[10], &domain.Account{
			ID: id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4, Ext: ext,
		})
	}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, &captureLogStore{}, nil)
	cfg := Config{MaxBodySize: 1 << 20, FailoverAttempts: 2, UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second, UsageCapture: true}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	// 出口池：调度器快照与 codex 适配层用**同一个**（M3 起 codex 客户端的
	// transport 取自 sel.Egress.HTTP.Transport，两者不同源就等于没接线——
	// 请求会绕过测试替身直接打真实 codex 端点）。
	egPool := testEgressPool(newProxyOfficialRewriteTransportWithAssert(t, upstream))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour, Egress: egPool}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	key := activeKey(1, 1, 10)
	key.ProtocolConverts = pcs
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": key}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	clients := aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport}, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: &fakeFailureStore{}, Failer: sched, Log: nil})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: 20 * time.Millisecond}, &captureLogStore{}, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	codex := sdkbridge.NewCodex(failure, egPool, nil, sdkbridge.RotationDeps{})
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{Codex: codex, Continuation: s, ContBind: bind})
	t.Cleanup(func() { _ = p.rec.Close(context.Background()) })
	return p
}

// TestCodexSkipsContinuation C3-10：Codex 凭据不做续接。
//   - 直连流式不入队（原 TestCodexDirectContinuationEnqueued，断言反转）：用未
//     Start 的 worker，若入队则条目留在队列里（Queued 可确定观测，无需等待）。
//   - 首个预留为 Codex 时带 previous_response_id（m1 修订）：照常先查绑定，
//     未命中不 410、不钉选，照常派发（HTTP 与 WS）。
//   - 首个预留为 Codex、id 由 API key 账号产出（有绑定）：仍钉回原账号（HTTP 与 WS）。
//   - 混合组首个预留为 API key：续接解析照旧，缺失绑定 410 且零派发（HTTP 与 WS）。
func TestCodexSkipsContinuation(t *testing.T) {
	t.Run("direct_stream_not_enqueued", func(t *testing.T) {
		_, s, _ := contFixture(t)
		up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespDone}})
		defer up.Close()
		w := NewContBindWorker(s, nil, ContBindConfig{}) // 不 Start：入队即留在队列
		p := newCodexContProxy(t, credential.TypeCodexPAT,
			map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, s, w)

		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", b)
		require.Contains(t, string(b), "resp_t6", "上游 id 帧透传客户端")

		require.Zero(t, w.Queued(), "Codex 直连流式不得入队续接")
		require.Zero(t, w.Unattributed(), "Codex 不触发归属拒绝计数")
		require.Zero(t, w.Dropped())
		_, ok := contLookupBinding(t, s, contProtocolREST, "resp_t6")
		require.False(t, ok, "Codex 不建续接绑定")
	})

	t.Run("http_codex_first_prev_id_not_410", func(t *testing.T) {
		_, s, hook := contFixture(t)
		up, rec := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespDone}})
		defer up.Close()
		p := newCodexContProxy(t, credential.TypeCodexPAT,
			map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, s, nil)

		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()
		resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi","previous_response_id":"resp_missing"}`)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "Codex 首选、绑定未命中 → 不 410：body=%s", b)
		require.Contains(t, string(b), "resp_t6")
		rec.mu.Lock()
		calls := rec.calls
		rec.mu.Unlock()
		require.Equal(t, 1, calls, "照常派发到 Codex 账号")
		require.EqualValues(t, 1, hook.n.Load(), "照常先 contResolve（恰一次 Lookup），未命中交给 SDK")
	})

	t.Run("ws_codex_first_prev_id_not_410", func(t *testing.T) {
		_, s, hook := contFixture(t)
		up, hooks := newCodexWSUpstream(t, []int{200}, 1)
		defer up.Close()
		p, _ := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
			map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, up.URL, nil, &captureLogStore{})
		p.cont = s // 装配续接 store：非 Codex 首选时缺失绑定会 410 错误帧、零拨号

		srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
		defer srv.Close()
		c := dialResponsesWSHeaders(t, srv, http.Header{"Authorization": {"Bearer ck-1"}})
		defer c.CloseNow()
		require.NoError(t, c.Write(context.Background(), websocket.MessageText,
			[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi","previous_response_id":"rsp_missing"}`)))
		f := readResponsesWSFrame(t, c)
		require.NotContains(t, string(f), `"type":"error"`, "Codex 首选、绑定未命中 → 不回错误帧：%s", f)
		require.Contains(t, string(f), "rsp_ws_1")
		require.Equal(t, 1, hooks.upgradesN(), "照常拨号到 Codex 账号")
		require.EqualValues(t, 1, hook.n.Load(), "照常先 contResolve（恰一次 Lookup），未命中交给 SDK")
	})

	t.Run("api_key_id_pins_back_when_codex_first", func(t *testing.T) {
		for _, format := range []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS} {
			_, s, _ := contFixture(t)
			var hits atomic.Int32
			var authSeen atomic.Value
			var up *httptest.Server
			if format == domain.FormatOpenAIResponsesWS {
				up = contFakeWS(t, "rsp_api", &hits, &authSeen, "")
			} else {
				up = contUpstream(t, "resp_api", "sk-acc1", "", &hits, &authSeen)
			}
			defer up.Close()
			apiTpl := &domain.Template{ID: 1, Name: "api", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{format}, Models: []string{"gpt-4o"}}
			codexTpl := &domain.Template{ID: 2, Name: "codex", CredentialType: credential.TypeCodexPAT, SupportedFormats: []domain.RequestFormat{format}, Models: []string{"gpt-4o"}}
			apiAcc := contAcc(1, apiTpl, "sk-acc1", up.URL)
			codexAcc := &domain.Account{ID: 2, TemplateID: codexTpl.ID, Template: codexTpl, Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4, Ext: codexPATExt(2, "pat-2")}

			// 第一次：API key 首选，产出响应并写续接绑定。
			p1 := contProxy(t, format, []*domain.Account{apiAcc, codexAcc}, s)
			// 第二次：计划首选 Codex，previous_response_id 命中绑定 → 照旧钉回 API key 账号。
			p2 := contProxy(t, format, []*domain.Account{codexAcc, apiAcc}, s)
			if format == domain.FormatOpenAIResponsesWS {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				c := contWSDial(t, contWSServer(t, p1))
				require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
				for range 3 {
					_, _, err := c.Read(ctx)
					require.NoError(t, err)
				}
				c.CloseNow()
				c2 := contWSDial(t, contWSServer(t, p2))
				require.NoError(t, c2.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"again","previous_response_id":"rsp_api"}`)))
				_, f, err := c2.Read(ctx)
				require.NoError(t, err)
				require.NotContains(t, string(f), `"type":"error"`, "命中绑定钉回 API key 账号：%s", f)
				require.Contains(t, string(f), "rsp_api")
				c2.CloseNow()
				cancel()
			} else {
				w := httptest.NewRecorder()
				p1.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi"}`))
				require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
				w2 := httptest.NewRecorder()
				p2.HandleResponses(w2, contResponsesReq(`{"model":"gpt-4o","input":"again","previous_response_id":"resp_api"}`))
				require.Equal(t, http.StatusOK, w2.Code, "命中绑定钉回 API key 账号：body=%s", w2.Body.String())
			}
			require.EqualValues(t, 2, hits.Load(), "续接派发到产出该 id 的 API key 账号（%s）", format)
			require.Equal(t, "Bearer sk-acc1", authSeen.Load().(string))
		}
	})

	t.Run("mixed_api_key_first_unchanged", func(t *testing.T) {
		for _, format := range []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS} {
			_, s, _ := contFixture(t)
			var hits atomic.Int32
			var authSeen atomic.Value
			var up *httptest.Server
			if format == domain.FormatOpenAIResponsesWS {
				up = contFakeWS(t, "rsp_x", &hits, &authSeen, "")
			} else {
				up = contUpstream(t, "resp_x", "sk-acc1", "", &hits, &authSeen)
			}
			defer up.Close()
			apiTpl := &domain.Template{ID: 1, Name: "api", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{format}, Models: []string{"gpt-4o"}}
			codexTpl := &domain.Template{ID: 2, Name: "codex", CredentialType: credential.TypeCodexPAT, SupportedFormats: []domain.RequestFormat{format}, Models: []string{"gpt-4o"}}
			codexAcc := &domain.Account{ID: 2, TemplateID: codexTpl.ID, Template: codexTpl, Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4, Ext: codexPATExt(2, "pat-2")}
			p := contProxy(t, format, []*domain.Account{contAcc(1, apiTpl, "sk-acc1", up.URL), codexAcc}, s) // 计划首选 API key

			if format == domain.FormatOpenAIResponsesWS {
				srv := contWSServer(t, p)
				c := contWSDial(t, srv)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"x","previous_response_id":"rsp_missing"}`)))
				_, f, err := c.Read(ctx)
				require.NoError(t, err)
				require.Contains(t, string(f), `"type":"error"`, "API key 首选：缺失绑定照旧 fail-closed 错误帧")
				cancel()
				c.CloseNow()
			} else {
				w := httptest.NewRecorder()
				p.HandleResponses(w, contResponsesReq(`{"model":"gpt-4o","input":"hi","previous_response_id":"resp_missing"}`))
				require.Equal(t, http.StatusGone, w.Code, "API key 首选：缺失绑定照旧 410")
			}
			require.Zero(t, hits.Load(), "fail-closed 零派发（%s）", format)
		}
	})
}

// TestCodexConvertedContinuationNotEnqueued converted codex（客户端 Chat）：
// 不入队——客户端无法消费 Responses 续接 id；worker 零绑定、lookup miss。
func TestCodexConvertedContinuationNotEnqueued(t *testing.T) {
	_, s, _ := contFixture(t)
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, convCodexRespDelta, t6RespDone}})
	defer up.Close()
	w := startTestContBind(t, s, ContBindConfig{})
	p := newCodexContProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, s, w)

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"object":"chat.completion.chunk"`, "转换路径客户端收 chat chunk")

	require.Zero(t, w.Bound(), "converted 客户端非 Responses → 不入队")
	require.Zero(t, w.Unattributed(), "converted 不触发归属拒绝计数")
	_, ok := contLookupBinding(t, s, contProtocolREST, "resp_t6")
	require.False(t, ok, "converted 不应为客户端不可消费的 id 建绑定")
}
