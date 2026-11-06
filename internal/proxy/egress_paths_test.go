// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
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
	"github.com/is7qin/c3api/pkg/egress"
)

// egressFaceUpstream 逐面覆盖用的万能上游替身：按路径给出最小合法响应，并记录
// **是否见到出口标记头**（spec §9.3 的"出口可辨识"判据——`httptest` 下 RemoteAddr
// 对直连与经代理恒是 127.0.0.1，只能靠替身注入的头判定）。
type egressFaceUpstream struct {
	*httptest.Server

	mu       sync.Mutex
	paths    []string
	hits     int
	markedIn int // 带标记头的请求数
}

func newEgressFaceUpstream(t *testing.T) *egressFaceUpstream {
	t.Helper()
	u := &egressFaceUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.Server.Close)
	return u
}

func (u *egressFaceUpstream) counts() (hits, marked int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits, u.markedIn
}

func (u *egressFaceUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.hits++
	u.paths = append(u.paths, r.URL.Path)
	if r.Header.Get(egressMarkerHeader) == "1" {
		u.markedIn++
	}
	u.mu.Unlock()

	if r.Header.Get("Upgrade") != "" { // resp-ws 面
		u.serveWS(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	stream := bytes.Contains(body, []byte(`"stream":true`))
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	case strings.HasSuffix(r.URL.Path, "/messages"):
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"gpt-4o","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":0}}}`+"\n\n")
			fmt.Fprint(w, `event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
			fmt.Fprint(w, `event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`+"\n\n")
			fmt.Fprint(w, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"gpt-4o","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	case strings.HasSuffix(r.URL.Path, "/responses"):
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
			fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"rsp_1","status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"rsp_1","object":"response","status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`)
	case strings.Contains(r.URL.Path, "/images/"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"created":1750000000,"data":[{"b64_json":"aGVsbG8="}]}`)
	case strings.HasSuffix(r.URL.Path, "/alpha/search"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveWS 上游 resp-ws：接受升级、回一帧 completed 后正常关闭。
func (u *egressFaceUpstream) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err != nil {
		return
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"rsp_ws_1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// newEgressPathProxy 逐面出口覆盖用的网关：与 newTestProxyFormatLogs 同形（单模板/
// 单账号/单组、API key 鉴权），差别只有——**账号是否绑定出口代理**。出口池是
// `scheduler.Config.Egress` 那一个（与 main 同构），所以 `sel.Egress` 就是它解析
// 出来的 client：bound ⇒ 带代理的 client；unbound ⇒ `Pool.Direct()`。
func newEgressPathProxy(t *testing.T, upstream string, formats []domain.RequestFormat, proxyURL string, bound bool) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: formats, Models: []string{"gpt-4o"},
	}
	acc := &domain.Account{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
	}
	pool := egress.NewPool(&http.Client{Transport: http.DefaultTransport})
	if bound {
		spec := specOfProxyURL(t, "http", proxyURL, "", "")
		proxyID := int64(7)
		acc.ProxyID = &proxyID
		acc.Proxy = &domain.Proxy{
			ID: proxyID, Name: "px", Scheme: domain.ProxySchemeHTTP,
			Host: spec.Host, Port: spec.Port, Enabled: true,
		}
	}
	accs := map[int64][]*domain.Account{10: {acc}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture:          true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour, Egress: pool}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, noopLogStore{}, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, 10)}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, noopErrLogStore{}, nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{})
}

// egressFaceCase 一个出栈面的最小端到端用例：请求经网关 handler，响应经上游替身。
type egressFaceCase struct {
	name string
	// format 本面在网关侧对应的请求格式（日志/计费口径与出栈分支都由它决定）。
	format domain.RequestFormat
	// formats 模板声明支持的格式（**路由面**）：search 无独立路由——它复用
	// openai-responses 车道选号（`forward_search.go:130`），所以这一面的模板
	// 声明的仍是 responses（与既有 search 夹具同款）。nil ⇒ 只声明本面自己的格式。
	formats []domain.RequestFormat
	run     func(t *testing.T, p *Proxy)
}

// postEgressFace 发一个网关请求（带网关 key），返回响应记录器。
func postEgressFace(t *testing.T, handle http.HandlerFunc, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer ck-1")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	handle(rec, req)
	return rec
}

// egressFaceRouteFormat 每面在模板 SupportedFormats 里该怎么声明（nil ⇒ 用本面
// 自己的请求格式）。
func egressFaceRouteFormat(c egressFaceCase, own domain.RequestFormat) []domain.RequestFormat {
	if c.formats != nil {
		return c.formats
	}
	return []domain.RequestFormat{own}
}

// egressFaceCases 逐面覆盖清单（spec §9.3 的七面：chat 流+非流、responses、
// anthropic、images JSON+multipart、search、resp-ws）。每面带上自己的请求格式。
func egressFaceCases() []egressFaceCase {
	return []egressFaceCase{
		{name: "chat_nonstream", format: domain.FormatOpenAIChat, run: func(t *testing.T, p *Proxy) {
			rec := postEgressFace(t, p.HandleChat, "/v1/chat/completions", "application/json",
				[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Contains(t, rec.Body.String(), `"content":"hi"`)
		}},
		{name: "chat_stream", format: domain.FormatOpenAIChat, run: func(t *testing.T, p *Proxy) {
			rec := postEgressFace(t, p.HandleChat, "/v1/chat/completions", "application/json",
				[]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), "data: [DONE]")
		}},
		{name: "responses_nonstream", format: domain.FormatOpenAIResponses, run: func(t *testing.T, p *Proxy) {
			rec := postEgressFace(t, p.HandleResponses, "/v1/responses", "application/json",
				[]byte(`{"model":"gpt-4o","input":"hi"}`))
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Contains(t, rec.Body.String(), `"rsp_1"`)
		}},
		{name: "anthropic_stream", format: domain.FormatAnthropic, run: func(t *testing.T, p *Proxy) {
			rec := postEgressFace(t, p.HandleAnthropic, "/v1/messages", "application/json",
				[]byte(`{"model":"gpt-4o","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), "message_start")
		}},
		{name: "images_json", format: domain.FormatOpenAIImages, run: func(t *testing.T, p *Proxy) {
			rec := postEgressFace(t, p.HandleImagesGenerations, "/v1/images/generations", "application/json",
				[]byte(`{"model":"gpt-4o","prompt":"cat"}`))
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Contains(t, rec.Body.String(), "b64_json")
		}},
		{name: "images_multipart", format: domain.FormatOpenAIImages, run: func(t *testing.T, p *Proxy) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			require.NoError(t, mw.WriteField("model", "gpt-4o"))
			require.NoError(t, mw.WriteField("prompt", "make it red"))
			fw, err := mw.CreateFormFile("image", "a.png")
			require.NoError(t, err)
			_, err = fw.Write([]byte("PNG-binary-junk"))
			require.NoError(t, err)
			require.NoError(t, mw.Close())
			rec := postEgressFace(t, p.HandleImagesEdits, "/v1/images/edits", mw.FormDataContentType(), buf.Bytes())
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		}},
		{name: "search", formats: []domain.RequestFormat{domain.FormatOpenAIResponses}, run: func(t *testing.T, p *Proxy) {
			rec := postEgressFace(t, p.HandleSearch, "/v1/alpha/search", "application/json",
				[]byte(`{"model":"gpt-4o"}`))
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Contains(t, rec.Body.String(), `"results"`)
		}},
		{name: "responses_ws", format: domain.FormatOpenAIResponsesWS, run: func(t *testing.T, p *Proxy) {
			srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
			defer srv.Close()
			c := dialResponsesWS(t, srv)
			defer c.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(`{"model":"gpt-4o","input":"hi"}`)), "首帧必须能发出（上游已建连）")
			typ, frame, err := c.Read(ctx)
			require.NoError(t, err, "网关必须把上游帧透传回来")
			require.Equal(t, websocket.MessageText, typ)
			require.Contains(t, string(frame), "response.completed")
		}},
	}
}

// TestEgressPathsEveryFaceGoesThroughProxy spec §9.3 逐面覆盖：**绑定代理**时上游
// 只见替身注入的标记头；**解绑（Direct）**后标记头消失。每面各一条：chat（流+非流）、
// responses、anthropic、images（JSON+multipart）、search、resp-ws。
//
// 这条用例同时是"接线门"：任何一面忘了把 `sel.Egress.HTTP`（或 codex 面的
// `sel.Egress`）传下去，bound 子用例就会因为 `mark=0` 失败——而不是静默直连。
func TestEgressPathsEveryFaceGoesThroughProxy(t *testing.T) {
	for _, tc := range egressFaceCases() {
		t.Run(tc.name, func(t *testing.T) {
			formats := egressFaceRouteFormat(tc, tc.format)
			t.Run("bound", func(t *testing.T) {
				up := newEgressFaceUpstream(t)
				px := newEgressForwardProxy(t)
				p := newEgressPathProxy(t, up.URL, formats, px.URL, true)
				tc.run(t, p)
				hits, marked := up.counts()
				require.Equal(t, 1, hits, "上游必须被触达一次")
				require.Equal(t, 1, marked, "绑代理后上游必须看到 %s（出口替身注入）", egressMarkerHeader)
				require.Len(t, px.requested(), 1, "代理替身必须收到本次出栈")
			})
			t.Run("direct", func(t *testing.T) {
				up := newEgressFaceUpstream(t)
				p := newEgressPathProxy(t, up.URL, formats, "", false)
				tc.run(t, p)
				hits, marked := up.counts()
				require.Equal(t, 1, hits, "上游必须被触达一次")
				require.Zero(t, marked, "解绑（Pool.Direct）后标记头必须消失：直连不经过任何代理")
			})
		})
	}
}

// TestEgressPathsSelectionCarriesBinding 面外的装配事实（与逐面用例互补）：
// 绑定与否只体现在 `Selection.Egress` 与 `Selection.ProxyID` 上，热路径不判断。
func TestEgressPathsSelectionCarriesBinding(t *testing.T) {
	up := newEgressFaceUpstream(t)
	px := newEgressForwardProxy(t)

	bound := newEgressPathProxy(t, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, px.URL, true)
	sel, err := bound.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	defer sel.Release()
	require.NotNil(t, sel.Egress, "出口恒非 nil")
	require.NotEqual(t, (egress.Spec{}), sel.Egress.Spec, "绑定账号的出口带规格")
	require.NotNil(t, sel.ProxyID, "绑定行 id 与出口同源携带（失败日志要用）")
	require.Equal(t, int64(7), *sel.ProxyID)
	require.NotNil(t, sel.Egress.HTTP.Transport, "代理出口必须是自己的 transport（专属连接池）")

	direct := newEgressPathProxy(t, up.URL, []domain.RequestFormat{domain.FormatOpenAIChat}, "", false)
	sel2, err := direct.sched.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	defer sel2.Release()
	require.Equal(t, (egress.Spec{}), sel2.Egress.Spec, "未绑定 ⇒ 零值规格（直连）")
	require.Nil(t, sel2.ProxyID, "未绑定 ⇒ 无 proxy_id")
}

// 确保 json 包在本文件被用到（响应用例的断言只做子串匹配，这里保留一个编码入口，
// 便于将来替换成结构化断言时不必新增 import）。
var _ = json.Marshal
