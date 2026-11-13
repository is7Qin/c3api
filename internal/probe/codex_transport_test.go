// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestProbeCodexTransportNoAutoGzip：guardedCodexTransport 的下层是出口的 Codex
// 底座（关闭自动压缩）——底座是真实 *http.Transport 时，codex 探测请求不带
// Accept-Encoding。负例：改回 eg.HTTP.Transport ⇒ 收到 gzip，红。
func TestProbeCodexTransportNoAutoGzip(t *testing.T) {
	got := make(chan []string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Values("Accept-Encoding"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n\n"))
	}))
	defer srv.Close()
	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsCfg.ServerName = "example.com" // httptest 证书覆盖 example.com；codex origin 固定 chatgpt.com
	addr := srv.Listener.Addr().String()
	base := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: tlsCfg,
	}

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	res := newTestProber(t, base).Probe(context.Background(), spec)
	require.True(t, res.Success, "probe 结果: %+v", res)
	require.Empty(t, <-got, "codex 探测出站不得自动带 Accept-Encoding")
}
