// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
	"github.com/openai/openai-go/responses"
)

// decodeSSE 用 openai-go 共享 ssestream 上层消费 wire 字节，返回事件数与 Err()。
func decodeSSE[T any](body string) (int, error) {
	st := ssestream.NewStream[T](ssestream.NewDecoder(&http.Response{
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

// gatewayWire 经 Output 产出「保活注释帧 + 业务帧」的精确 wire 字节。
func gatewayWire(t *testing.T, frames ...string) string {
	t.Helper()
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{})
	_ = o.Commit()
	_ = o.Heartbeat()
	for _, f := range frames {
		_, err := o.WriteFrame([]byte(f))
		require.NoError(t, err)
	}
	require.NoError(t, o.DrainFlush())
	o.Release()
	return sw.String()
}

// TestGatewayKeepaliveShapeOpenAISDKAnchor 上层消费锚：网关保活恒 `: keepalive\n`
// （单换行）——openai-go Chat/Responses/Images 的 Stream.Next()/Err() 无错；
// 反例 `\n\n` 形态注释派发空事件 → 上层 JSON 解码失败（Err()!=nil）。
func TestGatewayKeepaliveShapeOpenAISDKAnchor(t *testing.T) {
	t.Run("chat single-newline keepalive parses", func(t *testing.T) {
		body := gatewayWire(t,
			`data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n",
			"data: [DONE]\n\n",
		)
		require.True(t, strings.HasPrefix(body, ": keepalive\n"), "网关保活恒单换行")
		n, err := decodeSSE[openai.ChatCompletionChunk](body)
		require.NoError(t, err, "单换行注释帧不得使上层 Err() 报错")
		require.Equal(t, 1, n, "keepalive 注释被忽略，仅一个 chat chunk")
	})
	t.Run("responses single-newline keepalive parses", func(t *testing.T) {
		body := gatewayWire(t,
			`data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n",
			`data: {"type":"response.completed","response":{"id":"r"}}`+"\n\n",
		)
		n, err := decodeSSE[responses.ResponseStreamEventUnion](body)
		require.NoError(t, err)
		require.Equal(t, 2, n)
	})
	t.Run("images single-newline keepalive parses", func(t *testing.T) {
		body := gatewayWire(t,
			`data: {"type":"image_generation.completed","b64_json":"aGk="}`+"\n\n",
		)
		n, err := decodeSSE[openai.ImageGenStreamEventUnion](body)
		require.NoError(t, err)
		require.Equal(t, 1, n)
	})
	t.Run("blank-line comment rejected by SDK", func(t *testing.T) {
		// 反例：`\n\n` 形态注释（旧 : ping\n\n）派发空事件 → 上层 JSON 解码失败。
		body := ": keepalive\n\n" + "data: [DONE]\n\n"
		_, err := decodeSSE[openai.ChatCompletionChunk](body)
		require.Error(t, err, "blank-line 注释必须被上层 Err() 报错（形态回归锚）")
	})
}
