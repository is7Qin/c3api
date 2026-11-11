// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 外部测试包：cap 证据用例需要复用 internal/probe 的生产 NewCappedTransport
// （probe 依赖 sdkbridge，因此只有 external test package 才能同时引用二者）。
package sdkbridge_test

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
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/probe"
	"github.com/is7qin/c3api/internal/sdkbridge"
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
	payload := sdkbridge.ProbeResponsesPayload("m")
	require.NotEmpty(t, payload)
	for range 2 {
		status, body, err := sdkbridge.ProbeResponses(context.Background(), "pat-abc", "acct-1", tr, payload, 5*time.Second)
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
		require.Equal(t, "m", gjson.GetBytes(c.body, "model").String())
		require.True(t, gjson.GetBytes(c.body, "stream").Bool())
		// 生产请求体必须带非空 input（只写 model/stream 会发出 "input":null）。
		require.Equal(t, "user", gjson.GetBytes(c.body, "input.0.role").String())
		require.Equal(t, "input_text", gjson.GetBytes(c.body, "input.0.content.0.type").String())
		require.Equal(t, "hi", gjson.GetBytes(c.body, "input.0.content.0.text").String())
	}

	// 401 判死码：静态探测只映射为结论，不触发 fatal 回写/轮转。
	revoked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"token_invalidated","message":"revoked"}}`))
	}))
	defer revoked.Close()
	status, raw, err := sdkbridge.ProbeResponses(context.Background(), "pat-bad", "", &probeRewriteTransport{target: revoked.URL}, payload, 5*time.Second)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Error(t, err)
	var fatal *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(err, &fatal))
	require.Contains(t, string(raw), "token_invalidated")

	// 普通 401：*HTTPError 原样保留状态与 body（原始 body 只在传输层；probe 只
	// 提取白名单 code，不回显给调用方）。
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad token"}}`))
	}))
	defer plain.Close()
	status, raw, err = sdkbridge.ProbeResponses(context.Background(), "pat-bad", "", &probeRewriteTransport{target: plain.URL}, payload, 5*time.Second)
	require.Equal(t, http.StatusUnauthorized, status)
	var httpErr *codexsdk.HTTPError
	require.True(t, errors.As(err, &httpErr))
	require.Contains(t, string(raw), "bad token")
}

// TestCodexProbeNon200Capped A5/A17：201 + >2 MiB body 经**生产
// NewCappedTransport**（不是测试自带 cap）截断——SDK 内部 io.ReadAll 只能拿到
// ≤2 MiB+1，且 body 恰好关闭一次。
func TestCodexProbeNon200Capped(t *testing.T) {
	huge := strings.Repeat("x", domain.ProbeMaxBodyBytes+4096)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(huge))
	}))
	defer up.Close()

	var wrapped *closeCountingBody
	tr := probe.NewCappedTransport(&probeRewriteTransport{target: up.URL, wrap: func(rc io.ReadCloser) io.ReadCloser {
		wrapped = &closeCountingBody{rc: rc}
		return wrapped
	}})
	status, raw, err := sdkbridge.ProbeResponses(context.Background(), "pat", "", tr, sdkbridge.ProbeResponsesPayload("m"), 5*time.Second)
	require.Equal(t, http.StatusCreated, status)
	var httpErr *codexsdk.HTTPError
	require.True(t, errors.As(err, &httpErr))
	require.LessOrEqual(t, len(raw), domain.ProbeMaxBodyBytes+1, "201 body 必须经生产 capTransport 截断到 2 MiB+1")
	require.Len(t, raw, domain.ProbeMaxBodyBytes+1)
	require.NotNil(t, wrapped, "transport 必须被生产 cap 包装")
	require.Equal(t, int64(1), wrapped.closes.Load(), "body 必须恰好关闭一次")
}

// TestCodexProbeStopsAtTerminal A20：上游发完 response.completed 后不关流——探测
// 必须立即返回而非等到超时；body 恰好关闭一次。
func TestCodexProbeStopsAtTerminal(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	defer close(release)

	var wrapped *closeCountingBody
	tr := &probeRewriteTransport{target: up.URL, wrap: func(rc io.ReadCloser) io.ReadCloser {
		wrapped = &closeCountingBody{rc: rc}
		return wrapped
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		status, body, err := sdkbridge.ProbeResponses(ctx, "pat", "acct-1", probe.NewCappedTransport(tr), sdkbridge.ProbeResponsesPayload("m"), 10*time.Second)
		done <- outcome{status: status, body: body, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler never started")
	}
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, http.StatusOK, got.status)
		require.Contains(t, string(got.body), "response.completed")
	case <-time.After(2 * time.Second):
		t.Fatal("probe waited for EOF after terminal event instead of returning")
	}
	require.NotNil(t, wrapped)
	require.Equal(t, int64(1), wrapped.closes.Load(), "body 必须恰好关闭一次")
}

// TestCodexProbeTransportError 传输失败 → status 0。
func TestCodexProbeTransportError(t *testing.T) {
	status, body, err := sdkbridge.ProbeResponses(context.Background(), "pat", "", errTransport{}, sdkbridge.ProbeResponsesPayload("m"), time.Second)
	require.Equal(t, 0, status)
	require.Nil(t, body)
	require.ErrorContains(t, err, "synthetic transport failure")
}

type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("synthetic transport failure")
}
