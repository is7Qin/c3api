// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/httpx"
)

// encodingRecorder 假上游/代理：逐请求记录 Accept-Encoding 与 RequestURI，并统计
// 被关闭的连接数（CloseClients 是否真的关到连接池的判据）。
type encodingRecorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	seen   []encodingSeen
	closed atomic.Int32
}

type encodingSeen struct {
	uri            string
	acceptEncoding []string
}

func newEncodingRecorder(t *testing.T) *encodingRecorder {
	t.Helper()
	rec := &encodingRecorder{}
	rec.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, encodingSeen{uri: r.RequestURI, acceptEncoding: r.Header.Values("Accept-Encoding")})
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	rec.srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed || st == http.StateHijacked {
			rec.closed.Add(1)
		}
	}
	rec.srv.Start()
	t.Cleanup(rec.srv.Close)
	return rec
}

// last 返回最近一次请求的记录。
func (r *encodingRecorder) last(t *testing.T) encodingSeen {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.seen, "上游没有收到请求")
	return r.seen[len(r.seen)-1]
}

// getAndDrain 发一次 GET 并读尽关闭（连接因此回到空闲池）。
func getAndDrain(t *testing.T, hc *http.Client, u string) {
	t.Helper()
	resp, err := hc.Get(u)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

// TestCodexTransportNoAutoGzip（C3-9）：用真实 *http.Transport 与 ShardedTransport
// 两种底座，Codex 出站（直连与代理）不带 Accept-Encoding，HTTP 面（非 Codex
// provider 共用的那份）仍自动带 gzip；CloseClients 后 HTTP 与 Codex 两份连接池都
// 被关闭；底座 RoundTripper 类型未知时 Codex 与 HTTP 是同一个 client。
//
// 负例：codexClientOf 漏设 DisableCompression ⇒ Codex 面收到 gzip，红；For 的代理
// 克隆漏派生 Codex（或直接用 HTTP）⇒ 代理臂收到 gzip，红；CloseClients 只关 HTTP ⇒
// 关闭计数只到 1，红；未知 RoundTripper 照搬 DefaultTransport 克隆 ⇒ NotSame，红。
func TestCodexTransportNoAutoGzip(t *testing.T) {
	bases := []struct {
		name string
		rt   func() http.RoundTripper
	}{
		{name: "transport", rt: func() http.RoundTripper { return &http.Transport{MaxIdleConns: 8} }},
		{name: "sharded", rt: func() http.RoundTripper {
			return httpx.NewShardedTransport(&http.Transport{MaxIdleConns: 8}, 2)
		}},
	}
	for _, tc := range bases {
		t.Run(tc.name, func(t *testing.T) {
			up := newEncodingRecorder(t)
			proxy := newEncodingRecorder(t)
			base := &http.Client{Transport: tc.rt()}
			pool := NewPool(base)

			direct := pool.Direct()
			require.Same(t, base, direct.HTTP, "HTTP 面直连仍是底座原样")
			require.NotNil(t, direct.Codex)
			require.NotSame(t, direct.HTTP, direct.Codex, "已知底座必须派生独立的 Codex client")
			require.NotSame(t, direct.HTTP.Transport, direct.Codex.Transport, "Codex 必须是独立连接池")

			getAndDrain(t, direct.Codex, up.srv.URL+"/codex")
			require.Empty(t, up.last(t).acceptEncoding, "Codex 直连出站不得自动带 Accept-Encoding")
			getAndDrain(t, direct.HTTP, up.srv.URL+"/other")
			require.Equal(t, []string{"gzip"}, up.last(t).acceptEncoding, "非 Codex 面仍自动带 gzip")

			host, port := splitHostPort(t, proxy.srv.Listener.Addr().String())
			c := pool.For(New("http", host, port, "", ""))
			require.NotNil(t, c.Codex)
			require.NotSame(t, c.HTTP, c.Codex)
			require.NotSame(t, c.HTTP.Transport, c.Codex.Transport, "代理出口的 Codex 也必须是独立连接池")

			getAndDrain(t, c.Codex, "http://upstream.invalid/codex")
			seen := proxy.last(t)
			require.Equal(t, "http://upstream.invalid/codex", seen.uri, "Codex 面必须经代理（绝对 URI）")
			require.Empty(t, seen.acceptEncoding, "Codex 经代理出站不得自动带 Accept-Encoding")
			getAndDrain(t, c.HTTP, "http://upstream.invalid/other")
			seen = proxy.last(t)
			require.Equal(t, "http://upstream.invalid/other", seen.uri)
			require.Equal(t, []string{"gzip"}, seen.acceptEncoding, "非 Codex 面经代理仍自动带 gzip")

			// 两份连接池各有一条到代理的空闲连接；摘除后关闭必须两份都关。
			require.Zero(t, proxy.closed.Load(), "关闭前不应有连接被关")
			CloseClients(pool.Detach(nil))
			require.Eventually(t, func() bool { return proxy.closed.Load() >= 2 }, 2*time.Second, 10*time.Millisecond,
				"CloseClients 必须同时关掉 HTTP 与 Codex 两份连接池的空闲连接")
		})
	}

	t.Run("unknown-roundtripper", func(t *testing.T) {
		var hits atomic.Int32
		base := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			hits.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
		})}
		pool := NewPool(base)
		require.Same(t, pool.Direct().HTTP, pool.Direct().Codex, "未知 RoundTripper 不派生：Codex 就是 HTTP")
		c := pool.For(New("http", "127.0.0.1", 3128, "", ""))
		require.Same(t, c.HTTP, c.Codex, "未知 RoundTripper 的代理出口同样 Codex == HTTP")

		getAndDrain(t, pool.Direct().Codex, "http://upstream.invalid/x")
		require.Equal(t, int32(1), hits.Load(), "Codex 面必须仍走测试的改写 transport（不得退回 DefaultTransport）")
		require.NotPanics(t, func() { CloseClients([]*Client{pool.Direct(), c}) })

		nilBase := NewPool(nil)
		require.Same(t, nilBase.Direct().HTTP, nilBase.Direct().Codex, "nil 底座（DefaultClient）不派生")
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
