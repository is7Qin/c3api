// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
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

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// ---------------------------------------------------------------------------
// 出站传输开关（spec-codex-outbound-transport §5 / §8.2 的 sdkbridge 组合用例）
//
// 这一组是"选择 = 实际"的**唯一**证明：其余门禁只证明"网关选了什么"（Selection
// 里的字段 + 响应头的值），只有这里能证明"请求真的走了那条腿"。所以判据一律是
// 假上游侧的**连接形态计数**（升级 / 普通 POST），不是缓存键、不是私有字段。
// ---------------------------------------------------------------------------

// transportWSCompletedFrame 最小 WS 终态帧（SDK 的 wsReader 见到它在交付后
// 停止再读，不必依赖对端 close frame）。
const transportWSCompletedFrame = `{"type":"response.completed","response":{"id":"resp_transport","object":"response","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`

// transportHTTPCompletedEvent 最小 HTTP SSE 终态事件（aggregator 见到 completed
// 即判定流完整，不会落"截断"错误）。
const transportHTTPCompletedEvent = `event: response.completed` + "\n" + `data: ` + transportWSCompletedFrame + "\n\n"

// transportUpstream 假上游：按 `Upgrade` 头把同一个 handler 分流成两条腿。
//
//   - 升级请求 → websocket.Accept（读一帧 = 请求帧，回一帧终态事件，随即关连接）
//   - 普通请求 → 最小 `response.completed` SSE 体（**不回空 body**：空 body 会让
//     两条腿除了"走哪条"之外还多一处差异，读数就没法只归因到传输）
type transportUpstream struct {
	mu       sync.Mutex
	upgrades int
	posts    int
	frames   []string
}

func newTransportUpstream(t *testing.T) (*httptest.Server, *transportUpstream) {
	t.Helper()
	u := &transportUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(func() {
		// 升级过的连接已 hijack，Close 不会替我们收尾——先断再关，防收尾挂住。
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv, u
}

func (u *transportUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if isWebsocketUpgrade(r) {
		u.mu.Lock()
		u.upgrades++
		u.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// 读一帧（SDK 只发一帧 response.create）：先读后写，避免与客户端发送竞态。
		if _, data, rerr := c.Read(r.Context()); rerr == nil {
			u.mu.Lock()
			u.frames = append(u.frames, string(data))
			u.mu.Unlock()
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(transportWSCompletedFrame))
		return
	}
	u.mu.Lock()
	u.posts++
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, transportHTTPCompletedEvent)
}

// counts 两条腿各自的到达次数（判据面：谁真的到了上游）。
func (u *transportUpstream) counts() (upgrades, posts int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrades, u.posts
}

// sentFrames 升级后收到的帧（逐帧字符串拷贝）。
func (u *transportUpstream) sentFrames() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.frames...)
}

// isWebsocketUpgrade 判定请求是否是要升级（头值可能带逗号列表）。
func isWebsocketUpgrade(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket")
}

// transportTestCodex 装配真 Codex：凭据 → 出口池（底座承载官方 URL 改写 transport
// ——它同时覆盖 HTTP POST 与 WS 拨号）。
func transportTestCodex(t *testing.T, srvURL string) *Codex {
	t.Helper()
	return NewCodex(nil, testEgressPool(newOfficialRewriteTransport(t, srvURL)), nil, RotationDeps{})
}

func transportTestCred() *domain.AccountCredential {
	return &domain.AccountCredential{AccountID: 21, PATKey: "pat-transport"}
}

// readAndCloseBody 读尽并恰好关闭一次（与 proxy 侧消费语义同形）。
func readAndCloseBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer func() { require.NoError(t, body.Close()) }()
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	return string(raw)
}

// TestCodexTransportMapping 映射面：domain 开关 → SDK 枚举的**唯一**映射点。
// 显式映射（而不是 `codexsdk.Transport(t)`）意味着脏值也只可能落到默认面——
// 一旦有人把 ws 折成 http，这条用例红；把 ws 直传成 SDK 枚举则依赖"字符串恰好
// 相等"的巧合，本表正是那个巧合的替代品。
func TestCodexTransportMapping(t *testing.T) {
	cases := []struct {
		in   domain.CodexResponsesTransport
		want codexsdk.Transport
	}{
		{in: domain.CodexResponsesTransportDefault, want: codexsdk.TransportHTTP},
		{in: domain.CodexResponsesTransportHTTP, want: codexsdk.TransportHTTP},
		{in: domain.CodexResponsesTransportWS, want: codexsdk.TransportWS},
		// 脏值（绕过写面的 DB 直改/导入）：fail-safe 到默认面，不静默切传输。
		{in: "WS", want: codexsdk.TransportHTTP},
		{in: "websocket", want: codexsdk.TransportHTTP},
		{in: " ", want: codexsdk.TransportHTTP},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, codexTransport(tc.in), "codexTransport(%q)", string(tc.in))
	}
}

// TestCodexStreamBodyTransportReachesChosenLeg 流式面组合用例（本轮最重要的门禁）：
// 直接给 StreamBody 传 transport 实参，判据是假上游侧的升级/POST 计数。
func TestCodexStreamBodyTransportReachesChosenLeg(t *testing.T) {
	cases := []struct {
		name      string
		transport domain.CodexResponsesTransport
		wantWS    bool
	}{
		{name: "ws", transport: domain.CodexResponsesTransportWS, wantWS: true},
		{name: "default", transport: domain.CodexResponsesTransportDefault},
		{name: "http", transport: domain.CodexResponsesTransportHTTP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, up := newTransportUpstream(t)
			a := transportTestCodex(t, srv.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := a.StreamBody(ctx, transportTestCred(), a.DirectEgress(),
				[]byte(`{"model":"gpt-5.6","input":[]}`), codexsdk.Identity{}, tc.transport)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body := readAndCloseBody(t, resp.Body)
			require.Contains(t, body, `response.completed`, "两条腿都必须交付终态事件")

			upgrades, posts := up.counts()
			if tc.wantWS {
				require.Equal(t, 1, upgrades, "transport=ws 必须真的升级上游连接")
				require.Equal(t, 0, posts, "transport=ws 不得退回 HTTP POST")
				// 升级后确实按 WS 协议发了一帧 response.create（不只是"握了手"）。
				frames := up.sentFrames()
				require.Len(t, frames, 1)
				require.Contains(t, frames[0], `"response.create"`)
			} else {
				require.Equal(t, 0, upgrades, "transport=%q 不得升级", string(tc.transport))
				require.Equal(t, 1, posts, "transport=%q 必须走 HTTP POST", string(tc.transport))
			}
		})
	}
}

// TestCodexResponsesTransportReachesChosenLeg 非流式面同款断言（Responses 走的是
// StreamBody 的 ws 臂 + 同一个聚合器，所以合成体形状不变——变的只有哪条腿）。
func TestCodexResponsesTransportReachesChosenLeg(t *testing.T) {
	cases := []struct {
		name      string
		transport domain.CodexResponsesTransport
		wantWS    bool
	}{
		{name: "ws", transport: domain.CodexResponsesTransportWS, wantWS: true},
		{name: "default", transport: domain.CodexResponsesTransportDefault},
		{name: "http", transport: domain.CodexResponsesTransportHTTP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, up := newTransportUpstream(t)
			a := transportTestCodex(t, srv.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			resp, err := a.Responses(ctx, transportTestCred(), a.DirectEgress(),
				[]byte(`{"model":"gpt-5.6","input":[]}`), codexsdk.Identity{}, tc.transport)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NotEmpty(t, resp.Raw, "合成体不得为空")
			require.Contains(t, string(resp.Raw), `"completed"`)

			upgrades, posts := up.counts()
			if tc.wantWS {
				require.Equal(t, 1, upgrades)
				require.Equal(t, 0, posts)
			} else {
				require.Equal(t, 0, upgrades)
				require.Equal(t, 1, posts)
			}
		})
	}
}

// TestCodexTransportIsClientKeyDimension 键维：传输是**构造期**参数，所以同账号
// 不同传输必须是不同 client 条目——否则"模板改成 ws、client 命中旧条目"会让
// Selection 说 ws 而请求仍走 HTTP。同时钉住 Key() 归一："" 与 "http" 同条目。
func TestCodexTransportIsClientKeyDimension(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := transportTestCred()
	eg := a.DirectEgress()

	e, wsClient, err := a.clientFor(cred, eg, domain.CodexResponsesTransportWS)
	require.NoError(t, err)
	_, httpClient, err := a.clientFor(cred, eg, domain.CodexResponsesTransportHTTP)
	require.NoError(t, err)
	require.NotSame(t, wsClient, httpClient, "同身份不同传输必须是不同 client（构造期参数无法替换）")
	require.Len(t, a.clients.clients[transportTestCred().AccountID], 2, "ws 与 http 各占一条")

	_, defaultClient, err := a.clientFor(cred, eg, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.Same(t, httpClient, defaultClient, `"" 与 "http" 必须落同一条目（Key() 归一）`)
	require.Len(t, a.clients.clients[transportTestCred().AccountID], 2, `"" 不得再开一条`)

	// 键里的确有 transport 维（不是"两个条目恰好不同"）：ws 维的键命中 ws 条目，
	// 而零值键（= Key() 归一的 HTTP 面）命中 HTTP 条目——两组键各自落桶。
	keyWS := clientKey{transport: domain.CodexResponsesTransportWS.Key()}
	require.Same(t, wsClient, testCachedClient(a, e, keyWS), "ws 条目按含 transport 维的键落桶")
	require.Same(t, httpClient, testCachedClient(a, e, clientKey{}), "零值键落 HTTP 条目（Key() 归一面）")
	require.NotSame(t, wsClient, testCachedClient(a, e, clientKey{}), "零值键不得命中 ws 条目")
}

// TestCodexTransportSwitchChangesLegWithinOneInstance 键维的**行为面**证据：同一个
// Codex 实例、同一个身份，传输从 http 切到 ws 再切回 http —— 每次切换都必须真的
// 换到对应那条腿（只断言 len(clients)==2 证明不了"换条目 ⇒ 换传输"）。
func TestCodexTransportSwitchChangesLegWithinOneInstance(t *testing.T) {
	srv, up := newTransportUpstream(t)
	a := transportTestCodex(t, srv.URL)
	cred := transportTestCred()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	payload := []byte(`{"model":"gpt-5.6","input":[]}`)

	call := func(transport domain.CodexResponsesTransport) {
		t.Helper()
		resp, err := a.StreamBody(ctx, cred, a.DirectEgress(), payload, codexsdk.Identity{}, transport)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = readAndCloseBody(t, resp.Body)
	}

	call(domain.CodexResponsesTransportDefault)
	upgrades, posts := up.counts()
	require.Equal(t, 0, upgrades)
	require.Equal(t, 1, posts)

	call(domain.CodexResponsesTransportWS)
	upgrades, posts = up.counts()
	require.Equal(t, 1, upgrades, "切到 ws 必须真的升级（旧 HTTP 条目不得被复用）")
	require.Equal(t, 1, posts)

	call(domain.CodexResponsesTransportHTTP)
	upgrades, posts = up.counts()
	require.Equal(t, 1, upgrades)
	require.Equal(t, 2, posts, `显式 http 与 "" 同条目 ⇒ 复用已构造的 HTTP client，再打一次 POST`)

	// 三次调用只该有两条条目（""/"http" 归一 + ws）。
	_, _, err := a.clientFor(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.Len(t, a.clients.clients[transportTestCred().AccountID], 2)
}

// TestCodexTranslateErrorMapsWSBackpressure 背压：SDK sentinel → 领域 sentinel。
// 双向链都要能 errors.Is（网关按领域语义分类，同时保留 SDK 原始面可回溯），
// 且不得被 fatal 判定误当成账号级终止、也不得被当成"网络故障"。
func TestCodexTranslateErrorMapsWSBackpressure(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	mapped := a.translateError(nil, codexsdk.ErrWSBackpressure)
	require.ErrorIs(t, mapped, domain.ErrUpstreamBackpressure, "领域 sentinel 必须可判")
	require.ErrorIs(t, mapped, codexsdk.ErrWSBackpressure, "原始 SDK 错误链必须保留")
	require.False(t, IsFatal(mapped), "背压不是账号级终止（不得触发摘号/冷却）")
	require.NotErrorIs(t, mapped, domain.ErrPartitionUnavailable)
}
