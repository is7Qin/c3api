// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// 客户端错误帧协议形状 + 真实 SDK 消费回归（§2.3）：网关已提交流在中途上游读取
// 失败时补写的 error 帧，必须被真实客户端 SDK 识别为「流失败」——openai-go
// `ssestream` 判据为 data 顶层 `error` 键（`packages/ssestream/ssestream.go:169/181`），
// Anthropic SDK 判据为 `event: error` + `type:error` 信封。

package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicstream "github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/openai/openai-go"
	openaisse "github.com/openai/openai-go/packages/ssestream"
	"github.com/openai/openai-go/responses"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// decodeOpenAIEvents 用 openai-go 共享 ssestream 消费 wire 字节，返回逐事件对象
// 与 Stream.Err()（与 pkg/sserelay 的 decodeSSE 同构，跨包各自实现）。
func decodeOpenAIEvents[T any](body string) ([]T, error) {
	st := openaisse.NewStream[T](openaisse.NewDecoder(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    httptest.NewRequest(http.MethodPost, "/v1/x", nil),
	}), nil)
	var out []T
	for st.Next() {
		out = append(out, st.Current())
	}
	return out, st.Err()
}

// decodeOpenAISSE 只关心「正常事件数 + 流错误」的薄封装。
func decodeOpenAISSE[T any](body string) (int, error) {
	events, err := decodeOpenAIEvents[T](body)
	return len(events), err
}

// TestChatStreamErrorFrameConsumedByOpenAISDK §2.3：真实
// openai-go SDK 消费「业务帧 → 上游读取失败 → 错误帧」——下游接受首帧后上游截断，
// 网关补写客户端协议 error 帧，SDK 必须 Stream.Err() != nil（此前只发
// {"message":…}：SDK 当普通 chunk 解码成功 → Err()==nil → 截断被当正常结束）。
func TestChatStreamErrorFrameConsumedByOpenAISDK(t *testing.T) {
	up, rec := fakeAcceptedAbortOpenAI(t)
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")

	p.HandleChat(rec, req)

	body := rec.Body.String()
	require.Contains(t, body, "event: error\ndata: ", "已提交流中止必须补写 error 帧")
	// 载荷按 OpenAI 协议：顶层 `error` 键 + type=server_error。
	require.Contains(t, body, `"error":{"message":`, "顶层 error 键（SDK 判据）")
	require.Contains(t, body, `"type":"server_error"`)

	// 真实 SDK 消费：错误帧被识别为流失败，仅业务帧产出正常事件。
	n, err := decodeOpenAISSE[openai.ChatCompletionChunk](body)
	require.Error(t, err, "openai-go SDK 必须把错误帧识别为失败（Stream.Err()!=nil）")
	require.Equal(t, 1, n, "仅业务帧被消费；错误帧不产出正常事件")
}

// TestResponsesErrorFrameConsumedByOpenAISDK 同一 OpenAI 形态错误帧也被 Responses
// SDK 解码器识别为失败（Chat/Responses 共享 ssestream 判据）。
func TestResponsesErrorFrameConsumedByOpenAISDK(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		string(buildErrorFrame(domain.FormatOpenAIResponses, "upstream connection error", true))

	n, err := decodeOpenAISSE[responses.ResponseStreamEventUnion](body)
	require.Error(t, err, "Responses SDK 必须把错误帧识别为失败")
	require.Equal(t, 1, n)
}

// TestResponsesFailedFrameDecodedByOpenAISDK spec-responses-terminal-truth §4-14：
// D2 追加的第二帧必须能被 openai-go 的**严格**解码器解出——字段缺失/类型不符会
// 让客户端在「流失败」之上再炸一次解码错误，用户看到的就不是真原因了。
func TestResponsesFailedFrameDecodedByOpenAISDK(t *testing.T) {
	frame := buildErrorFrame(domain.FormatOpenAIResponses, "boom", true)
	idx := bytes.Index(frame, []byte("event: response.failed"))
	require.Greater(t, idx, 0, "Responses 错误帧必须带 response.failed 终态帧")

	events, err := decodeOpenAIEvents[responses.ResponseStreamEventUnion](string(frame[idx:]))
	require.NoError(t, err, "openai-go 必须能解码 response.failed 合成帧")
	require.Len(t, events, 1)
	require.Equal(t, "response.failed", events[0].Type)
}

// TestAnthropicErrorFrameConsumedBySDK §2.3：真实 anthropic SDK 消费
// 「业务帧 → 上游读取失败 → caller 补错误帧」完整链路——上游中途 panic 断流，
// 网关按 Anthropic 协议补写 `event: error` + `type:error` 信封，SDK 必须
// Stream.Err() != nil（`event: error` → SDK newAPIError 分支），且错误帧前的
// 业务帧仍被正常消费（非空流被识别为中途失败，而非正常结束）。
func TestAnthropicErrorFrameConsumedBySDK(t *testing.T) {
	up := fakeAnthropic(t, "abort-stream")
	defer up.Close()
	p := newTestProxyFormat(t, up.URL, domain.FormatAnthropic)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleAnthropic(rec, req)

	body := rec.Body.String()
	require.Contains(t, body, "event: error\ndata: ", "已提交流中止必须补写 error 帧")
	require.Contains(t, body, `"type":"error"`, "Anthropic 形态顶层 type=error")
	require.Contains(t, body, `"type":"api_error"`, "内层 error.type=api_error")

	// 真实 SDK 消费：业务帧产出正常事件，错误帧被识别为流失败。
	st := anthropicstream.NewStream[anthropic.MessageStreamEventUnion](
		anthropicstream.NewDecoder(&http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    httptest.NewRequest(http.MethodPost, "/v1/messages", nil),
		}), nil)
	n := 0
	for st.Next() {
		n++
	}
	require.Error(t, st.Err(), "anthropic SDK 必须把错误帧识别为失败（Stream.Err()!=nil）")
	require.GreaterOrEqual(t, n, 1, "错误帧前的业务帧被正常消费（非空流）")
}

// TestBuildErrorFrameProtocolPayload §2.3 + spec-responses-terminal-truth §3.4：
// 错误帧载荷按客户端协议——
//   - OpenAI（Chat/Images）：单帧，顶层 `error` 键；
//   - Anthropic：单帧，`type:error` 信封；
//   - Responses：**两帧**——error 在前（openai-go 首帧即失败）+ response.failed
//     终态在后（pi-ai 只认 completed/failed/incomplete）。
func TestBuildErrorFrameProtocolPayload(t *testing.T) {
	// single 解析「单帧」形态：帧头恒 event: error、载荷按协议。
	single := func(format domain.RequestFormat) map[string]json.RawMessage {
		t.Helper()
		frame := buildErrorFrame(format, "boom", true)
		require.True(t, bytes.HasPrefix(frame, []byte("event: error\ndata: ")), "帧头恒 event: error")
		require.True(t, bytes.HasSuffix(frame, []byte("\n\n")), "帧尾恒空行")
		require.Equal(t, 1, bytes.Count(frame, []byte("event: ")), "%s：不得出现第二条终态帧", format)
		data := bytes.TrimSuffix(frame[len("event: error\ndata: "):], []byte("\n\n"))
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &m))
		return m
	}

	for _, f := range []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIImages} {
		m := single(f)
		require.Contains(t, m, "error", "%s：顶层 error 键（openai-go ssestream 判据）", f)
		require.NotContains(t, m, "type", "%s：OpenAI 形态无顶层 type", f)
		var inner map[string]string
		require.NoError(t, json.Unmarshal(m["error"], &inner))
		require.Equal(t, "server_error", inner["type"])
		require.Equal(t, "boom", inner["message"])
	}

	m := single(domain.FormatAnthropic)
	var outerType string
	require.NoError(t, json.Unmarshal(m["type"], &outerType))
	require.Equal(t, "error", outerType, "Anthropic 形态顶层 type=error")
	var inner map[string]string
	require.NoError(t, json.Unmarshal(m["error"], &inner))
	require.Equal(t, "api_error", inner["type"])
	require.Equal(t, "boom", inner["message"])

	// Responses：两帧、顺序固定（error 在前）。
	frame := buildErrorFrame(domain.FormatOpenAIResponses, "boom", true)
	frames := strings.Split(strings.TrimSuffix(string(frame), "\n\n"), "\n\n")
	require.Len(t, frames, 2, "Responses = error 帧 + response.failed 帧")
	require.True(t, strings.HasPrefix(frames[0], "event: error\ndata: "), "error 帧在前")
	require.True(t, strings.HasPrefix(frames[1], "event: response.failed\ndata: "), "response.failed 帧在后")

	var failed struct {
		Type           string `json:"type"`
		SequenceNumber int    `json:"sequence_number"`
		Response       struct {
			ID        string            `json:"id"`
			Object    string            `json:"object"`
			CreatedAt int64             `json:"created_at"`
			Status    string            `json:"status"`
			Model     string            `json:"model"`
			Output    []json.RawMessage `json:"output"`
			Error     struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(frames[1], "event: response.failed\ndata: ")), &failed))
	require.Equal(t, "response.failed", failed.Type)
	require.Equal(t, 0, failed.SequenceNumber, "sequence_number 是 openai-go 的 required")
	require.Equal(t, "failed", failed.Response.Status)
	require.Equal(t, "response", failed.Response.Object)
	require.NotZero(t, failed.Response.CreatedAt, "created_at 是 required")
	require.NotNil(t, failed.Response.Output, "output 必须是 []（null 会炸严格解码器）")
	require.Equal(t, "upstream_error", failed.Response.Error.Code)
	require.Equal(t, "boom", failed.Response.Error.Message, "两条帧携带同一 message")

	// 已见客户端终态时**不追加**（否则一条流两个终态）：只留 error 帧。
	noTerminal := buildErrorFrame(domain.FormatOpenAIResponses, "boom", false)
	require.Equal(t, 1, bytes.Count(noTerminal, []byte("event: ")), "已见终态 ⇒ 只写 error 帧")
	require.NotContains(t, string(noTerminal), "response.failed")
}
