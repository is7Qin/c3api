// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"github.com/is7qin/c3api/internal/domain"
)

// chat 上游的 SSE 帧**没有 event: 行、顶层没有 type 字段**（官方 chunk 形如
// `{"id":…,"object":"chat.completion.chunk","choices":[…]}`），所以现有
// `Map` 的"按 type 推断事件名"在这里必然失败并原样透传 —— 对 mess/resp 客户端
// 来说就是"收到一堆不是本协议格式的帧"。故 chat 源的两个方向**不进透传**，
// 由本文件自行解析（spec §5.5）。
//
// 判据（spec §5.5 第 2 条）：优先 `object=="chat.completion.chunk"`，降级兼容
// "choices 键存在" ∨ `data=="[DONE]"` ∨ "error 键存在"。**不看 event: 名、也不看
// reasoning_content 是否存在**（那是第三方扩展）。
func (m *StreamMapper) mapChatChunk(data []byte) ([]byte, bool) {
	if m.done {
		// 终止帧已产出（[DONE] 或 Finish）：之后到达的任何帧一律丢弃并计数。
		noteDrop(m.dir, "frame_after_terminal")
		return nil, true
	}
	if isDoneData(data) {
		return m.chatTerminal(), false
	}
	ev, err := decodeObj(data)
	if err != nil {
		return nil, true
	}
	if e, ok := ev["error"].(map[string]any); ok && e != nil {
		return m.chatError(e), false
	}
	return m.chatChunk(ev)
}

// isDoneData `data: [DONE]` 判定（chat 流的唯一显式终止标记）。
func isDoneData(data []byte) bool {
	if len(data) != 6 {
		return false
	}
	return string(data) == "[DONE]"
}

// chatChunk 一个 chat.completion.chunk → 客户端帧。
func (m *StreamMapper) chatChunk(ev map[string]any) ([]byte, bool) {
	if id, ok := str(ev, "id"); ok && m.id == "" {
		m.id = id
	}
	if model, ok := str(ev, "model"); ok && m.model == "" {
		m.model = model
	}
	// choices 缺失/空 ⇒ 只在 usage 是**对象**时取用量（include_usage 的末帧形态）。
	// 注意（F17）：开了 include_usage 后其它帧都带 `"usage":null` —— 键存在、值
	// 为 null，绝不能当用量帧处理，否则每帧都会刷新一次用量。
	choices, hasChoices := arr(ev, "choices")
	if !hasChoices || len(choices) == 0 {
		if u, ok := ev["usage"].(map[string]any); ok && u != nil {
			m.absorbChatUsage(u)
			return nil, true
		}
		return nil, true
	}
	if len(choices) > 1 {
		for i := 1; i < len(choices); i++ {
			noteDrop(m.dir, "choices_nonzero")
		}
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return nil, true
	}
	var out []byte
	out = append(out, m.chatStart()...)
	if d, ok := c["delta"].(map[string]any); ok && d != nil {
		out = append(out, m.chatDelta(d)...)
	}
	// finish_reason **只记录不终止**：标准帧序是 finish_reason → usage(末帧) →
	// [DONE]，见到它就收尾会把紧随其后的真实用量丢掉（终态用量变 0）。
	if fr, ok := str(c, "finish_reason"); ok && fr != "" {
		m.stopReason = fr
	}
	if u, ok := ev["usage"].(map[string]any); ok && u != nil {
		m.absorbChatUsage(u)
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, false
}

// chatStart 首帧前置：mess 目标发 message_start，resp 目标发 response.created。
// 上游可能从不发 delta.role（fakeupstream 现状即如此），首个内容 chunk 前必须
// 补发（不变量 §5.6 #1）。
func (m *StreamMapper) chatStart() []byte {
	if m.started {
		return nil
	}
	m.started = true
	if m.dir == domain.ProtocolConvertRespToChat {
		return EncodeFrame("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": m.id, "object": "response", "created_at": 0, "status": "in_progress",
				"model": m.model, "output": []any{}, "usage": nil,
			},
		})
	}
	return EncodeFrame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": m.id, "type": "message", "role": "assistant", "model": m.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			// 占位：chat 的 input 用量只在末帧出现，而 message_start.message.usage
			// 在 Anthropic 侧是必填。
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// chatDelta 一个 delta → 客户端帧（thinking / 文本 / 工具调用）。
func (m *StreamMapper) chatDelta(d map[string]any) []byte {
	m.ensureChat()
	var out []byte
	if t, ok := str(d, "reasoning_content"); ok && t != "" {
		out = append(out, m.chatThinking(t)...)
	}
	if t, ok := str(d, "content"); ok && t != "" {
		out = append(out, m.chatText(t)...)
	}
	if tcs, ok := arr(d, "tool_calls"); ok {
		for i, tc := range tcs {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, m.chatToolCall(i, tcm)...)
		}
	}
	return out
}

// chatThinking 思考增量：mess 目标落 thinking 块的 thinking_delta；resp 目标落
// reasoning 项的 summary 增量。**不写 signature**（chat 无签名通道，不发明）。
//
// 累积与发帧分开做：首片也要进累积器，否则终止帧里的完整文本会缺第一段
// （曾漏在此处：output_item.done 只带后半截）。
func (m *StreamMapper) chatThinking(t string) []byte {
	var out []byte
	if m.thinkIdx < 0 {
		m.thinkIdx = m.chatAlloc()
		m.blockStarted[m.thinkIdx] = true
		m.thinkBlocks[m.thinkIdx] = &thinkingBlock{Type: "thinking"}
		if m.dir == domain.ProtocolConvertRespToChat {
			out = EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": m.thinkIdx,
				"item": map[string]any{
					"id": m.thinkID(m.thinkIdx), "type": "reasoning",
					"status": "in_progress", "summary": []any{},
				},
			})
		} else {
			out = EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": m.thinkIdx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			})
		}
	}
	if b := m.thinkBlocks[m.thinkIdx]; b != nil {
		b.Thinking += t
	}
	if m.dir == domain.ProtocolConvertRespToChat {
		return append(out, EncodeFrame("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": m.thinkID(m.thinkIdx),
			"output_index": m.thinkIdx, "summary_index": 0, "delta": t,
		})...)
	}
	return append(out, EncodeFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": m.thinkIdx,
		"delta": map[string]any{"type": "thinking_delta", "thinking": t},
	})...)
}

// chatText 正文增量（同样：累积先于发帧，首片也累积）。
func (m *StreamMapper) chatText(t string) []byte {
	var out []byte
	if m.textIdx < 0 {
		m.textIdx = m.chatAlloc()
		m.blockStarted[m.textIdx] = true
		if m.dir == domain.ProtocolConvertRespToChat {
			out = EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": m.textIdx,
				"item": map[string]any{
					"id": m.itemID(m.textIdx), "type": "message", "status": "in_progress",
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
				},
			})
		} else {
			out = EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": m.textIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
	}
	m.textByIndex[m.textIdx] += t
	if m.dir == domain.ProtocolConvertRespToChat {
		return append(out, EncodeFrame("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": m.itemID(m.textIdx),
			"output_index": m.textIdx, "content_index": 0, "delta": t,
		})...)
	}
	return append(out, EncodeFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": m.textIdx,
		"delta": map[string]any{"type": "text_delta", "text": t},
	})...)
}

// chatFCID function_call 项的 id：与既有 resp 目标的流式映射保持一致
// （`fc_` + call_id，**不带 index**）。call_id 才是工具结果的匹配键，id 只是展示
// 用，两处写法必须相同，否则同一响应在流式与非流式下 id 不一致。
func chatFCID(callID string) string { return "fc_" + callID }

// chatToolCall 工具调用增量。chat 的 tool_calls 分片到达：首片带 id/name，之后
// 只带 arguments 片段。键用 `tool_calls[i].index`（缺失时按数组下标兜底）——同一
// 个工具的后续分片必须落回同一个内容块。
func (m *StreamMapper) chatToolCall(pos int, tcm map[string]any) []byte {
	key := intOr0(tcm, "index")
	if _, ok := tcm["index"]; !ok {
		key = int64(pos)
	}
	idx, seen := m.toolIdx[key]
	id, _ := str(tcm, "id")
	name := ""
	args := ""
	if fn, ok := tcm["function"].(map[string]any); ok && fn != nil {
		name, _ = str(fn, "name")
		args, _ = str(fn, "arguments")
	}
	if !seen {
		idx = m.chatAlloc()
		m.toolIdx[key] = idx
		m.blockStarted[idx] = true
		m.fcIDs[idx] = id
		m.fcNames[idx] = name
		m.argsByIndex[idx] = args
		if m.dir == domain.ProtocolConvertRespToChat {
			out := EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": idx,
				"item": map[string]any{
					"id": chatFCID(id), "type": "function_call", "call_id": id,
					"name": name, "arguments": "", "status": "in_progress",
				},
			})
			if args == "" {
				return out
			}
			return append(out, EncodeFrame("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": chatFCID(id),
				"output_index": idx, "delta": args,
			})...)
		}
		out := EncodeFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
		})
		if args == "" {
			return out
		}
		return append(out, EncodeFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})...)
	}
	if args == "" {
		return nil
	}
	if id == "" {
		id = m.fcIDs[idx]
	}
	m.argsByIndex[idx] += args
	if m.dir == domain.ProtocolConvertRespToChat {
		return EncodeFrame("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": chatFCID(id),
			"output_index": idx, "delta": args,
		})
	}
	return EncodeFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
}

// absorbChatUsage 累加 chat 用量（覆盖语义：后到的末帧覆盖先到的值）。
func (m *StreamMapper) absorbChatUsage(u map[string]any) {
	m.it = intOr0(u, "prompt_tokens")
	m.ot = intOr0(u, "completion_tokens")
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok && d != nil {
		m.cached = intOr0(d, "cached_tokens")
		m.cacheCreate = intOr0(d, "cache_write_tokens")
	}
}

// chatTerminal 产出终止帧（恰好一次）：mess 目标 = 关块 + message_delta +
// message_stop；resp 目标 = 各 open item 的 done + response.completed。
//
// 这是 `data: [DONE]` 与**正常 EOF**（sserelay.Config.Terminal）共用的唯一出口，
// 靠 m.done 去重。
func (m *StreamMapper) chatTerminal() []byte {
	if m.done {
		return nil
	}
	m.done = true
	m.ensureChat()
	if m.dir == domain.ProtocolConvertRespToChat {
		return m.chatRespTerminal()
	}
	out := m.chatStart()
	out = append(out, m.closeChatBlocks()...)
	out = append(out, EncodeFrame("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": messStopFromChat(m.stopReason), "stop_sequence": nil},
		"usage": m.chatMessUsage(),
	})...)
	return append(out, EncodeFrame("message_stop", map[string]any{"type": "message_stop"})...)
}

// chatRespTerminal resp 目标的终止：先把每个已开始的块补一条 output_item.done
// （客户端会按 done 累积 output，缺了就是丢内容），再发 response.completed，
// 其 output 与流内 done 项逐字一致。
func (m *StreamMapper) chatRespTerminal() []byte {
	out := m.chatStart()
	items := m.chatRespItems()
	for _, it := range items {
		idx, _ := it["__index"].(int64)
		delete(it, "__index")
		out = append(out, EncodeFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": idx, "item": it,
		})...)
	}
	status, incomplete := chatRespStatus(m.stopReason)
	// output 必须是 JSON 数组（[]any），不能是 []map[string]any 的接口值。
	output := make([]any, 0, len(items))
	for _, it := range items {
		output = append(output, it)
	}
	resp := map[string]any{
		"id": m.id, "object": "response", "created_at": 0, "status": status,
		"model": m.model, "output": output, "usage": m.chatRespUsage(),
	}
	if incomplete != "" {
		resp["incomplete_details"] = map[string]any{"reason": incomplete}
	}
	return append(out, EncodeFrame("response.completed", map[string]any{
		"type": "response.completed", "response": resp,
	})...)
}

// chatRespItems 已累积的 resp output 项（顺序：reasoning → message → function_call，
// 与流内 added 的先后一致）。`__index` 是内部携带的 output_index（发帧时移除）。
func (m *StreamMapper) chatRespItems() []map[string]any {
	var out []map[string]any
	if m.thinkIdx >= 0 {
		b := m.thinkBlocks[m.thinkIdx]
		text := ""
		if b != nil {
			text = b.Thinking
		}
		out = append(out, map[string]any{
			"__index": m.thinkIdx, "id": m.thinkID(m.thinkIdx), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": text}},
		})
	}
	if m.textIdx >= 0 {
		out = append(out, map[string]any{
			"__index": m.textIdx, "id": m.itemID(m.textIdx), "type": "message", "role": "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": m.textByIndex[m.textIdx], "annotations": []any{},
			}},
		})
	}
	for key, idx := range m.toolIdx {
		id := m.fcIDs[idx]
		out = append(out, map[string]any{
			"__index": idx, "id": chatFCID(id), "type": "function_call",
			"call_id": id, "name": m.fcNames[idx], "arguments": m.argsByIndex[idx],
			"status": "completed",
		})
		_ = key
	}
	return out
}

// closeChatBlocks mess 目标的关块：按 index 升序补 content_block_stop。
func (m *StreamMapper) closeChatBlocks() []byte {
	return m.closeOpenBlocks()
}

// chatMessUsage mess 目标的终态用量（三项恒写 + net 口径）。
func (m *StreamMapper) chatMessUsage() map[string]any {
	net := respInputNet(m.it, m.cached, m.cacheCreate)
	if m.it < m.cached+m.cacheCreate {
		noteDrop(m.dir, "usage_negative_clamped")
	}
	return map[string]any{
		"input_tokens":                net,
		"cache_read_input_tokens":     m.cached,
		"cache_creation_input_tokens": m.cacheCreate,
		"output_tokens":               m.ot,
	}
}

// chatRespUsage resp 目标的终态用量（input_tokens_details 两键恒写）。
func (m *StreamMapper) chatRespUsage() map[string]any {
	net := respInputNet(m.it, m.cached, m.cacheCreate)
	if m.it < m.cached+m.cacheCreate {
		noteDrop(m.dir, "usage_negative_clamped")
	}
	return map[string]any{
		"input_tokens": net,
		"input_tokens_details": map[string]any{
			"cached_tokens":      m.cached,
			"cache_write_tokens": m.cacheCreate,
		},
		"output_tokens":         m.ot,
		"total_tokens":          net + m.ot,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
}

// chatError 上游 error 帧 → 客户端错误帧（先关块再报，客户端才不会停在半截块）。
func (m *StreamMapper) chatError(e map[string]any) []byte {
	if m.done {
		noteDrop(m.dir, "frame_after_terminal")
		return nil
	}
	m.done = true
	m.ensureChat()
	msg, _ := str(e, "message")
	if m.dir == domain.ProtocolConvertRespToChat {
		out := m.chatStart()
		return append(out, EncodeFrame("error", map[string]any{
			"type": "error", "code": "api_error", "message": msg,
		})...)
	}
	out := m.chatStart()
	out = append(out, m.closeChatBlocks()...)
	return append(out, EncodeFrame("error", map[string]any{
		"type": "error", "error": map[string]any{"type": "api_error", "message": msg},
	})...)
}

// ensureChat chat 源方向的块状态懒初始化（与 ensureBlocks 同构，另加工具索引表）。
func (m *StreamMapper) ensureChat() {
	if m.toolIdx != nil {
		return
	}
	m.ensureBlocks()
	m.toolIdx = map[int64]int64{}
	m.thinkIdx = -1
	m.textIdx = -1
}

// chatStreamUsage 当前累计的 chat 用量（测试读；映射本身不依赖它）。
func (m *StreamMapper) chatStreamUsage() (it, ot, cached, write int64) {
	return m.it, m.ot, m.cached, m.cacheCreate
}

// chatAlloc 单调分配内容 index（text / thinking / tool_use 共用一张表，与
// resp→mess 的 contentIndex 语义一致：index 只增不减、按出现顺序）。
func (m *StreamMapper) chatAlloc() int64 {
	idx := m.nextIndex
	m.nextIndex++
	return idx
}
