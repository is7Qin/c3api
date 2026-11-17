// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// spec-responses-terminal-truth F3/F4：转换车道 RespToMess（client=Responses、
// 上游 Anthropic）的终态真理——
//   - 上游干净 EOF 且没发 message_stop ⇒ Finish() 必须补 response.completed
//     （修复前 Finish 对该方向返回 nil ⇒ 客户端零终态，只看到「流提前结束」）；
//   - 上游 error 事件 ⇒ 必须产出带 type 的 response.failed 终态
//     （修复前是无 type 的裸载荷 {code,message}，客户端既不认为失败也不认为终态）。
package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// respToMessFeed 喂一段 RespToMess 流并累积客户端帧。
func respToMessFeed(t *testing.T, m *StreamMapper, pairs ...string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for i := 0; i+1 < len(pairs); i += 2 {
		frame, drop := m.Map(pairs[i], []byte(pairs[i+1]))
		require.False(t, drop, "帧应被映射（%s）", pairs[i])
		validateFrames(t, frame)
		frames = append(frames, parseSSEFrames(t, frame)...)
	}
	return frames
}

// TestRespToMessFinishEmitsResponsesTerminal F3：上游发 message_start + 文本后
// 干净 EOF（无 message_stop）⇒ 客户端最后一个事件仍是 Responses 终态。
func TestRespToMessFinishEmitsResponsesTerminal(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertRespToMess)
	frames := respToMessFeed(t, m,
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","usage":{"input_tokens":10}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
	)
	require.False(t, m.Done(), "无 message_stop ⇒ 还没有终态")
	require.Empty(t, framesOfType(frames, "response.completed"), "截断时流里本无终态")

	// 干净 EOF：caller_converted 的 sserelay.Config.Terminal seam 调的就是 Finish。
	term := m.Finish()
	require.NotEmpty(t, term, "client=Responses 的 EOF 必须有终态兜底（修复前为 nil）")
	validateFrames(t, term)
	frames = append(frames, parseSSEFrames(t, term)...)

	last := frames[len(frames)-1]
	require.Equal(t, "response.completed", last.event)
	require.Equal(t, "response.completed", frameType(last), "最后一个事件必须是 Responses 终态")
	resp, ok := last.data["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "completed", resp["status"])
	require.True(t, m.Done())
	require.Nil(t, m.Finish(), "恰好一次（m.done 去重）")
}

// TestRespToMessUpstreamErrorEmitsResponseFailed F4：上游 error 事件 ⇒ 客户端收到
// 带 type 的 response.failed（带上游 code/message），不是无 type 的裸载荷。
func TestRespToMessUpstreamErrorEmitsResponseFailed(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertRespToMess)
	respToMessFeed(t, m,
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","usage":{"input_tokens":10}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
	)

	frame, drop := m.Map("error", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"boom"}}`))
	require.False(t, drop, "上游 error 必须映射成终态帧（不得静默丢弃）")
	validateFrames(t, frame)
	frames := parseSSEFrames(t, frame)
	require.Len(t, frames, 1, "上游 error ⇒ 恰好一条客户端终态帧")
	require.Equal(t, "response.failed", frames[0].event)
	require.Equal(t, "response.failed", frameType(frames[0]), "必须是带 type 的终态帧（旧实现是无 type 裸载荷）")

	resp, ok := frames[0].data["response"].(map[string]any)
	require.True(t, ok, "response 信封必须存在（strict 客户端按它解码）")
	require.Equal(t, "failed", resp["status"])
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "overloaded_error", errObj["code"], "code 取上游 error.type")
	require.Equal(t, "boom", errObj["message"], "message 取上游 error.message")

	require.True(t, m.Done(), "error 帧即终态")
	require.Nil(t, m.Finish(), "已产 failed ⇒ EOF 不再补 completed（不得双终态）")
}
