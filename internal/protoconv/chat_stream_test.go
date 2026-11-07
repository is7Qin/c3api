// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"bytes"
	"strings"
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
	require.EqualValues(t, 1, DropCounts()[string(domain.ProtocolConvertMessToChat)+"|resp|frame_after_terminal"])

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
	require.Zero(t, m.it+m.ot+m.cached+m.cacheCreate, "usage:null 不得被当成用量")

	// 真正的用量帧（choices:[] + usage 对象）才更新。
	_, drop := m.Map("", []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`))
	require.True(t, drop, "用量帧不产内容帧")
	require.EqualValues(t, 100, m.it)
	require.EqualValues(t, 20, m.ot)
	require.EqualValues(t, 40, m.cached)
	require.EqualValues(t, 10, m.cacheCreate)
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

// TestChatStreamErrorFrame 上游 error 帧 → 客户端拿到**失败终态**，且不再有正常完成。
//
// 两个目标各一条（spec §5.6/§5.7）：mess 发 `error`，resp 发 `response.failed`；
// 两者都必须在报错前关掉已开的内容块（mess 由 `content_block_stop` 体现），
// 且上游错误文本原样透传、不得换笼统文案。原来的 mess 单方向用例已并入这里。
func TestChatStreamErrorFrame(t *testing.T) {
	errChunk := []byte(`{"error":{"message":"boom","type":"invalid_request_error"}}`)
	for _, tc := range []struct {
		name string
		dir  domain.ProtocolConvert
		// want 必须有、恰好一次；notWant 正常完成标记（不许出现）。
		// failedStatus 非空时要求响应级 status 是该值（response.failed 的 status
		// 是 "failed"；output[] 里已完成 item 自己的 status 不受此约束）。
		want         string
		notWant      string
		failedStatus string
		// ordered 需要证明顺序的两段：前者下标必须小于后者（空 = 不检查）。
		ordered [2]string
	}{
		{
			name: "mess_target", dir: domain.ProtocolConvertMessToChat,
			want: `"type":"error"`, notWant: `"stop_reason":"end_turn"`,
			ordered: [2]string{`"type":"content_block_stop"`, `"type":"error"`},
		},
		{
			name: "resp_target", dir: domain.ProtocolConvertRespToChat,
			want: `"type":"response.failed"`, notWant: `"type":"response.completed"`,
			failedStatus: `"status":"failed"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewStreamMapper(tc.dir)
			_, _ = m.Map("", chatChunk("hi")) // 先打开一个内容块

			out, drop := m.Map("", errChunk)
			require.False(t, drop, "错误块不得被静默丢弃")
			all := string(out) + string(m.Finish())

			require.Equal(t, 1, strings.Count(all, tc.want), "失败终态恰好一次")
			require.NotContains(t, all, tc.notWant, "错误结局不得声明正常完成")
			require.Contains(t, all, "boom", "上游错误文本必须原样带给客户端")
			if tc.failedStatus != "" {
				require.Contains(t, all, tc.failedStatus, "失败终态的响应级 status 必须是 failed")
			}
			if tc.ordered[0] != "" {
				require.Less(t,
					strings.Index(all, tc.ordered[0]), strings.Index(all, tc.ordered[1]),
					"关块必须早于报错（客户端才不会停在半截块）")
			}
		})
	}
}
