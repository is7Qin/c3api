// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// S4 验收（spec §3.4 G1/G3a/G3b/G4/G6/G11）：网关侧会话身份与推进/退休语义。
//
// 假上游是**可复用连接**的 codex responses WS 服务端（同一连接循环读帧、逐帧回
// created/metadata/completed、不主动关闭），所以 `upgrades`（升级次数）是"是否复用"
// 的唯一判据；帧内 `client_metadata` 是"网关实际发了什么身份"的唯一判据。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// sessionWSStep 上游对**一次**请求的响应脚本：created →（可选）metadata →
// completed。usage=false 的 completed 不带 usage（非流式臂据此判定"未取到 usage"
// ⇒ 不推进）。
type sessionWSStep struct {
	id    string
	ts    string // response.metadata 携带的事件级 ts（空 = 不发）
	usage bool
}

func (s sessionWSStep) frames() []string {
	out := []string{
		`{"type":"response.created","response":{"id":"` + s.id + `","object":"response","status":"in_progress"}}`,
	}
	if s.ts != "" {
		out = append(out, `{"type":"response.metadata","response_id":"`+s.id+`","headers":{"x-codex-turn-state":"`+s.ts+`"}}`)
	}
	usage := ""
	if s.usage {
		usage = `,"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}`
	}
	out = append(out, `{"type":"response.completed","response":{"id":"`+s.id+`","object":"response","status":"completed"`+usage+`}}`)
	return out
}

// sessionWSReq 一次上游请求的关键身份字段（帧内 client_metadata）。
type sessionWSReq struct {
	installation string
	thread       string
	window       string
	ts           string
}

// sessionWSUpstream 可复用连接的 codex responses 假上游。
type sessionWSUpstream struct {
	srv         *httptest.Server
	handshakeTS string
	// prewarmTS 非空：对 SDK 的预热帧（generate:false，HTTP→WS 新连接首请求，官方
	// 0.169.0 行为）在 response.metadata 签发这个 ts。预热帧单独记进 prewarms，
	// 不消耗 steps、不进 reqs、不投递 arrived、不受 gate 阻塞——reqs 只含真实
	// response.create，"第 N 个请求"类断言的含义不变。
	prewarmTS string
	prewarms  []sessionWSReq
	gate      <-chan struct{} // 非 nil：收到请求帧后阻塞到它关闭才回帧（G6 占满租借用）

	mu       sync.Mutex
	steps    []sessionWSStep
	last     sessionWSStep
	upgrades int
	reqs     []sessionWSReq
	posts    int
	bodies   [][]byte // HTTP 腿的请求体（G5 的"无身份键"判据）
	paths    []string
	conns    []*websocket.Conn
	closed   int
	closedCh chan struct{}
	closedON sync.Once
	// arrived 每收到一个请求帧就投递一次（用例的确定性同步点：通道 + deadline，
	// 不用 sleep）。
	arrived chan sessionWSReq
}

func newSessionWSUpstream(t *testing.T, handshakeTS string, steps ...sessionWSStep) (*httptest.Server, *sessionWSUpstream) {
	t.Helper()
	u := &sessionWSUpstream{
		handshakeTS: handshakeTS,
		closedCh:    make(chan struct{}),
		arrived:     make(chan sessionWSReq, 16),
	}
	if len(steps) > 0 {
		u.steps = steps
		u.last = steps[len(steps)-1]
	}
	u.srv = httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(func() {
		u.mu.Lock()
		conns := u.conns
		u.conns = nil
		u.mu.Unlock()
		for _, c := range conns {
			_ = c.CloseNow()
		}
		u.srv.CloseClientConnections()
		u.srv.Close()
	})
	return u.srv, u
}

func (u *sessionWSUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if !isCodexWSUpgrade(r) {
		u.serveHTTP(w, r)
		return
	}
	u.serveWS(w, r)
}

// isCodexWSUpgrade 判定 WS 升级请求（与 coder/websocket Accept 的判据一致）。
func isCodexWSUpgrade(r *http.Request) bool {
	for _, v := range r.Header.Values("Connection") {
		if v == "Upgrade" || v == "upgrade" {
			return r.Header.Get("Upgrade") == "websocket"
		}
	}
	return false
}

func (u *sessionWSUpstream) serveWS(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.upgrades++
	hs, gate := u.handshakeTS, u.gate
	u.mu.Unlock()
	if hs != "" {
		// Accept 会把当前 w.Header() 写进 101 响应 ⇒ 这就是"握手响应头签发 ts"。
		w.Header().Set(codexsdk.HeaderTurnState, hs)
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	u.mu.Lock()
	u.conns = append(u.conns, c)
	u.mu.Unlock()
	defer c.CloseNow()
	for {
		_, data, rerr := c.Read(r.Context())
		if rerr != nil {
			u.mu.Lock()
			u.closed++
			u.mu.Unlock()
			u.closedON.Do(func() { close(u.closedCh) })
			return
		}
		cm := gjson.GetBytes(data, "client_metadata")
		req := sessionWSReq{
			installation: cm.Get("x-codex-installation-id").String(),
			thread:       cm.Get("thread_id").String(),
			window:       cm.Get("x-codex-window-id").String(),
			ts:           cm.Get("x-codex-turn-state").String(),
		}
		if isPrewarmFrame(data) {
			u.mu.Lock()
			u.prewarms = append(u.prewarms, req)
			pw := sessionWSStep{id: fmt.Sprintf("pw-%d", len(u.prewarms)), ts: u.prewarmTS}
			u.mu.Unlock()
			for _, f := range pw.frames() {
				if werr := c.Write(r.Context(), websocket.MessageText, []byte(f)); werr != nil {
					return
				}
			}
			continue
		}
		u.mu.Lock()
		u.reqs = append(u.reqs, req)
		step := u.nextStepLocked()
		u.mu.Unlock()
		select {
		case u.arrived <- req:
		default:
		}
		if gate != nil {
			<-gate // 占住租借：上游不回帧，网关的流式 handler 阻塞在读上
		}
		for _, f := range step.frames() {
			if werr := c.Write(r.Context(), websocket.MessageText, []byte(f)); werr != nil {
				return
			}
		}
	}
}

// serveHTTP 非 WS 腿：记录请求体/路径后按端点回包（非 responses 面用例只关心
// "没新增 WS 连接、没带身份键"）。
func (u *sessionWSUpstream) serveHTTP(w http.ResponseWriter, r *http.Request) {
	b := readFakeUpstreamBody(r)
	u.mu.Lock()
	u.posts++
	u.bodies = append(u.bodies, b)
	u.paths = append(u.paths, r.URL.Path)
	u.mu.Unlock()
	switch {
	case strings.Contains(r.URL.Path, "images"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
	case strings.Contains(r.URL.Path, "usage"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"plan_type":"plus"}`)
	default:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+sessionWSStep{id: "http1"}.frames()[0]+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
}

func (u *sessionWSUpstream) nextStepLocked() sessionWSStep {
	if len(u.steps) == 0 {
		return u.last
	}
	s := u.steps[0]
	u.steps = u.steps[1:]
	u.last = s
	return s
}

func (u *sessionWSUpstream) prewarmRequests() []sessionWSReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]sessionWSReq(nil), u.prewarms...)
}

func (u *sessionWSUpstream) upgradeCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrades
}

func (u *sessionWSUpstream) requests() []sessionWSReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]sessionWSReq(nil), u.reqs...)
}

func (u *sessionWSUpstream) httpBodies() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// newSessionWSProxy 构造 codex WS 出站测试代理（单账号 10；K 与传输经选项注入）。
// 返回账号快照集合（用例要改 ext 后 reload）。
func newSessionWSProxy(t *testing.T, upstream string, slots int, opts ...codexRespProxyOption) (*Proxy, map[int64][]*domain.Account) {
	t.Helper()
	ext := codexPATExt(10, "pat-session")
	ext.CodexIdentity = &domain.CodexIdentity{InstallationID: "inst-session"}
	all := append([]codexRespProxyOption{withCodexIdentitySlots(slots), withCodexResponsesTransport(domain.CodexResponsesTransportWS)}, opts...)
	p, _, accs := newTestCodexRespProxyAccs(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: ext},
		upstream, nil, nil, &captureLogStore{},
		all...)
	return p, accs
}

// barrierServer 在每个请求的 handler **返回之后**投递一次计数：网关侧的
// finish→Release 全在 handler 内同步完成，所以"handler 已返回"就是"服务端已释放
// 租借"的确定性证据。客户端 `Do` 的返回不能当这个屏障（首帧/取消都比它早）。
type barrierServer struct {
	inner http.Handler
	done  chan int
	n     atomic.Int32
}

func newBarrierServer(t *testing.T, h http.Handler) (*httptest.Server, *barrierServer) {
	t.Helper()
	b := &barrierServer{inner: h, done: make(chan int, 64)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.inner.ServeHTTP(w, r)
		b.done <- int(b.n.Add(1))
	}))
	t.Cleanup(srv.Close)
	return srv, b
}

// waitHandlers 等到**累计**至少 n 个 handler 返回（通道 + timer deadline，禁 sleep）。
// 用累计量而非"下一次"：多个请求的 handler 返回顺序不保证与发起顺序一致，逐个等
// "下一个"会把颠倒的两次收尾错认成同一次，于是在租借仍被持有时就放行——G6 的 N2：
// 取消请求的客户端 `Do` 已返回、服务端还没 Release，下一轮直接 429（上游无帧）。
func (b *barrierServer) waitHandlers(t *testing.T, n int, what string) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-b.done:
			if got >= n {
				return
			}
		case <-timer.C:
			t.Fatalf("%s：handler 未在 deadline 内返回（服务端未收尾）", what)
		}
	}
}

// sessionStream 一次流式请求的客户端收尾面（Do 的返回值通道 + cancel）。
type sessionStream struct {
	cancel context.CancelFunc
	done   chan struct {
		resp *http.Response
		err  error
	}
}

// startSessionStream 起一个流式 /v1/responses 请求（不读 body，占住租借）。
func startSessionStream(t *testing.T, srv *httptest.Server, parent context.Context) *sessionStream {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"input":"hi"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	s := &sessionStream{cancel: cancel, done: make(chan struct {
		resp *http.Response
		err  error
	}, 1)}
	go func() {
		resp, err := http.DefaultClient.Do(req)
		s.done <- struct {
			resp *http.Response
			err  error
		}{resp: resp, err: err}
	}()
	return s
}

// wait 等客户端收尾（通道 + deadline；响应体不读，直接关）。
func (s *sessionStream) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case r := <-s.done:
		if r.resp != nil {
			r.resp.Body.Close()
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s：客户端未在 deadline 内收尾", what)
	}
}

// cancelAndRelease 取消请求并放行被卡住的上游，再等客户端收尾（取消路径标准收尾）。
func (s *sessionStream) cancelAndRelease(t *testing.T, gate chan struct{}, what string) {
	t.Helper()
	s.cancel()
	close(gate)
	s.wait(t, what)
}

// waitArrived 等上游收到一个请求帧（租借已被占住的确定性同步点）。
func waitArrived(t *testing.T, u *sessionWSUpstream, what string) {
	t.Helper()
	select {
	case <-u.arrived:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s：上游未在 deadline 内收到请求帧", what)
	}
}

// setGate 换掉上游的阻塞门（nil = 后续新连接不再阻塞）。
func (u *sessionWSUpstream) setGate(ch <-chan struct{}) {
	u.mu.Lock()
	u.gate = ch
	u.mu.Unlock()
}

// postResponsesOK 发一次非流式 responses 并读尽。
func postResponsesOK(t *testing.T, srv *httptest.Server, body string) {
	t.Helper()
	resp := postResponses(t, srv, body)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
}

// TestSessionG1WindowAdvanceKeepsConnection G1：同会话连续请求把 window 从 0 推到 1
// ⇒ 全程 `upgrades == 1`（window 不是连接身份），帧内 window 分别 t:0 / t:1。
func TestSessionG1WindowAdvanceKeepsConnection(t *testing.T) {
	up, u := newSessionWSUpstream(t, "", sessionWSStep{id: "g1", usage: true})
	p, _ := newSessionWSProxy(t, up.URL, 1)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// span 由 (thread, window) 确定性抽样 ∈ [48,96]，每轮边界位置都不同。
	// 两种固定次数都会偶发红：100 次在 span(0)+span(1) ≤ 100 时提前进入 :2
	// （约 0.6%），而 100 < span(0)+1 时根本看不到边界。因此驱动到"看见 window 变化"就停，
	// 上限 2*spanHi（正常 ≤ 97 次就停）。
	const maxDrives = 192
	var reqs []sessionWSReq
	for range maxDrives {
		postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
		reqs = u.requests()
		if n := len(reqs); n > 0 && reqs[n-1].window != reqs[0].window {
			break
		}
	}
	require.Equal(t, 1, u.upgradeCount(), "跨窗口不重连（window 不进连接身份）")

	thread := reqs[0].thread
	require.NotEmpty(t, thread)
	require.Equal(t, thread+":0", reqs[0].window, "首请求 window = 0")
	crossed := -1
	for i, r := range reqs {
		require.Equal(t, thread, r.thread, "同一 thread 全程不变（不换会话）")
		switch r.window {
		case thread + ":0":
			require.Equal(t, -1, crossed, "window 单调递增（不回落）")
		case thread + ":1":
			if crossed == -1 {
				crossed = i
			}
		default:
			t.Fatalf("第 %d 个请求 window = %q，期望 %s:0 或 :1", i, r.window, thread)
		}
	}
	require.NotEqual(t, -1, crossed, "%d 次内必须观察到跨窗（span 上界 96）", maxDrives)
	// 边界 = 第 1 个窗口的轮数 = span(0) ∈ [spanLo,spanHi]（codex-sdk/identity.go:66-69）。
	// 旧断言写成 49/97 就是差一：span=48 时边界落在下标 48。
	require.GreaterOrEqual(t, crossed, 48, "第 1 个窗口至少 48 轮（spanLo）")
	require.LessOrEqual(t, crossed, 96, "第 1 个窗口至多 96 轮（spanHi）")
	require.Equal(t, thread+":1", reqs[crossed].window)
	require.Equal(t, thread+":0", reqs[crossed-1].window, "跨越只发生一次")
}

// TestSessionG3bSlotTurnStateNotReplayedAcrossRequests G3b：槽位路径 ⇒ 上游握手、
// 预热与事件都签发 ts，网关不跨请求重放（Sink=nil、Turn.State 恒空）；连接复用照常。
// 唯一例外是 SDK 的 HTTP→WS 预热：新连接首请求在**同一次交换内**先发预热帧，真实帧
// 带预热签发的 ts（官方 0.169.0 行为，codex-sdk 49c2950 exchange.prewarm）；下一个
// 请求命中缓存连接、不预热，不带任何 ts。
func TestSessionG3bSlotTurnStateNotReplayedAcrossRequests(t *testing.T) {
	up, u := newSessionWSUpstream(t, "T1",
		sessionWSStep{id: "b1", ts: "T2", usage: true},
		sessionWSStep{id: "b2", usage: true})
	u.prewarmTS = "TP"
	p, _ := newSessionWSProxy(t, up.URL, 1)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)

	pws := u.prewarmRequests()
	require.Len(t, pws, 1, "只有新连接首请求预热")
	require.Equal(t, "", pws[0].ts, "预热帧不带 ts")
	reqs := u.requests()
	require.Len(t, reqs, 2)
	require.Equal(t, "TP", reqs[0].ts, "同一次交换内：真实帧带预热签发的 ts（官方行为）")
	require.Equal(t, "", reqs[1].ts, "跨请求不重放：握手 T1、事件 T2、预热 TP 都不带")
	require.Equal(t, 1, u.upgradeCount(), "不带 ts 不影响连接复用")
}

// TestSessionG4IsolationAcrossSessions G4：同账号两条会话交替 ⇒ 两条 thread 各一条
// 连接（upgrades == 2），帧内身份各自正确、不串台。
func TestSessionG4IsolationAcrossSessions(t *testing.T) {
	up, u := newSessionWSUpstream(t, "",
		sessionWSStep{id: "c1", usage: true},
		sessionWSStep{id: "c2", usage: true},
		sessionWSStep{id: "c3", usage: true},
		sessionWSStep{id: "c4", usage: true})
	p, _ := newSessionWSProxy(t, up.URL, 2) // K=2：轮转发会话
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	for range 4 {
		postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	}

	reqs := u.requests()
	require.Len(t, reqs, 4)
	require.Equal(t, 2, u.upgradeCount(), "两条会话 = 两条连接")
	require.NotEqual(t, reqs[0].thread, reqs[1].thread, "相邻请求落在不同会话")
	require.Equal(t, reqs[0].thread, reqs[2].thread, "轮转回到同一会话")
	require.Equal(t, reqs[1].thread, reqs[3].thread)
	require.Equal(t, reqs[0].thread+":0", reqs[0].window)
	require.Equal(t, reqs[1].thread+":0", reqs[1].window)
	require.Equal(t, "inst-session", reqs[0].installation)
	require.Equal(t, "inst-session", reqs[1].installation, "同账号同 installation")
}

// TestSessionG6LeaseExclusiveAndCancelDoesNotAdvance G6：租借独占 + 取消不推进。
//
//   - 独占：占满 K（=1）个租借（body 不读）时第 K+1 个请求**不得进入该账号的
//     claim**（按现有门禁失败，不排队）；
//   - 取消不推进：用**行为观测**而不是读会话私有计数器——策略 WMax=1 时任何一次
//     推进都会在一个窗口（48–96 轮）内把线程退掉，所以"取消 97 次（> 最大 span 96）
//     之后线程与 window 仍是首轮的"就是"这些取消一次都没计数"的可观测形式；
//     正向（成功才推进/退休）由 G1 覆盖。
//
// 服务端释放的同步点 = **handler 返回**（finish→Release 在 handler 内同步完成）：
// 客户端 `Do` 的返回只代表首帧/取消，早于服务端收尾，拿它当屏障会偶发 429（N2）。
func TestSessionG6LeaseExclusiveAndCancelDoesNotAdvance(t *testing.T) {
	gate := make(chan struct{})
	up, u := newSessionWSUpstream(t, "", sessionWSStep{id: "d1", usage: true})
	u.gate = gate
	p, _ := newSessionWSProxy(t, up.URL, 1, withCodexRotatePolicy(codexsdk.RotatePolicy{WMaxLo: 1, WMaxHi: 1}))
	srv, barrier := newBarrierServer(t, AIRouter(p))

	// 1) 占住唯一租借：流式请求，上游被 gate 卡住（不回帧），客户端不读 body。
	first := startSessionStream(t, srv, t.Context())
	waitArrived(t, u, "首个请求")

	// 2) 第 K+1 个请求：门禁满 ⇒ 不得进入 claim（失败即可，不是排队等待）。
	second := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	require.NotEqual(t, http.StatusOK, second.StatusCode,
		"占满后第 K+1 个请求必须失败（不得排队等待）：%s", string(secondBody))
	require.Equal(t, 1, u.upgradeCount(), "失败请求不得新开会话连接")
	first.cancelAndRelease(t, gate, "首个请求")
	// 首个请求的取消与失败请求的 handler 都返回后，服务端租借才算真释放（finish→
	// Release 在 handler 内）。等它们再进入第 3 步，否则下一轮会在 claim 前拿到
	// 429 no available account——上游一帧都收不到（CI 双核下的 N2）。
	barrier.waitHandlers(t, 2, "首个请求与失败请求")
	u.setGate(nil)

	// 3) 提前取消（未读到终态）⇒ 不推进、不退休。逐轮：占住 → 取消 → 等服务端收尾。
	const cancels = 97 // > spanHi(96)：任何一次计数都会跨过窗口 ⇒ 退休换线程
	for i := range cancels {
		ch := make(chan struct{})
		u.setGate(ch)
		s := startSessionStream(t, srv, t.Context())
		waitArrived(t, u, fmt.Sprintf("第 %d 个取消请求", i))
		s.cancelAndRelease(t, ch, fmt.Sprintf("第 %d 个取消请求", i))
		barrier.waitHandlers(t, i+3, fmt.Sprintf("第 %d 个取消请求", i))
	}
	u.setGate(nil)

	// 4) 一次成功请求：线程与 window 必须仍是首轮的（取消一次都没计数）。
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)

	reqs := u.requests()
	require.Len(t, reqs, cancels+2, "上游只看到首个请求 + 97 个取消请求 + 1 个成功请求（失败请求不进 claim）")
	thread := reqs[0].thread
	require.NotEmpty(t, thread)
	for i, r := range reqs {
		require.Equal(t, thread, r.thread, "第 %d 个请求的线程不变（取消不推进/不退休）", i)
		require.Equal(t, thread+":0", r.window, "第 %d 个请求的 window 仍是 0", i)
	}
}

// TestSessionG11InstallationChangeRebuildsSessions G11：改账号 ext 的 installation
// ⇒ 旧会话连接全关、按新 installation 重建（新 thread、window 归零、首请求不带 ts）。
func TestSessionG11InstallationChangeRebuildsSessions(t *testing.T) {
	up, u := newSessionWSUpstream(t, "", sessionWSStep{id: "e1", usage: true})
	p, accs := newSessionWSProxy(t, up.URL, 2)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 两条会话各建一条连接（两个在飞/已完成请求 → 两条 idle 连接）。
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	before := u.requests()
	require.Len(t, before, 2)
	require.Equal(t, 2, u.upgradeCount())

	// 改 installation（账号 ext 写入是管理面路径；reload 语义等价）。
	accs[10][0].Ext.CodexIdentity.InstallationID = "inst-session-new"
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)

	// 旧会话连接必须被关（逐个 RetireThread）。
	select {
	case <-u.closedCh:
	case <-time.After(15 * time.Second):
		t.Fatal("installation 变化后旧会话连接未被关闭")
	}

	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	after := u.requests()
	require.Len(t, after, 3)
	require.Equal(t, "inst-session-new", after[2].installation, "新会话取新 installation")
	require.NotEqual(t, before[0].thread, after[2].thread, "会话重建（新 thread）")
	require.Equal(t, after[2].thread+":0", after[2].window, "window 归零")
	require.Equal(t, "", after[2].ts, "新会话首请求不带 ts")
}

// TestSessionG5NonResponsesSurfaceNoIdentity G5：非 responses 面（Search/images/
// usage）不带身份、不新增 WS 连接。
func TestSessionG5NonResponsesSurfaceNoIdentity(t *testing.T) {
	up, u := newSessionWSUpstream(t, "", sessionWSStep{id: "f1", usage: true})
	p, accs := newSessionWSProxy(t, up.URL, 1)
	cred := domain.CredentialFromExt(accs[10][0].Ext)

	_, err := p.codex.Search(context.Background(), &cred, p.codex.DirectEgress(), []byte(`{"model":"gpt-5.6"}`))
	require.NoError(t, err)
	_, err = p.codex.GenerateImage(context.Background(), &cred, p.codex.DirectEgress(),
		&domain.ImageGenParams{Model: "gpt-image-1", Prompt: "cat"})
	require.NoError(t, err)
	_, err = p.codex.GetUsageSnapshot(context.Background(), &cred)
	require.NoError(t, err)

	require.Zero(t, u.upgradeCount(), "非 responses 面不得新增 WS 连接")
	bodies := u.httpBodies()
	require.Len(t, bodies, 3)
	for i, b := range bodies {
		require.False(t, gjson.GetBytes(b, "client_metadata").Exists(), "第 %d 个请求体不得出现 client_metadata", i)
		require.NotContains(t, string(b), "x-codex-installation-id", "第 %d 个请求体不得出现身份键", i)
		require.NotContains(t, string(b), "x-codex-window-id", "第 %d 个请求体不得出现 window 键", i)
	}
}

// mustPost 构造一次 /v1/responses 请求（Bearer ck-1）。G6 用它拿到 error 而不是
// 在 helper 里 require。
func mustPost(t *testing.T, srv *httptest.Server) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4o","input":"hi"}`)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestSessionAccountRemovalClosesTransportResources 账号移除的端到端接线（§3.2）：
// 账号从账号集合消失（重载的池注册表差分 = 实际移除事件）⇒ 其 WS 空闲连接被关、
// 请求不再路由到它；账号回来后按新条目重建（旧 client 不复活）。
func TestSessionAccountRemovalClosesTransportResources(t *testing.T) {
	up, u := newSessionWSUpstream(t, "", sessionWSStep{id: "arm1", usage: true})
	p, accs := newSessionWSProxy(t, up.URL, 1)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	require.Equal(t, 1, u.upgradeCount())

	// 账号消失（管理面删除 = 装载结果里不再有它）→ 重载 ⇒ 池注册表差分触发移除。
	account := accs[10][0]
	delete(accs, 10)
	require.NoError(t, p.sched.InvalidateAllSync())
	// 把 staged 静态根刷出去并清掉该路由的决策（生产里由编译车道对空组发布）；
	// publishTestRoutes 只遍历 loader 里仍有账号的组，这里是空组，得显式驱动。
	p.sched.PublishDecisionForTest(scheduler.RouteRefFor(10, string(domain.FormatOpenAIResponses), ""), nil)

	select {
	case <-u.closedCh:
	case <-time.After(15 * time.Second):
		t.Fatal("账号移除后该账号的 WS 空闲连接未被关闭")
	}

	gone := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	goneBody, _ := io.ReadAll(gone.Body)
	gone.Body.Close()
	require.NotEqual(t, http.StatusOK, gone.StatusCode, "账号已移除，不得再路由：%s", string(goneBody))
	require.Equal(t, 1, u.upgradeCount(), "失败请求不得新开连接")

	// 账号回来（重新导入）⇒ 重建条目/连接，新一轮会话可正常出网。
	accs[10] = []*domain.Account{account}
	require.NoError(t, p.sched.InvalidateAllSync())
	publishTestRoutes(t, p.sched)
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	require.Equal(t, 2, u.upgradeCount(), "旧 idle 已关闭：重新出网必须重拨")
	reqs := u.requests()
	require.Len(t, reqs, 2)
	require.NotEqual(t, reqs[0].thread, reqs[1].thread, "账号移除后是新会话（池不复活）")
}

// TestSessionG2NaturalRetirementClosesOldConnection G2（自然退休的**端到端接线**）：
// WMax=1 时窗口跨过即退休，链路 StepIdentity → pool.retire → ClientTable.RetireThread
// → SDK 必须真的把旧线程的空闲连接关掉；新线程首请求不带 ts。
//
// 为什么需要这一条：G2 的两半过去是分开测的（会话层注入 retire spy / 连接层直接调
// 适配层 RetireThread），变异「网关省略退休调用」（M4）只让前一半变红，连接层仍绿——
// 装配线（Config.RetireThread 忘接、或 StepIdentity 不调 pool.retire）掉链子时没有
// 任何用例现红。这里把三段合成一条真实请求路径。
func TestSessionG2NaturalRetirementClosesOldConnection(t *testing.T) {
	up, u := newSessionWSUpstream(t, "",
		sessionWSStep{id: "g2", ts: "T-win", usage: true})
	p, _ := newSessionWSProxy(t, up.URL, 1, withCodexRotatePolicy(codexsdk.RotatePolicy{WMaxLo: 1, WMaxHi: 1}))
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 驱动到线程变化：span ∈ [48,96] ⇒ 97 轮内必发生；且不在首轮发生（span ≥ 48）。
	firstThread := ""
	retiredAt := -1
	for i := range 97 {
		postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
		reqs := u.requests()
		if i == 0 {
			firstThread = reqs[0].thread
			continue
		}
		if reqs[len(reqs)-1].thread != firstThread {
			retiredAt = i
			break
		}
	}
	require.NotEqual(t, -1, retiredAt, "WMax=1 必须在窗口跨过后退休（≤97 轮）")
	require.GreaterOrEqual(t, retiredAt, 48, "首窗口至少 48 轮（span ≥ 48）")

	// 旧线程的空闲连接被关（退休的唯一可见结果）。
	select {
	case <-u.closedCh:
	case <-time.After(15 * time.Second):
		t.Fatal("自然退休后旧线程的空闲连接未被关闭（退休调用没接线）")
	}

	reqs := u.requests()
	last := reqs[len(reqs)-1]
	require.NotEqual(t, firstThread, last.thread, "退休后换新线程")
	// 新线程首请求在新连接上先预热（预热应答不签发 ts）；旧线程事件签发的 T-win
	// 不跨请求重放。
	require.GreaterOrEqual(t, len(u.prewarmRequests()), 2, "新线程新连接首请求预热")
	require.Empty(t, last.ts, "槽位路径不重放 ts（新线程首请求同样不带旧线程的 T-win）")
	require.GreaterOrEqual(t, u.upgradeCount(), 2, "新线程重拨（旧连接已摘除）")
}
