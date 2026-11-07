// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// chatChunk 一个最小 chat chunk（无 event: 名，与真实上游一致）。
func chatChunk(content string) []byte {
	return []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"` + content + `"},"finish_reason":null}],"usage":null}`)
}

// TestChatChunkNotPassedThrough spec §8(b2) 负例：chat chunk 绝不能原样透传
// （原样透传 = 客户端收到非本协议的字节，且所有 (d′) 词表校验都落空）。
func TestChatChunkNotPassedThrough(t *testing.T) {
	for _, dir := range []domain.ProtocolConvert{
		domain.ProtocolConvertMessToChat,
		domain.ProtocolConvertRespToChat,
	} {
		m := NewStreamMapper(dir)
		got, drop := m.Map("", chatChunk("hi"))
		require.False(t, drop, "%q 首个内容 chunk 不得被丢弃", dir)
		require.False(t, bytes.Equal(got, encodeDataFrame(chatChunk("hi"))),
			"%q 不得把 chat chunk 原样透传给客户端", dir)
	}
}

// TestChatChunkFirstFrame spec §8(b2) 正向：首个 chunk 必须带本协议的起始帧。
func TestChatChunkFirstFrame(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertMessToChat)
	out, drop := m.Map("", chatChunk("hi"))
	require.False(t, drop)
	require.NotEmpty(t, out)
	require.Contains(t, string(out), `"type":"message_start"`, "mess 目标首帧必须是 message_start")

	m = NewStreamMapper(domain.ProtocolConvertRespToChat)
	out, drop = m.Map("", chatChunk("hi"))
	require.False(t, drop)
	require.NotEmpty(t, out)
	require.Contains(t, string(out), `"type":"response.created"`, "resp 目标首帧必须是 response.created")
}

// TestChatStreamTerminalExactlyOnce 终止帧恰好一次：`[DONE]` 与 EOF（Finish）
// 是同一个出口，靠 m.done 去重；`[DONE]` 之后到达的帧全部丢弃并计数。
func TestChatStreamTerminalExactlyOnce(t *testing.T) {
	ResetDropCounts()
	t.Cleanup(ResetDropCounts)

	m := NewStreamMapper(domain.ProtocolConvertMessToChat)
	_, _ = m.Map("", chatChunk("hi"))
	out, _ := m.Map("", []byte(`[DONE]`))
	require.Contains(t, string(out), `"type":"message_stop"`)
	require.True(t, m.Done(), "产出终止帧后 Done 必须为 true")
	// EOF 再补一次：必须什么都没写。
	require.Empty(t, m.Finish(), "Finish 不得重复产出终止帧")
	// 终止后到达的帧：丢弃 + 计数。
	_, drop := m.Map("", chatChunk("late"))
	require.True(t, drop)
	require.EqualValues(t, 1, DropCounts()[string(domain.ProtocolConvertMessToChat)+"|frame_after_terminal"])

	// 反向：先 EOF（无 [DONE]），再收到 [DONE] 也不得再产帧。
	m2 := NewStreamMapper(domain.ProtocolConvertRespToChat)
	_, _ = m2.Map("", chatChunk("hi"))
	fin := m2.Finish()
	require.Contains(t, string(fin), `"type":"response.completed"`)
	require.Empty(t, m2.Finish())
	got, drop := m2.Map("", []byte(`[DONE]`))
	require.True(t, drop)
	require.Empty(t, got)
}

// TestChatUsageNullIsNotUsageFrame spec §5.5 第 3 条（F17）：开了 include_usage 后
// 每一帧都带 `"usage":null` —— 键存在、值为 null，**不是**用量帧。
func TestChatUsageNullIsNotUsageFrame(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertMessToChat)
	_, _ = m.Map("", chatChunk("hi"))
	it, ot, cached, write := m.chatStreamUsage()
	require.Zero(t, it+ot+cached+write, "usage:null 不得被当成用量")

	// 真正的用量帧（choices:[] + usage 对象）才更新。
	_, drop := m.Map("", []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`))
	require.True(t, drop, "用量帧不产内容帧")
	it, ot, cached, write = m.chatStreamUsage()
	require.EqualValues(t, 100, it)
	require.EqualValues(t, 20, ot)
	require.EqualValues(t, 40, cached)
	require.EqualValues(t, 10, write)
}

// TestChatFinishReasonDoesNotTerminate oracle 的阻断项：见到 finish_reason 就收尾
// 会把紧随其后的 usage 末帧丢掉，客户端终态用量变 0。
func TestChatFinishReasonDoesNotTerminate(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertMessToChat)
	_, _ = m.Map("", chatChunk("hi"))
	out, drop := m.Map("", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`))
	require.True(t, drop, "finish_reason 帧只记录停因，不产帧")
	require.False(t, m.Done(), "finish_reason 不得置 done")
	require.Empty(t, out)
	_, _ = m.Map("", []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":30,"completion_tokens":5}}`))
	term, _ := m.Map("", []byte(`[DONE]`))
	require.Contains(t, string(term), `"output_tokens":5`, "终止帧必须带上末帧的真实用量")
	require.Contains(t, string(term), `"stop_reason":"end_turn"`)
}

// TestChatStreamErrorFrame 上游 error 帧 → 先关块再报错（客户端才不会停在半截块）。
func TestChatStreamErrorFrame(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertMessToChat)
	_, _ = m.Map("", chatChunk("hi"))
	out, drop := m.Map("", []byte(`{"error":{"message":"boom","type":"invalid_request_error"}}`))
	require.False(t, drop)
	s := string(out)
	require.Contains(t, s, `"type":"content_block_stop"`, "报错前必须关块")
	require.Contains(t, s, `"type":"error"`)
	require.Contains(t, s, "boom")
	require.True(t, m.Done())
}
