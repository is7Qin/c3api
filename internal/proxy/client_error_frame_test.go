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

// decodeOpenAISSE 用 openai-go 共享 ssestream 消费 wire 字节，返回正常事件数与
// Stream.Err()（与 pkg/sserelay 的 decodeSSE 同构，跨包各自实现）。
func decodeOpenAISSE[T any](body string) (int, error) {
	st := openaisse.NewStream[T](openaisse.NewDecoder(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    httptest.NewRequest(http.MethodPost, "/v1/x", nil),
	}), nil)
	n := 0
	for st.Next() {
		n++
	}
	return n, st.Err()
}

// TestChatStreamErrorFrameConsumedByOpenAISDK MAJOR 回归（r5 / §2.3）：真实
// openai-go SDK 消费「业务帧 → 上游读取失败 → 错误帧」——上游中途 panic 断流，
// 网关补写客户端协议 error 帧，SDK 必须 Stream.Err() != nil（此前只发
// {"message":…}：SDK 当普通 chunk 解码成功 → Err()==nil → 截断被当正常结束）。
func TestChatStreamErrorFrameConsumedByOpenAISDK(t *testing.T) {
	up := fakeOpenAI(t, "abort-stream")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
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
		string(buildErrorFrame(domain.FormatOpenAIResponses, "upstream connection error"))

	n, err := decodeOpenAISSE[responses.ResponseStreamEventUnion](body)
	require.Error(t, err, "Responses SDK 必须把错误帧识别为失败")
	require.Equal(t, 1, n)
}

// TestAnthropicErrorFrameConsumedBySDK Anthropic 形态错误帧被官方 SDK 识别为失败
// （`event: error` → SDK newAPIError 分支）。
func TestAnthropicErrorFrameConsumedBySDK(t *testing.T) {
	body := string(buildErrorFrame(domain.FormatAnthropic, "upstream connection error"))
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
	require.Error(t, st.Err(), "anthropic SDK 必须把错误帧识别为失败")
	require.Zero(t, n)
}

// TestBuildErrorFrameProtocolPayload §2.3：错误帧载荷按客户端协议——
// OpenAI（Chat/Responses/Images）顶层 `error` 键；Anthropic `type:error` 信封。
func TestBuildErrorFrameProtocolPayload(t *testing.T) {
	payload := func(format domain.RequestFormat) map[string]json.RawMessage {
		t.Helper()
		frame := buildErrorFrame(format, "boom")
		require.True(t, bytes.HasPrefix(frame, []byte("event: error\ndata: ")), "帧头恒 event: error")
		require.True(t, bytes.HasSuffix(frame, []byte("\n\n")), "帧尾恒空行")
		data := bytes.TrimSuffix(frame[len("event: error\ndata: "):], []byte("\n\n"))
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &m))
		return m
	}

	for _, f := range []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses, domain.FormatOpenAIImages} {
		m := payload(f)
		require.Contains(t, m, "error", "%s：顶层 error 键（openai-go ssestream 判据）", f)
		require.NotContains(t, m, "type", "%s：OpenAI 形态无顶层 type", f)
		var inner map[string]string
		require.NoError(t, json.Unmarshal(m["error"], &inner))
		require.Equal(t, "server_error", inner["type"])
		require.Equal(t, "boom", inner["message"])
	}

	m := payload(domain.FormatAnthropic)
	var outerType string
	require.NoError(t, json.Unmarshal(m["type"], &outerType))
	require.Equal(t, "error", outerType, "Anthropic 形态顶层 type=error")
	var inner map[string]string
	require.NoError(t, json.Unmarshal(m["error"], &inner))
	require.Equal(t, "api_error", inner["type"])
	require.Equal(t, "boom", inner["message"])
}
