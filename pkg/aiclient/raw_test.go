// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChatCompletionStreamRawHeadersAndPath(t *testing.T) {
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	// 裸根约定：base_url 不含 /v1，openai 系由 rawPost 补 /v1 后拼路径
	// （/v1/chat/completions）；若 base 带 /v1 会拼出 /v1/v1/... 404。
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "Bearer sk-test", gotAuth)
	require.Equal(t, "application/json", gotCT)
}

func TestAnthMessageStreamRawUsesXAPIKeyAndV1Path(t *testing.T) {
	var gotKey, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {}\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.AnthMessageStreamRaw(context.Background(), 1, srv.URL, "sk-anth", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "sk-anth", gotKey)
	require.Equal(t, "application/json", gotCT)
}

func TestResponseStreamRawPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ResponseStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStreamRawPreservesRequestBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"model":"gpt-4o","stream":true}`), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "gpt-4o", gotBody["model"])
	require.Equal(t, true, gotBody["stream"])
}

func TestStreamRawNon200ResponseReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestStreamRawBaseURLWithTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	// 尾斜杠：裸根 + "/" 同样被 openaiBaseURL 归一（TrimSuffix）后补 /v1
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL+"/", "sk-test", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestStreamRawBaseURLChangeConverges 评审 C1 回归：URL 缓存键含 base_url
// 快照——同模板 ID 直接改 base_url（绕过管理 API 的 DB 直改 + 周期同步下发新
// 快照）后，新流量必须立即打到新地址；旧实现键仅 templateID，缓存不失效 →
// 流量打旧上游。模拟：同一 Factory、同模板 ID，先后传两个不同 base_url。
func TestStreamRawBaseURLChangeConverges(t *testing.T) {
	var hit atomic.Value // string：记录请求打到哪个上游
	serve := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit.Store(name)
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: x\n\n"))
		}))
	}
	oldSrv := serve("old")
	defer oldSrv.Close()
	newSrv := serve("new")
	defer newSrv.Close()

	f := NewFactory(oldSrv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, oldSrv.URL, "sk-test", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "old", hit.Load(), "首访打旧上游")

	// 同模板 ID、新 base_url（DB 直改后周期同步下发的新快照）→ 必须收敛到新地址
	resp, err = f.ChatCompletionStreamRaw(context.Background(), 1, newSrv.URL, "sk-test", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "new", hit.Load(), "缓存键含 baseURL：新快照立即收敛，不得打旧上游")

	// 旧快照键仍可复用（无重建）——语义等价旧实现按快照解析
	resp, err = f.ChatCompletionStreamRaw(context.Background(), 1, oldSrv.URL, "sk-test", []byte(`{"stream":true}`), nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "old", hit.Load(), "旧快照请求仍打旧上游")
}

func TestStreamRawContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.ChatCompletionStreamRaw(ctx, 1, srv.URL, "sk-test", []byte(`{"stream":true}`), nil)
	require.Error(t, err)
}

// headerCapture 在 Transport 写请求行之前捕获网关自建的出栈头再委托真实
// transport：httptest 服务端看到的是 net/http 写出时补齐后的样子（不带 UA 的
// 请求到了服务端也会是 Go-http-client/1.1 或 /2.0，取决于 h1/ALPN h2）——
// 只有这一层能区分「网关根本没设」与「Transport 补了默认」。
type headerCapture struct {
	base http.RoundTripper
	got  atomic.Value // http.Header
}

func (h *headerCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	h.got.Store(req.Header.Clone())
	return h.base.RoundTrip(req)
}

func (h *headerCapture) headers(t *testing.T) http.Header {
	t.Helper()
	v := h.got.Load()
	require.NotNil(t, v, "上游请求未发出，出栈头无从捕获")
	return v.(http.Header)
}

// TestRawRelayClientHeadersArrive R5（raw 面）：客户端 UA 与自定义 session 头
// 原样达上游；deny 头不出栈（Accept-Encoding 是计费回归门——透传后上游回 gzip
// 裸流，usage 抽取静默拿到压缩字节）；网关账号凭据在 relay 之后写，后写覆盖赢。
func TestRawRelayClientHeadersArrive(t *testing.T) {
	var gotSrv atomic.Value // http.Header：服务端视角
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSrv.Store(r.Header.Clone())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{})
	in := http.Header{
		"User-Agent":         {"curl/8.7.1"},
		"X-Opencode-Session": {"oc-1"},
		"Accept-Encoding":    {"gzip"},
		"Authorization":      {"Bearer client-side-gateway-key"},
		"Cookie":             {"tenant=leak"},
	}
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test", []byte(`{"stream":true}`), in)
	require.NoError(t, err)
	defer resp.Body.Close()

	srvHdr := gotSrv.Load().(http.Header)
	require.Equal(t, "curl/8.7.1", srvHdr.Get("User-Agent"), "客户端 UA 如实达上游")
	require.Equal(t, "oc-1", srvHdr.Get("X-Opencode-Session"), "自定义 session 头如实达上游")

	built := capRt.headers(t)
	_, ok := built["Accept-Encoding"]
	require.False(t, ok, "Accept-Encoding 不得出现在出栈头（deny＝计费回归门）")
	_, ok = built["Cookie"]
	require.False(t, ok, "Cookie（跨租户态）不得出现在出栈头")
	require.Equal(t, "Bearer sk-test", built.Get("Authorization"),
		"入站凭据被剔后网关写账号 key（不变量 #5：网关声明后写赢）")
}

// TestRawRelayNoUserAgentWhenClientSendsNone 护栏 G1：入站无 UA ⇒ 网关自建的
// 出栈头**不含 User-Agent 键**。只断言键不存在、绝不断言字面值——服务端视角
// 看到的 Go-http-client/1.1 还是 /2.0 取决于上游是 h1 还是 ALPN h2（实测两种
// 都出现过），写死字面值的测试会随协议漂。
func TestRawRelayNoUserAgentWhenClientSendsNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test",
		[]byte(`{"stream":true}`), http.Header{"X-Opencode-Session": {"oc-1"}})
	require.NoError(t, err)
	defer resp.Body.Close()

	built := capRt.headers(t)
	_, ok := built["User-Agent"]
	require.False(t, ok, "客户端没带 UA 时网关不得凭空造出 User-Agent 键")
	require.Equal(t, []string{"oc-1"}, built["X-Opencode-Session"])
}

// TestRawRelayStripsInboundFootprint R7 raw 面（spec §10）：客户端 IP / 转发路径
// 类头不得达上游。断言分两层——网关自建出栈头（headerCapture）整键不出现，
// 上游服务端视图同样看不到（客户端确实把它们发过来了，故服务端为空只可能是剔掉）。
func TestRawRelayStripsInboundFootprint(t *testing.T) {
	var gotSrv atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSrv.Store(r.Header.Clone())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	capRt := &headerCapture{base: srv.Client().Transport}
	f := NewFactory(&http.Client{Transport: capRt}, Config{})
	in := http.Header{"X-Opencode-Session": {"oc-1"}}
	for _, k := range inboundFootprintCanonical {
		in[k] = []string{"203.0.113.9"}
	}
	resp, err := f.ChatCompletionStreamRaw(context.Background(), 1, srv.URL, "sk-test",
		[]byte(`{"stream":true}`), in)
	require.NoError(t, err)
	defer resp.Body.Close()

	srvHdr := gotSrv.Load().(http.Header)
	built := capRt.headers(t)
	for _, k := range inboundFootprintCanonical {
		_, ok := built[k]
		require.False(t, ok, "入站足迹头 %s 不得出现在出栈头（map 槽位级断言）", k)
		_, ok = srvHdr[k]
		require.False(t, ok, "入站足迹头 %s 不得达上游（服务端视图）", k)
	}
	require.Equal(t, []string{"oc-1"}, built["X-Opencode-Session"], "哨兵键仍须透传")
}
