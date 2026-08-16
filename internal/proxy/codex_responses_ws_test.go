// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
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

// --- T4：codex 路径 resp-ws 本地 mock 上游测试（P3-5 可编程面） ---
// 真实上游不可控面（401 轮转 / 心跳节奏 / 伪装头断言）用本地可编程 mock 覆盖；
// 真实凭据 e2e（happy path / usage 逐字节一致）在 pg_codex_responses_ws_test.go。

// codexWSUpstream codex 路径 mock WS 上游（升级状态按序弹出，耗尽重复最后
// 一步）+ 握手头观测 + 帧观测（首个数据帧后下发事件流 created/delta/
// completed——与 aiclient 路径同帧断言逐字节一致 + 每帧回声）+ 读满
// frameLimit 帧后 1000 关闭帧。
type codexWSUpstream struct {
	mu         sync.Mutex
	upgrades   int
	headers    []http.Header
	frames     []string
	steps      []int
	last       int
	frameLimit int
}

func newCodexWSUpstream(t *testing.T, steps []int, frameLimit int) (*httptest.Server, *codexWSUpstream) {
	t.Helper()
	u := &codexWSUpstream{last: 200, frameLimit: frameLimit}
	if len(steps) > 0 {
		u.steps = steps
		u.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		u.mu.Lock()
		u.upgrades++
		u.headers = append(u.headers, r.Header.Clone())
		status := u.last
		if len(u.steps) > 0 {
			status = u.steps[0]
			u.steps = u.steps[1:]
			u.last = status
		}
		u.mu.Unlock()
		if status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream rejected"}}`))
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
			u.mu.Lock()
			u.frames = append(u.frames, string(msg))
			u.mu.Unlock()
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
			if u.frameLimit > 0 && n >= u.frameLimit {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, u
}

func (u *codexWSUpstream) upgradesN() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrades
}

func (u *codexWSUpstream) header(i int) http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[i]
}

func (u *codexWSUpstream) framesSnapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.frames...)
}

// codexWSRefreshMock refresh 端点 mock（steps 按序弹出耗尽重复最后一步；
// env override 构造期读取——先 Setenv 再建适配层）。
type codexWSRefreshMock struct {
	mu    sync.Mutex
	calls int
	steps []codexUpStep
	last  codexUpStep
}

func newCodexWSRefreshMock(t *testing.T, steps ...codexUpStep) *codexWSRefreshMock {
	t.Helper()
	m := &codexWSRefreshMock{last: codexUpStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		m.steps = steps
		m.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		m.mu.Lock()
		m.calls++
		step := m.last
		if len(m.steps) > 0 {
			step = m.steps[0]
			m.steps = m.steps[1:]
			m.last = step
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		_, _ = w.Write([]byte(step.body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", srv.URL)
	return m
}

func (m *codexWSRefreshMock) callsN() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// codexWSExt 构造带完整身份四元组的 codex-oauth 账号扩展（installation 账号
// 级永久 / session==thread 会话级 / window={thread}:0）。
func codexWSExt(accountID int64, at, rt string) *domain.AccountExt {
	ext := codexOAuthExt(accountID, at, rt)
	sess := "11111111-1111-7111-8111-111111111111"
	thread := "22222222-2222-7222-8222-222222222222"
	win := thread + ":0"
	ext.SessionID = &sess
	ext.ThreadID = &thread
	ext.WindowID = &win
	return ext
}

// newTestCodexWSProxy 构造 codex 类型 resp-ws 测试代理：模板（credType 类型 +
// resp-ws 格式 + gpt-4o）+ 携带 Ext 的账号（同组 10，可多账号）+ 装配适配层
// （统一失效回调走真实 T1 处理链——fakeFailureStore 落库替身 + 真实调度器
// FailAccount 摘除）。模板 BaseURL = mock 上游根（aiclient fullURLOf 拼完整
// responses 端点）。bill 为计费钩子（nil = 计费全关）。
func newTestCodexWSProxy(t *testing.T, credType credential.Type, accounts map[int64]*domain.AccountExt, upstream string, bill *BillingHooks, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credType,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	}
	accs := make(map[int64][]*domain.Account, 1)
	for id, ext := range accounts {
		accs[10] = append(accs[10], &domain.Account{
			ID: id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4, Ext: ext,
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
		GroupKeyRPM:           0, UsageCapture: true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	store := &fakeFailureStore{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: store, Failer: sched, Log: nil})
	// 错误明细 worker（4xx/fatal/network 失败行走 err_logs 分表——routeLog
	// 语义：失败行不入 usage_logs；短 FlushInterval 快速落袋供断言）。loop 用
	// 可取消 ctx（Close 等 loopDone——Background 永不取消会挂死）。
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize: 4096, BatchSize: 100,
		FlushInterval: 20 * time.Millisecond,
	}, logs, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, bill, errlogW)
	p.SetCodex(sdkbridge.NewCodex(failure))
	return p, store
}

// waitStoreLogs 等 store 出现 n 条明细（usage/errlog 双轨异步落袋——终态断言
// 前等待）。
func waitStoreLogs(t *testing.T, store *captureLogStore, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.logs) >= n
	}, 3*time.Second, 20*time.Millisecond, "明细必须落袋（usage/errlog 双轨）")
}

// dialResponsesWSHeaders 测试客户端拨网关（自定义头——伪装冲突面断言用）。
func dialResponsesWSHeaders(t *testing.T, srv *httptest.Server, h http.Header) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	c, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{
		HTTPHeader:      h,
		CompressionMode: websocket.CompressionContextTakeover,
	})
	require.NoError(t, err, "gateway WS 握手必须成功")
	return c
}

// TestCodexWSMockRotateRefreshSuccess 端到端主流程（oauth）：升级 401 → SDK
// 单飞 refresh（真端点 mock）→ 重拨 200 → 完整会话（事件流 + 回声 + 关闭）
// + usage 嗅探计费（5 计数与 aiclient 路径逐字节一致）+ 伪装四元组握手头断
// 言 + 透传面（客户端头保留 / session 头族 + OpenAI-Beta 剔除 / 网关 key 不
// 泄漏）+ 帧内 client_metadata 注入断言。
func TestCodexWSMockRotateRefreshSuccess(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{401, 200}, 3)
	defer up.Close()
	newCodexWSRefreshMock(t, codexUpStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	// 客户端带伪装冲突面头（session 头族 + OpenAI-Beta 旧版本）——必须被剔除
	//（P3-7/P3-8），不能覆盖账号伪装身份；X-Client-Version 正常透传。
	c := dialResponsesWSHeaders(t, srv, http.Header{
		"Authorization":       {"Bearer ck-1"},
		"X-Client-Version":    {"codex-1.2.3"},
		"Session-Id":          {"rogue-session"},
		"Thread-Id":           {"rogue-thread"},
		"X-Client-Request-Id": {"rogue-crid"},
		"OpenAI-Beta":         {"responses_websockets=2025-01-01"},
	})
	defer c.CloseNow()

	f1 := `{"type":"response.create","model":"gpt-4o","input":"hi"}`
	f2 := `{"type":"response.input_text.delta","delta":"typing"}`
	f3 := `{"type":"custom.mid","n":42}`
	for _, f := range []string{f1, f2, f3} {
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(f)))
	}
	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, string(readResponsesWSFrame(t, c)))
	}
	require.Equal(t, `{"type":"response.created","response":{"id":"rsp_ws_1","model":"gpt-4o"}}`, got[0])
	require.Equal(t, `{"type":"response.output_text.delta","delta":"hi"}`, got[1])
	require.Contains(t, got[2], `"type":"response.completed"`)
	require.Contains(t, got[2], `"input_tokens":3`)
	// 回声帧：payload 为客户端帧 + SDK 注入的 client_metadata（帧透传 1:1
	// 语义——注入层成本与真实 codex 客户端一致）
	for i, want := range []string{f1, f2, f3} {
		var echo struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		require.NoError(t, json.Unmarshal([]byte(got[3+i]), &echo))
		require.Equal(t, "echo", echo.Type)
		require.Contains(t, echo.Payload, gjson.Get(want, "type").String(), "帧内容原样透传")
		require.Contains(t, echo.Payload, `"client_metadata"`)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	// 握手面断言（P3-5 伪装头）：两次升级（401 首拨 + 轮转后重拨），账号鉴权
	// 注入（首拨旧 at / 重拨新 at——轮转生效）；网关 key 不泄漏；伪装四元组头
	// = ext 身份（客户端 rogue 头被剔除）；OpenAI-Beta = 网关默认（rogue 被剔
	// 除）；X-Client-Version 透传。
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	require.Equal(t, 2, hooks.upgrades, "401 轮转：首拨 + 重拨一次")
	require.Equal(t, "Bearer at-10", hooks.headers[0].Get("Authorization"), "首拨旧 at")
	require.Equal(t, "Bearer at-new", hooks.headers[1].Get("Authorization"), "轮转后新 at")
	h := hooks.headers[1]
	ext := codexWSExt(10, "", "")
	require.Equal(t, *ext.SessionID, h.Get("Session-Id"), "session-id = 账号 ext 身份（rogue 已剔除）")
	require.Equal(t, *ext.ThreadID, h.Get("Thread-Id"), "thread-id = 账号 ext 身份")
	require.Equal(t, *ext.ThreadID, h.Get("X-Client-Request-Id"), "x-client-request-id 缺省 = thread-id")
	require.Equal(t, *ext.WindowID, h.Get("X-Codex-Window-Id"), "window-id = {thread}:0")
	require.Equal(t, "responses_websockets="+aiclient.ResponsesWSBetaHeader, h.Get("OpenAI-Beta"), "网关默认 beta（rogue 已剔除）")
	require.Equal(t, "codex-1.2.3", h.Get("X-Client-Version"), "非冲突面客户端头透传")
	require.NotEqual(t, "Bearer ck-1", h.Get("Authorization"), "网关 key 不得直通上游")

	// 帧内 client_metadata 注入（伪装四元组帧级面——真实 codex 客户端语义）
	f0 := hooks.frames[0]
	require.Contains(t, f0, `"x-codex-installation-id":"`+ext.InstallationID+`"`)
	require.Contains(t, f0, `"session_id":"`+*ext.SessionID+`"`)
	require.Contains(t, f0, `"thread_id":"`+*ext.ThreadID+`"`)
	require.Contains(t, f0, `"x-codex-window-id":"`+*ext.WindowID+`"`)

	// usage 记录：5 计数 + 成功路径（与 aiclient 路径逐字节同口径）
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
	require.Equal(t, int64(1), lg.CacheReadTokens)
	require.Equal(t, int64(3), lg.CacheCreationTokens)
	require.Equal(t, domain.FormatOpenAIResponsesWS, lg.Format)
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
}

// TestCodexWSRefreshed401Terminal 轮转重连仍 401（DialError.Refreshed → 信封
// 401）：4xx 确定性拒绝透传不转移（错误事件帧 + Err4xx 记录；refresh 恰一次
// ——网关避免双份刷新）。
func TestCodexWSRefreshed401Terminal(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{401, 401}, 0)
	defer up.Close()
	rm := newCodexWSRefreshMock(t, codexUpStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	// 错误事件帧（含 401 文案）+ 关闭
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "401")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Equal(t, 2, hooks.upgrades, "轮转重拨一次（Refreshed 语义）")
	hooks.mu.Unlock()
	require.Equal(t, 1, rm.callsN(), "refresh 恰一次——网关不再二次刷新（避免双份）")
	// 4xx 失败行走 err_logs 分表（routeLog 语义：失败行不入 usage_logs）——
	// errlog worker 落袋后断言
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.Err4xx, store.logs[0].ErrorType)
	require.Equal(t, http.StatusUnauthorized, store.logs[0].StatusCode)
}

// TestCodexWSFatalNoTransfer 裸 fatal（refresh 判死 invalid_grant）→ 统一回
// 调上报（账号失效剔除）+ **该请求不转移**（P3-2）：双账号池中健康账号不被
// 触达（升级恰 1 次）；客户端收错误帧；失效上报恰一次（account 10）。
func TestCodexWSFatalNoTransfer(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{401}, 0)
	defer up.Close()
	newCodexWSRefreshMock(t, codexUpStep{status: 401, body: `{"error":"invalid_grant"}`})
	store := &captureLogStore{}
	p, recorder := newTestCodexWSProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{
		10: codexWSExt(10, "at-10", "rt-10"),
		20: codexWSExt(20, "at-20", "rt-20"), // 健康账号——不得被触达
	}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "refresh 被拒绝")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Equal(t, 1, hooks.upgrades, "该请求不转移——另一账号（健康）不得被触达")
	hooks.mu.Unlock()
	require.Equal(t, 1, recorderCalls(recorder), "统一回调恰一次")
	_, acc, _ := recorder.snapshot()
	require.Contains(t, []int64{10, 20}, acc, "失效上报归属被拨号账号（权重序列首个）")
	// 失效联动：调度器快照摘除（FailAccount → disabled）
	ri, ok := p.sched.Runtime(acc)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "fatal → 账号失效摘除")
	// 健康账号未占用并发槽（从未被选号）
	var other int64
	if acc == 10 {
		other = 20
	} else {
		other = 10
	}
	ro, ok := p.sched.Runtime(other)
	require.True(t, ok)
	require.Zero(t, ro.Concurrency, "健康账号未被选号（不转移）")
	// fatal 收尾记 code 0 ErrNetwork（失败行走 err_logs 分表）
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.ErrNetwork, store.logs[0].ErrorType, "fatal 收尾记 code 0 ErrNetwork")
	require.Equal(t, 0, store.logs[0].StatusCode)
}

// TestCodexWSRefreshErrorFailover 裸 RefreshError（refresh 5xx 耗尽）→ 正常
// failover：账号 10 连接级转移 → 账号 20 轮转成功 → 完整会话。
func TestCodexWSRefreshErrorFailover(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{401, 401, 200}, 3)
	defer up.Close()
	// 账号 10 的 refresh 三次尝试全 500（退避耗尽），账号 20 的 refresh 200
	newCodexWSRefreshMock(t,
		codexUpStep{status: 500, body: `{}`},
		codexUpStep{status: 500, body: `{}`},
		codexUpStep{status: 500, body: `{}`},
		codexUpStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`},
	)
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{
		10: codexWSExt(10, "at-10", "rt-10"),
		20: codexWSExt(20, "at-20", "rt-20"),
	}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	for _, f := range []string{
		`{"type":"response.create","model":"gpt-4o","input":"hi"}`,
		`{"type":"response.input_text.delta","delta":"typing"}`,
		`{"type":"custom.mid","n":42}`,
	} {
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(f)))
	}
	// 事件流 3 帧 + 回声 3 帧（读满 6——上游 frameLimit=3 后 1000 关闭）
	for i := 0; i < 6; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	require.Equal(t, 3, hooks.upgrades, "账号 A 首拨 401 + 账号 B 首拨 401 + 轮转重拨 200")
	// 两账号先拨顺序不保证（快照 map 迭代序）——断言集合语义
	a0 := hooks.headers[0].Get("Authorization")
	a1 := hooks.headers[1].Get("Authorization")
	require.Contains(t, []string{"Bearer at-10", "Bearer at-20"}, a0)
	require.Contains(t, []string{"Bearer at-10", "Bearer at-20"}, a1)
	require.NotEqual(t, a0, a1, "两账号各拨一次（均 401）")
	require.Equal(t, "Bearer at-new", hooks.headers[2].Get("Authorization"), "第二轮账号 refresh 后新 at")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType, "failover 后成功")
}

// TestCodexWSDeathFrameFatal WS 业务判死事件帧（T5 §3 唯一跨边界点）：
// token_invalidated 错误帧 → 适配层 FatalAuth（Auth.Fatal 毒化 + 统一失效
// 回调单次上报——写 failed_at + StatusDisabled，共用 T1 处理函数）；判死帧
// 照常透传客户端；会话随后 1000 正常收尾。
func TestCodexWSDeathFrameFatal(t *testing.T) {
	// 定制上游：首客户端帧后下发判死事件帧 + created + completed + 回声，
	// 3 帧后 1000 关闭（codexWSUpstream 固定事件流不含判死帧——独立 handler）
	mu := &codexWSUpstream{last: 200, frameLimit: 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		mu.mu.Lock()
		mu.upgrades++
		mu.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
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
			mu.mu.Lock()
			mu.frames = append(mu.frames, string(msg))
			mu.mu.Unlock()
			if !streamed {
				streamed = true
				for _, f := range []string{
					// 判死事件帧（error.code = token_invalidated——SDK 判死码集）
					`{"type":"error","error":{"code":"token_invalidated","message":"access token invalidated","param":null,"type":"token_invalidated"}}`,
					`{"type":"response.created","response":{"id":"rsp_ws_1","model":"gpt-4o"}}`,
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
			if n >= mu.frameLimit {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	defer srv.Close()
	store := &captureLogStore{}
	p, recorder := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, srv.URL, nil, store)

	s := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer s.Close()
	c := dialResponsesWS(t, s)
	defer c.CloseNow()
	for _, f := range []string{
		`{"type":"response.create","model":"gpt-4o","input":"hi"}`,
		`{"type":"response.input_text.delta","delta":"typing"}`,
		`{"type":"custom.mid","n":42}`,
	} {
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(f)))
	}
	// 事件流（判死帧 + created + completed）+ 3 回声——判死帧照常透传客户端
	//（错误事件属业务流），上游 3 帧后 1000 关闭
	got := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		got = append(got, string(readResponsesWSFrame(t, c)))
	}
	require.Contains(t, got[0], `"type":"error"`)
	require.Contains(t, got[0], `"token_invalidated"`, "判死帧透传客户端")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	// 失效上报恰一次（T5 §3 显式 FailureHandler——共用 T1 处理函数）
	require.Equal(t, 1, recorderCalls(recorder), "帧判死 → 统一回调恰一次")
	_, acc, reason := recorder.snapshot()
	require.Equal(t, int64(10), acc)
	require.Contains(t, reason, "token_invalidated", "last_error 留痕失效原因摘要")
	// 调度摘除持久化面：快照置 disabled
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "帧判死 → 账号失效摘除")
	// 会话正常收尾（上游 1000 → 成功）
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestSniffCodexWSDeath sniff 纯函数：判死码集语义（error.code/error.type ∈
// {token_invalidated, token_revoked}——值大小写不敏感，复用 SDK 判死码集
// auth_errors.go:28-31 + classifyAT401 字段路径）；type=error 帧前提；非判死
// 错误事件/业务帧误含错误帧标记 → nil（不误判死）。
func TestSniffCodexWSDeath(t *testing.T) {
	// error.code 命中（判死码值大写形态同样命中——解析层 EqualFold）
	hit := sniffCodexWSDeath([]byte(`{"type":"error","error":{"code":"token_invalidated","message":"x"}}`))
	require.NotNil(t, hit)
	require.Equal(t, "token_invalidated", hit.Code)
	require.Contains(t, string(hit.Raw), "token_invalidated", "Raw 原帧保留（诊断用）")
	// error.type 命中（token_revoked）
	hit2 := sniffCodexWSDeath([]byte(`{"type":"error","error":{"message":"y","type":"token_revoked"}}`))
	require.NotNil(t, hit2)
	require.Equal(t, "token_revoked", hit2.Code)
	// 判死码值任意大小写 → 命中（EqualFold 语义与 SDK classifyAT401 一致）
	hit3 := sniffCodexWSDeath([]byte(`{"type":"error","error":{"code":"TOKEN_INVALIDATED"}}`))
	require.NotNil(t, hit3)
	require.Equal(t, "token_invalidated", hit3.Code)
	// 非错误帧（业务内容误含错误帧标记）→ 不判死
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"response.output_text.delta","delta":"token_invalidated"}`)))
	// 非判死错误事件（server_error 等业务错误透传不判死）
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"error","error":{"code":"server_error","message":"x"}}`)))
	// 普通帧 / 空 / 非 JSON
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"response.created","response":{"id":"r"}}`)))
	require.Nil(t, sniffCodexWSDeath(nil))
	require.Nil(t, sniffCodexWSDeath([]byte("binary\x00token_invalidated")))
}

// TestCodexWSAdapterMissing 适配层未装配（SetCodex nil）→ 501 语义显式拒绝
// （错误帧 + 不上游接触——防 nil 误走凭据缺失 502）。
func TestCodexWSAdapterMissing(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{200}, 0)
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: {AccountID: 10, CredentialType: credential.TypeCodexPAT, InstallationID: "i"}}, up.URL, nil, store)
	p.SetCodex(nil) // 模拟 main 未装配

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), "codex responses unavailable")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	require.Equal(t, 0, hooks.upgrades, "501 本地拒绝——无上游接触")
}

// --- 心跳单源（30s 节奏——本地可编程面） ---

// manualWSPingUpstream 手动 WS 服务端（心跳计数专用）：coder/websocket 的
// ping 帧由库内部自动回 pong、不向应用面暴露（read.go readLoop→handleControl），
// 帧级计数需手动解析：Hijack + 101 + 逐帧读（掩码解包）→ ping 计数/时间戳 +
// 回 pong + 数据帧回声 + 关闭帧回执。
type manualWSPingUpstream struct {
	mu        sync.Mutex
	pings     []time.Time
	closeCode int
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func newManualWSPingUpstream(t *testing.T) (*httptest.Server, *manualWSPingUpstream) {
	t.Helper()
	m := &manualWSPingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		h := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
		accept := base64.StdEncoding.EncodeToString(h[:])
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")
		for {
			var hdr [2]byte
			if _, err := io.ReadFull(conn, hdr[:]); err != nil {
				return
			}
			opcode := hdr[0] & 0x0f
			n := uint64(hdr[1] & 0x7f)
			if n == 126 {
				var ext [2]byte
				if _, err := io.ReadFull(conn, ext[:]); err != nil {
					return
				}
				n = uint64(binary.BigEndian.Uint16(ext[:]))
			} else if n == 127 {
				var ext [8]byte
				if _, err := io.ReadFull(conn, ext[:]); err != nil {
					return
				}
				n = binary.BigEndian.Uint64(ext[:])
			}
			var mask [4]byte
			if hdr[1]&0x80 != 0 {
				if _, err := io.ReadFull(conn, mask[:]); err != nil {
					return
				}
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(conn, payload); err != nil {
				return
			}
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
			switch opcode {
			case 0x9: // ping → 计数 + 回 pong（payload 必须回显——coderws Ping
				// 按 payload 匹配 activePings conn.go:328-332）
				m.mu.Lock()
				m.pings = append(m.pings, time.Now())
				m.mu.Unlock()
				_ = writeManualFrame(conn, 0x0A, payload)
			case 0x8: // close → 回执
				m.mu.Lock()
				m.closeCode = int(binary.BigEndian.Uint16(payload))
				m.mu.Unlock()
				_ = writeManualFrame(conn, 0x08, payload)
				return
			case 0x1: // text → 回声
				_ = writeManualFrame(conn, 0x01, payload)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, m
}

func (m *manualWSPingUpstream) pingsSnapshot() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.pings...)
}

// writeManualFrame 手动 WS 帧写出（服务端侧，不掩码）：7-bit 直接长度 +
// 126 扩展形态（测试帧 < 65536）。注意不能 byte(len) 直写——≥128 字节时
// 最高位即 MASK 位，客户端解析错位。
func writeManualFrame(conn net.Conn, opcode byte, payload []byte) error {
	hdr := make([]byte, 0, 4+len(payload))
	hdr = append(hdr, 0x80|opcode)
	if len(payload) < 126 {
		hdr = append(hdr, byte(len(payload)))
	} else {
		hdr = append(hdr, 126, byte(len(payload)>>8), byte(len(payload)))
	}
	hdr = append(hdr, payload...)
	_, err := conn.Write(hdr)
	return err
}

// TestCodexWSHeartbeatCadence 心跳单源节奏：编排层 30s（测试缩短 200ms）节奏
// 发 WS ping——SDK 已 WithPingInterval(0) 禁内部心跳（单一所有者），仅编排层
// ticker；pong 由上游自动回（Ping 解除阻塞）；节奏断言（间隔 ≈ 200ms，无双
// 源翻倍速率）。
func TestCodexWSHeartbeatCadence(t *testing.T) {
	up, m := newManualWSPingUpstream(t)
	pat := "pat-1"
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: {AccountID: 10, CredentialType: credential.TypeCodexPAT, InstallationID: "i", PATKey: &pat}}, up.URL, nil, store)
	p.wsHeartbeatInterval = 200 * time.Millisecond // 测试缩短观测

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	// 排空回声帧（背景读——注意：coderws Read 的 ctx 过期会拆连接，绝不能用
	// 短超时轮询；用无超时背景读 + 连接关闭自然退出）
	drainDone := make(chan struct{})
	defer close(drainDone)
	go func() {
		for {
			select {
			case <-drainDone:
				return
			default:
			}
			if _, _, err := c.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	// 等 ≥3 个 ping（窗口 2s——200ms 节奏 3 个需 ~600ms；慢机余量足）
	deadline := time.Now().Add(2 * time.Second)
	for len(m.pingsSnapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	pings := m.pingsSnapshot()
	require.Len(t, pings, 3, "编排层心跳必须按节奏到达（200ms×3）")
	require.LessOrEqual(t, len(pings), 8, "无双源翻倍速率（SDK 心跳已禁）")
	// 间隔断言：≈200ms 节奏（[50ms, 500ms]——慢机 ticker 抖动余量）
	for i := 1; i < len(pings); i++ {
		gap := pings[i].Sub(pings[i-1])
		require.GreaterOrEqual(t, gap, 50*time.Millisecond, "无翻倍速率（双源）")
		require.LessOrEqual(t, gap, 500*time.Millisecond, "节奏不塌缩")
	}
	// 会话正常收尾（客户端关闭 → abort 记录——与 aiclient 路径同语义；上游
	// 关闭传播帧为 no-op：relayCancel 已先拆 SDK 连接，与 aiclient 路径行为
	// 等价——该面不单独断言）。relay 收尾异步（client-loop 错误传播 → 分类 →
	// finish → 槽释放）：等槽释放（relay 完成）后关 recorder 排空再断言。
	// 注：abort 双轨（usage_logs 放行 + err_logs 错误明细）——store 可能有 2
	// 条，按 error_type 匹配断言。
	c.CloseNow()
	require.Eventually(t, func() bool {
		ri, ok := p.sched.Runtime(10)
		return ok && ri.Concurrency == 0
	}, 3*time.Second, 20*time.Millisecond, "relay 必须完成收尾（槽释放）")
	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	var found bool
	for _, l := range store.logs {
		if l.ErrorType == domain.ErrAbort {
			found = true
			require.Equal(t, http.StatusOK, l.StatusCode)
		}
	}
	require.True(t, found, "客户端关闭 → abort 记录（usage/errlog 任一轨）")
}

// --- 纯函数单测 ---

// TestCodexWSPassthroughHeaders 透传面单测：hop-by-hop/网关 key 剔除（复用
// wsPassthroughHeaders）+ session 头族 + OpenAI-Beta 剔除（P3-7/P3-8）；其余
// 原样。
func TestCodexWSPassthroughHeaders(t *testing.T) {
	h := http.Header{
		"Connection":          {"upgrade"},
		"Sec-Websocket-Key":   {"k"},
		"Authorization":       {"Bearer ck-1"},
		"Session-Id":          {"s"},
		"Thread-Id":           {"t"},
		"X-Client-Request-Id": {"c"},
		"X-Codex-Window-Id":   {"w"},
		"OpenAI-Beta":         {"responses_websockets=2025-01-01"},
		"X-Client-Version":    {"codex-1.2.3"},
		"User-Agent":          {"ua"},
	}
	out := codexWSPassthroughHeaders(h)
	require.Empty(t, out.Get("Connection"))
	require.Empty(t, out.Get("Sec-Websocket-Key"))
	require.Empty(t, out.Get("Authorization"))
	require.Empty(t, out.Get("Session-Id"))
	require.Empty(t, out.Get("Thread-Id"))
	require.Empty(t, out.Get("X-Client-Request-Id"))
	require.Empty(t, out.Get("X-Codex-Window-Id"))
	require.Empty(t, out.Get("OpenAI-Beta"))
	require.Equal(t, "codex-1.2.3", out.Get("X-Client-Version"))
	require.Equal(t, "ua", out.Get("User-Agent"))
}

// TestCodexIdentityFromExt 伪装四元组组装：ext 身份 → Session/CodexMeta 映射
// （session==thread / window={thread}:0 / installation 帧级）；缺列 → 空值。
func TestCodexIdentityFromExt(t *testing.T) {
	ext := codexWSExt(10, "", "")
	sess, meta := codexIdentityFromExt(ext)
	require.Equal(t, *ext.SessionID, sess.SessionID)
	require.Equal(t, *ext.ThreadID, sess.ThreadID)
	require.Equal(t, *ext.WindowID, sess.WindowID)
	require.Empty(t, sess.ClientRequestID, "SDK 缺省回退 thread-id")
	require.Equal(t, ext.InstallationID, meta.InstallationID)
	require.Equal(t, *ext.SessionID, meta.SessionID)
	require.Equal(t, *ext.ThreadID, meta.ThreadID)
	require.Equal(t, *ext.WindowID, meta.WindowID)

	emptySess, emptyMeta := codexIdentityFromExt(nil)
	require.Zero(t, emptySess)
	require.Zero(t, emptyMeta)
	// 缺列（旧数据）→ 空值不注入
	partial := &domain.AccountExt{AccountID: 10, CredentialType: credential.TypeCodexOAuth, InstallationID: "inst-1"}
	s2, m2 := codexIdentityFromExt(partial)
	require.Empty(t, s2.SessionID)
	require.Empty(t, m2.ThreadID)
	require.Equal(t, "inst-1", m2.InstallationID)
}
