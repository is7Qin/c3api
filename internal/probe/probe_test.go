// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/egress"
)

func newTestProber(t *testing.T, base http.RoundTripper, timeout time.Duration) *Prober {
	t.Helper()
	hc := &http.Client{Transport: base}
	factory := aiclient.NewFactory(hc, aiclient.Config{})
	return New(factory, egress.NewPool(hc), nil, timeout, domain.ProbeMaxConcurrency)
}

func staticSpec(baseURL string) domain.ProbeSpec {
	return domain.ProbeSpec{
		AccountID:      7,
		TemplateID:     9,
		CredentialType: credential.TypeAPIKey,
		Format:         domain.FormatOpenAIChat,
		RequestedModel: "client-model",
		UpstreamModel:  "upstream-model",
		BaseURL:        baseURL,
		Credential:     "sk-test",
	}
}

type capturedReq struct {
	method    string
	path      string
	rawQuery  string
	host      string
	auth      string
	apiKey    string
	accountID string
	body      []byte
}

func captureChan() chan capturedReq { return make(chan capturedReq, 2) }

// TestProbeChat A2：chat 命中 {base}/v1/chat/completions、Bearer、映射后模型、
// 目标 host；成功判定与回显字段。
func TestProbeChat(t *testing.T) {
	got := captureChan()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- capturedReq{method: r.Method, path: r.URL.Path, host: r.Host, auth: r.Header.Get("Authorization"), body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer up.Close()

	p := newTestProber(t, nil, 5*time.Second)
	res := p.Probe(context.Background(), staticSpec(up.URL))
	require.True(t, res.Success)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, domain.FormatOpenAIChat, res.Format)
	require.Equal(t, "client-model", res.RequestedModel)
	require.Equal(t, "upstream-model", res.UpstreamModel)
	require.Empty(t, res.Error)
	require.Empty(t, res.Detail)
	require.GreaterOrEqual(t, res.LatencyMS, int64(0))

	c := <-got
	require.Equal(t, http.MethodPost, c.method)
	require.Equal(t, "/v1/chat/completions", c.path)
	require.Equal(t, strings.TrimPrefix(up.URL, "http://"), c.host, "出站必须是配置的 base_url host")
	require.Equal(t, "Bearer sk-test", c.auth)
	require.Equal(t, "upstream-model", gjson.GetBytes(c.body, "model").String())
	require.False(t, gjson.GetBytes(c.body, "stream").Bool())
	require.Equal(t, int64(16), gjson.GetBytes(c.body, "max_tokens").Int())
	require.Equal(t, "user", gjson.GetBytes(c.body, "messages.0.role").String())
	require.Equal(t, "hi", gjson.GetBytes(c.body, "messages.0.content").String())
}

// TestProbeResponses A3：responses 与 responses-ws 都打 HTTP responses 路径。
func TestProbeResponses(t *testing.T) {
	for _, format := range []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS} {
		t.Run(string(format), func(t *testing.T) {
			got := captureChan()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got <- capturedReq{method: r.Method, path: r.URL.Path, host: r.Host, auth: r.Header.Get("Authorization"), body: body}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`))
			}))
			defer up.Close()

			spec := staticSpec(up.URL)
			spec.Format = format
			res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), spec)
			require.True(t, res.Success)
			require.Equal(t, format, res.Format)

			c := <-got
			require.Equal(t, "/v1/responses", c.path, "responses-ws 也按 HTTP responses 探测")
			require.Equal(t, strings.TrimPrefix(up.URL, "http://"), c.host)
			require.Equal(t, "Bearer sk-test", c.auth)
			require.Equal(t, "upstream-model", gjson.GetBytes(c.body, "model").String())
			require.Equal(t, "input_text", gjson.GetBytes(c.body, "input.0.content.0.type").String())
			require.Equal(t, "hi", gjson.GetBytes(c.body, "input.0.content.0.text").String())
			require.Equal(t, int64(16), gjson.GetBytes(c.body, "max_output_tokens").Int())
			require.False(t, gjson.GetBytes(c.body, "stream").Bool())
		})
	}
}

// TestProbeAnthropic A3：messages 路径 + x-api-key 头（不发送 Authorization）。
func TestProbeAnthropic(t *testing.T) {
	got := captureChan()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- capturedReq{method: r.Method, path: r.URL.Path, host: r.Host, auth: r.Header.Get("Authorization"), apiKey: r.Header.Get("x-api-key"), body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi"}]}`))
	}))
	defer up.Close()

	spec := staticSpec(up.URL)
	spec.Format = domain.FormatAnthropic
	res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), spec)
	require.True(t, res.Success)

	c := <-got
	require.Equal(t, "/v1/messages", c.path)
	require.Equal(t, strings.TrimPrefix(up.URL, "http://"), c.host)
	require.Equal(t, "sk-test", c.apiKey)
	require.Empty(t, c.auth, "anthropic 只发 x-api-key")
	require.Equal(t, "upstream-model", gjson.GetBytes(c.body, "model").String())
	require.Equal(t, int64(16), gjson.GetBytes(c.body, "max_tokens").Int())
	require.Equal(t, "hi", gjson.GetBytes(c.body, "messages.0.content").String())
}

// TestProbeCustomPath A4：custom 的 upstream_paths 命中即原样使用；未配置回落
// 默认端点（同一 domain.ResolveUpstreamPath）。
func TestProbeCustomPath(t *testing.T) {
	got := captureChan()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- capturedReq{path: r.URL.Path, rawQuery: r.URL.RawQuery}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer up.Close()

	spec := staticSpec(up.URL)
	spec.CredentialType = credential.TypeCustom
	spec.UpstreamPaths = map[string]string{string(domain.UpstreamPathChat): "v1/custom/chat?api-version=2024-10-21"}
	res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), spec)
	require.True(t, res.Success)
	c := <-got
	require.Equal(t, "/v1/custom/chat", c.path)
	require.Equal(t, "api-version=2024-10-21", c.rawQuery)

	// ext 行缺失（service 归为 nil map）→ 回落默认端点。
	spec.UpstreamPaths = nil
	res = newTestProber(t, nil, 5*time.Second).Probe(context.Background(), spec)
	require.True(t, res.Success)
	c = <-got
	require.Equal(t, "/v1/chat/completions", c.path)
	require.Empty(t, c.rawQuery)
}

// TestProbePayloadValidation A7：空 body / {} / 空集合（空 choices、content:[]、
// tool_calls:[]、output:[]）→ success=false；正常载荷 → true。上游 message 与
// 原始 JSON 不出现在结果中，detail 只允许白名单 code。
func TestProbePayloadValidation(t *testing.T) {
	cases := []struct {
		name        string
		format      domain.RequestFormat
		status      int
		body        string
		wantSuccess bool
		wantError   string
		wantDetail  string
	}{
		{"chat-empty-body", domain.FormatOpenAIChat, 200, "", false, "upstream response missing usable output", ""},
		{"chat-empty-object", domain.FormatOpenAIChat, 200, `{}`, false, "upstream response missing usable output", ""},
		{"chat-empty-choice", domain.FormatOpenAIChat, 200, `{"choices":[{}]}`, false, "upstream response missing usable output", ""},
		{"chat-empty-content-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":[]}}]}`, false, "upstream response missing usable output", ""},
		{"chat-empty-content-string", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":""}}]}`, false, "upstream response missing usable output", ""},
		{"chat-empty-tool-calls", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"tool_calls":[]}}]}`, false, "upstream response missing usable output", ""},
		{"chat-content", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":"hi"}}]}`, true, "", ""},
		{"chat-content-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":[{"type":"text","text":"hi"}]}}]}`, true, "", ""},
		{"chat-tool-calls", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"tool_calls":[{"id":"c1"}]}}]}`, true, "", ""},
		{"chat-reasoning", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"reasoning_content":"think"}}]}`, true, "", ""},
		{"chat-finish-reason", domain.FormatOpenAIChat, 200, `{"choices":[{"finish_reason":"stop"}]}`, true, "", ""},
		{"chat-error-only", domain.FormatOpenAIChat, 200, `{"error":{"message":"boom sk-other-tenant req_abc123"}}`, false, "upstream response missing usable output", ""},
		{"responses-empty", domain.FormatOpenAIResponses, 200, `{}`, false, "upstream response missing usable output", ""},
		{"responses-empty-output", domain.FormatOpenAIResponses, 200, `{"output":[]}`, false, "upstream response missing usable output", ""},
		{"responses-output", domain.FormatOpenAIResponses, 200, `{"output":[{"type":"message"}]}`, true, "", ""},
		{"responses-output-text", domain.FormatOpenAIResponses, 200, `{"output_text":"hi"}`, true, "", ""},
		{"anthropic-empty", domain.FormatAnthropic, 200, `{"content":[]}`, false, "upstream response missing usable output", ""},
		{"anthropic-content", domain.FormatAnthropic, 200, `{"content":[{"type":"text","text":"hi"}]}`, true, "", ""},
		{"chat-http-error", domain.FormatOpenAIChat, 400, `{"error":{"message":"bad model"}}`, false, "upstream rejected request", ""},
		{"chat-http-error-safe-code", domain.FormatOpenAIChat, 400, `{"error":{"code":"model_not_found","type":"invalid_request_error","message":"bad model"}}`, false, "upstream rejected request", "code=model_not_found"},
		{"chat-http-error-unsafe-code", domain.FormatOpenAIChat, 400, `{"error":{"code":"bad code with spaces"}}`, false, "upstream rejected request", ""},
		{"chat-http-401", domain.FormatOpenAIChat, 401, `{"error":{"code":"token_invalidated","message":"no"}}`, false, "upstream rejected credentials", "code=token_invalidated"},
		{"chat-http-404", domain.FormatOpenAIChat, 404, "", false, "upstream endpoint or model not found", ""},
		{"chat-http-429", domain.FormatOpenAIChat, 429, "", false, "upstream rate limited", ""},
		{"chat-http-503", domain.FormatOpenAIChat, 503, "", false, "upstream error", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer up.Close()
			spec := staticSpec(up.URL)
			spec.Format = tc.format
			res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), spec)
			require.Equal(t, tc.status, res.StatusCode)
			require.Equal(t, tc.wantSuccess, res.Success)
			require.Equal(t, tc.wantError, res.Error)
			require.Equal(t, tc.wantDetail, res.Detail)
		})
	}
}

// TestProbeRedirectBlocked A11：302 不跟随——第二台 httptest 0 请求，结果失败且
// 状态码保留 302；malformed Location 的解析错误同样归一为固定 cross-origin 文案，
// 任何 Location/URL/host 文本都不得出现在结果中。
func TestProbeRedirectBlocked(t *testing.T) {
	var secondHits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"leak"}}]}`))
	}))
	defer second.Close()

	var firstHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		http.Redirect(w, r, second.URL+"/stolen", http.StatusFound)
	}))
	defer first.Close()

	res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), staticSpec(first.URL))
	require.False(t, res.Success)
	require.Equal(t, http.StatusFound, res.StatusCode)
	require.Equal(t, "upstream redirect rejected", res.Error)
	require.Empty(t, res.Detail)
	require.Equal(t, int64(1), firstHits.Load())
	require.Equal(t, int64(0), secondHits.Load(), "重定向目标不得收到请求（凭据不跨 origin）")
	rendered := res.Error + res.Detail
	require.NotContains(t, rendered, second.URL)
	require.NotContains(t, rendered, "/stolen")
	require.NotContains(t, rendered, first.URL)
	require.NotContains(t, rendered, strings.TrimPrefix(second.URL, "http://"))

	// malformed Location：Go 客户端在 CheckRedirect 之前解析失败，错误文本含
	// Location；结果必须只给固定分类文案（status 0），不回显任何 URL 片段。
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://[::1]:bad-location/%zz")
		w.WriteHeader(http.StatusFound)
	}))
	defer malformed.Close()
	res = newTestProber(t, nil, 5*time.Second).Probe(context.Background(), staticSpec(malformed.URL))
	require.False(t, res.Success)
	require.Equal(t, 0, res.StatusCode, "Location 解析失败 = 传输失败")
	require.Equal(t, "probe blocked cross-origin redirect", res.Error)
	require.Empty(t, res.Detail)
	require.NotContains(t, res.Error, "Location")
	require.NotContains(t, res.Error, "::1")
	require.NotContains(t, res.Error, malformed.URL)
}

// TestProbeTimeout A10：ctx 到期 → status 0 + 固定 "probe timed out"。用 channel
// 屏障（先等上游 started 再等结果，watchdog 兜底），不使用 sleep。
// 两个阶段：① 响应头未到；② 响应头已到但 body 一直不结束（读 body 超时）。
func TestProbeTimeout(t *testing.T) {
	require.Equal(t, 30*time.Second, DefaultTimeout, "单次探测硬上限必须是 30s 常量（不开配置键）")
	require.Equal(t, 4, DefaultMaxConcurrency)

	waitSignal := func(t *testing.T, ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: %s never signaled", t.Name(), what)
		}
	}
	waitResult := func(t *testing.T, ch <-chan domain.ProbeResult) domain.ProbeResult {
		t.Helper()
		select {
		case res := <-ch:
			return res
		case <-time.After(2 * time.Second):
			t.Fatal("probe did not return after timeout")
			return domain.ProbeResult{}
		}
	}

	t.Run("request-hangs-before-headers", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		var once sync.Once
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(started) })
			select {
			case <-r.Context().Done():
			case <-release:
			}
			close(done)
		}))
		defer up.Close()

		p := newTestProber(t, nil, 20*time.Millisecond)
		result := make(chan domain.ProbeResult, 1)
		go func() { result <- p.Probe(context.Background(), staticSpec(up.URL)) }()

		waitSignal(t, started, "upstream handler")
		res := waitResult(t, result)
		require.False(t, res.Success)
		require.Equal(t, 0, res.StatusCode, "传输失败 status_code = 0")
		require.Equal(t, "probe timed out", res.Error)

		close(release)
		waitSignal(t, done, "upstream handler completion")
	})

	t.Run("headers-then-body-hang", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			close(done)
		}))
		defer up.Close()

		p := newTestProber(t, nil, 50*time.Millisecond)
		result := make(chan domain.ProbeResult, 1)
		go func() { result <- p.Probe(context.Background(), staticSpec(up.URL)) }()

		waitSignal(t, started, "upstream headers")
		res := waitResult(t, result)
		require.False(t, res.Success)
		require.Equal(t, 0, res.StatusCode, "响应头已到但 body 读超时也必须 status 0")
		require.Equal(t, "probe timed out", res.Error)

		close(release)
		waitSignal(t, done, "upstream handler completion")
	})
}

// TestProbeResponseTooLarge A17：静态族 200 体 >2 MiB → 失败并关闭连接（恰好
// 一次；读取上限由生产 capTransport 承担）。
func TestProbeResponseTooLarge(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), domain.ProbeMaxBodyBytes+1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(huge)
	}))
	defer up.Close()

	var closes atomic.Int64
	tr := &countCloseTransport{next: http.DefaultTransport, closes: &closes}
	res := newTestProber(t, tr, 10*time.Second).Probe(context.Background(), staticSpec(up.URL))
	require.False(t, res.Success)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "response exceeds 2 MiB", res.Error)
	require.Empty(t, res.Detail)
	require.Equal(t, int64(1), closes.Load(), "溢出时 body 必须恰好关闭一次")
}

// TestCodexProbeStreamTooLarge A17：codex 200 体 >2 MiB → 失败（读取上限由
// sdkbridge 的 2 MiB+1 读取 + probe 判溢出一体承担）。
func TestCodexProbeStreamTooLarge(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), domain.ProbeMaxBodyBytes+1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(huge)
	}))
	defer up.Close()

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	tr := &rewriteTransport{target: up.URL}
	res := newTestProber(t, tr, 10*time.Second).Probe(context.Background(), spec)
	require.False(t, res.Success)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "response exceeds 2 MiB", res.Error)
}

// TestProbeCodexSSE codex 成功判定：response.completed + 非空 output。
func TestProbeCodexSSE(t *testing.T) {
	got := captureChan()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- capturedReq{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), accountID: r.Header.Get("ChatGPT-Account-ID"), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n\n"))
	}))
	defer up.Close()

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	spec.CodexAccountID = "acct-1"
	res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
	require.True(t, res.Success)

	c := <-got
	require.Equal(t, "/backend-api/codex/responses", c.path)
	require.Equal(t, "Bearer pat-test", c.auth)
	require.Equal(t, "upstream-model", gjson.GetBytes(c.body, "model").String())
	require.True(t, gjson.GetBytes(c.body, "stream").Bool())
	require.Equal(t, "acct-1", c.accountID)
	require.Equal(t, "user", gjson.GetBytes(c.body, "input.0.role").String())
	require.Equal(t, "hi", gjson.GetBytes(c.body, "input.0.content.0.text").String())
}

// TestProbeCodexMultilineEvent A20：response.completed 被拆成多行 data:，按事件块
// （空行分隔、多行 data 以 "\n" 合并）解析后仍必须判成功。
func TestProbeCodexMultilineEvent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\r\n" +
			"data: {\"type\":\"response.completed\",\r\n" +
			"data: \"response\":{\"output\":[{\"type\":\"message\"}]}}\r\n\r\n"))
	}))
	defer up.Close()

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
	require.True(t, res.Success)
	require.Empty(t, res.Error)
	require.Empty(t, res.Detail)
}

// TestProbeNoUpstreamTextLeak A19：上游错误体含其他租户 key、内部 request id 与
// 自由文本时，结果只出固定分类文案 + 白名单 code；注入串不得出现在任何结果字段。
func TestProbeNoUpstreamTextLeak(t *testing.T) {
	const (
		otherTenantKey = "sk-other-tenant"
		internalReqID  = "req_abc123"
	)
	injected := []string{otherTenantKey, internalReqID}

	assertClean := func(t *testing.T, res domain.ProbeResult) {
		t.Helper()
		rendered := fmt.Sprintf("%+v", res)
		for _, leak := range injected {
			require.NotContains(t, rendered, leak)
		}
	}

	t.Run("static-status-error", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"` +
				otherTenantKey + ` failed for ` + internalReqID + `: free-form upstream text"}}`))
		}))
		defer up.Close()

		res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), staticSpec(up.URL))
		require.False(t, res.Success)
		require.Equal(t, http.StatusTooManyRequests, res.StatusCode)
		require.Equal(t, "upstream rate limited", res.Error)
		require.Equal(t, "code=rate_limit_exceeded", res.Detail)
		assertClean(t, res)
	})

	t.Run("static-non-json-body", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(otherTenantKey + " internal=" + internalReqID + " free-form body"))
		}))
		defer up.Close()

		res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), staticSpec(up.URL))
		require.False(t, res.Success)
		require.Equal(t, "upstream rejected request", res.Error)
		require.Empty(t, res.Detail)
		assertClean(t, res)
	})

	t.Run("codex-failed-event", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`event: response.failed` + "\n" +
				`data: {"type":"response.failed","response":{"error":{"code":"upstream_stream_error","message":"` +
				otherTenantKey + ` ` + internalReqID + ` free-form"}}}` + "\n\n"))
		}))
		defer up.Close()

		spec := staticSpec("")
		spec.CredentialType = credential.TypeCodexPAT
		spec.Format = domain.FormatOpenAIResponses
		spec.Credential = "pat-test"
		res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
		require.False(t, res.Success)
		require.Equal(t, "upstream stream failed", res.Error)
		require.Equal(t, "code=upstream_stream_error", res.Detail)
		assertClean(t, res)
	})
}

// TestProbeRedaction A13：凭据与绑定代理密码的原文 / JSON 转义（含 `\/`）/
// QueryEscape / PathEscape / URL userinfo / 小写百分号十六进制形态一律替换为
// ***，并分别截断 error/detail。
func TestProbeRedaction(t *testing.T) {
	secrets := []string{
		`sk-a"b\c` + "\n" + `d e+f`,
		`p@ss w"rd/+=`,
		`foo/bar`,
		`a+b@c`,
	}
	for _, secret := range secrets {
		forms := redactionForms(secret)
		require.Contains(t, forms, secret)
		for _, form := range forms {
			require.NotEmpty(t, form)
			out := redactText("before "+form+" after", []string{secret})
			require.NotContains(t, out, form, "形式 %q 必须被脱敏", form)
			require.Contains(t, out, "***")
		}
	}

	spec := domain.ProbeSpec{
		Credential:    `foo/bar`,
		ProxyPassword: `a+b@c`,
	}
	jsonSlash := strings.ReplaceAll(jsonEscape(spec.Credential), "/", `\/`)
	userinfo := strings.TrimPrefix(url.UserPassword("u", spec.ProxyPassword).String(), "u:")
	lowerQuery := lowerPercentHex(url.QueryEscape(spec.ProxyPassword))
	lowerPath := lowerPercentHex(url.PathEscape(spec.Credential))
	require.Equal(t, `foo\/bar`, jsonSlash)
	require.Equal(t, "a+b%40c", userinfo)
	require.Equal(t, "a%2bb%40c", lowerQuery)
	require.Equal(t, "foo%2fbar", lowerPath)

	res := finalizeResult(spec, domain.ProbeResult{
		Error:  "json " + jsonSlash + " userinfo " + userinfo + " raw " + spec.Credential,
		Detail: "lower-query " + lowerQuery + " lower-path " + lowerPath + " proxy " + spec.ProxyPassword,
	})
	for _, form := range []string{jsonSlash, userinfo, lowerQuery, lowerPath, spec.Credential, spec.ProxyPassword} {
		require.NotContains(t, res.Error+res.Detail, form)
	}
	require.Contains(t, res.Error, "***")
	require.Contains(t, res.Detail, "***")

	long := strings.Repeat("z", domain.ProbeDetailMaxLen+100)
	res = finalizeResult(spec, domain.ProbeResult{Error: long, Detail: long})
	require.LessOrEqual(t, len(res.Error), domain.ProbeErrorMaxLen)
	require.LessOrEqual(t, len(res.Detail), domain.ProbeDetailMaxLen)
}

// TestProbeOriginGuard origin 锁定按 scheme + hostname + 规范化端口比较。
func TestProbeOriginGuard(t *testing.T) {
	pass := &stubTransport{}
	g := &originGuard{next: pass, scheme: "https", host: "api.example.com", port: "443"}
	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/responses", http.NoBody)
	require.NoError(t, err)
	_, err = g.RoundTrip(req)
	require.NoError(t, err)
	require.Equal(t, 1, pass.calls)

	for _, raw := range []string{
		"http://api.example.com/v1/responses",       // 降级协议
		"https://api.example.com:8443/v1/responses", // 换端口
		"https://other.example.com/v1/responses",    // 换 host
	} {
		req, err := http.NewRequest(http.MethodPost, raw, http.NoBody)
		require.NoError(t, err)
		_, err = g.RoundTrip(req)
		require.ErrorContains(t, err, "probe blocked request to")
	}
	require.Equal(t, 1, pass.calls, "被拒绝的请求不得触达下游 transport")
}

type stubTransport struct{ calls int }

func (s *stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
}

// countCloseTransport 包装响应体以断言关闭次数（生产 capTransport 在其上层）。
type countCloseTransport struct {
	next   http.RoundTripper
	closes *atomic.Int64
}

func (t *countCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.Body != nil {
		resp.Body = &countCloseBody{rc: resp.Body, closes: t.closes}
	}
	return resp, nil
}

type countCloseBody struct {
	rc     io.ReadCloser
	closes *atomic.Int64
}

func (b *countCloseBody) Read(p []byte) (int, error) { return b.rc.Read(p) }
func (b *countCloseBody) Close() error {
	b.closes.Add(1)
	return b.rc.Close()
}

// rewriteTransport 把 codex 固定 origin 的请求改写到 httptest 服务（测试 seam：
// 生产装配的下层是出口 transport，守卫在改写之前已经完成 origin 校验）。
type rewriteTransport struct{ target string }

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(r.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	cu := *req.URL
	cu.Scheme = u.Scheme
	cu.Host = u.Host
	clone.URL = &cu
	return http.DefaultTransport.RoundTrip(clone)
}
