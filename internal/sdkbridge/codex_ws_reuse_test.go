// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// S3 门禁（spec §4 A–F + G6）：WS 连接跨轮复用 + 会话级 turn-state。
//
// 与 codex_transport_test.go 的假上游**刻意不同**：那里收到一帧就回 completed
// 并关连接，于是"每轮一条新连接"与"复用一条连接"在它上面读数相同——复用类用例
// 恒红/恒绿，没有判别力。这里的上游在同一连接上**循环读帧、逐帧回 completed、
// 不主动关闭**，`upgrades`（升级次数）才成为"是否复用"的唯一判据。
//
// **主判据**落在假上游侧（升级次数 / 请求帧里的 client_metadata / HTTP 请求头 /
// TCP dial 次数）——私有字段是"网关以为发了什么"，上游才是"实际到了什么"。
//
// 例外（有意为之，评审已确认）：C 与 D 各有一条补充断言直接读网关私有槽
// （`testSessionTurnState`），因为"槽里此刻是什么"没有对外观测面——上游只能证明
// "下一次请求带了什么"。私有读只作补充、不取代上游侧判据：同一用例里"实际发出去
// 的 ts"仍由帧序列 `wsRequestTS` 单独断言。

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// ---------------------------------------------------------------------------
// 假上游（可复用连接）
// ---------------------------------------------------------------------------

// wsReuseStep 上游对**一次**请求的响应脚本。
//
//	created=true  → 先发 response.created（事件 ts 用例的帧序前提：SDK 的
//	                turnStateTap 只在收到 `response.created` 之后才认 metadata 的
//	                归属，fail-safe 会丢弃先到的 metadata）
//	ts 非空       → 在 created 之后发 response.metadata 携带该 ts（事件级签发面）
//	末尾恒发 response.completed（SDK 以它判定本次取数结束并把连接交回缓存）
type wsReuseStep struct {
	created bool
	id      string
	ts      string
	// lateTS 在**终态帧之后**再投一封同 response_id 的 metadata：G9 用它证明
	// "关闭之后到达的 metadata 不会回到已关闭 exchange 的 Sink"（只写不发 ts 的
	// 断言在别的会话上跑，恒真，没有判别力）。
	lateTS string
}

// frames 组装本步要写的帧（顺序写死：created → metadata → completed → late metadata）。
func (s wsReuseStep) frames() []string {
	var out []string
	if s.created {
		out = append(out, `{"type":"response.created","response":{"id":"`+s.id+`","object":"response","status":"in_progress"}}`)
	}
	if s.ts != "" {
		// 形状对齐 SDK metadataTurnState：顶层 response_id + headers 对象。
		out = append(out, `{"type":"response.metadata","response_id":"`+s.id+`","headers":{"x-codex-turn-state":"`+s.ts+`"}}`)
	}
	out = append(out, `{"type":"response.completed","response":{"id":"`+s.id+`","object":"response","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)
	if s.lateTS != "" {
		out = append(out, `{"type":"response.metadata","response_id":"`+s.id+`","headers":{"x-codex-turn-state":"`+s.lateTS+`"}}`)
	}
	return out
}

// wsReuseUpstream 支持连接复用的假上游：WS 腿同连接循环服务，HTTP 腿回最小
// completed 体。TLS（wss/https 都要能握手）——测试 transport 用 InsecureSkipVerify。
type wsReuseUpstream struct {
	srv *httptest.Server

	handshakeTS string // WS 握手响应头签发值（空 = 不签发）

	mu       sync.Mutex
	steps    []wsReuseStep
	last     wsReuseStep
	upgrades int
	posts    int
	wsTS     []string // 每次 WS 请求帧的 client_metadata.x-codex-turn-state（按到达序）
	httpTS   []string // 每次 HTTP 请求的 x-codex-turn-state 头（按到达序）
	closed   int      // 上游观察到的 WS 连接关闭次数
	opened   int      // 接受过的 WS 连接总数（open = opened - closed）
	conns    []*websocket.Conn
	// closedEvents 每观察到一次对端关闭就投递一次（缓冲足够，永不阻塞）：
	// G7 的"被淘汰的连接确实被关掉"用通道 + deadline 等，不用 sleep。
	closedEvents chan struct{}

	// wsClosed 在**首次**观察到对端关闭时关闭（E 的确定性等待点：不用 sleep）。
	wsClosed chan struct{}
	closedON sync.Once
}

func newWSReuseUpstream(t *testing.T, handshakeTS string, steps ...wsReuseStep) *wsReuseUpstream {
	t.Helper()
	u := &wsReuseUpstream{handshakeTS: handshakeTS, wsClosed: make(chan struct{}, 1), closedEvents: make(chan struct{}, 256)}
	if len(steps) > 0 {
		u.steps = steps
		u.last = steps[len(steps)-1]
	}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(u.handle))
	u.srv.StartTLS()
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
	return u
}

func (u *wsReuseUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if isWebsocketUpgrade(r) {
		u.serveWS(w, r)
		return
	}
	u.serveHTTP(w, r)
}

// serveWS 一条连接服务**多个**请求：读一帧 → 按步回帧 → 继续读（不关）。
func (u *wsReuseUpstream) serveWS(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.upgrades++
	u.opened++
	hs := u.handshakeTS
	u.mu.Unlock()
	if hs != "" {
		// Accept 会把当前 w.Header() 一并写进 101 响应（coder/websocket
		// accept.go: w.Header().Set(...) + WriteHeader(101)），所以这就是
		// "握手响应头签发 turn-state" 的注入点。
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
			select {
			case u.closedEvents <- struct{}{}:
			default:
			}
			u.closedON.Do(func() { close(u.wsClosed) })
			return
		}
		u.mu.Lock()
		u.wsTS = append(u.wsTS, gjson.GetBytes(data, "client_metadata."+codexsdk.HeaderTurnState).String())
		step := u.nextStepLocked()
		u.mu.Unlock()
		for _, frame := range step.frames() {
			if werr := c.Write(r.Context(), websocket.MessageText, []byte(frame)); werr != nil {
				return
			}
		}
	}
}

// serveHTTP HTTP 腿：记录请求头 → 签发响应头（step.ts 非空时）→ 回最小 completed
// SSE 体（读尽即完整，aggregator/relay 都不会判截断）。
func (u *wsReuseUpstream) serveHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.posts++
	u.httpTS = append(u.httpTS, r.Header.Get(codexsdk.HeaderTurnState))
	step := u.nextStepLocked()
	u.mu.Unlock()
	if step.ts != "" {
		w.Header().Set(codexsdk.HeaderTurnState, step.ts)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	_, _ = io.WriteString(w, "data: "+wsReuseStep{id: step.id}.frames()[0]+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	f.Flush()
}

// nextStepLocked 弹出下一步（耗尽重复最后一步）；调用方持 mu。
func (u *wsReuseUpstream) nextStepLocked() wsReuseStep {
	if len(u.steps) == 0 {
		return u.last
	}
	s := u.steps[0]
	u.steps = u.steps[1:]
	u.last = s
	return s
}

func (u *wsReuseUpstream) upgradeCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrades
}

func (u *wsReuseUpstream) postCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.posts
}

func (u *wsReuseUpstream) closedCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.closed
}

// openCount 上游此刻仍打开的 WS 连接数（G7 的"空闲数 ≤ 构造期上限"判据面）。
func (u *wsReuseUpstream) openCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.opened - u.closed
}

// waitClosed 等上游观察到 n 次连接关闭（通道 + deadline，禁 sleep）。
func (u *wsReuseUpstream) waitClosed(t *testing.T, n int, what string) {
	t.Helper()
	for range n {
		select {
		case <-u.closedEvents:
		case <-time.After(15 * time.Second):
			t.Fatalf("%s：上游未在 deadline 内观察到第 %d 次连接关闭", what, n)
		}
	}
}

// wsRequestTS 每次 WS 请求帧里带的 turn-state（判据面：网关**实际发出**了什么）。
func (u *wsReuseUpstream) wsRequestTS() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.wsTS...)
}

// httpRequestTS 每次 HTTP 请求带的 turn-state 头。
func (u *wsReuseUpstream) httpRequestTS() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.httpTS...)
}

// addr 上游监听地址（自建 transport 的 DialContext 目标）。
func (u *wsReuseUpstream) addr() string { return u.srv.Listener.Addr().String() }

// ---------------------------------------------------------------------------
// 装配件
// ---------------------------------------------------------------------------

// wsReuseCodex 装配真 Codex：出口池的 transport 用**真 `*http.Transport`**（不是
// 包装器）——WS 拨号与 HTTP POST 都从这里出网，E 的 dial 计数才有意义；URL 重写
// 靠 DialContext 直接改指测试地址（官方 URL 的 host 被忽略）。
func wsReuseCodex(t *testing.T, up *wsReuseUpstream) (*Codex, *atomic.Int64) {
	t.Helper()
	var dials atomic.Int64
	tr := wsReuseTransport(t, up)
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, network, addr)
	}
	return NewCodex(nil, testEgressPool(tr), nil, RotationDeps{}), &dials
}

// wsReuseTransport 真 `*http.Transport`，DialContext 直指假上游（TLS 自签）。
func wsReuseTransport(t *testing.T, up *wsReuseUpstream) *http.Transport {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, up.addr())
		},
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     time.Minute,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func wsReuseCred() *domain.AccountCredential {
	return &domain.AccountCredential{AccountID: 9, PATKey: "pat-reuse"}
}

// wsReuseSess 测试侧的会话，与 scheduler.codexSession **同形**：ts 是一个普通
// 字段，Sink 就是它自己（一次赋值）。额外记一份签发序列供断言——生产会话没有
// 这一面（SDK 不再持有 ts，"槽里此刻是什么"在适配层已无观测面）。
type wsReuseSess struct {
	mu      sync.Mutex
	session codexsdk.Session
	meta    codexsdk.CodexMeta
	ts      string
	seen    []string
}

func newWSReuseSess(installationID string) *wsReuseSess {
	thread := codexsdk.NewUUIDv7()
	return &wsReuseSess{
		session: codexsdk.Session{SessionID: thread, ThreadID: thread, WindowID: thread + ":0"},
		meta: codexsdk.CodexMeta{
			InstallationID: installationID,
			SessionID:      thread,
			ThreadID:       thread,
			WindowID:       thread + ":0",
		},
	}
}

// SetTurnState SDK 的 TurnStateSink 落点：一次赋值（+ 记录序列）。
func (s *wsReuseSess) SetTurnState(ts string) {
	s.mu.Lock()
	s.ts = ts
	s.seen = append(s.seen, ts)
	s.mu.Unlock()
}

func (s *wsReuseSess) turnState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ts
}

func (s *wsReuseSess) issued() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *wsReuseSess) threadKey() string {
	return s.meta.InstallationID + "|" + s.meta.ThreadID + "|" + s.meta.ThreadID
}

// identity 本次请求的身份：ts = 会话当前值，Sink = 会话（与网关同构）。
func (s *wsReuseSess) identity() codexsdk.Identity {
	return codexsdk.Identity{Session: s.session, Meta: s.meta, TurnState: s.turnState(), Sink: s}
}

// wsReuseStream 发一次 ws 臂 StreamBody 并读尽关闭（与 proxy 侧消费语义同形：
// 读完终态帧 → Close → 连接**归还缓存**，这正是复用的唯一出口）。
func wsReuseStream(t *testing.T, a *Codex, cred *domain.AccountCredential, id codexsdk.Identity, transport domain.CodexResponsesTransport) *http.Response {
	t.Helper()
	// ctx 必须活到 body 读完（exchange 的取帧走 ctx）：用 t.Context()，不要在这里
	// defer cancel——那会把刚返回的 body 直接读成 context canceled。
	resp, err := a.StreamBody(t.Context(), cred, a.DirectEgress(), []byte(`{"model":"gpt-5.6","input":[]}`), id, transport)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp
}

// ---------------------------------------------------------------------------
// A 跨轮复用
// ---------------------------------------------------------------------------

// TestCodexWSReuseAcrossTurns A：同会话连续两轮，两轮 ts 不同 ⇒ upgrades == 1。
//
// 第 1 轮带 T1（会话当前值），握手签发 T1，随后 `response.metadata` 发 T2 ⇒ Sink 把
// 会话更新成 T2；第 2 轮带 T2。两轮 ts 不同却仍是同一条连接 —— 这正是 S1（连接
// 身份不含 ts）+ G1/G2（ts 不再是构造期/缓存键面）+ G6（复用真的开着）三件事合起
// 来的唯一可见结果。
func TestCodexWSReuseAcrossTurns(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "r1", ts: "T2"},
		wsReuseStep{created: true, id: "r2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	sess := newWSReuseSess("inst")
	sess.SetTurnState("T1")

	readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)
	readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)

	require.Equal(t, []string{"T1", "T2"}, up.wsRequestTS(), "两轮各带自己的 ts（事件签发值下一轮生效）")
	require.Equal(t, "T2", sess.turnState(), "会话最终值是事件签发值（握手 T1 被 T2 覆盖）")
	issued := sess.issued()
	require.NotEmpty(t, issued)
	require.Equal(t, "T2", issued[len(issued)-1], "签名值只经 Sink 交付，最后落的是事件值")
	require.Equal(t, 1, up.upgradeCount(), "两轮 ts 不同仍复用同一条连接（upgrades == 1）")
}

// TestCodexAggregateTurnStateKeepsEventIssuedValue 聚合臂（非流式 `Responses`）在
// WS 新握手时先收到握手签发值（T1），随后 `response.metadata` 事件签发新值（T2）。
// 适配层**不得**在请求返回后用响应头快照回写会话：那会把 T2 倒退成 T1。
//
// 判据两条：① Sink 序列以 T2 收尾（事件值最后落）；② 下一次请求帧里带 T2。
func TestCodexAggregateTurnStateKeepsEventIssuedValue(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "agg1", ts: "T2"},
		wsReuseStep{created: true, id: "agg2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	sess := newWSReuseSess("inst")

	_, err := a.Responses(t.Context(), cred, a.DirectEgress(),
		[]byte(`{"model":"gpt-5.6","input":[]}`), sess.identity(), domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	require.Equal(t, []string{"T1", "T2"}, sess.issued(), "握手 T1 → 事件 T2（返回阶段不回写旧快照）")
	require.Equal(t, "T2", sess.turnState(), "会话保留事件签发值")

	// 黑盒面：下一次同会话请求必须带 T2（不是 T1）。
	_, err = a.Responses(t.Context(), cred, a.DirectEgress(),
		[]byte(`{"model":"gpt-5.6","input":[]}`), sess.identity(), domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	require.Equal(t, []string{"", "T2"}, up.wsRequestTS(),
		"轮首不带；续传必须带**事件**签发值 T2（不是握手快照 T1）")
}

// ---------------------------------------------------------------------------
// B 轮内重放
// ---------------------------------------------------------------------------

// TestCodexWSTurnReplayWithinTurn B：同一轮内重复请求携带**同一个** ts（第一次
// 由握手签发、后两次重放它），全程 upgrades == 1。
//
// 轮首不带 ts——这正是 M2（把 ts 搬回连接身份/构造期）现形的地方：构造期快照停在
// ""，第二次请求要带 T1 就只能重建 client ⇒ upgrades 变 2。
func TestCodexWSTurnReplayWithinTurn(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "b1"},
		wsReuseStep{created: true, id: "b2"},
		wsReuseStep{created: true, id: "b3"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	sess := newWSReuseSess("inst")

	for range 3 {
		readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)
	}

	require.Equal(t, []string{"", "T1", "T1"}, up.wsRequestTS(),
		"轮首不带 → 握手签发 T1 → 轮内两次重放同一个 ts")
	require.Equal(t, 1, up.upgradeCount(), "轮内重放不换连接")
}

// ---------------------------------------------------------------------------
// C 会话侧 ts 不跨会话串台
// ---------------------------------------------------------------------------

// TestCodexWSNoTurnStateAcrossSessions C：两条会话各带自己的 ts，交错请求 ⇒
// 连接按 thread 分开（upgrades == 2）、帧内 ts 各归各（不串台）。
//
// 这条钉住"存储属会话"：把 ts 放回账号级/客户端级单值（旧机制）就会让 B 会话
// 带上 A 的 ts（G4 的同族）。
func TestCodexWSNoTurnStateAcrossSessions(t *testing.T) {
	up := newWSReuseUpstream(t, "",
		wsReuseStep{created: true, id: "a1"},
		wsReuseStep{created: true, id: "b1"},
		wsReuseStep{created: true, id: "a2"},
		wsReuseStep{created: true, id: "b2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	aSess, bSess := newWSReuseSess("inst"), newWSReuseSess("inst")
	aSess.SetTurnState("TA")
	bSess.SetTurnState("TB")

	for range 2 {
		readAndCloseBody(t, wsReuseStream(t, a, cred, aSess.identity(), domain.CodexResponsesTransportWS).Body)
		readAndCloseBody(t, wsReuseStream(t, a, cred, bSess.identity(), domain.CodexResponsesTransportWS).Body)
	}

	require.Equal(t, []string{"TA", "TB", "TA", "TB"}, up.wsRequestTS(), "两会话 ts 各自正确（不串台）")
	require.Equal(t, 2, up.upgradeCount(), "两会话 = 两条 thread 连接（G4）")
}

// ---------------------------------------------------------------------------
// D 事件级捕获
// ---------------------------------------------------------------------------

// TestCodexWSTurnStateFromMetadataEvent D：上游只在 `response.metadata` 事件里给
// ts ⇒ Sink 写进会话 ⇒ 网关下一次请求带它，且 upgrades 不增。
//
// 帧序是前提（created → metadata）：SDK 的 tap 用 `response.created` 记本轮
// response_id 做归属判定，先到的 metadata 会被 fail-safe 丢弃（按设计）。
func TestCodexWSTurnStateFromMetadataEvent(t *testing.T) {
	up := newWSReuseUpstream(t, "",
		wsReuseStep{created: true, id: "d1", ts: "T3"},
		wsReuseStep{created: true, id: "d2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	sess := newWSReuseSess("inst")

	readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, []string{"T3"}, sess.issued(), "事件 ts 经 Sink 写入会话")

	readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)

	require.Equal(t, []string{"", "T3"}, up.wsRequestTS(), "下一次请求带上事件捕获的 ts")
	require.Equal(t, 1, up.upgradeCount(), "事件捕获不触发重连")
}

// ---------------------------------------------------------------------------
// F HTTP 腿
// ---------------------------------------------------------------------------

// TestCodexHTTPLegCarriesTurnStatePerRequest F：`""`/`"http"` 两个传输开关下，
// 每次 HTTP 请求头都带**当前**会话 ts，且全程零升级（不走 ws 臂）。
func TestCodexHTTPLegCarriesTurnStatePerRequest(t *testing.T) {
	for _, transport := range []domain.CodexResponsesTransport{domain.CodexResponsesTransportDefault, domain.CodexResponsesTransportHTTP} {
		t.Run(string(transport), func(t *testing.T) {
			up := newWSReuseUpstream(t, "",
				wsReuseStep{created: true, id: "f1", ts: "T2"},
				wsReuseStep{created: true, id: "f2"},
			)
			a, _ := wsReuseCodex(t, up)
			cred := wsReuseCred()
			sess := newWSReuseSess("inst")
			sess.SetTurnState("T1")

			readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), transport).Body)
			// 上游签发新值（HTTP 响应头）→ Sink → 会话当前值更新
			readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), transport).Body)

			require.Equal(t, []string{"T1", "T2"}, up.httpRequestTS(), "每次请求带当前 ts（逐请求求值）")
			require.Equal(t, 0, up.upgradeCount(), "HTTP 腿不升级")
			require.Equal(t, 2, up.postCount())
		})
	}
}

// ---------------------------------------------------------------------------
// E 关池 / 退休不动 h2/h1
// ---------------------------------------------------------------------------

// TestCodexDropPathsCloseWSOnly E：三条 drop 路径（entry 覆盖 / 线程退休 /
// evict）各触发一次 ⇒ 假上游观察到 WS 关闭；**紧接**一次 HTTP 腿请求，
// DialContext 计数不增（底层连接池没被打掉）。
//
// transport 必须是**真 `*http.Transport`**：SDK 的 CloseIdleConnections 只对
// `client.Transport.(*http.Transport)` 断言——包装器下那条分支永不执行，正确实现
// 与 M3 变异都会是"计数不增"，用例就没有判别力了。
func TestCodexDropPathsCloseWSOnly(t *testing.T) {
	cases := []struct {
		name string
		drop func(t *testing.T, a *Codex, cred *domain.AccountCredential, sess *wsReuseSess)
	}{
		{
			// entryFor 覆盖旧 entry：外部凭据变更（PAT 变化）⇒ sig 不同 ⇒ 旧条目弃用
			name: "entry-overwrite",
			drop: func(t *testing.T, a *Codex, cred *domain.AccountCredential, _ *wsReuseSess) {
				t.Helper()
				next := *cred
				next.PATKey = "pat-reuse-rotated"
				resp, err := a.StreamBody(t.Context(), &next, a.DirectEgress(),
					[]byte(`{"model":"m"}`), codexsdk.Identity{}, domain.CodexResponsesTransportHTTP)
				require.NoError(t, err)
				// 必须读尽关掉：否则这条底层连接被占用，第 4 步的"不新增 dial"
				// 断言会为**测试自身的泄漏**变红（与被测行为无关）。
				readAndCloseBody(t, resp.Body)
			},
		},
		{
			// 线程退休（§3.2 ④）：只摘该 thread 的空闲连接
			name: "retire-thread",
			drop: func(t *testing.T, a *Codex, cred *domain.AccountCredential, sess *wsReuseSess) {
				t.Helper()
				a.RetireThread(cred.AccountID, sess.threadKey())
			},
		},
		{
			// 来源版本失效：fatal 上报对该来源条目走 evictIfCurrent（仍当前 ⇒ 摘除整条目）
			name: "evict-current",
			drop: func(t *testing.T, a *Codex, cred *domain.AccountCredential, _ *wsReuseSess) {
				t.Helper()
				require.True(t, a.clients.evictIfCurrent(a.clients.Get(cred.AccountID)),
					"fatal 的来源条目仍当前 ⇒ 摘除")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newWSReuseUpstream(t, "")
			a, dials := wsReuseCodex(t, up)
			cred := wsReuseCred()
			sess := newWSReuseSess("inst")

			// 1) 预热：HTTP 腿一次请求（占一条底层空闲连接）
			resp, err := a.StreamBody(t.Context(), cred, a.DirectEgress(),
				[]byte(`{"model":"m"}`), codexsdk.Identity{}, domain.CodexResponsesTransportHTTP)
			require.NoError(t, err)
			readAndCloseBody(t, resp.Body)
			require.Equal(t, int64(1), dials.Load(), "预热只拨一次")
			require.Equal(t, 0, up.upgradeCount())

			// 2) 一条**空闲**（已归还缓存）的 WS 连接
			readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)
			require.Equal(t, 1, up.upgradeCount())
			require.Equal(t, int64(2), dials.Load(), "WS 拨号另计一次")

			// 3) 触发 drop 路径 ⇒ 上游必须看到 WS 关闭
			tc.drop(t, a, cred, sess)
			select {
			case <-up.wsClosed:
			case <-time.After(15 * time.Second):
				t.Fatalf("上游未观察到 WS 连接关闭（drop 路径没退役 idle）")
			}
			require.Equal(t, 1, up.closedCount())

			// 4) 紧接一次 HTTP 腿请求：底层连接池不受影响 ⇒ 不新增 dial
			before := dials.Load()
			resp, err = a.StreamBody(t.Context(), cred, a.DirectEgress(),
				[]byte(`{"model":"m"}`), codexsdk.Identity{}, domain.CodexResponsesTransportHTTP)
			require.NoError(t, err)
			readAndCloseBody(t, resp.Body)
			require.Equal(t, before, dials.Load(),
				"关 WS 空闲连接不得打掉底层连接池（CloseWSIdleConnections，不是 CloseIdleConnections）")
		})
	}
}

// TestCodexRetireThreadOnlyThatThread 退休只摘该 threadKey 的空闲连接（G7 的
// 会话面）：另一条 thread 的连接仍在 idle 表里 ⇒ 它的下一次请求不重连；被退休的
// thread 下一次请求必须重拨（不残留、不复活）。
func TestCodexRetireThreadOnlyThatThread(t *testing.T) {
	up := newWSReuseUpstream(t, "",
		wsReuseStep{created: true, id: "k1"},
		wsReuseStep{created: true, id: "d1"},
		wsReuseStep{created: true, id: "d2"},
		wsReuseStep{created: true, id: "k2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	keep, drop := newWSReuseSess("inst"), newWSReuseSess("inst")

	readAndCloseBody(t, wsReuseStream(t, a, cred, keep.identity(), domain.CodexResponsesTransportWS).Body)
	readAndCloseBody(t, wsReuseStream(t, a, cred, drop.identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 2, up.upgradeCount(), "两条 thread 各一条连接")

	a.RetireThread(cred.AccountID, drop.threadKey())
	select {
	case <-up.wsClosed:
	case <-time.After(15 * time.Second):
		t.Fatal("退休未关闭该 thread 的空闲连接")
	}

	// 被退休的 thread：下一条连接必须重拨（旧连接已摘除、不复活）。
	readAndCloseBody(t, wsReuseStream(t, a, cred, drop.identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 3, up.upgradeCount(), "退休后该 thread 重拨")

	// 未受影响的 thread：命中它自己的空闲连接（不重拨）。
	readAndCloseBody(t, wsReuseStream(t, a, cred, keep.identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 3, up.upgradeCount(), "另一条 thread 的连接不受影响")
}

// ---------------------------------------------------------------------------
// S4 验收（G2/G7/G8/G9）：退休只摘 idle、关闭不复活、回调边界
// ---------------------------------------------------------------------------

// TestCodexRetireThreadG2NewThreadStartsWithNoTurnState G2（退休）：旧线程的连接
// 被关、新线程首请求不带 ts（会话侧在退休时已清空 ts）。
func TestCodexRetireThreadG2NewThreadStartsWithNoTurnState(t *testing.T) {
	up := newWSReuseUpstream(t, "T-old",
		wsReuseStep{created: true, id: "old1"},
		wsReuseStep{created: true, id: "new1"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	oldSess := newWSReuseSess("inst")
	oldSess.SetTurnState("T-old")
	readAndCloseBody(t, wsReuseStream(t, a, cred, oldSess.identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 1, up.upgradeCount())

	// 退休：① 记旧 threadKey ② 清 ts（会话侧）③ 新 thread ④ 退役旧线程空闲连接。
	newSess := newWSReuseSess("inst")
	a.RetireThread(cred.AccountID, oldSess.threadKey())
	select {
	case <-up.wsClosed:
	case <-time.After(15 * time.Second):
		t.Fatal("退休未摘掉旧线程的空闲连接")
	}

	readAndCloseBody(t, wsReuseStream(t, a, cred, newSess.identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 2, up.upgradeCount(), "新线程新连接")
	require.Equal(t, []string{"T-old", ""}, up.wsRequestTS(), "新线程首请求不带旧 ts")
}

// TestCodexRetireThreadG7ClosedClientDoesNotReviveIdle G7：退休/关闭后不残留；旧
// client 的在飞连接归还后**不复活**（Close 之后 put 拒收，由 Release 终止）。
func TestCodexRetireThreadG7ClosedClientDoesNotReviveIdle(t *testing.T) {
	up := newWSReuseUpstream(t, "",
		wsReuseStep{created: true, id: "inflight"},
		wsReuseStep{created: true, id: "after"},
	)
	a, _ := wsReuseCodex(t, up)
	oldCred := wsReuseCred()
	sess := newWSReuseSess("inst")

	// 在飞：拿到 body 但先不读（连接被 exchange 独占，不在 idle 表里）。
	inFlight := wsReuseStream(t, a, oldCred, sess.identity(), domain.CodexResponsesTransportWS)
	require.Equal(t, 1, up.upgradeCount())

	// 凭据替换 ⇒ 旧条目弃用 + CloseWSIdleConnections（在飞连接不受影响）。
	next := *oldCred
	next.PATKey = "pat-reuse-rotated"
	resp, err := a.StreamBody(t.Context(), &next, a.DirectEgress(),
		[]byte(`{"model":"m"}`), codexsdk.Identity{}, domain.CodexResponsesTransportHTTP)
	require.NoError(t, err)
	readAndCloseBody(t, resp.Body)

	// 在飞连接读完归还：client 已 closed ⇒ put 拒收 ⇒ 终止（不复活成 idle）。
	readAndCloseBody(t, inFlight.Body)
	select {
	case <-up.wsClosed:
	case <-time.After(15 * time.Second):
		t.Fatal("已关闭 client 的在飞连接归还后未被终止（复活成 idle）")
	}

	// 下一次请求必须重拨（旧 idle 不存在了）。
	readAndCloseBody(t, wsReuseStream(t, a, &next, sess.identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 2, up.upgradeCount(), "旧 client 的 idle 不残留：新凭据重拨")
}

// TestCodexRetireThreadG8OnlyAfterBodyClose G8（退休时序）：body Close **之前**
// 上游观察不到连接关闭（退休只摘 idle，在飞连接不受影响）；Close 之后才观察到。
func TestCodexRetireThreadG8OnlyAfterBodyClose(t *testing.T) {
	up := newWSReuseUpstream(t, "",
		wsReuseStep{created: true, id: "seq1"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	sess := newWSReuseSess("inst")

	inflight := wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS)
	// 在飞期间退休：连接不在 idle 表里 ⇒ 不得被关闭。
	a.RetireThread(cred.AccountID, sess.threadKey())
	// 正向判据（不用等待/负向窗口）：在飞 body 仍能读到终态帧——连接没被杀。
	got := readAndCloseBody(t, inflight.Body)
	require.Contains(t, string(got), "response.completed", "在飞连接不因退休被关闭")

	// Close 之后（连接已归还 idle）：再退休 ⇒ 上游观察到关闭。
	a.RetireThread(cred.AccountID, sess.threadKey())
	select {
	case <-up.wsClosed:
	case <-time.After(15 * time.Second):
		t.Fatal("body Close 之后退休未关闭 idle 连接")
	}
	require.Equal(t, 1, up.closedCount())
}

// TestCodexTurnStateSinkG9NoCallbackAfterClose G9（回调边界）：body Close 之后
// Sink 不再被调用（计数不增）；聚合臂同理。
//
// 判别力来自**同一连接上迟到的 metadata**：上游在终态帧之后仍发一封同 response_id
// 的 metadata（lateTS）。若实现把"已关闭 exchange 的 Sink"留着（或把它挂在会话上
// 继续写），下一次请求读到这封迟到帧就会把 T-late 写进会话——本用例断言它不会：
// 会话终值恒为关闭前拿到的 T2，计数也只在第二个交换自己的帧上增长。
func TestCodexTurnStateSinkG9NoCallbackAfterClose(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "s1", ts: "T2", lateTS: "T-late"},
		wsReuseStep{created: true, id: "s2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	sess := newWSReuseSess("inst")

	readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)
	afterClose := len(sess.issued())
	require.NotZero(t, afterClose)
	require.Equal(t, "T2", sess.turnState(), "关闭时值是关闭前签发的 T2")

	// 同一连接复用给第二条请求：它可能先读到那封**迟到** metadata（response_id 属于
	// 上一轮）。迟到帧不得回到已关闭 exchange 的 Sink ⇒ 会话仍只能是本轮的签发值。
	readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)
	require.NotEqual(t, "T-late", sess.turnState(), "关闭之后到达的 metadata 不得写入已关闭会话")
	require.GreaterOrEqual(t, len(sess.issued()), afterClose)
	for _, ts := range sess.issued()[afterClose:] {
		require.NotEqual(t, "T-late", ts, "已关闭 exchange 的迟到帧不得回调 Sink")
	}

	// 聚合臂同理：调用返回（= 内部 Close 完成）后计数冻结。
	aggSess := newWSReuseSess("inst")
	_, err := a.Responses(t.Context(), cred, a.DirectEgress(),
		[]byte(`{"model":"gpt-5.6","input":[]}`), aggSess.identity(), domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	aggCount := len(aggSess.issued())
	require.NotZero(t, aggCount)
	require.Equal(t, aggSess.issued()[aggCount-1], aggSess.turnState(),
		"聚合臂以**最后一次**签发值收尾（握手快照不回写）")
	readAndCloseBody(t, wsReuseStream(t, a, cred, sess.identity(), domain.CodexResponsesTransportWS).Body)
	require.Len(t, aggSess.issued(), aggCount, "聚合返回后 Sink 计数不增")
}

// TestCodexWSConnCacheBoundedByConstructionTimeLimit G7（连接池上限）：空闲连接数
// 不超过**构造期**上限（`WithWSConnCache(n)` 的 n），被淘汰的连接在上游侧真的关闭。
//
// 判据全在上游：opened/closed 是"实际建立/关闭了几条 TCP"。容量 2 + 4 条会话各一轮
// ⇒ 上游最多同时挂着 2 条；第 3/4 轮把最早的挤掉（put 的满则驱逐最旧）。再验证
// ①最近的两条仍复用（不重拨）②被淘汰的那条必须重拨——否则"上限"只是没生效。
func TestCodexWSConnCacheBoundedByConstructionTimeLimit(t *testing.T) {
	const capacity = 2
	up := newWSReuseUpstream(t, "", wsReuseStep{created: true, id: "cap"})
	a := NewCodex(nil, testEgressPool(wsReuseTransport(t, up)), NewClientTable(), RotationDeps{},
		Options{WSConnCache: capacity})
	cred := wsReuseCred()

	sessions := make([]*wsReuseSess, 4)
	for i := range sessions {
		sessions[i] = newWSReuseSess("inst")
		readAndCloseBody(t, wsReuseStream(t, a, cred, sessions[i].identity(), domain.CodexResponsesTransportWS).Body)
	}
	require.Equal(t, 4, up.upgradeCount(), "四条会话各一条连接")
	up.waitClosed(t, 2, "容量驱逐")
	require.LessOrEqual(t, up.openCount(), capacity, "空闲连接数不得超过构造期上限")

	// ① 最近两条仍在 idle 表里 ⇒ 复用（不新增 upgrade）。
	readAndCloseBody(t, wsReuseStream(t, a, cred, sessions[3].identity(), domain.CodexResponsesTransportWS).Body)
	readAndCloseBody(t, wsReuseStream(t, a, cred, sessions[2].identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 4, up.upgradeCount(), "最近两条会话命中自己的 idle 连接")

	// ② 被淘汰的会话必须重拨（旧 idle 已被终止，不复活）。
	readAndCloseBody(t, wsReuseStream(t, a, cred, sessions[0].identity(), domain.CodexResponsesTransportWS).Body)
	require.Equal(t, 5, up.upgradeCount(), "被驱逐的会话重拨")
}
