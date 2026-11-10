// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"strconv"
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
// 补 ⇒ 终态 item 的 call_id / name 必须写回；块身份在 start 帧发出前就定死
// （响应 id + 内容 index），迟到只回填 call_id，**不改 item id**，也不推迟 start
// （推迟会让后到的块先 start，Anthropic 客户端按 start 顺序追加块会索引错位）。
func TestChatStreamToolLateIDAndNameBackfill(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertRespToChat, true,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"name":"h","arguments":":1}"}}]},"finish_reason":null}]}`,
	)

	ids := map[string]bool{}
	var added int
	for _, f := range frames {
		switch f.event {
		case "response.output_item.added":
			added++
			id := f.data.Get("item.id").Str
			require.Regexp(t, `^fc_`, id, "item id 必须是 fc_ 前缀")
			ids[id] = true
		case "response.function_call_arguments.delta":
			id := f.data.Get("item_id").Str
			require.Regexp(t, `^fc_`, id)
			ids[id] = true
		case "response.output_item.done":
			ids[f.data.Get("item.id").Str] = true
		}
	}
	require.Equal(t, 1, added, "整串分片只允许一个 added")
	require.Len(t, ids, 1, "added/delta/done 必须共用同一个 item id：%v", ids)

	resp := terminalResponse(t, frames)
	require.Equal(t, int64(1), resp.Get("output.#").Int())
	require.Equal(t, "call_late", resp.Get("output.0.call_id").Str,
		"迟到的 id 必须写回 call_id（不得是空串）")
	require.Equal(t, "h", resp.Get("output.0.name").Str,
		"迟到的 name 必须写回 name（不得是空串）")
	for id := range ids {
		require.Equal(t, id, resp.Get("output.0.id").Str, "终态 item id 必须与流内一致")
	}
	require.Equal(t, `{"k":1}`, resp.Get("output.0.arguments").Str)
}

// TestChatStreamToolLateIDMessTarget mess 目标同一规则：tool_use.id 在 start 帧
// 发出时就定死（迟到用响应 id + index 占位），不先空后变——客户端拿它回传
// tool_result，空 id 会把下一轮配对打断。
func TestChatStreamToolLateIDMessTarget(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertMessToChat, true,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"name":"h","arguments":":1}"}}]},"finish_reason":null}]}`,
	)
	var starts int
	for _, f := range frames {
		if f.event != "content_block_start" || f.data.Get("content_block.type").Str != "tool_use" {
			continue
		}
		starts++
		require.NotEmpty(t, f.data.Get("content_block.id").Str,
			"start 帧的 tool_use.id 不得为空")
	}
	require.Equal(t, 1, starts, "整串分片只允许一个 tool_use start")
}

// TestChatStreamToolNeverIDMessTarget 始终收不到 call id：start 帧仍立刻发出
// （占位 id），且**先**于 stop；否则客户端收到孤立的 content_block_stop。
func TestChatStreamToolNeverIDMessTarget(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertMessToChat, true,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"","function":{"name":"h","arguments":"{\"k\":1}"}}]},"finish_reason":"tool_calls"}]}`,
	)
	var startAt, stopAt = -1, -1
	for i, f := range frames {
		switch f.event {
		case "content_block_start":
			if f.data.Get("content_block.type").Str == "tool_use" {
				startAt = i
				require.NotEmpty(t, f.data.Get("content_block.id").Str)
			}
		case "content_block_stop":
			stopAt = i
		}
	}
	require.NotEqual(t, -1, startAt, "tool_use start 必须发出")
	require.NotEqual(t, -1, stopAt, "块必须收尾")
	require.Less(t, startAt, stopAt, "start 必须先于 stop")
}

// interleavedChunks 交错到达：工具 0（无 id）→ 正文 → 工具 1（有 id）→ 工具 0
// 续片（补 id）。这是「推迟 start」会让索引错位的典型序列。
func interleavedChunks() []string {
	return []string{
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"a","arguments":"{\"a\""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"b","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}]}`,
	}
}

// TestChatStreamToolInterleavedBlocksMessTarget 交错到达时 Anthropic 客户端按
// content_block_start 的顺序 append 块（SDK 的 accumulate_event 就是这么做的），
// 因此 start 必须按 index 升序：工具块不能因为 call id 迟到就被后到的正文/工具
// 抢先 start。同时每个 delta 只能落在已 start 的 index 上。
func TestChatStreamToolInterleavedBlocksMessTarget(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertMessToChat, true, interleavedChunks()...)

	var starts []int64
	started := map[int64]bool{}
	type startInfo struct {
		typ string
		id  string
	}
	byIndex := map[int64]startInfo{}
	for _, f := range frames {
		switch f.event {
		case "content_block_start":
			idx := f.data.Get("index").Int()
			require.False(t, started[idx], "index %d 重复 start", idx)
			started[idx] = true
			starts = append(starts, idx)
			typ := f.data.Get("content_block.type").Str
			byIndex[idx] = startInfo{typ: typ, id: f.data.Get("content_block.id").Str}
		case "content_block_delta":
			idx := f.data.Get("index").Int()
			require.True(t, started[idx], "delta 落在未 start 的 index %d", idx)
		case "content_block_stop":
			idx := f.data.Get("index").Int()
			require.True(t, started[idx], "stop 落在未 start 的 index %d", idx)
		}
	}
	require.Equal(t, []int64{0, 1, 2}, starts, "start 必须按内容 index 升序")
	require.Equal(t, "tool_use", byIndex[0].typ)
	require.Equal(t, "call_c_0", byIndex[0].id, "迟到 id 的占位身份（响应 id + index）")
	require.Equal(t, "text", byIndex[1].typ)
	require.Equal(t, "tool_use", byIndex[2].typ)
	require.Equal(t, "call_b", byIndex[2].id)
}

// TestChatStreamToolInterleavedBlocksRespTarget 同一交错序列的 resp 目标：
// added 的 output_index 升序、每个 delta 的 item_id 与对应 added 一致、done /
// completed 的 item 身份与流内一致，终态 output 顺序与 output_index 一致。
func TestChatStreamToolInterleavedBlocksRespTarget(t *testing.T) {
	frames := chatStreamRun(t, domain.ProtocolConvertRespToChat, true, interleavedChunks()...)

	var addedOrder []int64
	byIndex := map[int64]string{}
	for _, f := range frames {
		switch f.event {
		case "response.output_item.added", "response.output_item.done":
			idx := f.data.Get("output_index").Int()
			id := f.data.Get("item.id").Str
			if prev, ok := byIndex[idx]; ok {
				require.Equal(t, prev, id, "index %d 的 item id 在 added/done 之间不得变化", idx)
				continue
			}
			byIndex[idx] = id
			if f.event == "response.output_item.added" {
				addedOrder = append(addedOrder, idx)
			}
		case "response.function_call_arguments.delta":
			idx := f.data.Get("output_index").Int()
			id, ok := byIndex[idx]
			require.True(t, ok, "delta 落在未 added 的 index %d", idx)
			require.Equal(t, id, f.data.Get("item_id").Str, "delta 的 item_id 必须与该 index 的 added 一致")
		}
	}
	require.Equal(t, []int64{0, 1, 2}, addedOrder, "added 必须按 output_index 升序")
	require.Equal(t, "fc_c_0", byIndex[0], "迟到工具块的稳定身份")
	require.Equal(t, "msg_c_1", byIndex[1])
	require.Equal(t, "fc_call_b", byIndex[2])

	resp := terminalResponse(t, frames)
	require.Equal(t, int64(3), resp.Get("output.#").Int())
	for i := int64(0); i < 3; i++ {
		require.Equal(t, byIndex[i], resp.Get("output."+gjsonIndex(i)+".id").Str,
			"终态 output 顺序与 id 必须与流内一致")
	}
	require.Equal(t, "call_a", resp.Get("output.0.call_id").Str)
	require.Equal(t, `{"a":1}`, resp.Get("output.0.arguments").Str)
}

func gjsonIndex(i int64) string { return strconv.FormatInt(i, 10) }

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
