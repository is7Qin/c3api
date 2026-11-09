// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// Codex 代表性路径的分配基线（oracle N1 / spec §4 请求路径门禁）：
//
//	ResponsesHTTP   —— 非流式：HTTP POST + SSE 聚合（SDK 合成完整响应体）
//	ResponsesWS     —— 聚合臂走 WS exchange（帧聚合）
//	StreamBodyWS    —— 流式臂走 WS exchange（raw body 透传，读尽 + Close）
//	OwnerPrepare    —— 请求准备一步（条目句柄 + owner；HEAD 的请求路径成本）
//	OwnerHash       —— 旧口径的纯散列（对照：说明"准备"比"散列"多出来的是查表/锁）
//
// 上游是同一台机上的自签 TLS 假服务（HTTP + WS 同端口），transport 用 DialContext
// 重写地址——与生产同一条适配层代码路径（entry → client 桶 → SDK 调用），只把网络
// 换成 loopback。A/B 时基线 worktree 用同名用例的 c6014967 适配版跑**同一条命令**。

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// benchCodexCompletedFrame 最小 completed 事件：HTTP 腿（SSE）与 WS 腿（单帧）共用。
const benchCodexCreatedFrame = `{"type":"response.created","response":{"id":"bench","object":"response","status":"in_progress"}}`

const benchCodexCompletedFrame = `{"type":"response.completed","response":{"id":"bench","object":"response","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`

// benchCodexUpstream HTTP + WS 同端口的假上游：HTTP 腿回"completed + [DONE]"，
// WS 腿读一帧回一帧、连接保留（复用路径与生产同构）。
type benchCodexUpstream struct{ srv *httptest.Server }

func newBenchCodexUpstream(b *testing.B) *benchCodexUpstream {
	b.Helper()
	u := &benchCodexUpstream{}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if benchCodexWSUpgrade(r) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			for {
				if _, _, err := c.Read(r.Context()); err != nil {
					return
				}
				if err := c.Write(r.Context(), websocket.MessageText, []byte(benchCodexCreatedFrame)); err != nil {
					return
				}
				if err := c.Write(r.Context(), websocket.MessageText, []byte(benchCodexCompletedFrame)); err != nil {
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+benchCodexCompletedFrame+"\n\ndata: [DONE]\n\n")
	}))
	u.srv.StartTLS()
	b.Cleanup(u.srv.Close)
	return u
}

// benchCodexAdapter 真适配层 + 指向假上游的出口池（WS 与 HTTP 都从这里出网）。
func benchCodexAdapter(b *testing.B) (*Codex, *domain.AccountCredential, []byte) {
	b.Helper()
	up := newBenchCodexUpstream(b)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, up.srv.Listener.Addr().String())
		},
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // 自签 loopback 假上游
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     time.Minute,
	}
	b.Cleanup(tr.CloseIdleConnections)
	a := NewCodex(nil, egress.NewPool(&http.Client{Transport: tr}), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 1, PATKey: "bench-pat"}
	return a, cred, []byte(`{"model":"gpt-5.6","input":[]}`)
}

// benchCodexIdentity 固定会话身份（与生产同构的"线程级"复用键）：匿名身份在 SDK 里
// 不参与连接复用（首帧/握手每次重建），不代表生产请求路径。
func benchCodexIdentity() codexsdk.Identity {
	sess := codexsdk.Session{SessionID: "bench-session", ThreadID: "bench-thread", WindowID: "bench-thread:0"}
	return codexsdk.Identity{
		Session: sess,
		Meta: codexsdk.CodexMeta{
			InstallationID: "bench-install", SessionID: "bench-session",
			ThreadID: "bench-thread", WindowID: "bench-thread:0",
		},
	}
}

// benchCodexResponses 聚合臂（HTTP 非流式 / WS 聚合）：判据只看"成功拿到完整响应体"，
// 断言失败用 b.Fatal（基准外的错误面不在本基线范围内）。
func benchCodexResponses(b *testing.B, transport domain.CodexResponsesTransport) {
	a, cred, payload := benchCodexAdapter(b)
	id := benchCodexIdentity()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := a.Responses(ctx, cred, a.DirectEgress(), payload, id, transport)
		if err != nil {
			b.Fatal(err)
		}
		if len(resp.Raw) == 0 {
			b.Fatal("empty response body")
		}
	}
}

// BenchmarkCodexResponsesHTTPNonStream HTTP 非流式（SSE 聚合成完整响应体）。
func BenchmarkCodexResponsesHTTPNonStream(b *testing.B) {
	benchCodexResponses(b, domain.CodexResponsesTransportDefault)
}

// BenchmarkCodexResponsesWSAggregate WS 聚合臂（同一入口，transport=ws）。
func BenchmarkCodexResponsesWSAggregate(b *testing.B) {
	benchCodexResponses(b, domain.CodexResponsesTransportWS)
}

// BenchmarkCodexStreamBodyWS WS 流式臂：取 raw body、读尽、Close（归还连接）。
func BenchmarkCodexStreamBodyWS(b *testing.B) {
	a, cred, payload := benchCodexAdapter(b)
	id := benchCodexIdentity()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := a.StreamBody(ctx, cred, a.DirectEgress(), payload, id, domain.CodexResponsesTransportWS)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCodexOwnerPrepare 请求准备一步：Prepare（条目查表/复用 + 读稳定 owner）。
// 这是 HEAD 请求路径新增的成本面（entry 命中时只有一次表锁 + map 读，零堆分配）。
func BenchmarkCodexOwnerPrepare(b *testing.B) {
	a, cred, _ := benchCodexAdapter(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := a.Prepare(cred)
		if err != nil {
			b.Fatal(err)
		}
		if p.Owner() == ([16]byte{}) {
			b.Fatal("empty owner")
		}
	}
}

// BenchmarkCodexOwnerHash 旧口径对照：对凭据快照做纯散列（无查表/无锁）。
func BenchmarkCodexOwnerHash(b *testing.B) {
	_, cred, _ := benchCodexAdapter(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if CredOwner(cred) == ([16]byte{}) {
			b.Fatal("empty owner")
		}
	}
}

// benchCodexWSUpgrade 判定本次请求是不是 WS 升级（假上游按 Upgrade 头分流）。
func benchCodexWSUpgrade(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket")
}
