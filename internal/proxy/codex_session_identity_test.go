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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
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
	gate        <-chan struct{} // 非 nil：收到请求帧后阻塞到它关闭才回帧（G6 占满租借用）

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
	b, _ := io.ReadAll(r.Body)
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
func newSessionWSProxy(t *testing.T, upstream string, slots int) (*Proxy, map[int64][]*domain.Account) {
	t.Helper()
	ext := codexPATExt(10, "pat-session")
	ext.CodexIdentity = &domain.CodexIdentity{InstallationID: "inst-session"}
	p, _, accs := newTestCodexRespProxyAccs(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: ext},
		upstream, nil, nil, &captureLogStore{},
		withCodexIdentitySlots(slots),
		withCodexResponsesTransport(domain.CodexResponsesTransportWS))
	return p, accs
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

	// span ∈ [48,96]：驱动到跨过去（100 次足够，且第二次跨越还来不及发生）。
	for range 100 {
		postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	}

	reqs := u.requests()
	require.Len(t, reqs, 100)
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
	require.GreaterOrEqual(t, crossed, 49, "第 1 个窗口至少 48 轮（48 轮的成功请求之后才跨）")
	require.LessOrEqual(t, crossed, 97, "第 1 个窗口至多 96 轮")
	require.Equal(t, thread+":1", reqs[crossed].window)
	require.Equal(t, thread+":0", reqs[crossed-1].window, "跨越只发生一次")
}

// TestSessionG3aReplayPolicyOn G3a：策略 true ⇒ 握手签发 T1、事件签发 T2，下一请求
// 带 T2（"更新即跟随、跟随即复用"），且不重连。
func TestSessionG3aReplayPolicyOn(t *testing.T) {
	up, u := newSessionWSUpstream(t, "T1",
		sessionWSStep{id: "a1", ts: "T2", usage: true},
		sessionWSStep{id: "a2", usage: true})
	p, _ := newSessionWSProxy(t, up.URL, 1)
	p.turnStateReplay = true // 内部构造函数注入策略（§5.1）
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)

	reqs := u.requests()
	require.Len(t, reqs, 2)
	require.Equal(t, "", reqs[0].ts, "轮首不带 ts")
	require.Equal(t, "T2", reqs[1].ts, "带事件签发值 T2（不是握手快照 T1）")
	require.Equal(t, 1, u.upgradeCount(), "ts 更新不换连接")
	require.Equal(t, reqs[0].thread, reqs[1].thread)
}

// TestSessionG3bReplayPolicyOff G3b：策略 false（生产默认）⇒ 上游握手/事件都签发，
// 网关任何请求都不带（既不发送也不捕获）；连接复用照常。
func TestSessionG3bReplayPolicyOff(t *testing.T) {
	up, u := newSessionWSUpstream(t, "T1",
		sessionWSStep{id: "b1", ts: "T2", usage: true},
		sessionWSStep{id: "b2", usage: true})
	p, _ := newSessionWSProxy(t, up.URL, 1)
	require.False(t, p.turnStateReplay, "生产默认必须 false（P1 门禁前不重放）")
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)

	reqs := u.requests()
	require.Len(t, reqs, 2)
	for i, r := range reqs {
		require.Equal(t, "", r.ts, "第 %d 个请求不得带 ts", i)
	}
	require.Equal(t, 1, u.upgradeCount(), "策略 false 不影响连接复用")
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

// TestSessionG6LeaseExclusiveAndCancelDoesNotAdvance G6：租借独占——占满 K 个租借
// （body 不读）时第 K+1 个请求不得进入该账号的 claim（按现有门禁失败，不排队）；
// 提前取消 ⇒ 计数器不推进。
func TestSessionG6LeaseExclusiveAndCancelDoesNotAdvance(t *testing.T) {
	gate := make(chan struct{})
	up, u := newSessionWSUpstream(t, "", sessionWSStep{id: "d1", usage: true})
	u.gate = gate
	p, _ := newSessionWSProxy(t, up.URL, 1)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	// 1) 占住唯一租借：流式请求，上游被 gate 卡住（不回帧），客户端不读 body。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"input":"hi"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer ck-1")
	req.Header.Set("Content-Type", "application/json")
	type firstResult struct {
		resp *http.Response
		err  error
	}
	firstDone := make(chan firstResult, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		firstDone <- firstResult{resp: resp, err: err}
	}()
	// 租借已被占住的确定性同步点：上游收到请求帧（通道 + deadline）。
	select {
	case <-u.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("上游未在 deadline 内收到首个请求帧")
	}

	// 2) 第二个请求：门禁满 ⇒ 不得进入 claim（失败即可，不是排队等待）。
	second := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	require.NotEqual(t, http.StatusOK, second.StatusCode,
		"占满后第 K+1 个请求必须失败（不得排队等待）：%s", string(secondBody))
	require.Equal(t, 1, u.upgradeCount(), "失败请求不得新开会话连接")

	_, windowN, turns, ok := p.sched.CodexSessionStateForTest(10)
	require.True(t, ok)
	require.Zero(t, windowN)
	require.Zero(t, turns, "在飞/失败请求不推进计数器")

	// 3) 提前取消（未读到终态）⇒ 不推进、不退休。首个请求返回 = 网关侧 handler
	// 已收尾（通道 + deadline，不用 sleep）。
	cancel()
	select {
	case fr := <-firstDone:
		if fr.resp != nil {
			fr.resp.Body.Close()
		}
	case <-time.After(15 * time.Second):
		t.Fatal("取消后首个请求未返回")
	}
	_, _, turns, _ = p.sched.CodexSessionStateForTest(10)
	require.Zero(t, turns, "取消的请求不推进计数器")

	// 4) 放行上游并让下一次请求成功：若取消被计入，turns 会是 2（不是 1）。
	close(gate)
	postResponsesOK(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	thread, _, turns, ok := p.sched.CodexSessionStateForTest(10)
	require.True(t, ok)
	require.Equal(t, uint64(1), turns, "只有成功取到 usage 的那一次推进（取消不计数）")
	require.NotEmpty(t, thread)
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
