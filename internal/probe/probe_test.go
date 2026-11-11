// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/egress"
)

func newTestProber(t *testing.T, base http.RoundTripper, timeout time.Duration) *Prober {
	t.Helper()
	hc := &http.Client{Transport: base}
	factory := aiclient.NewFactory(hc, aiclient.Config{})
	return New(factory, egress.NewPool(hc), nil, timeout, domain.ProbeMaxConcurrency)
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: %s never signaled", t.Name(), what)
	}
}

func waitProbeResult(t *testing.T, ch <-chan domain.ProbeResult) domain.ProbeResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not return in time")
		return domain.ProbeResult{}
	}
}

// headersBarrierTransport 在底层 RoundTrip 返回响应（即响应头已到达客户端）
// 时关闭 seen；body 阶段用例必须等它再取消父 ctx，避免把「请求头阶段」误当
// 「响应体阶段」而掩盖 body 读取回归。
type headersBarrierTransport struct {
	next http.RoundTripper
	once sync.Once
	seen chan struct{}
}

func newHeadersBarrier(next http.RoundTripper) *headersBarrierTransport {
	return &headersBarrierTransport{next: next, seen: make(chan struct{})}
}

func (t *headersBarrierTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if resp != nil {
		t.once.Do(func() { close(t.seen) })
	}
	return resp, err
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
		credential  string
		wantSuccess bool
		wantError   string
		wantDetail  string
	}{
		{"chat-empty-body", domain.FormatOpenAIChat, 200, "", "", false, "upstream response missing usable output", ""},
		{"chat-empty-object", domain.FormatOpenAIChat, 200, `{}`, "", false, "upstream response missing usable output", ""},
		{"chat-empty-choice", domain.FormatOpenAIChat, 200, `{"choices":[{}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-choices-object-parent", domain.FormatOpenAIChat, 200, `{"choices":{"0":{"message":{"content":"hi"}}}}`, "", false, "upstream response missing usable output", ""},
		{"chat-empty-content-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":[]}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-null-content-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":[null]}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-scalar-content-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":[1]}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-string-content-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":["hi"]}}]}`, "", true, "", ""},
		{"chat-empty-content-string", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":""}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-empty-tool-calls", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"tool_calls":[]}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-null-tool-calls", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"tool_calls":[null]}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-tool-calls-object-parent", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"tool_calls":{"0":{"id":"c1"}}}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-content", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":"hi"}}]}`, "", true, "", ""},
		{"chat-content-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"content":[{"type":"text","text":"hi"}]}}]}`, "", true, "", ""},
		{"chat-tool-calls", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"tool_calls":[{"id":"c1"}]}}]}`, "", true, "", ""},
		{"chat-reasoning", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"reasoning_content":"think"}}]}`, "", true, "", ""},
		{"chat-reasoning-array", domain.FormatOpenAIChat, 200, `{"choices":[{"message":{"reasoning_content":[]}}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-finish-reason", domain.FormatOpenAIChat, 200, `{"choices":[{"finish_reason":"stop"}]}`, "", true, "", ""},
		{"chat-finish-reason-array", domain.FormatOpenAIChat, 200, `{"choices":[{"finish_reason":[]}]}`, "", false, "upstream response missing usable output", ""},
		{"chat-error-only", domain.FormatOpenAIChat, 200, `{"error":{"message":"boom sk-other-tenant req_abc123"}}`, "", false, "upstream response missing usable output", ""},
		{"responses-empty", domain.FormatOpenAIResponses, 200, `{}`, "", false, "upstream response missing usable output", ""},
		{"responses-empty-output", domain.FormatOpenAIResponses, 200, `{"output":[]}`, "", false, "upstream response missing usable output", ""},
		{"responses-output-object-parent", domain.FormatOpenAIResponses, 200, `{"output":{"0":{"type":"message"}}}`, "", false, "upstream response missing usable output", ""},
		{"responses-output-type-array", domain.FormatOpenAIResponses, 200, `{"output":[{"type":[]}]}`, "", false, "upstream response missing usable output", ""},
		{"responses-output-type-number", domain.FormatOpenAIResponses, 200, `{"output":[{"type":1}]}`, "", false, "upstream response missing usable output", ""},
		{"responses-output-null", domain.FormatOpenAIResponses, 200, `{"output":[null]}`, "", false, "upstream response missing usable output", ""},
		{"responses-output-text-array", domain.FormatOpenAIResponses, 200, `{"output_text":[]}`, "", false, "upstream response missing usable output", ""},
		{"responses-output", domain.FormatOpenAIResponses, 200, `{"output":[{"type":"message"}]}`, "", true, "", ""},
		{"responses-output-text", domain.FormatOpenAIResponses, 200, `{"output_text":"hi"}`, "", true, "", ""},
		{"anthropic-empty", domain.FormatAnthropic, 200, `{"content":[]}`, "", false, "upstream response missing usable output", ""},
		{"anthropic-content-object-parent", domain.FormatAnthropic, 200, `{"content":{"0":{"type":"text"}}}`, "", false, "upstream response missing usable output", ""},
		{"anthropic-content-type-array", domain.FormatAnthropic, 200, `{"content":[{"type":[]}]}`, "", false, "upstream response missing usable output", ""},
		{"anthropic-content", domain.FormatAnthropic, 200, `{"content":[{"type":"text","text":"hi"}]}`, "", true, "", ""},
		{"chat-http-error", domain.FormatOpenAIChat, 400, `{"error":{"message":"bad model"}}`, "", false, "upstream rejected request", ""},
		{"chat-http-error-safe-code", domain.FormatOpenAIChat, 400, `{"error":{"code":"model_not_found","type":"invalid_request_error","message":"bad model"}}`, "", false, "upstream rejected request", "code=model_not_found"},
		{"chat-http-error-unsafe-code", domain.FormatOpenAIChat, 400, `{"error":{"code":"bad code with spaces"}}`, "", false, "upstream rejected request", ""},
		{"chat-http-401", domain.FormatOpenAIChat, 401, `{"error":{"code":"authentication_error","message":"no"}}`, "", false, "upstream rejected credentials", "code=authentication_error"},
		{"chat-http-401-short-credential", domain.FormatOpenAIChat, 401, `{"error":{"message":"no"}}`, "up", false, "upstream rejected credentials", ""},
		{"chat-http-400-short-credential-code", domain.FormatOpenAIChat, 400, `{"error":{"code":"invalid_api_key","message":"no"}}`, "api", false, "upstream rejected request", "code=invalid_api_key"},
		{"chat-http-404", domain.FormatOpenAIChat, 404, "", "", false, "upstream endpoint or model not found", ""},
		{"chat-http-429", domain.FormatOpenAIChat, 429, "", "", false, "upstream rate limited", ""},
		{"chat-http-503", domain.FormatOpenAIChat, 503, "", "", false, "upstream error", ""},
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
			if tc.credential != "" {
				spec.Credential = tc.credential
			}
			res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), spec)
			require.Equal(t, tc.status, res.StatusCode)
			require.Equal(t, tc.wantSuccess, res.Success)
			require.Equal(t, tc.wantError, res.Error)
			require.Equal(t, tc.wantDetail, res.Detail)
		})
	}

	// codex：response.output.0 必须是对象，null/标量不算可用输出。
	t.Run("codex-null-output", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[null]}}\n\n"))
		}))
		defer up.Close()
		spec := staticSpec("")
		spec.CredentialType = credential.TypeCodexPAT
		spec.Format = domain.FormatOpenAIResponses
		spec.Credential = "pat-test"
		res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
		require.False(t, res.Success)
		require.Equal(t, 200, res.StatusCode)
		require.Equal(t, "upstream response missing usable output", res.Error)
		require.Empty(t, res.Detail)
	})
}

// TestProbeDeepJSON A13：2 MiB 的 `[` 必须先被线性深度守卫拒绝，绝不进入递归
// gjson（无守卫时实测可达数百 MB 内存）；深层 SSE 事件同样被忽略。
func TestProbeDeepJSON(t *testing.T) {
	deep := bytes.Repeat([]byte("["), domain.ProbeMaxBodyBytes)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(deep)
	}))
	defer up.Close()

	begin := time.Now()
	res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), staticSpec(up.URL))
	require.False(t, res.Success)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "upstream response missing usable output", res.Error)
	require.Empty(t, res.Detail)
	require.Less(t, time.Since(begin), 2*time.Second, "深度守卫必须线性快速拒绝")

	// 超深错误体同样不得进入 gjson。
	require.Empty(t, errorCodeDetail(deep), "超深错误体在 gjson 之前被拒绝")

	// 守卫本身：恰好 128 层通过，129 层拒绝；字符串内的括号不计数。
	require.True(t, domain.ShallowJSON([]byte(strings.Repeat("[", domain.ProbeMaxJSONDepth)), domain.ProbeMaxJSONDepth))
	require.False(t, domain.ShallowJSON([]byte(strings.Repeat("[", domain.ProbeMaxJSONDepth+1)), domain.ProbeMaxJSONDepth))
	require.True(t, domain.ShallowJSON([]byte(`{"a":"[[[[[[["}`), domain.ProbeMaxJSONDepth), "字符串内容不参与深度计数")

	// 深层 SSE 事件不是终态，且被忽略而不是交给递归解析器。
	success, errMsg, detail := validateCodexSSE(append([]byte("data: "), deep...))
	require.False(t, success)
	require.Equal(t, "upstream stream failed", errMsg)
	require.Empty(t, detail)
}

// TestProbeCodexNewlineFlood A20：2 MiB 纯换行 body 经单遍扫描必须被索引消费，
// 不能产生 bytes.Split 的百万级行头分配（旧实现约 48 MiB）。
func TestProbeCodexNewlineFlood(t *testing.T) {
	flood := bytes.Repeat([]byte("\n"), domain.ProbeMaxBodyBytes)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	success, errMsg, detail := validateCodexSSE(flood)
	runtime.ReadMemStats(&after)

	require.False(t, success)
	require.Equal(t, "upstream stream failed", errMsg)
	require.Empty(t, detail)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("2 MiB newline flood parse allocated %d bytes", allocated)
	require.Less(t, allocated, uint64(8<<20), "SSE 扫描不得为每行分配切片头")
}

// TestProbeCodexNamedFailureWins A4/B4：事件名失败（error / response.failed）优先于
// data.type 的 response.completed；反向 data.type 失败也压过事件名成功。
func TestProbeCodexNamedFailureWins(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"output":[{"type":"message"}]}}`
	for _, name := range []string{"error", "response.failed"} {
		success, errMsg, detail := validateCodexSSE([]byte("event: " + name + "\ndata: " + completed + "\n\n"))
		require.False(t, success, "event: %s + data.type=response.completed 必须失败", name)
		require.Equal(t, "upstream stream failed", errMsg)
		require.Empty(t, detail)
	}

	// 反向：data.type 失败 + 事件名成功 → 失败（detail 仍只取白名单 code）。
	success, errMsg, detail := validateCodexSSE([]byte(
		"event: response.completed\ndata: {\"type\":\"response.failed\",\"error\":{\"code\":\"server_error\"}}\n\n"))
	require.False(t, success)
	require.Equal(t, "upstream stream failed", errMsg)
	require.Equal(t, "code=server_error", detail)

	// 事件名与 data.type 都是 completed 时仍是成功，不能误伤。
	success, errMsg, detail = validateCodexSSE([]byte("event: response.completed\ndata: " + completed + "\n\n"))
	require.True(t, success)
	require.Empty(t, errMsg)
	require.Empty(t, detail)
}

// TestProbeCodexFieldWhitespaceConsistent A4：终态检测与校验必须采用同一字段
// 解析规则——行首空白的 ` data:` 也是 data 行。
func TestProbeCodexFieldWhitespaceConsistent(t *testing.T) {
	// 与 sdkbridge.codexTerminalBlock 相同的合并语义：后一行 data 污染多行事件，
	// 事件非法 → 既不是终态也不能判成功。
	body := []byte("event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n" +
		" data: garbage\n\n")
	success, errMsg, detail := validateCodexSSE(body)
	require.False(t, success, "行首空白 data 必须参与合并，不能被忽略成成功")
	require.Equal(t, "upstream stream failed", errMsg)
	require.Empty(t, detail)

	// 行首空白本身不影响字段识别：整行前置空白的 event/data 仍可成功。
	body = []byte(" event: response.completed\n" +
		" data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n\n")
	success, errMsg, _ = validateCodexSSE(body)
	require.True(t, success)
	require.Empty(t, errMsg)
}

// TestProbeCodexTinyEventFlood B4：2 MiB 的 `data:0\n\n` 微事件洪泛必须失败且
// 分配 O(body)（旧实现为每个事件保留描述符/缓冲，约 16 MiB+）。
func TestProbeCodexTinyEventFlood(t *testing.T) {
	event := []byte("data:0\n\n")
	flood := bytes.Repeat(event, domain.ProbeMaxBodyBytes/len(event))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	success, errMsg, detail := validateCodexSSE(flood)
	runtime.ReadMemStats(&after)

	require.False(t, success)
	require.Equal(t, "upstream stream failed", errMsg)
	require.Empty(t, detail)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("2 MiB tiny-event flood validation allocated %d bytes", allocated)
	require.Less(t, allocated, uint64(8<<20), "微事件洪泛的分配必须是 O(body)，不是 O(events)")
}

// chunkedBody 按固定块大小交付 body：确定性构造「终态空行落在最后一次被
// room 截断的 read 内、其后仍有预读尾巴」的边界。
type chunkedBody struct {
	data   []byte
	chunk  int
	off    int
	closes atomic.Int64
}

func (b *chunkedBody) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.chunk {
		n = b.chunk
	}
	if n > len(b.data)-b.off {
		n = len(b.data) - b.off
	}
	copy(p, b.data[b.off:b.off+n])
	b.off += n
	return n, nil
}

func (b *chunkedBody) Close() error {
	b.closes.Add(1)
	return nil
}

// bodyResponseTransport 直接返回 200 响应（无网络），body 由测试控制。
type bodyResponseTransport struct{ body io.ReadCloser }

func (t *bodyResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       t.body,
	}, nil
}

// TestProbeCodexTerminalOffsetNoOverflow B4/A4：终态事件之后紧跟 >2 MiB 尾巴时，
// 只返回到终态空行为止的字节——同一次 read 的预读尾巴不得把有效终态误判成
// 2 MiB 溢出。
func TestProbeCodexTerminalOffsetNoOverflow(t *testing.T) {
	limit := int(domain.ProbeMaxBodyBytes) + 1
	chunk := 1000
	// 终态空行落在最后一次（被 room 截断的）read 内：end ∈ (2097000, 2097152]。
	end := limit/chunk*chunk + 100
	head := []byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}],\"pad\":\"")
	tail := []byte("\"}}\n\n")
	require.Less(t, len(head)+len(tail), end)
	event := make([]byte, 0, end)
	event = append(event, head...)
	event = append(event, bytes.Repeat([]byte("x"), end-len(head)-len(tail))...)
	event = append(event, tail...)
	require.Len(t, event, end)

	data := append(append([]byte{}, event...), bytes.Repeat([]byte("y"), domain.ProbeMaxBodyBytes)...)
	body := &chunkedBody{data: data, chunk: chunk}
	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	res := newTestProber(t, &bodyResponseTransport{body: body}, 5*time.Second).Probe(context.Background(), spec)
	require.True(t, res.Success, "终态后的预读尾巴不得造成 2 MiB 溢出")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Empty(t, res.Error)
	require.Empty(t, res.Detail)
	require.Equal(t, int64(1), body.closes.Load(), "body 必须恰好关闭一次")
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

// TestProbeTimeout A10：超时/取消一律 status 0 + 固定文案。测试不提前武装短
// deadline：Prober 用长超时，先等上游 started 屏障，再取消父 ctx（channel 屏障，
// 不用 sleep）；DeadlineExceeded 的固定文案由 TestClassifyTransportError 覆盖。
// 两个阶段：① 响应头未到；② 响应头已到但 body 一直不结束。
func TestProbeTimeout(t *testing.T) {
	require.Equal(t, 30*time.Second, DefaultTimeout, "单次探测硬上限必须是 30s 常量（不开配置键）")
	require.Equal(t, 4, DefaultMaxConcurrency)

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

		p := newTestProber(t, nil, 10*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan domain.ProbeResult, 1)
		go func() { result <- p.Probe(ctx, staticSpec(up.URL)) }()

		waitSignal(t, started, "upstream handler")
		cancel()
		res := waitProbeResult(t, result)
		require.False(t, res.Success)
		require.Equal(t, 0, res.StatusCode, "传输失败 status_code = 0")
		require.Equal(t, "probe canceled", res.Error)

		close(release)
		waitSignal(t, done, "upstream handler completion")
	})

	t.Run("headers-then-body-hang", func(t *testing.T) {
		release := make(chan struct{})
		done := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			close(done)
		}))
		defer up.Close()

		barrier := newHeadersBarrier(http.DefaultTransport)
		p := newTestProber(t, barrier, 10*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan domain.ProbeResult, 1)
		go func() { result <- p.Probe(ctx, staticSpec(up.URL)) }()

		waitSignal(t, barrier.seen, "client received response headers")
		cancel()
		res := waitProbeResult(t, result)
		require.False(t, res.Success)
		require.Equal(t, 0, res.StatusCode, "响应头已到但 body 读取消也必须 status 0")
		require.Equal(t, "probe canceled", res.Error)

		close(release)
		waitSignal(t, done, "upstream handler completion")
	})
}

// TestClassifyTransportError A10 纯函数：DeadlineExceeded → probe timed out，
// Canceled → probe canceled，其他 → probe request failed。
func TestClassifyTransportError(t *testing.T) {
	require.Equal(t, "probe timed out", classifyTransportError(context.DeadlineExceeded))
	require.Equal(t, "probe canceled", classifyTransportError(context.Canceled))
	require.Equal(t, "probe request failed", classifyTransportError(nil))
	require.Equal(t, "probe request failed", classifyTransportError(errors.New("boom")))

	// 普通失败只因 host/URL 文本包含 "redirect" 不得误分类为 cross-origin 阻止。
	require.Equal(t, "probe request failed", classifyTransportError(&url.Error{
		Op:  "Post",
		URL: "https://redirect.example.com/v1/chat/completions",
		Err: errors.New("dial tcp: lookup redirect.example.com: no such host"),
	}))
	require.Equal(t, "probe request failed", classifyTransportError(errors.New("redirect target unreachable")))

	// 只有 originGuard 哨兵与 malformed Location 解析失败归为 cross-origin 阻止。
	require.Equal(t, "probe blocked cross-origin redirect", classifyTransportError(fmt.Errorf("%w to https://evil.example.com", errBlockedOrigin)))
	require.Equal(t, "probe blocked cross-origin redirect", classifyTransportError(&url.Error{
		Op:  "Post",
		URL: "https://api.example.com/v1/chat/completions",
		Err: errors.New(`failed to parse Location header "http://[::1]:bad/%zz": invalid URL escape "%zz"`),
	}))
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
	var closes atomic.Int64
	tr := &countCloseTransport{next: &rewriteTransport{target: up.URL}, closes: &closes}
	res := newTestProber(t, tr, 10*time.Second).Probe(context.Background(), spec)
	require.False(t, res.Success)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "response exceeds 2 MiB", res.Error)
	require.Equal(t, int64(1), closes.Load(), "200 溢出时 body 必须恰好关闭一次")
}

// TestProbeCodexNon200Status A5/A17：SDK 对 201/204 也返回 HTTPError——探测必须
// 保留真实状态码并按 HTTP 响应分类，不得覆盖成 0 或 200 溢出文案。
func TestProbeCodexNon200Status(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"created-201", http.StatusCreated, `{"error":{"code":"invalid_api_key"}}`},
		{"no-content-204", http.StatusNoContent, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer up.Close()

			spec := staticSpec("")
			spec.CredentialType = credential.TypeCodexPAT
			spec.Format = domain.FormatOpenAIResponses
			spec.Credential = "pat-test"
			res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
			require.False(t, res.Success)
			require.Equal(t, tc.status, res.StatusCode, "非 200 的真实状态码必须原样保留")
			require.Equal(t, "upstream error", res.Error)
			if tc.status == http.StatusCreated {
				require.Equal(t, "code=invalid_api_key", res.Detail)
			} else {
				require.Empty(t, res.Detail)
			}
		})
	}
}

// TestProbeCodexNon200BodyCancel A10：非 200 响应头已到但 body 停住，SDK 吞掉
// body 读错误；取消父 ctx 必须在映射 SDK HTTPError 前生效 → status 0 + canceled。
func TestProbeCodexNon200BodyCancel(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
		close(done)
	}))
	defer up.Close()

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	// 必须等 transport 层真正收到非 200 响应头再取消，否则可能仍在请求头阶段。
	barrier := newHeadersBarrier(&rewriteTransport{target: up.URL})
	p := newTestProber(t, barrier, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan domain.ProbeResult, 1)
	go func() { result <- p.Probe(ctx, spec) }()

	waitSignal(t, barrier.seen, "client received non-200 headers")
	cancel()
	res := waitProbeResult(t, result)
	require.False(t, res.Success)
	require.Equal(t, 0, res.StatusCode, "非 200 body 读取消也必须 status 0")
	require.Equal(t, "probe canceled", res.Error)
	require.Empty(t, res.Detail)

	close(release)
	waitSignal(t, done, "upstream handler completion")
}

// TestProbeCodexNon200CappedWiring A5/A17：生产 guardedCodexTransport 走完整
// 装配（codex cap∘originGuard∘出口 transport）时，201/500 巨体经 SDK 内部
// io.ReadAll 只能拿到恰好 2 MiB（201 不享受 200 的 +1 字节），且 body 恰好关闭一次。
func TestProbeCodexNon200CappedWiring(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			huge := bytes.Repeat([]byte("x"), domain.ProbeMaxBodyBytes+4096)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write(huge)
			}))
			defer up.Close()

			spec := staticSpec("")
			spec.CredentialType = credential.TypeCodexPAT
			spec.Format = domain.FormatOpenAIResponses
			spec.Credential = "pat-test"
			var closes atomic.Int64
			p := newTestProber(t, &countCloseTransport{next: &rewriteTransport{target: up.URL}, closes: &closes}, 5*time.Second)
			transport, err := p.guardedCodexTransport(spec)
			require.NoError(t, err)
			gotStatus, body, callErr := sdkbridge.ProbeResponses(context.Background(), spec.Credential, spec.CodexAccountID, transport, sdkbridge.ProbeResponsesPayload(spec.UpstreamModel), 5*time.Second)
			require.Equal(t, status, gotStatus)
			require.Error(t, callErr)
			require.Len(t, body, domain.ProbeMaxBodyBytes, "codex 非 200（含 201）cap 必须恰好 2 MiB")
			require.Equal(t, int64(1), closes.Load(), "body 必须恰好关闭一次")
		})
	}
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

// TestProbeCodexIncompleteEvent A20：首行 data 本身是完整 completed JSON，但后续
// data 行使合并事件非法——终态只能在空行边界判定；合并后 JSON 无效 → 失败，
// 不得按首行提前判成功。
func TestProbeCodexIncompleteEvent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n" +
			"data: garbage\n\n"))
	}))
	defer up.Close()

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
	require.False(t, res.Success, "合并后的非法事件不得判成功")
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "upstream stream failed", res.Error)
	require.Empty(t, res.Detail)
}

// TestProbeCodexWhitespaceNotBoundary A20：completed 后仅一个空白行不是事件边界；
// 紧随其后的 data: garbage 使整个事件非法，探测不得在空白行提前判成功。
func TestProbeCodexWhitespaceNotBoundary(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n" +
			" \n" +
			"data: garbage\n\n"))
	}))
	defer up.Close()

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
	require.False(t, res.Success, "空白行不是事件边界，后续 garbage 必须使事件非法")
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "upstream stream failed", res.Error)
}

// TestProbeCodexErrorTerminal A20：error 事件也是终态——上游发完 error + 空行后
// 不关流，探测必须立即返回失败（status 200 + upstream stream failed）而不是超时。
func TestProbeCodexErrorTerminal(t *testing.T) {
	written := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(written)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	defer close(release)

	spec := staticSpec("")
	spec.CredentialType = credential.TypeCodexPAT
	spec.Format = domain.FormatOpenAIResponses
	spec.Credential = "pat-test"
	p := newTestProber(t, &rewriteTransport{target: up.URL}, 10*time.Second)
	result := make(chan domain.ProbeResult, 1)
	go func() { result <- p.Probe(context.Background(), spec) }()

	waitSignal(t, written, "upstream wrote error event")
	res := waitProbeResult(t, result)
	require.False(t, res.Success)
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "upstream stream failed", res.Error)
	require.Equal(t, "code=server_error", res.Detail)
}

// TestProbeCodexNamedFailureEmptyData：失败事件名即使 data 为空 / [DONE] /
// 非 JSON 也必须是失败终态，不得被后续 completed 覆盖或拖到超时。
func TestProbeCodexNamedFailureEmptyData(t *testing.T) {
	cases := []struct {
		name  string
		event string
	}{
		{"error-empty", "event: error\n\n"},
		{"error-done", "event: error\ndata: [DONE]\n\n"},
		{"error-garbage", "event: error\ndata: garbage\n\n"},
		{"failed-empty", "event: response.failed\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			written := make(chan struct{})
			release := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.event)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				close(written)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer up.Close()
			defer close(release)

			spec := staticSpec("")
			spec.CredentialType = credential.TypeCodexPAT
			spec.Format = domain.FormatOpenAIResponses
			spec.Credential = "pat-test"
			p := newTestProber(t, &rewriteTransport{target: up.URL}, 10*time.Second)
			result := make(chan domain.ProbeResult, 1)
			go func() { result <- p.Probe(context.Background(), spec) }()

			waitSignal(t, written, "upstream wrote named failure event")
			res := waitProbeResult(t, result)
			require.False(t, res.Success)
			require.Equal(t, 200, res.StatusCode)
			require.Equal(t, "upstream stream failed", res.Error)
			require.Empty(t, res.Detail)
		})
	}
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

	t.Run("static-injected-code-and-type", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"` + otherTenantKey + `","type":"` + internalReqID +
				`","message":"free-form upstream text"}}`))
		}))
		defer up.Close()

		res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), staticSpec(up.URL))
		require.False(t, res.Success)
		require.Equal(t, "upstream rejected request", res.Error)
		require.Empty(t, res.Detail, "表外 error.code/error.type 一律丢弃")
		assertClean(t, res)
	})

	t.Run("static-known-code", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key"}}`))
		}))
		defer up.Close()

		res := newTestProber(t, nil, 5*time.Second).Probe(context.Background(), staticSpec(up.URL))
		require.False(t, res.Success)
		require.Equal(t, "code=invalid_api_key", res.Detail, "表内公开码仍以 code=<value> 回显")
		assertClean(t, res)
	})

	t.Run("codex-failed-event", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`event: response.failed` + "\n" +
				`data: {"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"` +
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
		require.Equal(t, "code=rate_limit_exceeded", res.Detail)
		assertClean(t, res)
	})

	t.Run("codex-failed-injected-code", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`event: response.failed` + "\n" +
				`data: {"type":"response.failed","response":{"error":{"code":"` + otherTenantKey +
				`","type":"` + internalReqID + `","message":"free-form"}}}` + "\n\n"))
		}))
		defer up.Close()

		spec := staticSpec("")
		spec.CredentialType = credential.TypeCodexPAT
		spec.Format = domain.FormatOpenAIResponses
		spec.Credential = "pat-test"
		res := newTestProber(t, &rewriteTransport{target: up.URL}, 5*time.Second).Probe(context.Background(), spec)
		require.False(t, res.Success)
		require.Equal(t, "upstream stream failed", res.Error)
		require.Empty(t, res.Detail, "表外 error.code/error.type 一律丢弃")
		assertClean(t, res)
	})
}

// TestProbeResultTruncation：error/detail 分别按 1024/2048 字节截断。
func TestProbeResultTruncation(t *testing.T) {
	long := strings.Repeat("z", domain.ProbeDetailMaxLen+100)
	res := finalizeResult(domain.ProbeSpec{}, domain.ProbeResult{Error: long, Detail: long})
	require.Len(t, res.Error, domain.ProbeErrorMaxLen)
	require.Len(t, res.Detail, domain.ProbeDetailMaxLen)
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

// TestProbeAdmissionBounded：并发槽满后只允许有界 waiter；等待队列也满时立即
// probe busy，不再保留更多请求的内存上下文。
func TestProbeAdmissionBounded(t *testing.T) {
	p := newTestProber(t, nil, 10*time.Second)
	for i := 0; i < cap(p.sem); i++ {
		p.sem <- struct{}{}
	}
	for i := 0; i < cap(p.waiters); i++ {
		p.waiters <- struct{}{}
	}
	res := p.Probe(context.Background(), staticSpec("http://127.0.0.1:1"))
	require.False(t, res.Success)
	require.Equal(t, "probe busy", res.Error)
	require.Zero(t, res.StatusCode)

	// 队列满也不能掩盖已经取消的 ctx。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	res = p.Probe(canceled, staticSpec("http://127.0.0.1:1"))
	require.Equal(t, "probe canceled", res.Error)

	// 等待名额只在排队期间持有：请求拿到并发槽或取消后必须释放 waiter。
	p2 := newTestProber(t, nil, 10*time.Second)
	for i := 0; i < cap(p2.sem); i++ {
		p2.sem <- struct{}{}
	}
	waitCtx, waitCancel := context.WithCancel(context.Background())
	done := make(chan domain.ProbeResult, 1)
	go func() { done <- p2.Probe(waitCtx, staticSpec("http://127.0.0.1:1")) }()
	require.Eventually(t, func() bool { return len(p2.waiters) == 1 }, time.Second, time.Millisecond)
	waitCancel()
	require.Equal(t, "probe canceled", (<-done).Error)
	require.Eventually(t, func() bool { return len(p2.waiters) == 0 }, time.Second, time.Millisecond)
}
