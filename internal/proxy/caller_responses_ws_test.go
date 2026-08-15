// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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
		HTTPHeader:      http.Header{"Authorization": {"Bearer gk-1"}, "X-Client-Version": {"codex-1.2.3"}},
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
	// 透传 + 网关 key 不得泄漏 + hop-by-hop 剔除。
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	require.Len(t, hooks.headers, 1)
	h := hooks.headers[0]
	require.Equal(t, "2026-02-06", h.Get("Responses-Websockets"), "上游握手必须带 beta 头（现役唯一）")
	require.Equal(t, "Bearer sk-upstream", h.Get("Authorization"), "账号鉴权注入")
	require.Equal(t, "codex-1.2.3", h.Get("X-Client-Version"), "客户端头透传")
	require.NotEqual(t, "Bearer gk-1", h.Get("Authorization"), "网关 key 不得直通上游")
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

// 非升级请求 → 400 本地拒绝（无记录，同 invalid JSON 语义）。
func TestResponsesWSRequiresUpgrade400(t *testing.T) {
	up := fakeResponsesWS(t, &fakeWSHooks{})
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatOpenAIResponsesWS)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer gk-1")
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
	req.Header.Set("Authorization", "Bearer gk-1")
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusUpgradeRequired, rec.Code, "非 upgrade 请求不得按 WS 处理")
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
