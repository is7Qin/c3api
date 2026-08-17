// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
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

// --- T6：codex 类型 resp HTTP 路径本地 mock 上游测试 ---
// 真实上游不可控面（401 轮转 / 信封 / 帧规格）用本地可编程 mock 覆盖；真实凭据
// e2e（happy path / 计费落库）在 pg_codex_responses_http_test.go。

// T6 事件 fixture（对齐 codex-sdk responses_test.go：created/item.done/completed
// 形状；usage 顶层五计数含 cache 明细——P1-1 双路径断言共用）。SDK 聚合器从
// output_item.done 事件提取 item 对象（合成体 output 只含 item——t6RespItem）。
const (
	t6RespCreated = `{"type":"response.created","response":{"id":"resp_t6","object":"response","status":"in_progress","model":"gpt-5.6"}}`
	t6RespItem    = `{"id":"msg_1","status":"completed","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}`
	t6RespItemEv  = `{"type":"output_item.done","item":` + t6RespItem + `}`
	t6RespUsage   = `{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}`
	t6RespDone    = `{"type":"response.completed","response":{"id":"resp_t6","object":"response","status":"completed"},"usage":` + t6RespUsage + `}`
)

// codexHTTPUpstream codex 类型 resp HTTP 路径 mock 上游（/v1/responses 端点）：
// 记录鉴权头/请求体/turn-state 请求头；步骤按序弹出（耗尽重复最后一步）——
// 200 步 → 逐 events 发 SSE data: 行 + [DONE]；非 200 → JSON 错误体；步骤
// turnState 非空 → 200 响应头签发 x-codex-turn-state（HOST-2 断言面）。
type codexHTTPUpstream struct {
	mu         sync.Mutex
	calls      int
	auths      []string
	bodies     [][]byte
	turnStates []string // 每次请求的 x-codex-turn-state 请求头
	steps      []codexHTTPStep
	last       codexHTTPStep
}

type codexHTTPStep struct {
	status int
	events []string // SSE data 载荷（status==200 时逐行下发 + [DONE]）
	body   string   // 非 200 错误体
	// turnState 响应头签发值（非空 → 200 响应携带 x-codex-turn-state——
	// HOST-2 mock 上游签发面）。
	turnState string
}

func newCodexHTTPUpstream(t *testing.T, steps ...codexHTTPStep) (*httptest.Server, *codexHTTPUpstream) {
	t.Helper()
	c := &codexHTTPUpstream{last: codexHTTPStep{status: 500, body: `{}`}}
	if len(steps) > 0 {
		c.steps = steps
		c.last = steps[len(steps)-1]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.calls++
		c.auths = append(c.auths, r.Header.Get("Authorization"))
		c.bodies = append(c.bodies, b)
		c.turnStates = append(c.turnStates, r.Header.Get("x-codex-turn-state"))
		step := c.last
		if len(c.steps) > 0 {
			step = c.steps[0]
			c.steps = c.steps[1:]
			c.last = step
		}
		c.mu.Unlock()
		if step.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(step.status)
			_, _ = w.Write([]byte(step.body))
			return
		}
		if step.turnState != "" {
			w.Header().Set("x-codex-turn-state", step.turnState)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range step.events {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func (c *codexHTTPUpstream) callsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *codexHTTPUpstream) auth(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auths[i]
}

func (c *codexHTTPUpstream) body(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[i]
}

func (c *codexHTTPUpstream) turnState(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnStates[i]
}

// uuidv7Re UUIDv7 格式（8-4-4-4-12 十六进制，version 位 = 7——SDK NewUUIDv7
// 产物；client_metadata.turn_id 断言面，META-2）。
var uuidv7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isUUIDv7(s string) bool { return uuidv7Re.MatchString(s) }

// codexPATExt 构造 codex-pat 账号扩展（PAT 静态凭据——无 oauth 列）。
func codexPATExt(accountID int64, pat string) *domain.AccountExt {
	return &domain.AccountExt{
		AccountID: accountID, CredentialType: credential.TypeCodexPAT, PATKey: &pat,
	}
}

// newTestCodexRespProxy 构造 codex 类型 resp HTTP 测试代理：模板（credType 类
// 型 + openai-responses 格式 + gpt-4o；mapping = 模板 ModelMapping——nil = 无
// 映射）+ 携带 Ext 的账号（同组 10，可多账号）+ 装配适配层（统一失效回调走
// 真实 T1 处理链——fakeFailureStore 落库替身 + 真实调度器 FailAccount 摘除）。
// 模板 BaseURL = mock 上游根（ResponsesWSURL 拼完整 /v1/responses 端点）。
// bill 为计费钩子（nil = 计费全关）。
func newTestCodexRespProxy(t *testing.T, credType credential.Type, accounts map[int64]*domain.AccountExt, upstream string, mapping map[string]string, bill *BillingHooks, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credType,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
		ModelMapping:     mapping,
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
	// 语义：失败行不入 usage_logs；短 FlushInterval 快速落袋供断言）。
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

// postResponses 向网关发 /v1/responses 请求（Bearer ck-1），返回响应。
func postResponses(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// splitSSEFrames 把 `data: X\n\n` 帧流拆成 X 载荷序列（测试辅助；fixture 内无
// \n\n 嵌入）。
func splitSSEFrames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.TrimPrefix(line, "data: "))
	}
	return out
}

// TestCodexResponsesMockNonstreamComposite 非流式主流程（oauth + ModelMapping）：
// SDK 合成体透传（网关侧断言）+ setModel 改写落位（wire model = 映射模型）+
// stream:true 注入 + 顶层 usage 五计数（P1-1 非流式路径）+ cred 传递（Bearer
// at-10）。
func TestCodexResponsesMockNonstreamComposite(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, map[string]string{"gpt-4o": "gpt-5.6"}, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	want := `{"id":"resp_t6","object":"response","status":"completed","output":[` + t6RespItem + `],"usage":` + t6RespUsage + `}`
	require.Equal(t, want, string(b), "合成体透传（id/output 流序/usage 原样）")

	// wire 断言：setModel 改写（映射 gpt-4o → gpt-5.6 落位——P2-2）+ stream:true
	// 注入（SDK 无条件覆盖）+ 其余字段保留 + 凭据传递
	upc.mu.Lock()
	defer upc.mu.Unlock()
	require.Equal(t, 1, upc.calls)
	require.Equal(t, "Bearer at-10", upc.auths[0], "cred 传递断言（oauth 初始 at）")
	require.Equal(t, "gpt-5.6", gjson.GetBytes(upc.bodies[0], "model").String(), "setModel 改写生效（ModelMapping 对 codex 账号不静默失效）")
	if !gjson.GetBytes(upc.bodies[0], "stream").Bool() {
		t.Fatalf("非流式 wire 必须 stream:true（SDK 注入）, body = %s", upc.bodies[0])
	}
	require.Equal(t, "hi", gjson.GetBytes(upc.bodies[0], "input").String(), "注入不应动其余字段")

	// META-2 伪装身份注入：installation_id（account_ext 持久化——codexOAuthExt
	// 带 installation 无 session/thread/window 列 → 缺列不注入）+ turn_id 自动
	// UUIDv7（SDK 恒带面）。
	cm := gjson.GetBytes(upc.bodies[0], "client_metadata")
	require.Equal(t, "inst-"+strings.Repeat("0", 32), cm.Get("x-codex-installation-id").String(), "installation_id（ext 持久化）注入")
	require.True(t, isUUIDv7(cm.Get("turn_id").String()), "turn_id UUIDv7 格式（SDK 自动）")
	require.False(t, cm.Get("session_id").Exists(), "缺列（session/thread/window）不注入")

	// usage 断言（P1-1 非流式：合成体 Raw 顶层解析）
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(10), lg.InputTokens)
	require.Equal(t, int64(20), lg.OutputTokens)
	require.Equal(t, int64(30), lg.TotalTokens)
	require.Equal(t, int64(2), lg.CacheReadTokens)
	require.Equal(t, int64(4), lg.CacheCreationTokens)
	require.Equal(t, domain.FormatOpenAIResponses, lg.Format)
	require.Equal(t, "gpt-4o", lg.Model)
	require.Equal(t, "gpt-5.6", lg.MappedModel, "映射后模型落 MappedModel")
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
}

// TestCodexResponsesNonstreamTimeout 非流式超时（B-P2-7）：黑洞上游（接受请求
// 永不回体）→ UpstreamTimeout 包 ctx 超时触发 → 连接级错误转移 → 耗尽 502；
// 请求在超时窗口内完成（TCP 黑洞读停滞不无限挂起——并发槽/连接/goroutine 不
// 被占用，failover 可转移）。
func TestCodexResponsesNonstreamTimeout(t *testing.T) {
	// release 放行黑洞 handler 返回（服务端不可观测客户端断连——Go http 服务
	// 端 handler 阻塞期不处理 FIN，r.Context() 不触发；客户端侧连接已被 transport
	// 关闭，非泄漏）。
	release := make(chan struct{})
	defer close(release)
	blackhole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(blackhole.Close)
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		blackhole.URL, nil, nil, store)
	p.cfg.UpstreamTimeout = 200 * time.Millisecond // 缩短超时窗口（fixture 默认 5s）

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	start := time.Now()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Less(t, time.Since(start), 5*time.Second, "黑洞上游必须超时返回（不无限挂起）")
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "超时 → 连接级 → 耗尽 502")
	require.Contains(t, string(b), "all upstream attempts failed")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.ErrNetwork, store.logs[0].ErrorType, "超时收尾记 code 0 ErrNetwork")
	require.Zero(t, store.logs[0].StatusCode)
}

// TestCodexResponsesMockStreamPassthrough 流式主流程（PAT 静态直连）：客户端
// 帧规格（P2-1）——每载荷重帧 `data: <payload>\n\n`、event: 行不出现、流末补发
// data: [DONE]；usage 顶层嗅探（P1-1 流式路径：completed 帧五计数）；stream:
// true 原样透传（wire 保持客户端值）；PAT cred 传递。
func TestCodexResponsesMockStreamPassthrough(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	body := string(b)
	require.NotContains(t, body, "event:", "event: 行不出现（SDK 交付载荷重帧——P2-1 帧规格）")
	require.Equal(t, []string{t6RespCreated, t6RespItemEv, t6RespDone, "[DONE]"},
		splitSSEFrames(body), "逐载荷重帧 + 流末补发 [DONE]")

	// wire 断言：stream:true 原样透传（客户端已带）+ PAT 凭据
	upc.mu.Lock()
	defer upc.mu.Unlock()
	require.Equal(t, 1, upc.calls)
	require.Equal(t, "Bearer pat-10", upc.auths[0], "PAT 静态直连（cred 传递断言）")
	if !gjson.GetBytes(upc.bodies[0], "stream").Bool() {
		t.Fatalf("客户端已带 stream:true 应原样透传, body = %s", upc.bodies[0])
	}
	require.Equal(t, "gpt-4o", gjson.GetBytes(upc.bodies[0], "model").String(), "未映射 → 模型不改写")
	// 未配置 identity（codexPATExt 无身份列）→ 仍恒带 turn_id（META-1 最小面）
	cm := gjson.GetBytes(upc.bodies[0], "client_metadata")
	require.True(t, isUUIDv7(cm.Get("turn_id").String()), "未配置 identity 仍注入自动 turn_id")
	require.False(t, cm.Get("x-codex-installation-id").Exists(), "未配置不注入静态键")

	// usage 断言（P1-1 流式：fn 嗅探 response.completed 帧顶层 usage）
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(10), lg.InputTokens)
	require.Equal(t, int64(20), lg.OutputTokens)
	require.Equal(t, int64(30), lg.TotalTokens)
	require.Equal(t, int64(2), lg.CacheReadTokens)
	require.Equal(t, int64(4), lg.CacheCreationTokens)
	require.Equal(t, domain.FormatOpenAIResponses, lg.Format)
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "成功路径必须释放并发槽")
}

// t6RespCallEv 工具调用输出事件 fixture（HOST-2 轮边界判定面——item.type
// function_call → 轮继续信号）。
const t6RespCallEv = `{"type":"output_item.done","item":{"id":"call_1","status":"completed","type":"function_call","name":"shell","arguments":"{}","call_id":"call_1"}}`

// postResponsesTS 向网关发 /v1/responses 请求并携带 x-codex-turn-state 头
//（HOST-2 透传优先断言面；空 = 不带头）。
func postResponsesTS(t *testing.T, srv *httptest.Server, body, turnState string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	if turnState != "" {
		req.Header.Set("x-codex-turn-state", turnState)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestCodexResponsesTurnStateCarryAndClear turn-state 头回传（HOST-2——非流式
// 路径）：轮首请求未带 → 上游签发 ts-1 → held；同轮后续（响应含工具调用——
// 轮继续）自动注入 x-codex-turn-state；轮结束（completed 无工具调用）→ 清除
// → 跨轮不回传。对齐真实 codex 轮级实例语义（client.rs:498 new_session +
// stream_events_utils.rs needs_follow_up + 真实测试 turn_state.rs）。
func TestCodexResponsesTurnStateCarryAndClear(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t,
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespCallEv, t6RespDone}, turnState: "ts-1"},
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}},
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 轮首：未带 turn-state → 上游不收到头；响应签发 ts-1 + 工具调用（轮继续）
	resp := postResponsesTS(t, srv, `{"model":"gpt-4o","input":"hi"}`, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(b), `"type":"function_call"`, "工具调用项透传（轮继续信号）")

	// 同轮后续：自动注入 held（ts-1）；响应无签发（请求已带）→ held 保持
	resp = postResponsesTS(t, srv, `{"model":"gpt-4o","input":"hi"}`, "")
	defer resp.Body.Close()
	b, _ = io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// 轮结束（completed 无工具调用）→ 清除 → 跨轮不回传
	resp = postResponsesTS(t, srv, `{"model":"gpt-4o","input":"hi"}`, "")
	defer resp.Body.Close()
	b, _ = io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	upc.mu.Lock()
	defer upc.mu.Unlock()
	require.Equal(t, 3, upc.calls)
	require.Equal(t, "", upc.turnStates[0], "轮首请求不带头")
	require.Equal(t, "ts-1", upc.turnStates[1], "同轮续传（注入 held）")
	require.Equal(t, "", upc.turnStates[2], "轮结束清除——跨轮不回传")
}

// TestCodexResponsesTurnStateStreamCarryAndClear turn-state 头回传（HOST-2——
// 流式路径）：与 TestCodexResponsesTurnStateCarryAndClear 同语义序列（轮首无
// 头 → 同轮续传 → 轮结束清除）。
func TestCodexResponsesTurnStateStreamCarryAndClear(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t,
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespCallEv, t6RespDone}, turnState: "ts-1"},
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}},
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	for i := 0; i < 3; i++ {
		resp := postResponsesTS(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`, "")
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(b), "[DONE]")
	}

	upc.mu.Lock()
	defer upc.mu.Unlock()
	require.Equal(t, 3, upc.calls)
	require.Equal(t, "", upc.turnStates[0], "轮首请求不带头")
	require.Equal(t, "ts-1", upc.turnStates[1], "同轮续传（注入 held）")
	require.Equal(t, "", upc.turnStates[2], "轮结束清除——跨轮不回传")
}

// TestCodexResponsesTurnStatePassthrough 透传优先（HOST-2）：客户端自带
// x-codex-turn-state → 原值透传不覆盖（客户端自管）；响应签发值回写 held →
// 后续未带请求注入新签发值。
func TestCodexResponsesTurnStatePassthrough(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t,
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespCallEv, t6RespDone}, turnState: "ts-up"})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 客户端自带 client-ts → 透传优先（不被网关 held 覆盖）
	resp := postResponsesTS(t, srv, `{"model":"gpt-4o","input":"hi"}`, "client-ts")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// 后续未带 → 注入响应签发值（ts-up——服务端为准）
	resp = postResponsesTS(t, srv, `{"model":"gpt-4o","input":"hi"}`, "")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	upc.mu.Lock()
	defer upc.mu.Unlock()
	require.Equal(t, 2, upc.calls)
	require.Equal(t, "client-ts", upc.turnStates[0], "客户端自带 → 透传优先不覆盖")
	require.Equal(t, "ts-up", upc.turnStates[1], "响应签发值回写 held → 后续注入")
}

// TestCodexResponses401RotateFailover 401 非判死 → SDK 单飞 refresh → 自动重试
// 一次成功（oauth 轮转形态：新旧 at 序列断言）。
func TestCodexResponses401RotateFailover(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer at-10" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range []string{t6RespCreated, t6RespItemEv, t6RespDone} {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	rm := newCodexWSRefreshMock(t, codexUpStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		srv.URL, nil, nil, store)

	gw := httptest.NewServer(AIRouter(p))
	defer gw.Close()
	resp := postResponses(t, gw, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, `"resp_t6"`, gjson.GetBytes(b, "id").Raw, "轮转后成功聚合")
	require.Equal(t, 1, rm.callsN(), "refresh 恰一次（单飞）")
	mu.Lock()
	require.Equal(t, []string{"Bearer at-10", "Bearer at-new"}, auths, "请求序列 = 旧 at 401 → 新 at 重试")
	mu.Unlock()
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType, "轮转后成功路径")
	store.mu.Unlock()
}

// TestCodexResponsesEnvelope4xxPassthrough 信封 4xx（403）→ 确定性拒绝透传不
// 转移：网关 403 + 原始 body 原样；Err4xx 走 err_logs；上游恰一次接触。
func TestCodexResponsesEnvelope4xxPassthrough(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 403, body: `{"detail":"Forbidden"}`})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "无 transmit 规则 4xx → 归一 502（泄漏修复）")
	require.Contains(t, string(b), `"upstream rejected request"`, "归一固定文案")
	require.NotContains(t, string(b), "Forbidden", "上游原始 body 不得透传")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.Err4xx, store.logs[0].ErrorType, "4xx 失败行走 err_logs 分表")
	require.Equal(t, http.StatusForbidden, store.logs[0].StatusCode)
	require.Equal(t, 1, upc.callsN(), "4xx 确定性拒绝不转移")
}

// TestCodexResponses429Failover 信封 429 → Kind429 转移：首账号 429 → 次账号
// 成功（failover 分类不变——statusOf/upstreamBody 复用面）。
func TestCodexResponses429Failover(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t,
		codexHTTPStep{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`},
		codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10"), 20: codexPATExt(20, "pat-20")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, `"resp_t6"`, gjson.GetBytes(b, "id").Raw, "failover 后成功")
	require.Equal(t, 2, upc.callsN(), "429 → 转移其它账号（恰两次上游接触）")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrNone, store.logs[0].ErrorType, "failover 后成功路径")
}

// TestCodexResponsesFatalNoRetry 裸 fatal（401 判死 token_invalidated）→ 统一
// 回调上报（账号失效标记 + 快照摘除）+ 不重试同账号：耗尽 502 + ErrNetwork；
// 上游恰一次接触；失效上报恰一次（account 10）。
func TestCodexResponsesFatalNoRetry(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 401, body: `{"error":{"code":"token_invalidated"}}`})
	defer up.Close()
	store := &captureLogStore{}
	p, recorder := newTestCodexRespProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "fatal 耗尽 → 502")
	require.Contains(t, string(b), "all upstream attempts failed")
	require.Equal(t, 1, upc.callsN(), "判死不重试——上游恰一次接触")
	require.Equal(t, 1, recorderCalls(recorder), "统一回调恰一次")
	_, acc, _ := recorder.snapshot()
	require.Equal(t, int64(10), acc)
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "fatal → 账号失效摘除（failover 不重试同账号）")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.ErrNetwork, store.logs[0].ErrorType, "fatal 收尾记 code 0 ErrNetwork")
	require.Equal(t, 0, store.logs[0].StatusCode)
}

// TestCodexResponsesExtMissing 配置损坏（codex 账号缺 ext 快照）→ 连接级错误
// 转移（耗尽 502 + ErrNetwork）；不上报失效（避免 account 0 无谓上报）。
func TestCodexResponsesExtMissing(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, recorder := newTestCodexRespProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: nil}, // ext 快照缺失（配置损坏）
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "连接级错误耗尽 → 502")
	require.Equal(t, 0, upc.callsN(), "配置错误不触达上游")
	require.Equal(t, 0, recorderCalls(recorder), "配置错误不上报失效（account 0 无谓上报防线）")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.ErrNetwork, store.logs[0].ErrorType)
	require.Equal(t, 0, store.logs[0].StatusCode)
}

// TestCodexResponsesAdapterMissing 适配层未装配（SetCodex nil）→ 501 语义显式
// 拒绝（防 nil 误走凭据缺失 502；err_logs ErrBilling；上游零接触）。
func TestCodexResponsesAdapterMissing(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store)
	p.codex = nil // 装配缺失模拟（main 未 SetCodex）

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	require.Contains(t, string(b), "adapter not wired")
	require.Equal(t, 0, upc.callsN(), "未装配不触达上游")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.ErrBilling, store.logs[0].ErrorType, "501 本地拒绝走 err_logs")
	require.Equal(t, http.StatusNotImplemented, store.logs[0].StatusCode)
}

// TestCodexResponsesStreamEnvelope403PreFrame 首帧前 4xx 信封（P1 修复回归：
// 头延至首个 fn 调用）：流式上游 403 → 客户端 403 + 原文案（非 200 空流 + 裸
// 错误体）；无 [DONE] 无 SSE 帧；Err4xx 记录；不转移。
func TestCodexResponsesStreamEnvelope403PreFrame(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 403, body: `{"detail":"Forbidden"}`})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "首帧前 4xx 信封 → 归一 502（非 200 空流）")
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"), "归一走 JSON 信封（非 text/event-stream）")
	require.Contains(t, string(b), `"upstream rejected request"`, "归一固定文案")
	require.NotContains(t, string(b), "Forbidden", "上游原始 body 不得透传（泄漏修复）")
	require.NotContains(t, string(b), "[DONE]", "信封失败无 [DONE]")
	require.NotContains(t, string(b), "data:", "信封失败无 SSE 帧")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.Err4xx, store.logs[0].ErrorType, "4xx 走 err_logs 分表")
	require.Equal(t, http.StatusForbidden, store.logs[0].StatusCode)
	require.Equal(t, 1, upc.callsN(), "4xx 确定性拒绝不转移")
}

// TestCodexResponsesStreamFailoverExhausted 流式 failover 耗尽（P1 修复回归）：
// 上游恒 5xx → 客户端 502 JSON 信封（writeErr——非裸写进 SSE 体）；无 [DONE]
// 无 SSE 帧；Err5xx 记录。
func TestCodexResponsesStreamFailoverExhausted(t *testing.T) {
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 500, body: `{"error":{"message":"upstream exploded"}}`})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "耗尽 → 502（非 200 SSE 空流）")
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"), "502 走 JSON 信封（非 text/event-stream）")
	require.Contains(t, string(b), "all upstream attempts failed")
	require.NotContains(t, string(b), "data:", "502 不进 SSE 体")
	require.NotContains(t, string(b), "[DONE]", "耗尽无 [DONE]")
	waitStoreLogs(t, store, 1)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, domain.Err5xx, store.logs[0].ErrorType, "耗尽记 Err5xx")
	require.Equal(t, http.StatusInternalServerError, store.logs[0].StatusCode, "耗尽记录 StatusCode = 最后一次尝试码（500——与既有耗尽路径语义一致；HTTP 面 502 由 writeErr 承担）")
}

// --- 流式收尾双分支（直接调用——编排级 failover 循环外的流中止语义） ---

// frameFailWriter 第 failFrom 次 Write 起失败（首帧写成功、后续帧写失败的流
// 中止路径测试替身；Flush 计数供逐帧 flush 断言）。
type frameFailWriter struct {
	header   http.Header
	status   int
	writes   int
	failFrom int
	flushes  int
	body     bytes.Buffer
}

func (w *frameFailWriter) Header() http.Header  { return w.header }
func (w *frameFailWriter) WriteHeader(code int) { w.status = code }
func (w *frameFailWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writes >= w.failFrom {
		return 0, errors.New("client write failed")
	}
	return w.body.Write(b)
}
func (w *frameFailWriter) Flush() { w.flushes++ }

// selectCodexAccount 直接调用前置：按 codex 类型选号（占并发槽）。
func selectCodexAccount(t *testing.T, p *Proxy, accountID int64) *scheduler.Selection {
	t.Helper()
	sel, err := p.sched.Select(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, accountID, sel.AccountID)
	return sel
}

// TestCodexResponsesStreamMidstreamWriteError 流中止（fn 写出失败——客户端断开
// 等价症状但 r.Context() 存活 = 上游侧问题）：recordStreamAbort + 连接级/5xx 分流
// + 不补发 [DONE]；200 已写出（statusOf(err)=0 归连接级）。
func TestCodexResponsesStreamMidstreamWriteError(t *testing.T) {
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store)
	sel := selectCodexAccount(t, p, 10)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := &frameFailWriter{header: make(http.Header), failFrom: 4} // 帧1（3 写）成功，帧2 首写失败

	code, respBody, handled, callErr := p.callCodexResponses(req.Context(), w, req, "req-1", 10, time.Now(), sel, []byte(`{"model":"gpt-4o","stream":true}`), true)
	require.True(t, handled, "流中止已收尾（handled）")
	require.Equal(t, 0, code)
	require.Nil(t, respBody)
	require.NoError(t, callErr, "流中止按记录收尾，错误不返回到 failover 循环")
	require.Equal(t, http.StatusOK, w.status, "200 已写出（流已开始）")
	require.Equal(t, 4, w.writes, "首帧 3 段直写成功 + 帧2 首段写失败（失败调用计入）")
	require.NotContains(t, w.body.String(), "[DONE]", "上游错误不补发 [DONE]")
	require.GreaterOrEqual(t, w.flushes, 1, "每帧 flush（P2-1）")

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "上游流中止 → 连接级/5xx 分流")
	require.Zero(t, ri.Concurrency, "收尾释放并发槽")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType, "recordStreamAbort → 200 ErrAbort 记录")
	require.Equal(t, http.StatusOK, store.logs[0].StatusCode)
	require.Zero(t, store.logs[0].TotalTokens, "completed 帧未送达（中止前未收 usage 帧）→ 0")
}

// TestCodexResponsesStreamDoneWriteAbort 流正常结束（SDK 已消费 [DONE]/EOF）
// 但补发 [DONE] 帧写出失败（客户端已断）：按 abort 收尾——usage 照记（completed
// 帧已嗅探）+ 不 MarkResult（客户端行为非上游错误）+ [DONE] 未送达。
func TestCodexResponsesStreamDoneWriteAbort(t *testing.T) {
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store)
	sel := selectCodexAccount(t, p, 10)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// 3 帧 × 3 写 = 9 写成功，[DONE] 帧首写（第 10 写）失败
	w := &frameFailWriter{header: make(http.Header), failFrom: 10}

	code, respBody, handled, callErr := p.callCodexResponses(req.Context(), w, req, "req-1", 10, time.Now(), sel, []byte(`{"model":"gpt-4o","stream":true}`), true)
	require.True(t, handled)
	require.Equal(t, 0, code)
	require.Nil(t, respBody)
	require.NoError(t, callErr)
	require.Equal(t, 10, w.writes, "3 载荷帧 9 段直写成功 + [DONE] 首段写失败（失败调用计入）")
	require.NotContains(t, w.body.String(), "[DONE]", "[DONE] 未送达（客户端已断）")
	require.NotContains(t, w.body.String(), "event:", "event: 行不出现")

	p.sched.FlushRules()
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "客户端断开不 MarkResult（不冷却）")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType)
	require.Equal(t, http.StatusOK, store.logs[0].StatusCode)
	require.Equal(t, int64(10), store.logs[0].InputTokens, "completed 帧 usage 已嗅探——abort 记录不丢用量")
	require.Equal(t, int64(20), store.logs[0].OutputTokens)
	require.Equal(t, int64(30), store.logs[0].TotalTokens)
	require.Equal(t, int64(2), store.logs[0].CacheReadTokens)
	require.Equal(t, int64(4), store.logs[0].CacheCreationTokens)
}

// gateWriter 首个 Write 阻塞（模拟慢客户端）直到 release 关闭；放行后：
// ctx 未取消 → 正常写；ctx 已取消 → 返回 ctx.Err()（真实断连的写失败语义——
// 评审修法：断开路径确定走 abort 分支，消除"SDK 读循环赶在 ctx 生效前读完
// 已缓冲小 body → 成功分支"的时序 flake——0c304d8 既有，非 T6 引入）。
type gateWriter struct {
	header  http.Header
	status  int
	ctx     context.Context // 客户端请求 ctx（取消 → Write 失败，仿真实写失败）
	first   chan struct{}   // 首个 Write 已到达（通知测试）
	release chan struct{}   // 放行（关闭后 Write 正常）
	flushes int
	body    bytes.Buffer
}

func (w *gateWriter) Header() http.Header  { return w.header }
func (w *gateWriter) WriteHeader(code int) { w.status = code }
func (w *gateWriter) Write(b []byte) (int, error) {
	select {
	case <-w.first: // 已放行
	default:
		close(w.first)
		<-w.release // 阻塞直到测试放行（模拟客户端停滞窗口）
	}
	if w.ctx != nil && w.ctx.Err() != nil {
		return 0, w.ctx.Err() // 客户端已断——确定性走 abort 分支
	}
	return w.body.Write(b)
}
func (w *gateWriter) Flush() { w.flushes++ }

// TestCodexResponsesStreamClientDisconnect 客户端断开（r.Context() 取消）：
// 不 MarkResult + 200 ErrAbort 记录（上游已消费请求——成功请求丢日志防线）。
func TestCodexResponsesStreamClientDisconnect(t *testing.T) {
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespItemEv, t6RespDone}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		up.URL, nil, nil, store)
	sel := selectCodexAccount(t, p, 10)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	w := &gateWriter{header: make(http.Header), ctx: ctx, first: make(chan struct{}), release: make(chan struct{})}

	done := make(chan struct{})
	var code int
	var handled bool
	go func() {
		defer close(done)
		code, _, handled, _ = p.callCodexResponses(ctx, w, req, "req-1", 10, time.Now(), sel, []byte(`{"model":"gpt-4o","stream":true}`), true)
	}()
	<-w.first // 首帧写出中（阻塞）——模拟客户端停滞窗口
	cancel()  // 客户端断开
	close(w.release)
	<-done
	require.True(t, handled)
	require.Equal(t, 0, code)
	require.Equal(t, http.StatusOK, w.status)

	p.sched.FlushRules()
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "客户端断开不 MarkResult（上游无错）")
	require.Zero(t, ri.Concurrency, "abort 收尾释放并发槽")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType, "客户端断开 → 200 ErrAbort（记录不丢）")
	require.Equal(t, http.StatusOK, store.logs[0].StatusCode)
	require.Equal(t, domain.FormatOpenAIResponses, store.logs[0].Format)
}
