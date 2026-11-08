// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// 流式工具调用**归并**（I5）的独立用例：`chat_stream.go` 的 chatToolCall /
// chatRespItems 只在被"分片错序"喂到时才暴露问题，golden 语料（一片一个工具、
// 顺序规整）打不到。本文件不复用 golden，直接构造帧序列 —— 改坏归并逻辑
// （不按 id 扫块列表命中、用 delta 里的数组下标当主键、块列表顺序不再等于分配序、
// 迟到 id/name 不写回）都必须让这里变红。
//
// 帧收集方式：`StreamMapper.Map` 每喂一个 chat chunk 产出**零个或多个**客户端帧
// （drop=true 时无字节），流末由 `Finish()` 补终止帧；把字节按 `\n\n` 拆开即可
// 拿到逐帧的 event 名与 data 载荷。

// chatStreamFrame 一帧客户端 SSE 的解析结果。
type chatStreamFrame struct {
	event string       // event: 行的名字
	data  gjson.Result // data: 行的 JSON 载荷
}

// chatStreamRun 喂一串 chat chunk（可选再喂 `[DONE]`），最后跑一次 EOF 收尾，
// 返回按到达顺序拆开的客户端帧。
func chatStreamRun(t *testing.T, dir domain.ProtocolConvert, done bool, chunks ...string) []chatStreamFrame {
	t.Helper()
	m := NewStreamMapper(dir)
	var raw []byte
	for i, c := range chunks {
		frame, drop := m.Map("", []byte(c))
		if drop {
			continue
		}
		require.NotEmpty(t, frame, "第 %d 个 chunk 未丢弃却没有产出字节", i)
		raw = append(raw, frame...)
	}
	if done {
		frame, drop := m.Map("", []byte("[DONE]"))
		require.False(t, drop, "`[DONE]` 不得被丢弃")
		raw = append(raw, frame...)
	}
	raw = append(raw, m.Finish()...)
	return splitChatStreamFrames(t, raw)
}

// splitChatStreamFrames 把 mapper 输出字节拆成帧；每帧必须带 event: 名与合法
// JSON 的 data: 载荷（`[DONE]` 不算 JSON：本文件的四个方向都不会产出它）。
//
// 切帧委托 parseSSEFrames（reasoning_stream_test.go：按 "\n\n" 分块、取 event:/data:
// 行、要求载荷是 JSON 对象），这里不重复实现；唯一的差别是载荷表示——本文件要
// `f.data.Get("…")` 的路径断言，故把每帧 raw 再包一层 gjson.Result。
func splitChatStreamFrames(t *testing.T, raw []byte) []chatStreamFrame {
	t.Helper()
	require.NotEmpty(t, raw, "帧序列不得为空")
	frames := parseSSEFrames(t, raw)
	out := make([]chatStreamFrame, 0, len(frames))
	for _, fr := range frames {
		require.NotEmpty(t, fr.event, "每帧都必须带 event: 名（载荷 %q）", fr.raw)
		f := chatStreamFrame{event: fr.event, data: gjson.Parse(fr.raw)}
		require.True(t, f.data.Exists(), "每帧都必须带合法 JSON 的 data: 载荷（%q）", fr.raw)
		out = append(out, f)
	}
	return out
}

// terminalResponse 取终态 `response.completed` 的 response 对象（恰好一帧）。
func terminalResponse(t *testing.T, frames []chatStreamFrame) gjson.Result {
	t.Helper()
	var got []gjson.Result
	for _, f := range frames {
		if f.event != "response.completed" {
			continue
		}
		require.Equal(t, "response.completed", f.data.Get("type").Str,
			"event 名与载荷 type 必须同值")
		got = append(got, f.data.Get("response"))
	}
	require.Len(t, got, 1, "response.completed 必须恰好一帧")
	return got[0]
}

// chatToolFrameSeq 两工具分片交错到达的 chat chunk 序列（I5 核心形态）：
//
//	chunk1: tool_calls[0] 首片 —— index:0、id:call_a、name:f、args `{"a"`
//	chunk2: tool_calls[0] 首片 —— index:1、id:call_b、name:g、args `{}`
//	chunk3: tool_calls[0] 续片 —— index:0、**无 id/name**、args `:1}`
//	chunk4: 末帧 —— finish_reason:tool_calls + usage
//
// 关键点：chunk3 的 delta 数组下标又是 0（每帧的 tool_calls 数组都从 0 开始），
// 只有把 `tool_calls[i].index` 当主键、`id` 当别名，才能把第三片接回 call_a。
func chatToolFrameSeq() []string {
	return []string{
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"a\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"g","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":null}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	}
}

// TestChatStreamToolInterleavedFragments：两个工具的分片交错到达 ⇒ 终态 output
// 必须按**首片到达序**（call_a, call_b）列出两项，参数各按片拼接，重复 20 次都
// 不依赖 map 迭代顺序。
func TestChatStreamToolInterleavedFragments(t *testing.T) {
	chunks := chatToolFrameSeq()

	for round := 0; round < 20; round++ {
		frames := chatStreamRun(t, domain.ProtocolConvertRespToChat, true, chunks...)

		var added []string
		for _, f := range frames {
			if f.event == "response.output_item.added" {
				added = append(added, f.data.Get("item.call_id").Str)
			}
		}
		require.Equal(t, []string{"call_a", "call_b"}, added,
			"第 %d 次：output_item.added 的顺序必须是首片到达序", round)

		resp := terminalResponse(t, frames)
		require.Equal(t, "completed", resp.Get("status").Str)
		require.Equal(t, "c", resp.Get("id").Str)

		require.Equal(t, int64(2), resp.Get("output.#").Int(), "终态必须恰好两项")
		require.Equal(t, "function_call", resp.Get("output.0.type").Str)
		require.Equal(t, "call_a", resp.Get("output.0.call_id").Str)
		require.Equal(t, "fc_call_a", resp.Get("output.0.id").Str)
		require.Equal(t, "f", resp.Get("output.0.name").Str)
		require.Equal(t, `{"a":1}`, resp.Get("output.0.arguments").Str)
		require.Equal(t, "function_call", resp.Get("output.1.type").Str)
		require.Equal(t, "call_b", resp.Get("output.1.call_id").Str)
		require.Equal(t, "fc_call_b", resp.Get("output.1.id").Str)
		require.Equal(t, "g", resp.Get("output.1.name").Str)
		require.Equal(t, `{}`, resp.Get("output.1.arguments").Str)

		// finish_reason:tool_calls 不得被翻译成 unfinished/incomplete；末帧用量必须吸收。
		require.Equal(t, int64(10), resp.Get("usage.input_tokens").Int())
		require.Equal(t, int64(5), resp.Get("usage.output_tokens").Int())
		require.Equal(t, int64(15), resp.Get("usage.total_tokens").Int())
	}
}

// TestChatStreamToolSameIDNewIndexKeepsOneItem：同一 id 的分片中途把 index 从 0
// 改报成 1 ⇒ 不得开新块（否则一个应答被切成两个 function_call）。
func TestChatStreamToolSameIDNewIndexKeepsOneItem(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertRespToChat, true,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"a\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_a","function":{"arguments":"}"}}]},"finish_reason":null}]}`,
	)

	var addedCallA int
	var deltaIndexes []int64
	for _, f := range frames {
		switch f.event {
		case "response.output_item.added":
			if f.data.Get("item.call_id").Str == "call_a" {
				addedCallA++
			}
		case "response.function_call_arguments.delta":
			deltaIndexes = append(deltaIndexes, f.data.Get("output_index").Int())
		}
	}
	require.Equal(t, 1, addedCallA, "改 index 的分片不得再发一个 call_a 的 output_item.added")
	require.Equal(t, []int64{0, 0}, deltaIndexes,
		"两片参数必须落在同一个 output_index（第二片不得新开块）")

	resp := terminalResponse(t, frames)
	require.Equal(t, int64(1), resp.Get("output.#").Int(), "终态只能有一个 function_call")
	require.Equal(t, "function_call", resp.Get("output.0.type").Str)
	require.Equal(t, "call_a", resp.Get("output.0.call_id").Str)
	require.Equal(t, `{"a"}`, resp.Get("output.0.arguments").Str,
		"arguments 必须是两片拼接后的完整值")
}

// TestChatStreamToolLateIDAndNameBackfill：首片没有 id/name（迟到），后续分片才
// 补 ⇒ 终态 item 的 call_id / name 必须写回，不能是空串；item id 与 call_id 同源。
func TestChatStreamToolLateIDAndNameBackfill(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertRespToChat, true,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"name":"h","arguments":":1}"}}]},"finish_reason":null}]}`,
	)

	resp := terminalResponse(t, frames)
	require.Equal(t, int64(1), resp.Get("output.#").Int())
	require.Equal(t, "call_late", resp.Get("output.0.call_id").Str,
		"迟到的 id 必须写回 item 的 call_id（不得是空串）")
	require.Equal(t, "h", resp.Get("output.0.name").Str,
		"迟到的 name 必须写回 item 的 name（不得是空串）")
	require.Equal(t, "fc_call_late", resp.Get("output.0.id").Str,
		"item id 必须是 fc_ + call_id 形态")
	require.Equal(t, "fc_"+resp.Get("output.0.call_id").Str, resp.Get("output.0.id").Str,
		"item id 与 call_id 必须同源")
	require.Equal(t, `{"k":1}`, resp.Get("output.0.arguments").Str)
}

// TestChatStreamToolIndexRewriteAliasesLaterIDLessFragments：id 别名 + index 表
// 必须**双向**对齐——改了 index 之后，同一新 index 上再来的**无 id** 分片也要落回
// 原块（只写别名表、不把新 index 登记到块上的实现会在这里切出第二个空 id 项）。
func TestChatStreamToolIndexRewriteAliasesLaterIDLessFragments(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertRespToChat, true,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"a\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_a","function":{"arguments":":1"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]},"finish_reason":null}]}`,
	)

	var added int
	for _, f := range frames {
		if f.event == "response.output_item.added" {
			added++
		}
	}
	require.Equal(t, 1, added, "整串分片只允许开一个块")

	resp := terminalResponse(t, frames)
	require.Equal(t, int64(1), resp.Get("output.#").Int(), "终态只能有一项")
	require.Equal(t, "call_a", resp.Get("output.0.call_id").Str)
	require.Equal(t, "f", resp.Get("output.0.name").Str)
	require.Equal(t, `{"a":1}`, resp.Get("output.0.arguments").Str,
		"三片参数必须按到达序拼到一个块上")
}

// TestChatStreamToolMergedBlocksOnMessTarget：同一串交错帧喂 `mess_to_chat`
// 方向 ⇒ content_block_start 的 tool_use 块 id/name 与逐片 input_json_delta
// 拼接结果也正确（证明归并不是 resp 目标专属）。
func TestChatStreamToolMergedBlocksOnMessTarget(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertMessToChat, true, chatToolFrameSeq()...)

	type toolBlock struct {
		id, name string
		args     strings.Builder
	}
	blocks := map[int64]*toolBlock{}
	var order []int64
	var stops []int64
	var delta stopInfo

	for _, f := range frames {
		switch f.event {
		case "content_block_start":
			require.Equal(t, "tool_use", f.data.Get("content_block.type").Str,
				"本流只有工具块，start 必须是 tool_use")
			idx := f.data.Get("index").Int()
			blocks[idx] = &toolBlock{
				id:   f.data.Get("content_block.id").Str,
				name: f.data.Get("content_block.name").Str,
			}
			order = append(order, idx)
		case "content_block_delta":
			require.Equal(t, "input_json_delta", f.data.Get("delta.type").Str)
			idx := f.data.Get("index").Int()
			b, ok := blocks[idx]
			require.True(t, ok, "delta 落到了没有 start 的 index %d", idx)
			b.args.WriteString(f.data.Get("delta.partial_json").Str)
		case "content_block_stop":
			stops = append(stops, f.data.Get("index").Int())
		case "message_delta":
			delta.stopReason = f.data.Get("delta.stop_reason").Str
			delta.stopSeq = f.data.Get("delta.stop_sequence")
			delta.input = f.data.Get("usage.input_tokens").Int()
			delta.output = f.data.Get("usage.output_tokens").Int()
			delta.cacheRead = f.data.Get("usage.cache_read_input_tokens").Int()
			delta.cacheWrite = f.data.Get("usage.cache_creation_input_tokens").Int()
		}
	}

	require.Equal(t, []int64{0, 1}, order, "内容 index 按期（不是按 delta 数组下标）升序分配")
	require.Len(t, blocks, 2, "两个工具 = 两个 tool_use 块（不得切碎）")

	require.Equal(t, "call_a", blocks[0].id)
	require.Equal(t, "f", blocks[0].name)
	require.Equal(t, `{"a":1}`, blocks[0].args.String(), "index 0 的参数必须是两片拼接")
	require.Equal(t, "call_b", blocks[1].id)
	require.Equal(t, "g", blocks[1].name)
	require.Equal(t, `{}`, blocks[1].args.String())

	require.Equal(t, []int64{0, 1}, stops, "两个块都必须关闭")

	require.Equal(t, "tool_use", delta.stopReason, "finish_reason:tool_calls → tool_use")
	require.True(t, delta.stopSeq.Type == gjson.Null, "stop_sequence 必须是 null")
	require.Equal(t, int64(10), delta.input)
	require.Equal(t, int64(5), delta.output)
	require.Equal(t, int64(0), delta.cacheRead)
	require.Equal(t, int64(0), delta.cacheWrite)

	var stopsN int
	for _, f := range frames {
		if f.event == "message_stop" {
			stopsN++
		}
	}
	require.Equal(t, 1, stopsN, "message_stop 恰好一次")
}

// stopInfo mess 目标 message_delta 上要断言的字段。
type stopInfo struct {
	stopReason string
	stopSeq    gjson.Result
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
}
