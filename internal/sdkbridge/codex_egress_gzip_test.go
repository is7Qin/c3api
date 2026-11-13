// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// dialPinnedTransport 是**真实的** *http.Transport：任何 host 都拨到 srv，TLS 按
// httptest 证书的 example.com 校验（SDK 打的是固定的 https://chatgpt.com）。用真实
// *http.Transport 而不是改写 RoundTripper，出口池才会派生 Codex 底座。
func dialPinnedTransport(srv *httptest.Server) *http.Transport {
	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsCfg.ServerName = "example.com"
	addr := srv.Listener.Addr().String()
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: tlsCfg,
	}
}

// TestCodexEgressTransportNoAutoGzip（C3-9 的 sdkbridge 接线）：Codex HTTP（含 ws
// 腿的 HTTPClient）与 DialPrepared 两处 WithTransport 都用出口的 Codex 底座——上游
// 收到的请求与握手都没有 Accept-Encoding；同一出口的 HTTP 面仍自动带 gzip。
// 负例：任一处改回 eg.HTTP.Transport ⇒ 对应臂收到 gzip，红。
func TestCodexEgressTransportNoAutoGzip(t *testing.T) {
	up := &transportUpstream{}
	var mu sync.Mutex
	seen := map[string][]string{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leg := "post"
		if isWebsocketUpgrade(r) {
			leg = "upgrade"
		}
		if r.Method == http.MethodGet && !isWebsocketUpgrade(r) {
			leg = "plain"
		}
		mu.Lock()
		seen[leg] = append(seen[leg], r.Header.Get("Accept-Encoding"))
		mu.Unlock()
		if leg == "plain" {
			w.WriteHeader(http.StatusOK)
			return
		}
		up.handle(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	pool := egress.NewPool(&http.Client{Transport: dialPinnedTransport(srv)})
	require.NotSame(t, pool.Direct().HTTP, pool.Direct().Codex)
	a := NewCodex(nil, pool, nil, RotationDeps{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := []byte(`{"model":"gpt-5.6","input":[]}`)

	for _, tr := range []domain.CodexResponsesTransport{domain.CodexResponsesTransportHTTP, domain.CodexResponsesTransportWS} {
		resp, err := a.StreamBody(ctx, transportTestCred(), a.DirectEgress(), payload, codexsdk.Identity{}, tr)
		require.NoError(t, err, "transport=%s", tr)
		require.Contains(t, readAndCloseBody(t, resp.Body), "response.completed")
	}

	prep, err := a.Prepare(transportTestCred())
	require.NoError(t, err)
	c, err := a.DialPrepared(ctx, prep, a.DirectEgress(), codexsdk.Identity{}, codexsdk.WithPingInterval(0))
	require.NoError(t, err)
	require.NoError(t, c.CloseNow())

	resp, err := pool.Direct().HTTP.Get("https://chatgpt.com/plain")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{""}, seen["post"], "Codex HTTP 出站不得自动带 Accept-Encoding")
	require.Equal(t, []string{"", ""}, seen["upgrade"], "Codex ws 腿与 DialPrepared 的握手不得自动带 Accept-Encoding")
	require.Equal(t, []string{"gzip"}, seen["plain"], "同一出口的 HTTP 面仍自动带 gzip")
}
