// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// responsesWSCompletedFrame 假上游的 response.completed 事件帧（usage 5 计数：
// input 3 / output 5 / total 8 / cache_read 1 / cache_creation 3=2+1）。
const responsesWSCompletedFrame = `{"type":"response.completed","response":{"id":"rsp_ws_1","status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8,"input_tokens_details":{"cached_tokens":1,"text_tokens":2,"audio_tokens":0},"output_tokens_details":{"reasoning_tokens":2,"text_tokens":3,"audio_tokens":0},"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":1}}}}`

// fakeWSHooks 假上游观测面（断言用）。
type fakeWSHooks struct {
	mu      sync.Mutex
	headers []http.Header // 每次握手头快照
	frames  []string      // 收到的客户端帧（原样字节）
	// frameLimit 读取多少个客户端帧后主动关闭（0 = 不主动关闭，等对侧关）
	frameLimit int
}

// fakeResponsesWS 假上游（resp-ws）：同一 /v1/responses 路径接受 WS 升级
// （真实上游无 /ws 后缀），握手头断言（账号鉴权 + beta 头），首个客户端帧后
// 下发事件流（response.created / output_text.delta / response.completed +
// usage），随后每帧回声（{"type":"echo","payload":<原帧>}，双向透传断言用），
// 读满 frameLimit 帧后发 1000 关闭帧。
func fakeResponsesWS(t *testing.T, hooks *fakeWSHooks) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		hooks.mu.Lock()
		hooks.headers = append(hooks.headers, r.Header.Clone())
		hooks.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Responses-Websockets") != aiclient.ResponsesWSBetaHeader {
			w.WriteHeader(400)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		streamed := false
		n := 0
		for {
			typ, msg, err := c.Read(context.Background())
			if err != nil {
				return
			}
			hooks.mu.Lock()
			hooks.frames = append(hooks.frames, string(msg))
			hooks.mu.Unlock()
			if !streamed {
				streamed = true
				for _, f := range []string{
					`{"type":"response.created","response":{"id":"rsp_ws_1","model":"gpt-4o"}}`,
					`{"type":"response.output_text.delta","delta":"hi"}`,
					responsesWSCompletedFrame,
				} {
					if err := c.Write(context.Background(), typ, []byte(f)); err != nil {
						return
					}
				}
			}
			payload, err := json.Marshal(map[string]any{"type": "echo", "payload": string(msg)})
			if err != nil {
				return
			}
			if err := c.Write(context.Background(), typ, payload); err != nil {
				return
			}
			n++
			if hooks.frameLimit > 0 && n >= hooks.frameLimit {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	return srv
}

// dialResponsesWS 测试客户端：拨网关 /v1/responses（upgrade 头 + 网关 key），
// 返回连接与读到的关闭错误（拨号即失败 → 直接 Fail）。
func dialResponsesWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	c, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{
		// 网关 key 双载体齐带（auth.go 任一非空即鉴权）：双载体都不得泄漏上游
		HTTPHeader: http.Header{
			"Authorization":    {"Bearer ck-1"},
			"X-Api-Key":        {"ck-1"},
			"X-Client-Version": {"codex-1.2.3"},
		},
		CompressionMode: websocket.CompressionContextTakeover,
	})
	require.NoError(t, err, "gateway WS 握手必须成功")
	return c
}

// readResponsesWSFrame 读取一个文本帧（关闭帧 → Fail）。
func readResponsesWSFrame(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	require.NoError(t, err, "read frame: %v", err)
	require.Equal(t, websocket.MessageText, typ)
	return b
}

// readResponsesWSClose 读取关闭帧（断言状态码）。
func readResponsesWSClose(t *testing.T, c *websocket.Conn, code websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var ce websocket.CloseError
	require.True(t, errors.As(err, &ce), "expect close frame, got %v", err)
	require.Equal(t, code, ce.Code)
}

// wsTestProxy 构造 resp-ws 模板测试代理 + 网关服务器。
func wsTestProxy(t *testing.T, upstream string, format domain.RequestFormat, logs *captureLogStore) (*Proxy, *httptest.Server) {
	t.Helper()
	p := newTestProxyFormatLogs(t, upstream, format, logs)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(srv.Close)
	return p, srv
}

// wsTestProxyBilling 构造注入计费钩子（Prices+Balances+TierPolicy）的 resp-ws
// 测试代理（BillingTier 落库断言用；policy nil = 恒透传）。
func wsTestProxyBilling(t *testing.T, upstream string, prices *fakePriceLookup, policy func(billing.Tier) billing.TierPolicyMode, logs *captureLogStore) (*Proxy, *httptest.Server) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, Models: []string{"gpt-4o"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, logs, &BillingHooks{
		Prices:     prices,
		Balances:   billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{}}, nil),
		TierPolicy: policy,
	})
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	t.Cleanup(srv.Close)
	return p, srv
}

// 端到端主流程：WS 握手（beta 头 + 账号鉴权 + 客户端头透传）、双向事件帧 1:1
// 透传（回声字节一致）、response.completed usage 嗅探计费（5 计数）。
func TestResponsesWSHandshakeAndBidirectionalPassthrough(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 3}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	// 成功行（none）入 usage_logs（放行路径语义，cost=0 不限）——WS usage 嗅探
	// 字段断言照旧
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()

	// 客户端连续发 3 帧（response.create + 2 个中间帧）：全部原样转发上游
	// （首帧模型无映射 → 字节不变），上游读满 3 帧后发 1000 关闭帧。
	f1 := `{"type":"response.create","model":"gpt-4o","input":"hi"}`
	f2 := `{"type":"response.input_text.delta","delta":"typing"}`
	f3 := `{"type":"custom.mid","n":42}`
	for _, f := range []string{f1, f2, f3} {
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(f)))
	}

	// 事件流 + 回声 + 关闭帧（上游已消费 3 帧 → 正常完成路径）。
	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, string(readResponsesWSFrame(t, c)))
	}
	require.Equal(t, `{"type":"response.created","response":{"id":"rsp_ws_1","model":"gpt-4o"}}`, got[0])
	require.Equal(t, `{"type":"response.output_text.delta","delta":"hi"}`, got[1])
	require.Contains(t, got[2], `"type":"response.completed"`)
	require.Contains(t, got[2], `"input_tokens":3`)
	// 回声帧：payload 与客户端发出字节逐字一致（中间帧零解析零改写直转）
	for i, want := range []string{f1, f2, f3} {
		var echo struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		require.NoError(t, json.Unmarshal([]byte(got[3+i]), &echo))
		require.Equal(t, "echo", echo.Type)
		require.Equal(t, want, echo.Payload, "回声帧必须与客户端帧字节一致（零解析透传）")
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	// 握手头断言（上游观测面）：beta 头现役唯一 + 账号鉴权注入 + 客户端头
	// 透传 + 网关 key 不得泄漏（Authorization/x-api-key 双载体）+ hop-by-hop
	// 剔除。
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	require.Len(t, hooks.headers, 1)
	h := hooks.headers[0]
	require.Equal(t, "2026-02-06", h.Get("Responses-Websockets"), "上游握手必须带 beta 头（现役唯一）")
	require.Equal(t, "Bearer sk-upstream", h.Get("Authorization"), "账号鉴权注入")
	require.Equal(t, "codex-1.2.3", h.Get("X-Client-Version"), "客户端头透传")
	require.NotEqual(t, "Bearer ck-1", h.Get("Authorization"), "网关 key 不得直通上游")
	require.Empty(t, h.Get("X-Api-Key"), "x-api-key 载体网关 key 不得直通上游（与 Authorization 同列剔除）")
	// Sec-WebSocket-Key：RFC 6455 要求每个客户端握手必带 key——上游看到的必是
	// 网关自身拨号生成的 key（coder Dial 总是重新生成并覆写，见 dial.go
	// Set("Sec-WebSocket-Key")）；客户端连接级 key 由构造保证不可能直通。
	require.NotEmpty(t, h.Get("Sec-WebSocket-Key"), "上游握手必须携带网关自身生成的 key（RFC 6455 硬性要求）")
	require.Len(t, hooks.frames, 3, "上游收到全部客户端帧")
	require.Equal(t, f1, hooks.frames[0])

	// 用量记录：5 计数 + 成功路径。
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(3), lg.InputTokens)
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(8), lg.TotalTokens)
	require.Equal(t, int64(1), lg.CacheReadTokens, "input_tokens_details.cached_tokens")
	require.Equal(t, int64(3), lg.CacheCreationTokens, "cache_creation 两 TTL 桶聚合 2+1")
	require.Equal(t, "gpt-4o", lg.Model)
	require.Equal(t, "", lg.MappedModel)
	require.Equal(t, domain.FormatOpenAIResponsesWS, lg.Format)
	require.Equal(t, int64(1), lg.UserID, "日志归属（鉴权 key 用户）")
	require.Equal(t, int64(1), lg.KeyID)
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
}

// ModelMapping 语义：首帧（response.create）模型改写为映射后模型（1 次 sjson
// 往返，非流式中间帧——与 chat/resp 的 setModel 同构）；日志 Model=请求模型、
// MappedModel=映射后模型。
func TestResponsesWSModelMapping(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
		ModelMapping:     map[string]string{"gpt-4o": "o3"},
	}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ { // created/delta/completed/echo → 关闭
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Len(t, hooks.frames, 1)
	require.Contains(t, hooks.frames[0], `"model":"o3"`, "首帧模型必须改写为映射后模型（上游视角）")
	hooks.mu.Unlock()

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "gpt-4o", store.logs[0].Model, "Model = 客户端请求模型")
	require.Equal(t, "o3", store.logs[0].MappedModel, "MappedModel = 映射后实际模型")
}

// 客户端提前断开：上游已消费请求（已完成 usage 帧已嗅探）→ 200 + ErrAbort
// 记录（token 取断前已嗅探值），不 MarkResult（不冷却无辜账号）。分表路由：
// abort 无计费（cost=0）→ err_logs 双轨豁免行（不入 usage_logs）。
func TestResponsesWSClientAbortRecordsUsage(t *testing.T) {
	up := fakeResponsesWS(t, &fakeWSHooks{frameLimit: 0}) // 不主动关闭，等网关关
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 3; i++ { // created/delta/completed 后立即关闭（echo 不等）
		_ = readResponsesWSFrame(t, c)
	}
	require.NoError(t, c.Close(websocket.StatusNormalClosure, "")) // 客户端主动结束会话

	// relay 感知客户端关闭是异步的：等记录进双轨（rec pending / errlog 队列）
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.logs) >= 1 || p.rec.Pending() > 0 || p.errlog.Queued() > 0
	}, 3*time.Second, 10*time.Millisecond, "relay 必须感知客户端关闭并记录用量")
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 2, "abort 双轨：usage_logs（放行路径 abort）+ err_logs（豁免队列）各一行，request_id 关联")
	lg := store.logs[0]
	require.Equal(t, domain.ErrAbort, lg.ErrorType)
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(3), lg.InputTokens, "断开前已嗅探的 usage 不丢")
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(8), lg.TotalTokens)
	require.Equal(t, lg.RequestID, store.logs[1].RequestID, "双轨行 request_id 关联")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "客户端断开后并发槽必须释放")
}

// 余额预检（WS 路径，与 handleFormat 同判据）：余额负 → 402（握手前 HTTP
// 拒绝，零升级零上游命中）——spec 2026-08-15 语义边界表：快照 <0 拒绝。
func TestResponsesWSInsufficientBalance402(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: -1}}, nil)
	require.NoError(t, bal.Reload(context.Background()), "快照加载（余额 -1）")
	store := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, store, nil)
	writer := &fakeDeductWriter{}
	f := billing.NewFlusher(billing.FlushConfig{
		FlushInterval: time.Hour, BalanceRefreshInterval: time.Hour,
	}, writer, rec, bal, nil)
	p := newTestProxyBillingT3Logs(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, bal, f, rec)

	// 完整升级请求（upgrade 头齐备）：预检在升级处理前拒绝 → 402 而非握手
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	recw := httptest.NewRecorder()
	p.HandleResponsesWS(recw, req)

	require.Equal(t, http.StatusPaymentRequired, recw.Code, "body=%s", recw.Body.String())
	require.Contains(t, recw.Body.String(), "insufficient balance", "402 文案说明余额不足")
	require.Zero(t, hits.Load(), "预检拒绝不得拨号上游（零升级）")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "预检在 Acquire 前：不占用并发槽")
	require.Zero(t, p.rec.Pending(), "预用量拒绝不产生明细 pending")

	require.NoError(t, f.Close(context.Background()))
	writer.mu.Lock()
	require.Empty(t, writer.calls, "预用量拒绝不进 billed flusher（无扣费无计费日志）")
	writer.mu.Unlock()
	require.NoError(t, rec.Close(context.Background()), "Recorder 手动 flush")
}

// 非升级请求 → 400 本地拒绝（无记录，同 invalid JSON 语义）。
func TestResponsesWSRequiresUpgrade400(t *testing.T) {
	up := fakeResponsesWS(t, &fakeWSHooks{})
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatOpenAIResponsesWS)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponsesWS(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "websocket upgrade required")
	require.Zero(t, p.rec.Pending(), "本地拒绝不记录用量")
}

// 模板不支持 resp-ws → 选号失败：升级后发 error 事件帧 + 关闭（WS 无 HTTP
// 状态码，错误语义经事件帧承载），记录 404 + ErrNoAccount。
func TestResponsesWSSelectErrorFrame(t *testing.T) {
	up := fakeResponsesWS(t, &fakeWSHooks{})
	defer up.Close()
	// 模板只支持 chat：resp-ws 无路由 → ErrFormatUnavailable
	_, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIChat, &captureLogStore{})

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	frame := readResponsesWSFrame(t, c)
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(frame, &ev))
	require.Equal(t, "error", ev.Type)
	require.Equal(t, "no account supports this request format", ev.Error.Message)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
}

// 路由分发：AIRouter 的 /v1/responses 按 upgrade 头分流（带 upgrade → resp-ws
// 编排，流式事件直转；无 upgrade → 既有 HTTP responses 处理）。
func TestAIRouterResponsesWSUpgradeDispatch(t *testing.T) {
	up := fakeResponsesWS(t, &fakeWSHooks{frameLimit: 1})
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatOpenAIResponsesWS)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 带 upgrade 头 → WS 流程（事件流经路由直转）
	c := dialResponsesWS(t, srv)
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	first := readResponsesWSFrame(t, c)
	require.Contains(t, string(first), `"type":"response.created"`)
	_ = c.Close(websocket.StatusNormalClosure, "")

	// 无 upgrade 头 → 既有 HTTP responses 流程（上游非 WS 请求 → Accept 400）
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusUpgradeRequired, rec.Code, "非 upgrade 请求不得按 WS 处理")
}

// blackHoleWSServer 黑洞上游：接受 TCP 连接后挂住不回 101（不写任何字节）——
// 模拟 accept 后永不回复的升级上游。Hijack 后读连接直到客户端断开才返回
// （网关拨号超时放弃 → 连接关闭 → 读错误 → 关连接退出），服务端 teardown
// 不被挂死的 handler 阻塞。coder/websocket 库仅 HTTPClient.Timeout>0 才自包
// ctx 超时（dial.go:78-84）——测试客户端 Timeout=0 → 网关拨号 wrap 的
// wsDialTimeout 是唯一 deadline（与生产装配同形）。
func blackHoleWSServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestResponsesWSBlackHoleDialTimeout 黑洞上游握手超时（T3 红绿核心）：上游
// 接受 TCP 但永不回 101 → 修复前拨号无界等待（failover 循环永久阻塞占死并发
// 槽）。修复后 wrapped ctx 超时 → 静态路 code=0 → failoverLoop 判定
// r.Context().Err()==nil（wrapped ctx 取消不向上传播——原 r.Context() 未取消）
// → 不落 499、按连接级 MarkResult(ResultError) 转移 → 下一轮/耗尽。注入短
// wsDialTimeout（50ms，FailoverAttempts=2 → 总耗时 ~100ms 级）：超时转移
// 必须在秒级完成（修复前此用例读帧 5s 超时即红）。
func TestResponsesWSBlackHoleDialTimeout(t *testing.T) {
	old := wsDialTimeout
	wsDialTimeout = 50 * time.Millisecond
	t.Cleanup(func() { wsDialTimeout = old })

	up := blackHoleWSServer(t)
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	start := time.Now()
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "all upstream attempts failed", "WS 耗尽固定文案")
	require.Less(t, time.Since(start), 5*time.Second, "超时转移必须在秒级完成（修复前此用例挂死）")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "黑洞超时 → ResultError 冷却")
	require.Zero(t, ri.Concurrency, "耗尽路径并发槽必须释放")
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNetwork, lg.ErrorType, "code=0 耗尽 → ErrNetwork（非 499/ErrAbort——wrapped ctx 取消不向上传播）")
	require.Equal(t, 0, lg.StatusCode)
	require.NotNil(t, lg.ErrorMessage, "错误文本必须落盘")
	require.Contains(t, *lg.ErrorMessage, "deadline exceeded", "错误文本 = 拨号超时错误原文")
}

// TestResponsesWSHandshakeUnderShortTimeout 短超时下正常握手零变化（T3 行为
// 契约）：注入 200ms 拨号超时（远小于默认 15s，仍 >> 本地握手时长）→ 正常上
// 游完整会话照常（成功记录 + usage 5 计数）——超时上限只咬黑洞上游，正常路径
// 零影响。
func TestResponsesWSHandshakeUnderShortTimeout(t *testing.T) {
	old := wsDialTimeout
	wsDialTimeout = 200 * time.Millisecond
	t.Cleanup(func() { wsDialTimeout = old })

	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ { // created/delta/completed/echo → 关闭
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(3), lg.InputTokens, "短超时下正常会话 usage 嗅探照常")
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(8), lg.TotalTokens)
}

// TestResponsesWSFailoverZeroReleasesSlot 防呆（spec 纵深，与 chat/search 同
// 款）：直构 failover_attempts=0（绕过 validate 的 >=1 下限——测试侧 p.cfg 改
// 写等价直构）时 failover 循环零次执行，首次 Select 已占并发槽——修复前槽永
// 不释放（组内账号耗尽后全组 429 死锁，重启才能恢复）；耗尽路径必须补
// Release。N=0 时 lastCode=0 → ErrNetwork 记录 + 固定错误帧文案。
func TestResponsesWSFailoverZeroReleasesSlot(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{200}, 0) // 循环不执行——上游不会被拨号
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)
	p.cfg.FailoverAttempts = 0 // 直构：绕过 validate 下限

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "all upstream attempts failed", "WS 耗尽固定文案")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	require.Equal(t, 0, hooks.upgradesN(), "N=0 循环零次执行：无上游拨号")
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "failover_attempts=0 首次选号占槽必须释放（防呆 Release）")
	require.Zero(t, p.rec.Pending(), "N=0 耗尽路径失败行不产生明细 pending（err_logs 承载）")
}

// TestResponsesWSDial4xxPassthrough 静态拨号 4xx 透传统一循环分类（行为契约：
// WS 静态拨号 4xx 从循环内搬入循环统一段——finish + 错误帧，emOr 语义保持）：
// 4xx 确定性拒绝不转移（错误帧带上游 body message，无 429 冷却）、Err4xx 记录
// + ErrorMessage 取归一错误文本（上游 body message，非原始 body）、并发槽释放。
func TestResponsesWSDial4xxPassthrough(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{403}, 0)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "upstream rejected", "错误帧取归一错误文本（emOr 语义）")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	require.Equal(t, 1, hooks.upgradesN(), "4xx 确定性拒绝不转移")

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "4xx 不 MarkResult（不冷却）")
	require.Zero(t, ri.Concurrency, "4xx 透传也必须释放并发槽")
	require.NoError(t, p.rec.Close(context.Background()), "Recorder 手动 flush")
	require.NoError(t, p.errlog.Close(context.Background()), "errlog 手动 flush（失败行走 err_logs）")
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.Err4xx, lg.ErrorType)
	require.Equal(t, http.StatusForbidden, lg.StatusCode)
	require.NotNil(t, lg.ErrorMessage, "错误文本必须落盘")
	require.Equal(t, "upstream rejected", *lg.ErrorMessage, "ErrorMessage = 归一错误文本（同错误帧）")
}

// TestResponsesWSDial429Failover 静态拨号 429 → 循环 429 分类（Result429 转移 +
// MarkResult httpStatus 429）：错误文本经 respBody 回传（归一 msg——纯文本经
// 骨架"提取为空直取原文"回退不丢）；耗尽记 Err429 + 状态码 429（WS 无
// Retry-After——固定错误帧文案）。
func TestResponsesWSDial429Failover(t *testing.T) {
	up, _ := newCodexWSUpstream(t, []int{429}, 0)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "all upstream attempts failed", "WS 耗尽固定文案")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.Status429, ri.Status, "429 → Result429 冷却")
	require.Zero(t, ri.Concurrency, "耗尽路径并发槽必须释放")
	require.NoError(t, p.rec.Close(context.Background()), "Recorder 手动 flush")
	require.NoError(t, p.errlog.Close(context.Background()), "errlog 手动 flush（失败行走 err_logs）")
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.Err429, lg.ErrorType)
	require.Equal(t, http.StatusTooManyRequests, lg.StatusCode)
	require.NotNil(t, lg.ErrorMessage, "错误文本必须落盘")
	require.Equal(t, "upstream rejected", *lg.ErrorMessage, "归一错误文本经直取原文回退不丢")
}

// TestResponsesWSDial5xxNormalized 修复性声明（WS 静态拨号 5xx 归一 lastCode）：
// 拨号 5xx → 循环 5xx 分支统一分类——耗尽记录 et=Err5xx + 状态码原样 500 +
// MarkResult httpStatus 5xx（现状 caller_responses_ws.go default 分支归 0 →
// ErrNetwork + httpStatus 0——WS 内部与 codex 5xx 分支不一致；统一后对齐 codex
// 分支与 HTTP 路径，规则 when 匹配面 http_status 恢复真实值）。
func TestResponsesWSDial5xxNormalized(t *testing.T) {
	up, _ := newCodexWSUpstream(t, []int{500}, 0) // 非 200 升级 → 500 拒绝 + JSON 错误体
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "all upstream attempts failed", "WS 耗尽固定文案")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "5xx → ResultError 冷却")
	require.Zero(t, ri.Concurrency, "耗尽路径并发槽必须释放")
	require.NoError(t, p.rec.Close(context.Background()), "Recorder 手动 flush")
	require.NoError(t, p.errlog.Close(context.Background()), "errlog 手动 flush（失败行走 err_logs）")
	store.mu.Lock()
	defer store.mu.Unlock()
	lg := store.logs[0]
	require.Equal(t, domain.Err5xx, lg.ErrorType, "5xx 归一：耗尽记 Err5xx（现状误记 ErrNetwork）")
	require.Equal(t, http.StatusInternalServerError, lg.StatusCode, "5xx 归一：耗尽记录状态码原样 500")
	require.NotNil(t, lg.ErrorMessage, "错误文本必须落盘")
	require.Equal(t, "upstream rejected", *lg.ErrorMessage, "错误文本 = 上游 body message")
}

// --- unit：预筛嗅探逻辑（热路径纪律） ---

func TestSniffResponsesCompleted(t *testing.T) {
	// 命中：completed 帧完整 usage → 5 计数正确
	u, ok := sniffResponsesCompleted([]byte(responsesWSCompletedFrame))
	require.True(t, ok)
	require.Equal(t, usageTuple{it: 3, ot: 5, tt: 8, cr: 1, cc: 3}, u)

	// 未命中：流式中间帧零解析直转（预筛 miss，不触达 gjson）
	_, ok = sniffResponsesCompleted([]byte(`{"type":"response.output_text.delta","delta":"hi"}`))
	require.False(t, ok)

	// 误命中：非 completed 帧内嵌该子串（嵌套 key-value，原始字节可真命中——
	// 字符串内容里的引号恒被转义，不可能误匹配）→ 预筛命中但 response.usage
	// 不存在 → ok=false 不更新（此前值保留）。旧行为解析出零值元组覆盖——
	// completed 终态唯一且恒在流末，最终值由真实 completed 帧覆盖（最后帧
	// 语义），实际等价（spec A-1 连带改写）。
	u, ok = sniffResponsesCompleted([]byte(`{"type":"response.output_text.delta","delta":"hi","meta":{"type":"response.completed"}}`))
	require.False(t, ok)
	require.Zero(t, u, "ok=false 返回零值元组（调用方不更新）")

	// completed 帧但 usage 缺失（error 终态形状）→ ok=false 不更新（不阻塞
	// 采集；此前值保留——completed 终态唯一、元组仅此处写入，此前值恒 0，
	// 与旧行为覆盖 0 等价）。
	u, ok = sniffResponsesCompleted([]byte(`{"type":"response.completed","response":{"id":"r"}}`))
	require.False(t, ok)
	require.Zero(t, u)
}

// TestRelayClassifyCloseFramePriority I-1 分类单元测试（确定性）：上游关闭帧
// 与客户端循环并发写失败（net.ErrClosed）的槽位组合——正常关闭帧恒优先
// （写失败只归因网络错误，无关闭帧时才判错）；客户端断开恒 abort；错误
// 关闭帧/失联恒 ResultError。错误槽兜底优先级 upErr > pingErr > upClose
// （错误关闭帧）。修复前写失败与关闭帧竞争 upErr 首写，先记录即误判
// （健康上游被冷却）。
func TestRelayClassifyCloseFramePriority(t *testing.T) {
	normal := &websocket.CloseError{Code: websocket.StatusNormalClosure}
	goingAway := &websocket.CloseError{Code: websocket.StatusGoingAway}
	errClose := &websocket.CloseError{Code: websocket.StatusInternalError}
	writeFail := net.ErrClosed
	clientClose := &websocket.CloseError{Code: websocket.StatusNormalClosure}
	timeout := errors.New("pong timeout")

	tests := []struct {
		name      string
		upClose   *websocket.CloseError
		upErr     error
		clientErr error
		pingErr   error
		want      relayEnd
		wantErr   error
	}{
		// --- 单槽独占（基线） ---
		{"正常关闭帧独占 → 成功", normal, nil, nil, nil, relayEndUpstreamClosed, nil},
		{"1001 离开帧独占 → 成功", goingAway, nil, nil, nil, relayEndUpstreamClosed, nil},
		{"错误关闭帧独占 → 错误", errClose, nil, nil, nil, relayEndUpstreamError, errClose},
		{"写失败独占 → 错误（归因网络）", nil, writeFail, nil, nil, relayEndUpstreamError, writeFail},
		{"ping 超时独占 → 错误", nil, nil, nil, timeout, relayEndUpstreamError, timeout},
		{"客户端断开独占 → abort", nil, nil, clientClose, nil, relayEndClientAbort, clientClose},

		// --- 正常关闭帧优先于一切（I-1：并发写失败不得推翻关闭帧） ---
		{"正常关闭帧 + 并发写失败 → 成功", normal, writeFail, nil, nil, relayEndUpstreamClosed, nil},

		// --- 错误槽兜底优先级 upErr > pingErr > upClose ---
		{"写失败优先于 ping 超时 → 错误（诊断取 upErr）", nil, writeFail, nil, timeout, relayEndUpstreamError, writeFail},
		{"ping 超时优先于错误关闭帧 → 错误（诊断取 pingErr）", errClose, nil, nil, timeout, relayEndUpstreamError, timeout},
		{"错误关闭帧 + 写失败 → 错误（诊断取 upErr）", errClose, writeFail, nil, nil, relayEndUpstreamError, writeFail},

		// --- 客户端断开分支（仅正常关闭帧可超越） ---
		{"客户端断开 + 写失败 → abort", nil, writeFail, clientClose, nil, relayEndClientAbort, clientClose},
		{"客户端断开 + 错误关闭帧 → abort", errClose, nil, clientClose, nil, relayEndClientAbort, clientClose},
		{"正常关闭帧 + 并发客户端断开 → 成功（流已完成）", normal, nil, clientClose, nil, relayEndUpstreamClosed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			end, endErr := relayClassify(tt.upClose, tt.upErr, tt.clientErr, tt.pingErr)
			require.Equal(t, tt.want, end)
			require.Equal(t, tt.wantErr, endErr)
		})
	}
}

// TestResponsesWSConcurrentWriteClose I-1 端到端竞态复现：上游关闭帧与客户端
// 活跃写帧并发——客户端持续写帧（flood），假上游读满 1 帧后立即流式下发 +
// 发 1000 关闭帧（不再读帧）。网关侧 up-loop 解码关闭帧的同时 client-loop
// 的 up.Write 必然失败（net.ErrClosed）——修复后关闭帧独立槽位 + 分类优先
// → 恒成功（200 ErrNone + 5 计数 usage）；修复前两错误竞争 upErr 首写，
// 写失败先记录即误判 ResultError（健康上游被冷却）。
func TestResponsesWSConcurrentWriteClose(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxy(t, up.URL, domain.FormatOpenAIResponsesWS, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	// 首帧触发上游事件流 + 关闭
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))

	// 持续写帧：制造"客户端循环写失败"与"上游关闭帧"并发（I-1 竞态窗口）
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`))
			cancel()
			if err != nil {
				return // 网关关闭后写失败即停
			}
		}
	}()

	seenCompleted := false
	for i := 0; i < 4; i++ { // created/delta/completed/echo 完整透传
		f := readResponsesWSFrame(t, c)
		if strings.Contains(string(f), `"type":"response.completed"`) {
			seenCompleted = true
		}
	}
	require.True(t, seenCompleted, "response.completed 必须完整透传")
	// 网关必须判成功（1000 关闭帧）——误判 ResultError 时客户端收到 1011
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	<-floodDone

	// 成功分类：ErrNone 200 + 5 计数 usage（并发写失败不得推翻关闭帧）
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType, "正常关闭帧优先 → 成功（不得误判冷却）")
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(3), lg.InputTokens)
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(8), lg.TotalTokens)
	require.Equal(t, int64(1), lg.CacheReadTokens)
	require.Equal(t, int64(3), lg.CacheCreationTokens)
}

// --- WS service_tier 计费接入（首帧 tier 提取 + 策略 + BillingTier 落库） ---

// TestResponsesWSBillingTierFast service_tier=fast（passthrough 默认）：首帧原样
// 透传上游（含字段）；BillingTier="fast" 落库，Cost 按 fast 倍率（260 ≠ auto
// 130）——WS 恒 auto 计费的金额错收修复钉死。
func TestResponsesWSBillingTierFast(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxyBilling(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","service_tier":"fast","input":"hi"}`)))
	for i := 0; i < 4; i++ { // created/delta/completed/echo → 关闭
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Len(t, hooks.frames, 1)
	require.Contains(t, hooks.frames[0], `"service_tier":"fast"`, "passthrough（默认）：首帧原样透传")
	hooks.mu.Unlock()

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "fast", store.logs[0].BillingTier, "WS service_tier=fast → BillingTier=fast 落库")
	require.Equal(t, int64(260), store.logs[0].Cost, "fast ×2.0：130×2 = 260 毫分（与 HTTP 同价）")
}

// TestResponsesWSBillingTierAuto 无 service_tier：BillingTier="auto"（与 HTTP
// 一致，billing_test.go:148 钉死同款语义），Cost 按基础价 130。
func TestResponsesWSBillingTierAuto(t *testing.T) {
	up := fakeResponsesWS(t, &fakeWSHooks{frameLimit: 1})
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxyBilling(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "auto", store.logs[0].BillingTier, "WS 无 service_tier → BillingTier=auto")
	require.Equal(t, int64(130), store.logs[0].Cost, "auto 基础价：130 毫分")
}

// TestResponsesWSBillingTierStrip strip 策略：首帧改写点（relayResponsesWS）删
// service_tier 字段（sjson.DeleteBytes 字节级）——上游帧不含该字段；剥离路径
// 计费照常（tier 已提取 → fast 档 260）。
func TestResponsesWSBillingTierStrip(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxyBilling(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyStrip }, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","service_tier":"fast","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Len(t, hooks.frames, 1)
	require.NotContains(t, hooks.frames[0], "service_tier", "strip 策略：上游帧不得含 service_tier 字段")
	require.Contains(t, hooks.frames[0], `"type":"response.create"`, "其余字段原样保留")
	require.Contains(t, hooks.frames[0], `"input":"hi"`)
	hooks.mu.Unlock()

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "fast", store.logs[0].BillingTier, "剥离路径计费照常（tier 已提取）")
	require.Equal(t, int64(260), store.logs[0].Cost)
}

// TestResponsesWSBillingTierReject reject 策略：Select 前拒绝——客户端收到 error
// 事件帧 + 1000 关闭（同 selectErrorMessage 路径），上游零命中；ErrBilling 走
// err_logs（usage_logs 无明细），拒绝记录带 tier。
func TestResponsesWSBillingTierReject(t *testing.T) {
	hooks := &fakeWSHooks{}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxyBilling(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}},
		func(billing.Tier) billing.TierPolicyMode { return billing.TierPolicyReject }, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","service_tier":"fast","input":"hi"}`)))
	frame := readResponsesWSFrame(t, c)
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(frame, &ev))
	require.Equal(t, "error", ev.Type)
	require.Equal(t, "service_tier rejected by gateway policy", ev.Error.Message)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Empty(t, hooks.frames, "reject 在 Select 前：不得拨号上游")
	hooks.mu.Unlock()
	require.Zero(t, p.rec.Pending(), "预用量拒绝不产生明细 pending")

	// 记录投递与错误帧写出并发：等 enqueue 落地（或已入队）再排空，防 Close
	// 先置 closed 把投递变丢弃（同 abort 双轨测试的 Eventually 模式）。
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.logs) >= 1 || p.errlog.Queued() > 0
	}, 3*time.Second, 10*time.Millisecond, "reject 必须投递 err_logs")
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1, "拒绝行走 err_logs（usage_logs 无明细）")
	require.Equal(t, domain.ErrBilling, store.logs[0].ErrorType)
	require.Equal(t, http.StatusBadRequest, store.logs[0].StatusCode)
	require.Equal(t, "fast", store.logs[0].BillingTier, "拒绝记录带 tier（rm 已注入 ctx）")
}

// TestResponsesWSBillingTierTypeError 类型错误（非 string/null）：400 错误帧 +
// 无记录（同 HTTP caller.go 类型错误 400 无记录语义；ErrBilling 只用于 reject
// 路径），不升级。
func TestResponsesWSBillingTierTypeError(t *testing.T) {
	hooks := &fakeWSHooks{}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	store := &captureLogStore{}
	p, srv := wsTestProxyBilling(t, up.URL, &fakePriceLookup{m: map[string]*domain.Pricing{"gpt-4o": proxyPricing()}}, nil, store)

	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","service_tier":123,"input":"hi"}`)))
	frame := readResponsesWSFrame(t, c)
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(frame, &ev))
	require.Equal(t, "error", ev.Type)
	require.Equal(t, "invalid request body: service_tier must be a string", ev.Error.Message)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Empty(t, hooks.frames, "类型错误不得拨号上游")
	hooks.mu.Unlock()
	require.Zero(t, p.rec.Pending(), "类型错误无记录（同 HTTP 400 无记录语义）")
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Empty(t, store.logs, "类型错误不产生任何记录（usage_logs + err_logs 均无）")
}
