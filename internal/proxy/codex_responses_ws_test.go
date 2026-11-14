// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
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
	codexsdk "github.com/is7Qin/codex-sdk"
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

// --- codex 路径 resp-ws 本地 mock 上游测试（可编程面） ---
// 真实上游不可控面（401 轮转 / 心跳节奏 / 伪装头断言）用本地可编程 mock 覆盖；
// 真实凭据 e2e（happy path / usage 5 计数与 aiclient 路径同值）在
// pg_codex_responses_ws_test.go。

// codexWSUpstream codex 路径 mock WS 上游（升级状态按序弹出，耗尽重复最后
// 一步）+ 握手头观测 + 帧观测（首个数据帧后下发事件流 created/delta/
// completed——与 aiclient 路径共用同一 completed 帧；每帧回声 = 客户端帧经 SDK
// codex 形状归一后的字节，区别于 aiclient 路径的零解析直转）+ 读满 frameLimit
// 帧后 1000 关闭帧。
type codexWSUpstream struct {
	mu       sync.Mutex
	upgrades int
	headers  []http.Header
	frames   []string
	// prewarms 记 SDK 在新建连接上先发的预热帧（generate:false；codex-sdk HTTP→WS
	// 预热，官方 0.169.0 行为）。预热帧按官方上游回 created/completed、不进 frames、
	// 不计入 frameLimit、不触发 frameScript：frames 只记真实 response.create，
	// "上游 create 恰 N"类断言的含义不变。
	prewarms []string
	// prewarmScript 非 nil 时改写对预热帧的应答：按序写这些帧后正常关闭（空切片 =
	// 不回任何帧直接关闭，模拟预热阶段连接错误）。nil = 官方 created/completed。
	prewarmScript []string
	// prewarmScriptLimit >0：prewarmScript 只作用于前 N 个预热帧（之后按官方应答）。
	prewarmScriptLimit int
	steps              []int
	last               int
	frameLimit         int
	// frameScript 非 nil 时改写"收到首个客户端帧之后"的行为：按序写这些帧，然后
	// 正常关闭（不回声、不发内置 created/delta/completed 事件流）。它把"上游只
	// 回一帧错误就收尾"变成可编程形态（静默/半闭形态仍用 steps+frameLimit）。
	frameScript []string
}

func newCodexWSUpstream(t *testing.T, steps []int, frameLimit int) (*httptest.Server, *codexWSUpstream) {
	t.Helper()
	u := &codexWSUpstream{last: 200, frameLimit: frameLimit}
	if len(steps) > 0 {
		u.steps = steps
		u.last = steps[len(steps)-1]
	}
	return newCodexWSServer(t, u), u
}

// newCodexWSScriptUpstream 同 newCodexWSUpstream，但首个客户端帧之后按序下发
// frameScript 并以 1000 关闭（复用同一个 handler 与观测面）。
func newCodexWSScriptUpstream(t *testing.T, frames ...string) (*httptest.Server, *codexWSUpstream) {
	t.Helper()
	u := &codexWSUpstream{last: 200, frameScript: frames}
	return newCodexWSServer(t, u), u
}

// newCodexWSFatalFirstUpstream 首帧判死上游：**第一次连接**收到 create 后立刻回
// fatalFrame 并 1000 关闭；后续连接回正常事件流（created/completed + 回声）。
//
// 不按凭据值区分：候选先后由调度器决定（两个同构候选不保证顺序），按"第一个
// 被拨的账号必死"建模，用例就不会因顺序翻转而假绿/假红——断言从实际看到的
// Authorization 反推。用来守"账号错误换号重试"的两条路径：零交付判死 = 换号，
// 单账号 = 耗尽收尾。
func newCodexWSFatalFirstUpstream(t *testing.T, fatalFrame string) (*httptest.Server, *codexWSUpstream) {
	t.Helper()
	// frameLimit=1：承接账号收到一份 create 就把整轮发完并正常关闭（用例客户端
	// 只写一帧，让它等第二帧会挂到超时）。
	u := &codexWSUpstream{last: 200, frameLimit: 1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" && r.URL.Path != "/backend-api/codex/responses" {
			w.WriteHeader(404)
			return
		}
		u.mu.Lock()
		u.upgrades++
		u.headers = append(u.headers, r.Header.Clone())
		fatal := u.upgrades == 1 // 第一个被拨的账号必死
		u.mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
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
			if isPrewarmFrame(msg) {
				if err := writePrewarmResponse(c, typ); err != nil {
					return
				}
				continue
			}
			u.mu.Lock()
			u.frames = append(u.frames, string(msg))
			u.mu.Unlock()
			if fatal {
				if err := c.Write(context.Background(), typ, []byte(fatalFrame)); err != nil {
					return
				}
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
			if !streamed {
				streamed = true
				for _, f := range []string{
					`{"type":"response.created","response":{"id":"rsp_ws_ok","model":"gpt-4o"}}`,
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
			if n >= u.frameLimit {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, u
}

// newCodexWSServer 是两种构造共用的 handler 装配（u 必须已填好 steps/frameScript）。
func newCodexWSServer(t *testing.T, u *codexWSUpstream) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" && r.URL.Path != "/backend-api/codex/responses" {
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
			if isPrewarmFrame(msg) {
				u.mu.Lock()
				u.prewarms = append(u.prewarms, string(msg))
				pwScript := u.prewarmScript
				if u.prewarmScriptLimit > 0 && len(u.prewarms) > u.prewarmScriptLimit {
					pwScript = nil
				}
				u.mu.Unlock()
				if pwScript != nil {
					for _, f := range pwScript {
						if err := c.Write(r.Context(), typ, []byte(f)); err != nil {
							return
						}
					}
					_ = c.Close(websocket.StatusNormalClosure, "")
					return
				}
				if err := writePrewarmResponse(c, typ); err != nil {
					return
				}
				continue
			}
			u.mu.Lock()
			u.frames = append(u.frames, string(msg))
			script := u.frameScript
			u.mu.Unlock()
			if script != nil {
				// 脚本模式：按序下发后正常关闭（不回声、不发内置事件流）。
				for _, f := range script {
					if err := c.Write(r.Context(), typ, []byte(f)); err != nil {
						return
					}
				}
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
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
	return srv
}

// isPrewarmFrame 判定 SDK HTTP→WS 预热帧（response.create 且 generate:false）。
func isPrewarmFrame(msg []byte) bool {
	return gjson.GetBytes(msg, "generate").Type == gjson.False
}

// writePrewarmResponse 按官方上游对预热帧的应答下发 created/completed（不带 id：
// 真实帧不链 previous_response_id、发全量，与改动前的真实帧字节一致）。
func writePrewarmResponse(c *websocket.Conn, typ websocket.MessageType) error {
	for _, f := range []string{
		`{"type":"response.created","response":{"object":"response","status":"in_progress"}}`,
		`{"type":"response.completed","response":{"object":"response","status":"completed"}}`,
	} {
		if err := c.Write(context.Background(), typ, []byte(f)); err != nil {
			return err
		}
	}
	return nil
}

func (u *codexWSUpstream) prewarmsSnapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.prewarms...)
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

// codexWSExt 构造 codex-oauth 账号扩展。持久身份只保留 installation_id；
// session/thread/window 运行时由槽身份给出（不再来自 ext）。
func codexWSExt(accountID int64, at, rt string) *domain.AccountExt {
	return codexOAuthExt(accountID, at, rt)
}

// newTestCodexWSProxy 构造 codex 类型 resp-ws 测试代理：模板（credType 类型 +
// resp-ws 格式 + gpt-4o）+ 携带 Ext 的账号（同组 10，可多账号）+ 装配适配层
// （统一失效回调走真实处理链——fakeFailureStore 落库替身 + 真实调度器
// FailAccount 摘除）。Codex 官方默认端点 via transport 重写到 mock。bill 为计费钩子（nil = 计费全关）。
func newTestCodexWSProxy(t *testing.T, credType credential.Type, accounts map[int64]*domain.AccountExt, upstream string, bill *BillingHooks, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "",
		CredentialType:   credType,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
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
	// 出口池：调度器快照与 codex 适配层用**同一个**（M3 起 codex 客户端的
	// transport 取自 sel.Egress.HTTP.Transport，两者不同源就等于没接线——
	// 请求会绕过测试替身直接打真实 codex 端点）。
	egPool := testEgressPool(newProxyOfficialRewriteTransportWithAssert(t, upstream))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour, Egress: egPool}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil, nil, true)
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
	codex := sdkbridge.NewCodex(failure, egPool, nil, sdkbridge.RotationDeps{})
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, bill, errlogW, Deps{Codex: codex})
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

// codexWSInvalidMidFrame 不带 model 的中间帧（升级前的用例原样）：SDK 整形拒绝
// （ErrInvalidRequestBody）。
const codexWSInvalidMidFrame = `{"type":"response.input_text.delta","delta":"typing"}`

// codexWSSessionWithLocalReject 写首帧并读 nFirst 帧（上游事件流 + 首帧回声）；
// 此时上游已无待下发帧，再写一帧不带 model 的中间帧 → 客户端收到本地拒绝错误帧
// （帧序确定）、conv_local_rejects 加一、会话继续；最后写 rest 并读 nRest 帧。
// 返回读到的上游帧（不含本地拒绝帧）。
func codexWSSessionWithLocalReject(t *testing.T, c *websocket.Conn, first string, nFirst int, rest []string, nRest int) []string {
	t.Helper()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(first)))
	got := make([]string, 0, nFirst+nRest)
	for range nFirst {
		got = append(got, string(readResponsesWSFrame(t, c)))
	}
	before := convLocalRejects.Load()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(codexWSInvalidMidFrame)))
	require.JSONEq(t, `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"invalid request body"}}`,
		string(readResponsesWSFrame(t, c)), "不带 model 的中间帧 → 本地拒绝错误帧")
	require.Equal(t, before+1, convLocalRejects.Load(), "conv_local_rejects 递增")
	for _, f := range rest {
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(f)))
	}
	for range nRest {
		got = append(got, string(readResponsesWSFrame(t, c)))
	}
	return got
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

// TestCodexWSBlackHoleDialTimeout codex 拨号同款超时：黑洞上游永不回
// 101 → wrapped ctx 超时 → SDK dialStatus(nil)=0（resp=nil 安全返回）→
// DialError{StatusCode:0} → handleCodexDialError 既有 default 分支连接级转移
// （零新分支）→ 耗尽错误帧 + 连接级/5xx 分流 冷却 + 并发槽释放。
func TestCodexWSBlackHoleDialTimeout(t *testing.T) {
	testHealthSink.reset()
	old := wsDialTimeout
	wsDialTimeout = 50 * time.Millisecond
	t.Cleanup(func() { wsDialTimeout = old })

	up := blackHoleWSServer(t)
	store := &captureLogStore{}
	pat := "sk-pat-10" // 缺 PATKey → 适配层 errCredentialIncomplete 快速失败（测不到超时）
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: {AccountID: 10, CredentialType: credential.TypeCodexPAT, CodexIdentity: &domain.CodexIdentity{InstallationID: "i"}, CodexPATKey: &pat}}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "Upstream request failed", "WS 耗尽 CustomMessage（honor msg）")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	p.sched.FlushRules() // MarkResult 异步投递：断言前排空
	require.Len(t, testHealthSink.throttlesFor(10), 1, "黑洞超时 → 连接级/5xx 分流惩罚")
	ri, ok := p.sched.Runtime(10) // codex 代理账号 ID = 组键 10（newTestCodexWSProxy）
	require.True(t, ok)
	require.Zero(t, ri.Concurrency, "耗尽路径并发槽必须释放")
	require.NoError(t, p.rec.Close(context.Background()))
	waitStoreLogs(t, store, 1) // errlog 异步落袋（20ms flush；worker 由 helper cleanup 收尾，不手动 Close）
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNetwork, lg.ErrorType)
	require.Equal(t, 0, lg.StatusCode)
	require.NotNil(t, lg.ErrorMessage, "错误文本必须落盘")
	require.Contains(t, *lg.ErrorMessage, "deadline exceeded", "错误文本 = 拨号超时错误原文")
}

// TestCodexWSMockRotateRefreshSuccess 端到端主流程（oauth）：升级 401 → SDK
// 单飞 refresh（真端点 mock）→ 重拨 200 → 完整会话（事件流 + 回声 + 关闭）
// + usage 嗅探计费（5 计数与 aiclient 路径同值，共用同一 completed 帧）+ 伪装四元组握手头断
// 言 + 客户端头一律不递（spec §12：session 头族/OpenAI-Beta 不得顶掉账号身份，
// 自定义头如 X-Client-Version 也不得出现）+ 网关 key 不泄漏 + 帧内
// client_metadata 注入断言。
func TestCodexWSMockRotateRefreshSuccess(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{401, 200}, 3)
	defer up.Close()
	newCodexWSRefreshMock(t, codexUpStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	// 客户端带伪装冲突面头（session 头族 + OpenAI-Beta 旧版本）与自定义头——按
	// spec §12 一律不递上游：前者不得覆盖账号伪装身份，后者
	//（X-Client-Version）自该裁决起也不再透传（见下方断言）。
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
	f2 := `{"type":"response.input_text.delta","model":"gpt-4o","delta":"typing"}`
	f3 := `{"type":"custom.mid","model":"gpt-4o","n":42}`
	// 首帧 → 事件流 3 帧 + 回声；不带 model 的中间帧 → 本地拒绝、会话继续；
	// f2/f3 → 两帧回声（got 不含本地拒绝帧）。
	got := codexWSSessionWithLocalReject(t, c, f1, 4, []string{f2, f3}, 2)
	require.Equal(t, `{"type":"response.created","response":{"id":"rsp_ws_1","model":"gpt-4o"}}`, got[0])
	require.Equal(t, `{"type":"response.output_text.delta","delta":"hi"}`, got[1])
	require.Contains(t, got[2], `"type":"response.completed"`)
	require.Contains(t, got[2], `"input_tokens":6`)
	// 回声帧 = 客户端帧经 SDK 整形后的字节（无条件生效，SDK 整形 §5.1）：白名单外
	// 键（f2 的 delta / f3 的 n）与客户端 type 丢弃、type 恒写 response.create；
	// stream:true / store:false 强制；字符串 input 规范成一条 user 消息，缺省 input
	// 写 []；非 lite（gpt-4o 表外 = 非 lite）缺省 tools:[] / tool_choice:"auto" /
	// parallel_tool_calls:true / reasoning:{}（表外模型不填 effort）/ include；
	// prompt_cache_key = 槽位 SessionID；按官方 WS 键序一次写出。prompt_cache_key
	// 之前的字节完全确定，逐字节断言；之后只能是 client_metadata 一个键。
	const pckKey = `"prompt_cache_key":"`
	const tail = `"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{},"store":false,"include":["reasoning.encrypted_content"],` + pckKey
	wantShapedPrefix := []string{
		`{"type":"response.create","model":"gpt-4o","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` + tail,
		`{"type":"response.create","model":"gpt-4o","stream":true,"input":[],` + tail,
		`{"type":"response.create","model":"gpt-4o","stream":true,"input":[],` + tail,
	}
	threadKeys := map[string]bool{}
	for i, want := range []string{f1, f2, f3} {
		var echo struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		require.NoError(t, json.Unmarshal([]byte(got[3+i]), &echo))
		require.Equal(t, "echo", echo.Type)
		idx := strings.Index(echo.Payload, pckKey)
		require.GreaterOrEqual(t, idx, 0, "非匿名帧必带 prompt_cache_key")
		require.Equal(t, wantShapedPrefix[i], echo.Payload[:idx+len(pckKey)],
			"整形后确定字节不一致（客户端帧 %q）：官方键序 + 缺省值 + 剥离", want)
		frame := gjson.Parse(echo.Payload)
		sid := frame.Get("prompt_cache_key").String()
		require.True(t, isUUIDv7(sid), "prompt_cache_key = 槽位 SessionID（UUIDv7）")
		require.True(t, strings.HasPrefix(echo.Payload[idx+len(pckKey)+len(sid):], `","client_metadata":{`),
			"prompt_cache_key 之后只有 client_metadata（无 text/generate/access_programs）")
		require.True(t, strings.HasSuffix(echo.Payload, "}}"), "client_metadata 对象 + 帧对象闭合")
		// client_metadata：键序每帧随机（官方 HashMap），按键集与值断言。
		cm := frame.Get("client_metadata")
		var keys []string
		cm.ForEach(func(k, _ gjson.Result) bool { keys = append(keys, k.String()); return true })
		require.ElementsMatch(t, []string{
			"session_id", "thread_id", "x-codex-window-id", "x-codex-installation-id",
			"turn_id", "root_turn_id", "x-codex-turn-metadata", "mcp_attribution",
			"guardian_credits_requested", "x-codex-ws-stream-request-start-ms",
		}, keys, "WS 真实帧 client_metadata 键集（槽位路径无 turn-state；非 lite；无 trace）")
		require.Equal(t, "inst-00000000000000000000000000000000", cm.Get("x-codex-installation-id").String())
		require.Equal(t, sid, cm.Get("session_id").String())
		require.Equal(t, sid, cm.Get("thread_id").String(), "槽内 session==thread")
		require.Equal(t, sid+":0", cm.Get("x-codex-window-id").String())
		require.True(t, isUUIDv7(cm.Get("turn_id").String()))
		require.Equal(t, cm.Get("turn_id").String(), cm.Get("root_turn_id").String())
		require.Regexp(t, `^[0-9]{13}$`, cm.Get("x-codex-ws-stream-request-start-ms").String(), "发送时刻毫秒（13 位）")
		require.Equal(t, "turn", gjson.Parse(cm.Get("x-codex-turn-metadata").String()).Get("request_kind").String())
		threadKeys[sid] = true
	}
	require.Len(t, threadKeys, 1, "同一连接三帧同一会话身份")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	hooks.mu.Lock()
	require.Len(t, hooks.frames, 3, "本地拒绝的中间帧不出网")
	hooks.mu.Unlock()

	// 握手面断言（伪装头）：两次升级（401 首拨 + 轮转后重拨），账号鉴权
	// 注入（首拨旧 at / 重拨新 at——轮转生效）；网关 key 不泄漏；伪装身份
	// session/thread/window = 运行时槽身份（UUIDv7；客户端 rogue 头被剔除）；
	// OpenAI-Beta = 网关默认（rogue 被剔除）；客户端自定义头（如
	// X-Client-Version）自 spec §12 起一律不递。
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	require.Equal(t, 2, hooks.upgrades, "401 轮转：首拨 + 重拨一次")
	require.Equal(t, "Bearer at-10", hooks.headers[0].Get("Authorization"), "首拨旧 at")
	require.Equal(t, "Bearer at-new", hooks.headers[1].Get("Authorization"), "轮转后新 at")
	h := hooks.headers[1]
	ext := codexWSExt(10, "", "")
	require.True(t, isUUIDv7(h.Get("Session-Id")), "session-id = 槽身份（UUIDv7；rogue 已剔除）")
	require.Equal(t, h.Get("Session-Id"), h.Get("Thread-Id"), "槽内 session==thread")
	require.Equal(t, h.Get("Thread-Id"), h.Get("X-Client-Request-Id"), "x-client-request-id 缺省 = thread-id")
	require.Equal(t, h.Get("Thread-Id")+":0", h.Get("X-Codex-Window-Id"), "window-id = {thread}:0")
	require.Equal(t, "responses_websockets="+aiclient.ResponsesWSBetaHeader, h.Get("OpenAI-Beta"), "网关默认 beta（rogue 已剔除）")
	// spec §12：codex 面只发 SDK 自己写的头 ⇒ 客户端 X-Client-Version 不再透传
	//（旧断言是「非冲突面客户端头透传」，属旧契约）。
	require.Empty(t, h.Get("X-Client-Version"), "codex 面不得递客户端头（spec §12）")
	require.NotEqual(t, "Bearer ck-1", h.Get("Authorization"), "网关 key 不得直通上游")

	// 帧内 client_metadata 注入（伪装身份帧级面——真实 codex 客户端语义）：
	// installation_id 取账号 ext；session/thread/window 取运行时槽身份。
	f0 := hooks.frames[0]
	require.Contains(t, f0, `"x-codex-installation-id":"`+ext.CodexIdentity.InstallationID+`"`)
	require.True(t, isUUIDv7(gjson.Get(f0, "client_metadata.session_id").String()), "帧内 session_id = 槽身份")
	require.Equal(t, gjson.Get(f0, "client_metadata.session_id").String(), gjson.Get(f0, "client_metadata.thread_id").String(), "帧内 session==thread")
	require.Equal(t, gjson.Get(f0, "client_metadata.thread_id").String()+":0", gjson.Get(f0, "client_metadata.x-codex-window-id").String(), "帧内 window={thread}:0")

	// usage 记录：5 计数 + 成功路径（与 aiclient 路径逐字节同口径）
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType)
	require.Equal(t, http.StatusOK, lg.StatusCode)
	require.Equal(t, int64(2), lg.InputTokens, "可计费输入 = 线上 input 6 − cached 1 − cache_write 3（P0-2 归一）")
	require.Equal(t, int64(5), lg.OutputTokens)
	require.Equal(t, int64(11), lg.TotalTokens)
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
	// 错误事件帧（R-1 规则驱动：Classify(Kind4xx)→UnifiedMessage/passthrough，默认归一仍为 "upstream rejected request"；可被 CustomMessage 改写，见 TestCodexWSDial401RuleCustomMessage）+ 关闭
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), "upstream rejected request", "4xx 归一 via rule engine default（无命中 CustomMessage 时 passthrough/归一文案）")
	require.NotContains(t, string(ef), "401", "SDK 拨号错误文本不得上用户帧")
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

// TestCodexWSDial401RuleCustomMessage 拨号 4xx 规则驱动文案与 punish 投递（R-1）：
// 自定义规则（Kind4xx + HTTP 401 + CustomMessage）改写 WS 错误帧，且 MarkResult 投递使账号状态按规则更新。
func TestCodexWSDial401RuleCustomMessage(t *testing.T) {
	testHealthSink.reset()
	up, _ := newCodexWSUpstream(t, []int{401, 401}, 0)
	defer up.Close()
	_ = newCodexWSRefreshMock(t, codexUpStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &captureLogStore{}

	// 定制规则引擎：种子后插入高优 CustomMessage 规则（punish=true → MarkResult 投递）。
	frs := &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	re := rule.New(rule.Config{}, frs, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	kind4xx := "4xx"
	customMsg := "dial-4xx-custom"
	_, err := frs.CreateRule(context.Background(), domain.Rule{
		Name: "custom-dial-401", Enabled: true, Priority: 5,
		When: domain.RuleWhen{Kind: &kind4xx, HTTPStatus: new(401)},
		Then: domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: new(int64(5000))}, CustomMessage: &customMsg},
	})
	require.NoError(t, err)
	require.NoError(t, re.Reload(context.Background()))

	// 手动装配代理（复用 newTestCodexWSProxy 装配逻辑，注入已含 custom 规则的 re；模板 BaseURL 空 + transport host 重写到 mock）。
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	}
	ext := codexWSExt(10, "at-10", "rt-10")
	accs := map[int64][]*domain.Account{10: {{
		ID: 10, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4, Ext: ext,
	}}}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, store, nil)
	cfg := Config{MaxBodySize: 1 << 20, FailoverAttempts: 2, UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second, UsageCapture: true}
	// 出口池：调度器快照与 codex 适配层用**同一个**（M3 起 codex 客户端的
	// transport 取自 sel.Egress.HTTP.Transport，两者不同源就等于没接线——
	// 请求会绕过测试替身直接打真实 codex 端点）。
	egPool := testEgressPool(newProxyOfficialRewriteTransportWithAssert(t, up.URL))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour, Egress: egPool}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, 10)}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	failureStore := &fakeFailureStore{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: failureStore, Failer: sched, Log: nil})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: 20 * time.Millisecond}, store, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	codexWs := sdkbridge.NewCodex(failure, egPool, nil, sdkbridge.RotationDeps{})
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{Codex: codexWs})

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	ef := readResponsesWSFrame(t, c)
	require.Contains(t, string(ef), `"type":"error"`)
	require.Contains(t, string(ef), customMsg, "自定义 CustomMessage 必须改写 dial 4xx 帧文案（R-1 UnifiedMessage）")
	require.NotContains(t, string(ef), "upstream rejected request", "命中自定义文案时不再回退默认归一文案")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	p.sched.FlushRules()
	ths := testHealthSink.throttlesFor(10)
	require.Len(t, ths, 1, "punish=true 必须投递 MarkResult → 规则 Throttle 生效")
	require.Equal(t, domain.ThrottleModeOpen, ths[0].Mode, "punish 规则的 Throttle 必须生效")
}

// TestCodexWSFatalStatus0RetriesHealthAccount 裸 fatal（refresh 判死
// invalid_grant，status 0）**也换号**：账号级终止与状态码无关——账户死了，
// 同样的请求换个账号就能成。双账号池里健康账号承接成功，客户端拿到正常事件流。
func TestCodexWSFatalStatus0RetriesHealthAccount(t *testing.T) {
	// 升级按序：账号 10 = 401（触发 refresh → 判死），账号 20 = 200（承接）
	up, hooks := newCodexWSUpstream(t, []int{401, 200}, 1)
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
	// 客户端拿到的是承接账号的正常流（判死不出网）
	frames := drainUntilClose(t, c)
	require.NotEmpty(t, frames)
	require.Contains(t, frames[0], `"response.created"`, "换号后从新账号重新开始这一轮；got=%q", frames[0])
	for _, f := range frames {
		require.NotContains(t, f, "refresh 被拒绝", "SDK 内部机制串不得上用户帧")
		require.NotContains(t, f, "token_invalidated", "判死痕迹不得泄漏给客户端")
	}

	hooks.mu.Lock()
	require.Equal(t, 2, hooks.upgrades, "账号 10 判死后换号到账号 20（各拨一次）")
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
	require.NotNil(t, store.logs[0].ErrorMessage, "fatal 原文仍落盘（唯一留痕）")
	require.Contains(t, *store.logs[0].ErrorMessage, "refresh 被拒绝", "落盘留痕不动——SDK 原文进 ErrorMessage")
}

// TestCodexWSDial429Failover codex WS 拨号 429（可重试类）→ 跨账号 failover：
// 账号 10 拨号 429 转移 → 账号 20 拨号成功 → 完整会话（事件流 + 回声 + 1000
// 关闭）。证明 codex WS 面 failover 按当轮账号重新拨号、凭据不串。
//
// 注：裸 RefreshError（refresh 5xx 耗尽）在重试矩阵下 code 0 + 无 callErr 归
// committed 终态，不再跨账号转移（codex WS 拨号级 failover 仅 429 类可达）。
func TestCodexWSDial429Failover(t *testing.T) {
	testHealthSink.reset()
	up, hooks := newCodexWSUpstream(t, []int{429, 200}, 3)
	defer up.Close()
	newCodexWSRefreshMock(t, codexUpStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{
		10: codexWSExt(10, "at-10", "rt-10"),
		20: codexWSExt(20, "at-20", "rt-20"),
	}, up.URL, nil, store)
	// 确定性车道：账号 10 先拨（429）→ 转移账号 20（200）。
	p.sched.PublishDecisionForTest(
		scheduler.RouteRefFor(10, string(domain.FormatOpenAIResponsesWS), "gpt-4o"),
		&scheduler.RouteDecision{Primary: []scheduler.CompiledCandidate{{AccountID: 10}, {AccountID: 20}}})

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	// 事件流 3 帧 + 回声 3 帧（上游 frameLimit=3 后 1000 关闭）；其间不带 model 的
	// 中间帧本地拒绝、会话继续。
	_ = codexWSSessionWithLocalReject(t, c, `{"type":"response.create","model":"gpt-4o","input":"hi"}`, 4,
		[]string{`{"type":"response.input_text.delta","model":"gpt-4o","delta":"typing"}`, `{"type":"custom.mid","model":"gpt-4o","n":42}`}, 2)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)

	hooks.mu.Lock()
	require.Equal(t, 2, hooks.upgrades, "账号 10 首拨 429 + 账号 20 转移拨号 200")
	require.Equal(t, "Bearer at-10", hooks.headers[0].Get("Authorization"), "首拨账号 10")
	require.Equal(t, "Bearer at-20", hooks.headers[1].Get("Authorization"), "转移后按新账号凭据重拨")
	hooks.mu.Unlock()
	p.sched.FlushRules()
	require.Len(t, testHealthSink.throttlesFor(10), 1, "账号 10 429 惩罚恰一次投递")
	require.Empty(t, testHealthSink.throttlesFor(20), "成功账号 20 不受罚")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	lg := store.logs[0]
	require.Equal(t, domain.ErrNone, lg.ErrorType, "failover 后成功")
	require.Equal(t, int64(20), lg.AccountID, "落账 = 成功尝试账号")
}

// TestCodexWSDeathFrameFatal WS 业务判死事件帧（§3 唯一跨边界点）在**零交付**
// 时不再透传客户端：判死帧到达时一帧业务帧都没写 ⇒ 请求还能换号，所以判死帧
// 被就地吞掉、本网络跳作废（旧契约是"判死帧照常透传"，那是把账号死亡变成用户
// 可见失败）。
//
// 单账号 ⇒ 换号没有候选 ⇒ 走耗尽收尾（网关错误帧 + 正常关闭），账号摘除、
// 上报恰一次。换号成功的正例见 TestCodexWSFatalFirstFrameRetriesAnotherAccount。
func TestCodexWSDeathFrameFatal(t *testing.T) {
	mu := &codexWSUpstream{last: 200, frameLimit: 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" && r.URL.Path != "/backend-api/codex/responses" {
			w.WriteHeader(404)
			return
		}
		srvMu := mu
		srvMu.mu.Lock()
		srvMu.upgrades++
		srvMu.mu.Unlock()
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
			srvMu.mu.Lock()
			srvMu.frames = append(srvMu.frames, string(msg))
			srvMu.mu.Unlock()
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
			if n >= srvMu.frameLimit {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	defer srv.Close()
	store := &captureLogStore{}
	p, recorder := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, srv.URL, nil, store)

	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer gw.Close()
	c := dialResponsesWS(t, gw)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	got := drainUntilClose(t, c)
	require.NotEmpty(t, got, "收尾必须给客户端一帧（网关错误帧）")
	for _, f := range got {
		require.NotContains(t, f, "token_invalidated", "判死帧不得透传客户端（%s）", f)
	}

	// 失效上报恰一次（§3 显式 FailureHandler——共用处理函数）
	require.Equal(t, 1, recorderCalls(recorder), "帧判死 → 统一回调恰一次")
	_, acc, reason := recorder.snapshot()
	require.Equal(t, int64(10), acc)
	require.Contains(t, reason, "token_invalidated", "last_error 留痕失效原因摘要")
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "帧判死 → 账号失效摘除")
	require.NoError(t, p.rec.Close(context.Background()))
}

// drainUntilClose 读到关闭为止（判死帧被吞掉后客户端只该看到网关自己的收尾帧）。
func drainUntilClose(t *testing.T, c *websocket.Conn) []string {
	t.Helper()
	var got []string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			var ce websocket.CloseError
			require.True(t, errors.As(err, &ce), "expect close frame, got %v", err)
			require.Equal(t, websocket.StatusNormalClosure, ce.Code)
			return got
		}
		got = append(got, string(b))
	}
}

// deactivatedWorkspaceFrame 用户实证的上游 402 业务错误（workspace 被停用）——
// WS 错误事件帧形态。原样贴进用例：形状变了就该红。
const deactivatedWorkspaceFrame = `{"type":"error","status":402,"error":{"message":"Payment Required","type":null,"code":"deactivated_workspace","param":null}}`

// TestCodexWSDeactivatedWorkspaceFrameFatal 业务判死帧（402 +
// deactivated_workspace）零交付：判死 → 账号摘除、上报一次带码；帧不达客户端
// （换号的前提）。单账号 ⇒ 换号无候选 ⇒ 耗尽收尾。
func TestCodexWSDeactivatedWorkspaceFrameFatal(t *testing.T) {
	srv, mu := newCodexWSFatalFirstUpstream(t, deactivatedWorkspaceFrame)
	defer srv.Close()

	store := &captureLogStore{}
	p, recorder := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, srv.URL, nil, store)

	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer gw.Close()
	c := dialResponsesWS(t, gw)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	got := drainUntilClose(t, c)
	for _, f := range got {
		require.NotContains(t, f, "deactivated_workspace", "判死帧不得透传客户端（%s）", f)
	}

	require.Equal(t, 1, recorderCalls(recorder), "业务判死帧 → 统一回调恰一次")
	_, acc, reason := recorder.snapshot()
	require.Equal(t, int64(10), acc)
	require.Contains(t, reason, "deactivated_workspace", "last_error 留痕失效原因（码）")
	ri, ok := p.sched.Runtime(10)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "业务判死 → 账号失效摘除")

	mu.mu.Lock()
	frames := append([]string(nil), mu.frames...)
	mu.mu.Unlock()
	require.Len(t, frames, 1, "上游收到的 create 恰 1（不重发）；frames=%v", frames)
	require.NoError(t, p.rec.Close(context.Background()))
}

// TestCodexWSFatalFirstFrameRetriesAnotherAccount **账号错误换号重试**的主门禁：
// 第一账号首帧就判死（402 + deactivated_workspace，零业务字节）⇒ 请求换到第二
// 账号并**成功交付**；第一账号被摘除、第二账号仍可用；客户端只看到第二账号的
// 正常事件流，看不到任何判死痕迹。
func TestCodexWSFatalFirstFrameRetriesAnotherAccount(t *testing.T) {
	srv, mu := newCodexWSFatalFirstUpstream(t, deactivatedWorkspaceFrame)
	defer srv.Close()

	store := &captureLogStore{}
	p, recorder := newTestCodexWSProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{
		10: codexWSExt(10, "at-10", "rt-10"),
		20: codexWSExt(20, "at-20", "rt-20"),
	}, srv.URL, nil, store)

	gw := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer gw.Close()
	c := dialResponsesWS(t, gw)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))

	// 客户端拿到的是**第二账号**的完整事件流（created / completed + 回声）
	got := drainUntilClose(t, c)
	require.Contains(t, got[0], "response.created", "换号后从新账号重新开始这一轮；got=%q", got[0])
	require.Contains(t, got[1], "response.completed")
	for _, f := range got {
		require.NotContains(t, f, "deactivated_workspace", "判死痕迹不得泄漏给客户端（%s）", f)
	}

	mu.mu.Lock()
	upgrades := mu.upgrades
	auths := make([]string, 0, len(mu.headers))
	for _, h := range mu.headers {
		auths = append(auths, h.Get("Authorization"))
	}
	frames := append([]string(nil), mu.frames...)
	mu.mu.Unlock()
	require.Equal(t, 2, upgrades, "判死账号之后必须换号（两个候选各拨一次）")
	require.NotEqual(t, auths[0], auths[1], "两次拨号必须落在不同账号")
	require.Len(t, frames, 2, "两个账号各收到一次 create（同一请求重发）")

	// 判决对象从实际拨号顺序反推（候选先后由调度器决定）
	dead, alive := int64(0), int64(0)
	switch auths[0] {
	case "Bearer at-10":
		dead, alive = 10, 20
	case "Bearer at-20":
		dead, alive = 20, 10
	}
	require.NotZero(t, dead, "首选必须带候选凭据；got=%q", auths[0])

	require.Equal(t, 1, recorderCalls(recorder), "只有被判死的账号上报一次")
	_, acc, reason := recorder.snapshot()
	require.Equal(t, dead, acc, "判决落在首选账号上")
	require.Contains(t, reason, "deactivated_workspace")

	riDead, ok := p.sched.Runtime(dead)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, riDead.Status, "判死账号被摘除")
	riAlive, ok := p.sched.Runtime(alive)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, riAlive.Status, "承接账号不受影响")

	require.NoError(t, p.rec.Close(context.Background()))
}

// TestCodexWSDial402DeactivatedWorkspaceFatal 升级面同一条判死（上游在 WS 升级
// 就返回 402 + 业务码）：DialError.Body 进同一分类器 ⇒ 判死 + 该请求**不转移**
// （fatal 终态：客户端拿固定鉴权失败文案，第二账号不拨号）。
//
// 与"业务帧面"分开守：两条承载路径的 body 来源不同（升级响应体 vs 帧），
// 断一条就有一个 402 只烧派发不判死。
func TestCodexWSDial402DeactivatedWorkspaceFatal(t *testing.T) {
	// 两个候选都在升级面被判死（同一具死体）⇒ 都摘除、都没换号价值 ⇒ 耗尽收尾。
	// 这条守"判死换号不会变成无限重试"：候选耗尽即收尾。
	var mu sync.Mutex
	upgrades := 0
	var authSeen string // 首个（也是唯一）升级请求的 Authorization（确定判决落在哪个账号）
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" && r.URL.Path != "/backend-api/codex/responses" {
			w.WriteHeader(404)
			return
		}
		mu.Lock()
		upgrades++
		if authSeen == "" {
			authSeen = r.Header.Get("Authorization")
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Payment Required","type":null,"code":"deactivated_workspace","param":null},"status":402}`))
	}))
	defer up.Close()

	store := &captureLogStore{}
	p, recorder := newTestCodexWSProxy(t, credential.TypeCodexOAuth, map[int64]*domain.AccountExt{
		10: codexWSExt(10, "at-10", "rt-10"),
		20: codexWSExt(20, "at-20", "rt-20"),
	}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()

	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	frames := drainUntilClose(t, c)
	require.NotEmpty(t, frames, "耗尽也要给客户端一帧")
	for _, f := range frames {
		require.NotContains(t, f, "deactivated_workspace", "判死痕迹不得泄漏给客户端")
	}

	mu.Lock()
	n := upgrades
	mu.Unlock()
	require.Equal(t, 2, n, "两个候选都被派发一次（第一个判死后换号，第二个也死才收尾）")

	// 两个账号都被判死、都摘除（谁先谁后由调度器决定，不做顺序断言）
	require.Equal(t, 2, recorderCalls(recorder), "两个候选各上报一次")
	for _, id := range []int64{10, 20} {
		ri, ok := p.sched.Runtime(id)
		require.True(t, ok)
		require.Equal(t, domain.StatusDisabled, ri.Status, "账号 %d 应被摘除", id)
	}

	require.NoError(t, p.rec.Close(context.Background()))
}

// TestSniffCodexWSDeath sniff 纯函数：判死码集语义（error.code/error.type ∈
// {token_invalidated, token_revoked}——值大小写不敏感，复用 SDK 判死码集
// auth_errors.go:28-31 + classifyAT401 字段路径）+ 网关侧业务判死码
// （402 deactivated_workspace，account_fatal.go）；type=error 帧前提（鉴权码
// 那一支）；非判死错误事件/业务帧误含错误帧标记 → nil（不误判死）。
func TestSniffCodexWSDeath(t *testing.T) {
	// error.code 命中（判死码值大写形态同样命中——解析层 EqualFold）
	hit := sniffCodexWSDeath([]byte(`{"type":"error","error":{"code":"token_invalidated","message":"x"}}`))
	require.NotNil(t, hit)
	var ap *codexsdk.AuthPermanentlyRevokedError
	require.ErrorAs(t, hit, &ap)
	require.Equal(t, "token_invalidated", ap.Code)
	require.Contains(t, string(ap.Raw), "token_invalidated", "Raw 原帧保留（诊断用）")
	// error.type 命中（token_revoked）
	hit2 := sniffCodexWSDeath([]byte(`{"type":"error","error":{"message":"y","type":"token_revoked"}}`))
	require.NotNil(t, hit2)
	var ap2 *codexsdk.AuthPermanentlyRevokedError
	require.ErrorAs(t, hit2, &ap2)
	require.Equal(t, "token_revoked", ap2.Code)
	// 判死码值任意大小写 → 命中（EqualFold 语义与 SDK classifyAT401 一致）
	hit3 := sniffCodexWSDeath([]byte(`{"type":"error","error":{"code":"TOKEN_INVALIDATED"}}`))
	require.NotNil(t, hit3)
	var ap3 *codexsdk.AuthPermanentlyRevokedError
	require.ErrorAs(t, hit3, &ap3)
	require.Equal(t, "token_invalidated", ap3.Code)
	// 业务判死码（402 deactivated_workspace）：两种承载都命中——带 type 的错误
	// 事件帧，与裸 error 信封体（上游 402 就是用户实证的那种形状）。
	for _, frame := range []string{
		`{"type":"error","status":402,"error":{"message":"Payment Required","type":null,"code":"deactivated_workspace","param":null}}`,
		`{"error":{"message":"Payment Required","type":null,"code":"deactivated_workspace","param":null},"status":402}`,
	} {
		got := sniffCodexWSDeath([]byte(frame))
		require.NotNil(t, got, "业务判死码必须命中：%s", frame)
		var ad *codexsdk.AccountDisabledError
		require.ErrorAs(t, got, &ad, "判定类型由 SDK 产出（网关零码集）")
		require.Equal(t, 402, ad.StatusCode, "状态码取帧内 status")
		require.Equal(t, "deactivated_workspace", ad.Detail)
	}
	// 非错误帧（业务内容误含错误帧标记）→ 不判死
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"response.output_text.delta","delta":"token_invalidated"}`)))
	// 错误帧里用户内容回显判死码字样（不是 error.code 字段）→ 不判死
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"response.output_text.delta","delta":"deactivated_workspace"}`)))
	// 402 但没有业务判死码（额度/计费类可恢复）→ 不判死
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"error","status":402,"error":{"code":"insufficient_quota"}}`)))
	// 非判死错误事件（server_error 等业务错误透传不判死）
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"error","error":{"code":"server_error","message":"x"}}`)))
	// 普通帧 / 空 / 非 JSON
	require.Nil(t, sniffCodexWSDeath([]byte(`{"type":"response.created","response":{"id":"r"}}`)))
	require.Nil(t, sniffCodexWSDeath(nil))
	require.Nil(t, sniffCodexWSDeath([]byte("binary\x00token_invalidated")))
	// typed-nil 门禁：返回 error 接口时必须**真** nil。ClassifyAccountFatal 返回
	// 具体指针类型，直接 return 会把 nil 指针装进 error（`!= nil` 成立）⇒ 每一帧
	// 都被当成判死帧，账号被毒化摘除、happy path 直接断流（require.Nil 走反射判空，
	// 抓不到这个）。这里用 `== nil` 直接看接口位。
	for _, f := range [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"r"}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		[]byte(`{"type":"error","error":{"code":"server_error"}}`),
		nil,
	} {
		require.True(t, sniffCodexWSDeath(f) == nil, "未命中必须返回真 nil（不是 typed-nil）：%s", f)
	}
}

// TestCodexWSAdapterMissing 适配层未装配（SetCodex nil）→ 501 语义显式拒绝
// （错误帧 + 不上游接触——防 nil 误走凭据缺失 502）。
func TestCodexWSAdapterMissing(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{200}, 0)
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-10")}, up.URL, nil, store)
	p.codex = nil // 模拟 main 未装配

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
		map[int64]*domain.AccountExt{10: {AccountID: 10, CredentialType: credential.TypeCodexPAT, CodexIdentity: &domain.CodexIdentity{InstallationID: "i"}, CodexPATKey: &pat}}, up.URL, nil, store)
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

// TestCodexWSUpstreamSeesDisguisedUA R4（端到端）：客户端带 curl UA + 伪造
// Originator ⇒ 上游握手看到的是 SDK 自己的伪装默认值。断言引库内常量
// codexsdk.DefaultCodexUserAgent / DefaultOriginator，绝不把指纹字符串抄进用例
// （它是随 SDK 版本漂移的值，硬编码必成腐化点）。
//
// 机理（本用例存在的理由）：dialCodexWS 不喂任何客户端头；SDK 侧 buildHeaders
// 设伪装默认（client.go:305-306），客户端 UA 无从顶掉。所以「网关侧不递 UA」
// 正是让 SDK 默认伪装值得以保留的那一步，网关不需要另设 UA。
func TestCodexWSUpstreamSeesDisguisedUA(t *testing.T) {
	up, hooks := newCodexWSUpstream(t, []int{200}, 3)
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newTestCodexWSProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexWSExt(10, "at-10", "rt-10")}, up.URL, nil, store)

	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	// 客户端把「伪装身份族 + 自定义头 + footprint + 网关自身写入」一并塞进来：按 spec §12
	// 裁决，codex 面只发 SDK 自己写的头，这些一个都不该到上游。
	c := dialResponsesWSHeaders(t, srv, http.Header{
		"Authorization":       {"Bearer ck-1"},
		"User-Agent":          {"curl/8.7.1"},
		"Originator":          {"evil-vscode"},
		"Session-Id":          {"client-session"},
		"Thread-Id":           {"client-thread"},
		"X-Client-Request-Id": {"client-req"},
		"X-Codex-Window-Id":   {"client-window"},
		"OpenAI-Beta":         {"responses_websockets=1999-01-01"},
		"X-Client-Version":    {"codex-1.2.3"},
		"X-Opencode-Session":  {"oc-1"},
		"X-Client-Originator": {"Zed"},
		"X-Request-Id":        {"req-1"},
	})
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	// 事件流 3 帧（created/delta/completed）+ 1 回声——只需握手头被上游观测即可。
	for range 4 {
		readResponsesWSFrame(t, c)
	}

	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	require.NotEmpty(t, hooks.headers, "上游握手未被观测")
	got := hooks.headers[0]
	require.Equal(t, codexsdk.DefaultCodexUserAgent, got.Get("User-Agent"),
		"上游必须看到 SDK 伪装默认 UA（客户端 curl UA 已被剔）")
	require.Equal(t, codexsdk.DefaultOriginator, got.Get("Originator"),
		"上游必须看到 SDK 伪装默认 originator")
	// spec §12：客户端头一律不递。分两类断言 ——
	// ① 纯客户端键（SDK 永不写）：必须整键不出现（X-Client-Version / X-Opencode-Session
	//    过去是透传的，属旧契约）。
	for _, k := range []string{"X-Client-Version", "X-Opencode-Session", "X-Client-Originator", "X-Request-Id"} {
		require.Empty(t, got.Get(k), "codex 面不得把客户端头 %s 递给上游（spec §12）", k)
		_, ok := got[k]
		require.False(t, ok, "客户端头 %s 不得出现在上游握手头里（map 槽位级断言）", k)
	}
	// ② 身份族键：SDK 会依账号 ext 自己写（网关身份）⇒ 断言的是「不是客户端那个值」，
	//    而不是「不存在」（写成不存在会与 SDK 的正常行为冲突）。
	for _, tc := range []struct{ key, clientVal string }{
		{"Session-Id", "client-session"},
		{"Thread-Id", "client-thread"},
		{"X-Client-Request-Id", "client-req"},
		{"X-Codex-Window-Id", "client-window"},
	} {
		require.NotEqual(t, tc.clientVal, got.Get(tc.key),
			"身份族 %s 必须由 SDK 依账号 ext 写，客户端值不得顶掉（spec §12）", tc.key)
	}
	// OpenAI-Beta 同理：由 SDK 自己写网关 beta 值，客户端值不得顶掉。
	require.NotEmpty(t, got.Get("OpenAI-Beta"), "beta 头由 SDK 写，必须存在")
	require.NotContains(t, got.Get("OpenAI-Beta"), "1999-01-01", "客户端 beta 值不得顶掉 SDK 的")
}

// 伪装身份装配已收敛到 scheduler.Selection.IdentityFor（一次给全四元组，注入
// 值取自槽状态、与账号 ext 无关）；此处不再有 proxy 侧组装函数可测——其正确性由
// scheduler 包 TestSelectionCodexIdentityFromSession 覆盖。
