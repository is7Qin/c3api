// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 本文件锁 P3 的 aiclient 装饰器契约（spec custom-request-patch §7.1/§9）：
//   - 执行时机在网关默认头之后（后写覆盖赢）；
//   - nil = 快路径，不 panic、行为不变；
//   - Host 走 req.Host（Header.Set("Host") 会被 net/http 忽略）；
//   - Accept-Encoding 运行期强制剔除（装饰器自己写了也剔）；
//   - {api_key} 展开属于调用方闭包职责，本包只保证闭包被调用且其写入生效。

// TestDecorateRunsAfterGatewayDefaults 装饰器看到的是网关完整默认头，且其写入
// 覆盖网关默认（Authorization/User-Agent/自定义头）。
func TestDecorateRunsAfterGatewayDefaults(t *testing.T) {
	var srvHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvHeader.Store(r.Header.Clone())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: x"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionRaw(context.Background(), 1, srv.URL, "v1/chat/completions", "sk-test",
		[]byte(`{}`), nil, hcOf(f), func(req *http.Request) {
			// 网关默认头已就位（顺序契约的正面证据）。
			require.Equal(t, "Bearer sk-test", req.Header.Get("Authorization"))
			require.Equal(t, "application/json", req.Header.Get("Content-Type"))
			require.Equal(t, "application/json", req.Header.Get("Accept"))
			// 装饰器后写覆盖赢。
			req.Header.Set("Authorization", "Bearer template-declared")
			req.Header.Set("User-Agent", "vibe-code/1.0")
			req.Header.Set("X-Vendor-Key", "vendor")
			req.Header.Del("Accept")
		})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := srvHeader.Load().(http.Header)
	require.Equal(t, "Bearer template-declared", got.Get("Authorization"), "装饰器必须覆盖网关默认 Authorization")
	require.Equal(t, "vibe-code/1.0", got.Get("User-Agent"), "装饰器可设置 UA")
	require.Equal(t, "vendor", got.Get("X-Vendor-Key"))
	_, ok := got["Accept"]
	require.False(t, ok, "装饰器可删除 Accept")
}

// TestDecorateNilFastPath 六个 HTTP 出口的 nil 装饰器都不 panic、行为与改动前一致。
func TestDecorateNilFastPath(t *testing.T) {
	var hits int32
	var gotAuth, gotAPIKey atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		gotAuth.Store(r.Header.Get("Authorization"))
		gotAPIKey.Store(r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: x"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	ctx := context.Background()
	body := []byte(`{"stream":true}`)
	t.Run("RawPost", func(t *testing.T) {
		resp, err := f.RawPost(ctx, 1, srv.URL, "v1/x", "Authorization", "Bearer sk-test", body, nil, hcOf(f), nil)
		require.NoError(t, err)
		resp.Body.Close()
	})
	t.Run("ChatCompletionRaw", func(t *testing.T) {
		resp, err := f.ChatCompletionRaw(ctx, 1, srv.URL, "v1/chat/completions", "sk-test", body, nil, hcOf(f), nil)
		require.NoError(t, err)
		resp.Body.Close()
	})
	t.Run("ResponseRaw", func(t *testing.T) {
		resp, err := f.ResponseRaw(ctx, 1, srv.URL, "v1/responses", "sk-test", body, nil, hcOf(f), nil)
		require.NoError(t, err)
		resp.Body.Close()
	})
	t.Run("SearchRaw", func(t *testing.T) {
		resp, err := f.SearchRaw(ctx, 1, srv.URL, "v1/alpha/search", "sk-test", body, nil, hcOf(f), nil)
		require.NoError(t, err)
		resp.Body.Close()
	})
	t.Run("AnthMessageRaw", func(t *testing.T) {
		resp, err := f.AnthMessageRaw(ctx, 1, srv.URL, "v1/messages", "sk-test", body, nil, hcOf(f), nil)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, "sk-test", gotAPIKey.Load().(string), "anthropic 面鉴权头口径不变")
	})
	t.Run("ImagesRaw", func(t *testing.T) {
		resp, err := f.ImagesRaw(ctx, 1, srv.URL, "v1/images/generations", "sk-test", "", body, nil, hcOf(f), nil)
		require.NoError(t, err)
		resp.Body.Close()
	})
	require.Equal(t, int32(6), atomic.LoadInt32(&hits))
	require.Equal(t, "Bearer sk-test", gotAuth.Load().(string), "nil 装饰器不应改变 openai 面鉴权头")
}

// TestDecorateAcceptEncodingAlwaysStripped 装饰器写 Accept-Encoding 也不得出站
// （出站响应编码不变量；保存期已 400，这是脏库值的运行期兜底）。
func TestDecorateAcceptEncodingAlwaysStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{})
	resp, err := f.ChatCompletionRaw(context.Background(), 1, srv.URL, "v1/chat/completions", "sk-test",
		[]byte(`{}`), http.Header{}, hcOf(f), func(req *http.Request) {
			req.Header.Set("Accept-Encoding", "gzip")
			req.Header.Set("X-Kept", "1")
		})
	require.NoError(t, err)
	resp.Body.Close()
	built := capRt.headers(t)
	_, ok := built["Accept-Encoding"]
	require.False(t, ok, "装饰器写入的 Accept-Encoding 必须被运行期剔除")
	require.Equal(t, []string{"1"}, built["X-Kept"], "其余装饰头照常出站")
}

// TestDecorateHostOverride Host 特例：装饰器写 req.Host（不是 Header）,上游
// 看到覆盖后的 Host；Header 里不得出现字面 Host 键。
func TestDecorateHostOverride(t *testing.T) {
	var srvHost atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvHost.Store(r.Host)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{})
	resp, err := f.ChatCompletionRaw(context.Background(), 1, srv.URL, "v1/chat/completions", "sk-test",
		[]byte(`{}`), nil, hcOf(f), func(req *http.Request) {
			req.Host = "virtual.example"
		})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, "virtual.example", srvHost.Load().(string), "Host 覆盖必须走 req.Host")
	built := capRt.headers(t)
	_, ok := built["Host"]
	require.False(t, ok, "Host 不得被写进 Header map")
}

// TestDecorateSeesAnthropicDefaults anthropic 面装饰器看到 x-api-key 与缺省
// Anthropic-Version，且可覆盖 Version（配置面权威）。
func TestDecorateSeesAnthropicDefaults(t *testing.T) {
	var srvHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvHeader.Store(r.Header.Clone())
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	f := NewFactory(srv.Client(), Config{})
	resp, err := f.AnthMessageRaw(context.Background(), 1, srv.URL, "v1/messages", "sk-anth",
		[]byte(`{}`), nil, hcOf(f), func(req *http.Request) {
			require.Equal(t, "sk-anth", req.Header.Get("x-api-key"))
			require.Equal(t, defaultAnthropicVersion, req.Header.Get("Anthropic-Version"))
			req.Header.Set("Anthropic-Version", "2025-01-01")
		})
	require.NoError(t, err)
	resp.Body.Close()
	got := srvHeader.Load().(http.Header)
	require.Equal(t, "2025-01-01", got.Get("Anthropic-Version"))
	require.Equal(t, "sk-anth", got.Get("x-api-key"))
}

// TestResponsesWSDialDecorateDecorate WS 握手装饰缝：DialOptions.HTTPHeader 与
// DialOptions.Host 在网关 Authorization/Responses-Websockets 之后生效；nil 快路径
// 也不 panic（P3 所有调用点传 nil）。
func TestResponsesWSDialDecorate(t *testing.T) {
	var gotHeader atomic.Value
	var gotHost atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader.Store(r.Header.Clone())
		gotHost.Store(r.Host)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	conn, resp, err := f.ResponsesWSDial(context.Background(), 1, srv.URL, "v1/responses", "sk-ws",
		http.Header{}, hcOf(f), func(opts *websocket.DialOptions) {
			require.Equal(t, "Bearer sk-ws", opts.HTTPHeader.Get("Authorization"))
			require.Equal(t, ResponsesWSBetaHeader, opts.HTTPHeader.Get("Responses-Websockets"))
			opts.HTTPHeader.Set("X-Handshake", "decorated")
			opts.Host = "virtual.example"
		})
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	require.NotNil(t, conn)
	_ = conn.CloseNow()

	got := gotHeader.Load().(http.Header)
	require.Equal(t, "decorated", got.Get("X-Handshake"), "DialOptions.HTTPHeader 装饰生效")
	require.Equal(t, "virtual.example", gotHost.Load().(string), "DialOptions.Host 覆盖生效")

	// nil 快路径：不 panic、握手头仍是网关默认。
	conn2, _, err := f.ResponsesWSDial(context.Background(), 1, srv.URL, "v1/responses", "sk-ws", http.Header{}, hcOf(f), nil)
	require.NoError(t, err)
	require.NotNil(t, conn2)
	_ = conn2.CloseNow()
}
