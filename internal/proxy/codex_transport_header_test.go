// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// --- 批次 5：生效传输头（spec §6）+ 转换车道钉 http（spec §8.2 的 proxy 条） ---
//
// 判据面说明：p.codex 是具体类型 *sdkbridge.Codex（本批不动 sdkbridge，也不
// 把适配层抽成接口），所以无法注入"记录 transport 实参"的假适配层。转换车道
// 钉 http 的判据改为**假上游的腿计数**（Upgrade vs POST）——它正是 transport
// 实参到达 SDK 后的可观测效果：若把 domain.CodexResponsesTransportHTTP 换成
// sel.CodexResponsesTransport（ws），上游会看到真实升级 ⇒ 用例红。这比"响应头
// 里没有 ws"强：转换车道根本不写头，头不能作为判据。

// codexLegWSCompletedFrame 最小 WS 终态帧（SDK wsReader 交付后聚合器收口）。
const codexLegWSCompletedFrame = `{"type":"response.completed","response":{"id":"resp_leg","object":"response","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`

// codexLegUpstream 双腿假上游：按 Upgrade 头分流并各自计数（判据面 = 哪条腿
// 真的收到了请求）。HTTP 腿回完整 SSE 事件序列（created/delta/item/completed +
// [DONE]），WS 腿读一帧后回终态帧——两条腿都能让原生/转换车道成功收口。
type codexLegUpstream struct {
	mu       sync.Mutex
	upgrades int
	posts    int
}

func newCodexLegUpstream(t *testing.T) (*httptest.Server, *codexLegUpstream) {
	t.Helper()
	u := &codexLegUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(u.handle))
	t.Cleanup(func() {
		// 升级过的连接已 hijack，Close 不会替我们收尾——先断再关，防收尾挂住。
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv, u
}

func (u *codexLegUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket") {
		u.mu.Lock()
		u.upgrades++
		u.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// 先读后写，避免与客户端发送竞态。新连接首请求 SDK 先发预热帧
		// （generate:false，官方 HTTP→WS 预热）：按官方上游回 created/completed 后
		// 继续读真实 response.create，再回终态帧。
		for {
			typ, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if isPrewarmFrame(msg) {
				if writePrewarmResponse(c, typ) != nil {
					return
				}
				continue
			}
			_ = c.Write(r.Context(), websocket.MessageText, []byte(codexLegWSCompletedFrame))
			return
		}
	}
	u.mu.Lock()
	u.posts++
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	for _, ev := range []string{convCodexRespCreated, convCodexRespDelta, convCodexRespItemEv, convCodexRespDone} {
		_, _ = io.WriteString(w, "data: "+ev+"\n\n")
		f.Flush()
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	f.Flush()
}

// counts 两条腿各自的到达次数。
func (u *codexLegUpstream) counts() (upgrades, posts int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrades, u.posts
}

// newCodexTransportProxy 构造 codex 类型 resp 测试代理，模板出站传输 = tr。
func newCodexTransportProxy(t *testing.T, upstream string, tr domain.CodexResponsesTransport) *Proxy {
	t.Helper()
	p, _ := newTestCodexRespProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")},
		upstream, nil, nil, &captureLogStore{}, withCodexResponsesTransport(tr))
	return p
}

// TestCodexResponsesUpstreamTransportHeader 原生车道：每个响应都带
// x-c3api-upstream-transport，值 = Name()（ws / 默认 http），流式与非流式两个
// 入口都覆盖；同时用腿计数钉住"头说的腿 = 真的到达的腿"。
func TestCodexResponsesUpstreamTransportHeader(t *testing.T) {
	cases := []struct {
		name   string
		tr     domain.CodexResponsesTransport
		stream bool
		want   string
		wsLeg  bool
	}{
		{name: "ws-nonstream", tr: domain.CodexResponsesTransportWS, want: "ws", wsLeg: true},
		{name: "default-nonstream", tr: domain.CodexResponsesTransportDefault, want: "http"},
		{name: "ws-stream", tr: domain.CodexResponsesTransportWS, stream: true, want: "ws", wsLeg: true},
		{name: "default-stream", tr: domain.CodexResponsesTransportDefault, stream: true, want: "http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, upc := newCodexLegUpstream(t)
			p := newCodexTransportProxy(t, up.URL, tc.tr)
			srv := httptest.NewServer(AIRouter(p))
			defer srv.Close()

			body := `{"model":"gpt-4o","input":"hi"}`
			if tc.stream {
				body = `{"model":"gpt-4o","stream":true,"input":"hi"}`
			}
			resp := postResponses(t, srv, body)
			defer resp.Body.Close()
			_, _ = io.ReadAll(resp.Body)

			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, tc.want, resp.Header.Get(headerUpstreamTransport),
				"生效传输头 = 网关选择的腿（Name()）")
			upgrades, posts := upc.counts()
			if tc.wsLeg {
				require.Equal(t, 1, upgrades, "头报 ws ⇒ 上游必须真的升级")
				require.Equal(t, 0, posts, "头报 ws ⇒ 不得回退 HTTP POST")
			} else {
				require.Equal(t, 0, upgrades, "头报 http ⇒ 不得升级")
				require.Equal(t, 1, posts, "头报 http ⇒ 走 HTTP POST")
			}
		})
	}
}

// TestCodexResponsesUpstreamTransportHeaderEarlyExits 两条早退也带头（spec §6
// 明写：一处写点覆盖 501 / errCodexExtMissing）。
func TestCodexResponsesUpstreamTransportHeaderEarlyExits(t *testing.T) {
	t.Run("adapter-missing-501", func(t *testing.T) {
		up, _ := newCodexLegUpstream(t)
		p := newCodexTransportProxy(t, up.URL, domain.CodexResponsesTransportWS)
		p.codex = nil // 装配缺失模拟（main 未 SetCodex）
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()

		resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		require.Equal(t, "ws", resp.Header.Get(headerUpstreamTransport),
			"501 早退也必须带生效传输头")
	})

	t.Run("ext-missing", func(t *testing.T) {
		up, _ := newCodexLegUpstream(t)
		p, _ := newTestCodexRespProxy(t, credential.TypeCodexOAuth,
			map[int64]*domain.AccountExt{10: nil}, // ext 快照缺失（配置损坏）
			up.URL, nil, nil, &captureLogStore{},
			withCodexResponsesTransport(domain.CodexResponsesTransportWS))
		srv := httptest.NewServer(AIRouter(p))
		defer srv.Close()

		resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		require.Equal(t, http.StatusBadGateway, resp.StatusCode, "连接级错误耗尽 → 502")
		require.Equal(t, "ws", resp.Header.Get(headerUpstreamTransport),
			"errCodexExtMissing 早退也必须带生效传输头")
	})
}

// TestCodexConvertedResponsesTransportHeaderAbsent 转换车道绕过
// callCodexResponses（直调 core）⇒ 响应头里没有生效传输头（spec §6）。
func TestCodexConvertedResponsesTransportHeaderAbsent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up, upc := newCodexLegUpstream(t)
			p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
				map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
				up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})

			body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`
			}
			rec := postChatConv(t, p, body, nil)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Empty(t, rec.Header().Get(headerUpstreamTransport),
				"转换车道天然不写生效传输头")
			_, posts := upc.counts()
			require.Equal(t, 1, posts, "转换车道走 HTTP POST")
		})
	}
}

// TestCodexConvertedResponsesTransportPinnedHTTP 转换车道钉 http（spec §8.2
// 的 proxy 条）：模板/选择说 ws 也必须以 http 调 core。判据 = 假上游腿计数
// （见文件头注释——不拿"头里没 ws"当判据）。
func TestCodexConvertedResponsesTransportPinnedHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up, upc := newCodexLegUpstream(t)
			p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
				map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
				up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{},
				withCodexResponsesTransport(domain.CodexResponsesTransportWS))

			body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`
			}
			rec := postChatConv(t, p, body, nil)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			upgrades, posts := upc.counts()
			require.Equal(t, 0, upgrades, "转换车道必须钉 http——不允许因 sel=ws 真的升级上游")
			require.Equal(t, 1, posts, "转换车道必须走 HTTP POST")
		})
	}
}

// TestCodexResponsesWSFailureDoesNotFallBackToHTTP WS 拨号失败不回落（spec §6
// 末段 / §8.2 proxy 条）：假上游不是 WS 端点（对 Upgrade 请求回 200 普通响应
// ⇒ 握手失败）。若 SDK/网关悄悄改走 http，同一上游会以 200 SSE 成功——所以
// "最终 502 且没有 200"就是"错误上抛、不回落"的判据。
func TestCodexResponsesWSFailureDoesNotFallBackToHTTP(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespDone}})
	defer up.Close()
	p := newCodexTransportProxy(t, up.URL, domain.CodexResponsesTransportWS)
	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()

	resp := postResponses(t, srv, `{"model":"gpt-4o","input":"hi"}`)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"ws 握手失败必须上抛（静默回落 http 会拿到 200）")
	require.Equal(t, "ws", resp.Header.Get(headerUpstreamTransport))
	require.NotZero(t, upc.callsN(), "握手确实到达了上游（只是没升级）")
}
