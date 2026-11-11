// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// probeCapture 是一次 codex 探测请求的可观测面。
type probeCapture struct {
	url       string
	method    string
	auth      string
	accountID string
	body      []byte
}

// probeRewriteTransport 把固定 https://chatgpt.com 端点改写到 httptest 服务
// （测试 seam；生产由 internal/probe 的 origin guard 先校验再落到出口 transport）。
type probeRewriteTransport struct {
	target string
	wrap   func(io.ReadCloser) io.ReadCloser
}

func (t *probeRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	cu := *req.URL
	cu.Scheme = u.Scheme
	cu.Host = u.Host
	clone.URL = &cu
	resp, err := http.DefaultTransport.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if t.wrap != nil {
		resp.Body = t.wrap(resp.Body)
	}
	return resp, nil
}

type closeCountingBody struct {
	rc     io.ReadCloser
	closes atomic.Int64
}

func (b *closeCountingBody) Read(p []byte) (int, error) { return b.rc.Read(p) }
func (b *closeCountingBody) Close() error {
	b.closes.Add(1)
	return b.rc.Close()
}

// TestCodexProbeStatic A5：端点/头/载荷；无条目缓存（两次调用打两次上游）；
// 401 判死分类映射为 status 401（无 fatal 回写、无轮转）；200 body 恰好关闭一次。
func TestCodexProbeStatic(t *testing.T) {
	got := make(chan probeCapture, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- probeCapture{
			url:       r.URL.String(),
			method:    r.Method,
			auth:      r.Header.Get("Authorization"),
			accountID: r.Header.Get("ChatGPT-Account-ID"),
			body:      body,
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n\n"))
	}))
	defer up.Close()

	var bodies []*closeCountingBody
	tr := &probeRewriteTransport{target: up.URL, wrap: func(rc io.ReadCloser) io.ReadCloser {
		b := &closeCountingBody{rc: rc}
		bodies = append(bodies, b)
		return b
	}}
	payload := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	for range 2 {
		status, body, err := ProbeResponses(context.Background(), "pat-abc", "acct-1", tr, payload, 5*time.Second)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, string(body), "response.completed")
	}
	require.Len(t, bodies, 2, "每次探测新建临时 SDK client（无条目缓存）")
	for _, b := range bodies {
		require.Equal(t, int64(1), b.closes.Load(), "200 body 必须恰好关闭一次")
	}

	for range 2 {
		c := <-got
		require.Equal(t, "/backend-api/codex/responses", c.url)
		require.Equal(t, http.MethodPost, c.method)
		require.Equal(t, "Bearer pat-abc", c.auth)
		require.Equal(t, "acct-1", c.accountID)
		require.Contains(t, string(c.body), `"model":"m"`)
	}

	// 401 判死码：静态探测只映射为结论，不触发 fatal 回写/轮转。
	revoked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"token_invalidated","message":"revoked"}}`))
	}))
	defer revoked.Close()
	status, raw, err := ProbeResponses(context.Background(), "pat-bad", "", &probeRewriteTransport{target: revoked.URL}, payload, 5*time.Second)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Error(t, err)
	var fatal *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(err, &fatal))
	require.Contains(t, string(raw), "token_invalidated")

	// 普通 401：*HTTPError 原样保留状态与 body。
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad token"}}`))
	}))
	defer plain.Close()
	status, raw, err = ProbeResponses(context.Background(), "pat-bad", "", &probeRewriteTransport{target: plain.URL}, payload, 5*time.Second)
	require.Equal(t, http.StatusUnauthorized, status)
	var httpErr *codexsdk.HTTPError
	require.True(t, errors.As(err, &httpErr))
	require.Contains(t, string(raw), "bad token")
}

// cappingProbeTransport 模拟 production capTransport 的非 2xx 上限（≤2 MiB）。
type cappingProbeTransport struct{ target string }

func (t *cappingProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	cu := *req.URL
	cu.Scheme = u.Scheme
	cu.Host = u.Host
	clone.URL = &cu
	resp, err := http.DefaultTransport.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	resp.Body = &testCappedBody{rc: resp.Body, remaining: domain.ProbeMaxBodyBytes}
	return resp, nil
}

type testCappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *testCappedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *testCappedBody) Close() error { return b.rc.Close() }

// TestCodexProbeNon200Capped A5/A17：非 200 巨体经 transport cap 截断——SDK
// 内部 io.ReadAll 只能拿到 ≤2 MiB。
func TestCodexProbeNon200Capped(t *testing.T) {
	huge := strings.Repeat("x", domain.ProbeMaxBodyBytes+4096)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(huge))
	}))
	defer up.Close()

	status, raw, err := ProbeResponses(context.Background(), "pat", "", &cappingProbeTransport{target: up.URL}, []byte(`{"model":"m","input":[],"stream":true}`), 5*time.Second)
	require.Equal(t, http.StatusInternalServerError, status)
	var httpErr *codexsdk.HTTPError
	require.True(t, errors.As(err, &httpErr))
	require.LessOrEqual(t, len(raw), domain.ProbeMaxBodyBytes, "非 200 body 必须被 capTransport 截断到 2 MiB")
	require.Len(t, raw, domain.ProbeMaxBodyBytes)
}

// TestCodexProbeTransportError 传输失败 → status 0。
func TestCodexProbeTransportError(t *testing.T) {
	status, body, err := ProbeResponses(context.Background(), "pat", "", errTransport{}, []byte(`{"model":"m","input":[],"stream":true}`), time.Second)
	require.Equal(t, 0, status)
	require.Nil(t, body)
	require.ErrorContains(t, err, "synthetic transport failure")
}

type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("synthetic transport failure")
}
