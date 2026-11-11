// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

import (
	"bytes"
	"context"
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

// TestProbePayloadValidation A7：空 body / {} / 空 choices → success=false；
// 上游 error 键取解码后文本；正常载荷 → true。
func TestProbePayloadValidation(t *testing.T) {
	cases := []struct {
		name        string
		format      domain.RequestFormat
		status      int
		body        string
		wantSuccess bool
		wantError   string
	}{
		{"chat-empty-body", domain.FormatOpenAIChat, 200, "", false, "no usable output"},
		{"chat-empty-object", domain.FormatOpenAIChat, 200, `{}`, false, "no usable output"},
		{"chat-empty-choice", domain.FormatOpenAIChat, 200, `{"choices":[{}]}`, false, "no usable output"},
		{"chat-content", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":"hi"}}]}`, true, ""},
		{"chat-tool-calls", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"tool_calls":[{"id":"c1"}]}}]}`, true, ""},
		{"chat-reasoning", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"reasoning_content":"think"}}]}`, true, ""},
		{"chat-finish-reason", domain.FormatOpenAIChat, 200, `{"choices":[{"finish_reason":"stop"}]}`, true, ""},
		{"chat-error-only", domain.FormatOpenAIChat, 200, `{"error":{"message":"boom"}}`, false, "boom"},
		{"responses-empty", domain.FormatOpenAIResponses, 200, `{}`, false, "no usable output"},
		{"responses-output", domain.FormatOpenAIResponses, 200, `{"output":[{"type":"message"}]}`, true, ""},
		{"responses-output-text", domain.FormatOpenAIResponses, 200, `{"output_text":"hi"}`, true, ""},
		{"anthropic-empty", domain.FormatAnthropic, 200, `{"content":[]}`, false, "no usable output"},
		{"anthropic-content", domain.FormatAnthropic, 200, `{"content":[{"type":"text","text":"hi"}]}`, true, ""},
		{"chat-http-error", domain.FormatOpenAIChat, 400, `{"error":{"message":"bad model"}}`, false, "bad model"},
		{"chat-http-error-no-body", domain.FormatOpenAIChat, 503, "", false, http.StatusText(503)},
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
			if tc.wantSuccess {
				require.Empty(t, res.Error)
			} else {
				require.Contains(t, res.Error, tc.wantError)
			}
		})
	}
}

// TestProbeRedirectBlocked A11：302 不跟随——第二台 httptest 0 请求，结果失败且
// 状态码保留 302。
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
	require.Equal(t, int64(1), firstHits.Load())
	require.Equal(t, int64(0), secondHits.Load(), "重定向目标不得收到请求（凭据不跨 origin）")
}

// TestProbeTimeout A10：ctx 到期 → status 0；用 channel 屏障而非 sleep。
func TestProbeTimeout(t *testing.T) {
	require.Equal(t, 30*time.Second, DefaultTimeout, "单次探测硬上限必须是 30s 常量（不开配置键）")
	require.Equal(t, 4, DefaultMaxConcurrency)
	started := make(chan struct{})
	done := make(chan struct{})
	release := make(chan struct{})
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
	res := p.Probe(context.Background(), staticSpec(up.URL))
	require.False(t, res.Success)
	require.Equal(t, 0, res.StatusCode, "传输失败 status_code = 0")
	require.Contains(t, res.Error, "context deadline exceeded")

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler never started")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream handler did not observe cancellation")
	}
}

// TestProbeResponseTooLarge A17：静态族 200 体 >2 MiB → 失败并关闭连接。
func TestProbeResponseTooLarge(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), domain.ProbeMaxBodyBytes+1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(huge)
	}))
	defer up.Close()

	res := newTestProber(t, nil, 10*time.Second).Probe(context.Background(), staticSpec(up.URL))
	require.False(t, res.Success)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "response exceeds 2 MiB", res.Error)
	require.Contains(t, res.Detail, "exceeds 2 MiB")
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
}

// TestProbeRedaction A13：凭据与绑定代理密码的原文 / JSON 转义 / URL 编码三种
// 形式一律替换为 ***，并分别截断 error/detail。
func TestProbeRedaction(t *testing.T) {
	secrets := []string{
		`sk-a"b\c` + "\n" + `d e+f`,
		`p@ss w"rd/+=`,
	}
	for _, secret := range secrets {
		forms := []string{secret, jsonEscape(secret), url.QueryEscape(secret), url.PathEscape(secret)}
		for _, form := range forms {
			require.NotEmpty(t, form)
			out := redactText("before "+form+" after", []string{secret})
			require.NotContains(t, out, form, "形式 %q 必须被脱敏", form)
			require.Contains(t, out, "***")
		}
	}

	spec := domain.ProbeSpec{
		Credential:    `sk-a"b\c` + "\n" + `d e+f`,
		ProxyPassword: `p@ss w"rd/+=`,
	}
	rawCred := spec.Credential
	jsonCred := jsonEscape(spec.Credential)
	urlPass := url.QueryEscape(spec.ProxyPassword)
	res := finalizeResult(spec, domain.ProbeResult{
		Error:  "dial http://user:" + spec.ProxyPassword + "@proxy: " + rawCred,
		Detail: "upstream echoed " + jsonCred + " and " + urlPass,
	})
	require.NotContains(t, res.Error, spec.ProxyPassword)
	require.NotContains(t, res.Error, rawCred)
	require.NotContains(t, res.Detail, jsonCred)
	require.NotContains(t, res.Detail, urlPass)
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
