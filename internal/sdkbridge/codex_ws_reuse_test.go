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
	"fmt"
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
}

// frames 组装本步要写的帧（顺序写死：created → metadata → completed）。
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
	conns    []*websocket.Conn

	// wsClosed 在**首次**观察到对端关闭时关闭（E 的确定性等待点：不用 sleep）。
	wsClosed chan struct{}
	closedON sync.Once
}

func newWSReuseUpstream(t *testing.T, handshakeTS string, steps ...wsReuseStep) *wsReuseUpstream {
	t.Helper()
	u := &wsReuseUpstream{handshakeTS: handshakeTS, wsClosed: make(chan struct{})}
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
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, up.addr())
		},
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     time.Minute,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return NewCodex(nil, testEgressPool(tr), RotationDeps{}), &dials
}

func wsReuseCred() *domain.AccountCredential {
	return &domain.AccountCredential{AccountID: 9, PATKey: "pat-reuse"}
}

// wsReuseStream 发一次 ws 臂 StreamBody 并读尽关闭（与 proxy 侧消费语义同形：
// 读完终态帧 → Close → 连接**归还缓存**，这正是复用的唯一出口）。
func wsReuseStream(t *testing.T, a *Codex, cred *domain.AccountCredential, sess *codexsdk.Session, meta *codexsdk.CodexMeta, clientTS string, transport domain.CodexResponsesTransport) *http.Response {
	t.Helper()
	// ctx 必须活到 body 读完（exchange 的取帧走 ctx）：用 t.Context()，不要在这里
	// defer cancel——那会把刚返回的 body 直接读成 context canceled。
	resp, err := a.StreamBody(t.Context(), cred, a.DirectEgress(), []byte(`{"model":"gpt-5.6","input":[]}`), sess, meta, clientTS, transport)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp
}

// seedTurnState 测试面：把会话槽预置为 ts（模拟"该会话此前已签发过"的起点——
// 唯一可信的写入口是 observer/capture，但用例需要可控的初始值）。
func seedTurnState(t *testing.T, a *Codex, cred *domain.AccountCredential, sess *codexsdk.Session, meta *codexsdk.CodexMeta, ts string) {
	t.Helper()
	e, err := a.entryFor(cred)
	require.NoError(t, err)
	e.turnStates.store(sessionKeyOf(sess, meta), ts)
}

// ---------------------------------------------------------------------------
// A 跨轮复用
// ---------------------------------------------------------------------------

// TestCodexWSReuseAcrossTurns A：同会话连续两轮，两轮 ts 不同 ⇒ upgrades == 1。
//
// 第 1 轮带 T1（预置），握手签发 T1，随后 `response.metadata` 发 T2 ⇒ observer 把
// 会话槽更新成 T2；第 2 轮 provider 读到 T2 并带上。两轮 ts 不同却仍是同一条
// 连接 —— 这正是 S1（连接身份不含 ts）+ G1/G2（ts 不再进构造期）+ G6（复用真的
// 开着）三件事合起来的唯一可见结果。
func TestCodexWSReuseAcrossTurns(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "r1", ts: "T2"},
		wsReuseStep{created: true, id: "r2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	seedTurnState(t, a, cred, nil, nil, "T1")

	resp1 := wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
	require.Equal(t, "T1", resp1.Header.Get(codexsdk.HeaderTurnState), "握手签发值回填合成响应头（S5）")
	readAndCloseBody(t, resp1.Body)

	resp2 := wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
	readAndCloseBody(t, resp2.Body)

	require.Equal(t, []string{"T1", "T2"}, up.wsRequestTS(), "两轮各带自己的 ts（第 2 轮的事件 ts 生效）")
	require.Equal(t, 1, up.upgradeCount(), "两轮 ts 不同仍复用同一条连接（upgrades == 1）")
}

// TestCodexAggregateTurnStateKeepsEventIssuedValue：聚合臂（非流式 `Responses`）
// 在 WS 新握手时，**合成响应头是握手快照**（T1）——它必须早于任何帧定死；而本轮
// `response.metadata` 事件已签发新值（T2）。网关**不得**在请求返回后用响应头快照
// 回写会话槽：那会把 T2 倒退成 T1，下一次同轮续传就重放旧值（oracle 评审 blocking-1）。
//
// 判据两条：① 槽里是最新签发值（T2）；② 下一次请求帧里带的是 T2（黑盒面）。
func TestCodexAggregateTurnStateKeepsEventIssuedValue(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "agg1", ts: "T2"},
		wsReuseStep{created: true, id: "agg2"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()

	resp, err := a.Responses(t.Context(), cred, a.DirectEgress(),
		[]byte(`{"model":"gpt-5.6","input":[]}`), nil, nil, "", domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	require.Equal(t, "T1", resp.TurnState, "合成响应头是握手快照（早于任何帧定死）")
	require.Equal(t, "T2", testSessionTurnState(a, cred.AccountID, nil, nil),
		"事件签发的新值必须留在槽里：返回阶段不得用握手快照回写")

	// 黑盒面：下一次同会话请求必须带 T2（不是 T1）。
	if _, err := a.Responses(t.Context(), cred, a.DirectEgress(),
		[]byte(`{"model":"gpt-5.6","input":[]}`), nil, nil, "", domain.CodexResponsesTransportWS); err != nil {
		t.Fatalf("第二次 Responses: %v", err)
	}
	require.Equal(t, []string{"", "T2"}, up.wsRequestTS(),
		"轮首不带；续传必须带**事件**签发值 T2（不是握手快照 T1）")
}

// ---------------------------------------------------------------------------
// B 轮内重放
// ---------------------------------------------------------------------------

// TestCodexWSTurnReplayWithinTurn B：同一轮内重复请求携带**同一个** ts（第一次
// 由握手签发、后两次重放它），全程 upgrades == 1。
//
// 轮首不带 ts（provider 读到空槽）——这正是 M2（把 provider 改回构造期 WithHeader）
// 现形的地方：构造期快照停在 ""，第二次请求要带 T1 就只能重建 client ⇒ upgrades
// 变 2。第 3 次与第 2 次同 ts（"轮内两次 metadata ts 相同"的字面读法）一并钉住。
func TestCodexWSTurnReplayWithinTurn(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "b1"},
		wsReuseStep{created: true, id: "b2"},
		wsReuseStep{created: true, id: "b3"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()

	for range 3 {
		resp := wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
		readAndCloseBody(t, resp.Body)
	}

	require.Equal(t, []string{"", "T1", "T1"}, up.wsRequestTS(),
		"轮首不带 → 握手签发 T1 → 轮内两次重放同一个 ts")
	require.Equal(t, 1, up.upgradeCount(), "轮内重放不换连接")
}

// ---------------------------------------------------------------------------
// C 不重放旧 ts（跨轮）
// ---------------------------------------------------------------------------

// TestCodexWSTurnStateClearedAcrossTurns C：第 1 轮两次（带 T1）→ 轮结束
// （ClearTurnState）→ 第 2 轮两次都**不带**上一轮的 ts，全程 upgrades == 1。
//
// 第 2 轮第二次是关键判据点（M5 的显形点）：若合成响应头按"恒回填 issued"实现，
// 第 2 轮第一次复用会拿到该连接握手时的 T1，网关把它存回会话槽，第二次就重放了
// 旧值——连接没换（upgrades 仍 1），但 ts 泄漏了。
func TestCodexWSTurnStateClearedAcrossTurns(t *testing.T) {
	up := newWSReuseUpstream(t, "T1",
		wsReuseStep{created: true, id: "c1"},
		wsReuseStep{created: true, id: "c2"},
		wsReuseStep{created: true, id: "c3"},
		wsReuseStep{created: true, id: "c4"},
	)
	a, _ := wsReuseCodex(t, up)
	cred := wsReuseCred()
	seedTurnState(t, a, cred, nil, nil, "T1")

	for range 2 { // 第 1 轮：两次都带 T1
		resp := wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
		readAndCloseBody(t, resp.Body)
	}
	a.ClearTurnState(cred.AccountID, nil, nil) // 轮结束：只清 ts（G5）
	require.Equal(t, "", testSessionTurnState(a, cred.AccountID, nil, nil), "轮结束清空会话槽")

	for range 2 { // 第 2 轮：两次都不带上一轮的 ts
		resp := wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
		readAndCloseBody(t, resp.Body)
	}

	require.Equal(t, []string{"T1", "T1", "", ""}, up.wsRequestTS(), "跨轮绝不重放上一轮 ts")
	require.Equal(t, 1, up.upgradeCount(), "清 ts 不动连接（round 边界不重连）")
}

// ---------------------------------------------------------------------------
// D 事件级捕获
// ---------------------------------------------------------------------------

// TestCodexWSTurnStateFromMetadataEvent D：上游只在 `response.metadata` 事件里给
// ts ⇒ observer 写进会话槽 ⇒ 网关下一次请求带它，且 upgrades 不增。
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

	resp1 := wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
	require.Equal(t, "", resp1.Header.Get(codexsdk.HeaderTurnState), "无握手签发 ⇒ 合成响应头不带 ts")
	readAndCloseBody(t, resp1.Body)
	// 事件 ts 是**读帧时**才到的（合成响应头早已定死），所以判据只能看会话槽。
	require.Equal(t, "T3", testSessionTurnState(a, cred.AccountID, nil, nil), "事件 ts 经 observer 写入会话槽")

	resp2 := wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
	readAndCloseBody(t, resp2.Body)

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
			up := newWSReuseUpstream(t, "")
			a, _ := wsReuseCodex(t, up)
			cred := wsReuseCred()
			seedTurnState(t, a, cred, nil, nil, "T1")

			resp1 := wsReuseStream(t, a, cred, nil, nil, "", transport)
			readAndCloseBody(t, resp1.Body)

			// 客户端自带值（透传）→ 成为该会话当前 ts
			resp2 := wsReuseStream(t, a, cred, nil, nil, "T2", transport)
			readAndCloseBody(t, resp2.Body)

			// 客户端未带 → provider 下发会话槽里的当前值
			resp3 := wsReuseStream(t, a, cred, nil, nil, "", transport)
			readAndCloseBody(t, resp3.Body)

			require.Equal(t, []string{"T1", "T2", "T2"}, up.httpRequestTS(), "每次请求带当前 ts（逐请求求值）")
			require.Equal(t, 0, up.upgradeCount(), "HTTP 腿不升级")
			require.Equal(t, 3, up.postCount())
		})
	}
}

// ---------------------------------------------------------------------------
// E 关池不动 h2/h1
// ---------------------------------------------------------------------------

// TestCodexDropPathsCloseWSOnly E：三条 drop 路径（entry 覆盖 / LRU 驱逐 /
// evict）各触发一次 ⇒ 假上游观察到 WS 关闭；**紧接**一次 HTTP 腿请求，
// DialContext 计数不增（底层连接池没被打掉）。
//
// transport 必须是**真 `*http.Transport`**：SDK 的 CloseIdleConnections 只对
// `client.Transport.(*http.Transport)` 断言——包装器下那条分支永不执行，正确实现
// 与 M3 变异都会是"计数不增"，用例就没有判别力了。
func TestCodexDropPathsCloseWSOnly(t *testing.T) {
	cases := []struct {
		name string
		drop func(t *testing.T, a *Codex, cred *domain.AccountCredential)
	}{
		{
			// entryFor 覆盖旧 entry：外部凭据变更（PAT 变化）⇒ sig 不同 ⇒ 旧条目弃用
			name: "entry-overwrite",
			drop: func(t *testing.T, a *Codex, cred *domain.AccountCredential) {
				t.Helper()
				next := *cred
				next.PATKey = "pat-reuse-rotated"
				resp, err := a.StreamBody(t.Context(), &next, a.DirectEgress(),
					[]byte(`{"model":"m"}`), nil, nil, "", domain.CodexResponsesTransportHTTP)
				require.NoError(t, err)
				// 必须读尽关掉：否则这条底层连接被占用，第 4 步的"不新增 dial"
				// 断言会为**测试自身的泄漏**变红（与被测行为无关）。
				readAndCloseBody(t, resp.Body)
			},
		},
		{
			// LRU 驱逐：塞满上限 + 2 个新身份 ⇒ 最久未用的两条（http 腿 + ws 腿的
			// 客户端）被驱逐
			name: "lru-evict",
			drop: func(t *testing.T, a *Codex, cred *domain.AccountCredential) {
				t.Helper()
				for i := range codexHTTPClientCacheMax + 2 {
					meta := &codexsdk.CodexMeta{InstallationID: fmt.Sprintf("evict-%d", i)}
					_, _, err := a.clientFor(cred, nil, meta, a.DirectEgress(), domain.CodexResponsesTransportDefault)
					require.NoError(t, err)
				}
			},
		},
		{
			// evict(accountID)：fatal 上报后的剔除路径
			name: "evict",
			drop: func(t *testing.T, a *Codex, cred *domain.AccountCredential) {
				t.Helper()
				a.evict(cred.AccountID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newWSReuseUpstream(t, "")
			a, dials := wsReuseCodex(t, up)
			cred := wsReuseCred()

			// 1) 预热：HTTP 腿一次请求（占一条底层空闲连接）
			resp, err := a.StreamBody(t.Context(), cred, a.DirectEgress(),
				[]byte(`{"model":"m"}`), nil, nil, "", domain.CodexResponsesTransportHTTP)
			require.NoError(t, err)
			readAndCloseBody(t, resp.Body)
			require.Equal(t, int64(1), dials.Load(), "预热只拨一次")
			require.Equal(t, 0, up.upgradeCount())

			// 2) 一条**空闲**（已归还缓存）的 WS 连接
			resp = wsReuseStream(t, a, cred, nil, nil, "", domain.CodexResponsesTransportWS)
			readAndCloseBody(t, resp.Body)
			require.Equal(t, 1, up.upgradeCount())
			require.Equal(t, int64(2), dials.Load(), "WS 拨号另计一次")

			// 3) 触发 drop 路径 ⇒ 上游必须看到 WS 关闭
			tc.drop(t, a, cred)
			select {
			case <-up.wsClosed:
			case <-time.After(15 * time.Second):
				t.Fatalf("上游未观察到 WS 连接关闭（drop 路径没调 CloseWSIdleConnections）")
			}
			require.Equal(t, 1, up.closedCount())

			// 4) 紧接一次 HTTP 腿请求：底层连接池不受影响 ⇒ 不新增 dial
			before := dials.Load()
			resp, err = a.StreamBody(t.Context(), cred, a.DirectEgress(),
				[]byte(`{"model":"m"}`), nil, nil, "", domain.CodexResponsesTransportHTTP)
			require.NoError(t, err)
			readAndCloseBody(t, resp.Body)
			require.Equal(t, before, dials.Load(),
				"关 WS 空闲连接不得打掉底层连接池（CloseWSIdleConnections，不是 CloseIdleConnections）")
		})
	}
}
