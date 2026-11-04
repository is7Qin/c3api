// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
	"strconv"
)

// respToMessRequest 客户端 resp 请求体 → anthropic messages 请求体。字段映射
// 按 Messages API 规范：
//   - instructions + input 内 system/developer 消息项 → 顶层 system（拼接）
//   - input → messages：message 项 → user/assistant 消息（文本块）；
//     function_call 项 → 追加到最近 assistant 消息的 tool_use 块；
//     function_call_output 项 → user 消息的 tool_result 块
//   - max_output_tokens → max_tokens（anthropic 必填；缺失则补常量 4096）
//   - tools → tools（parameters → input_schema）；tool_choice 归一化为对象
//     （required → any；{type:"function",name} → {type:"tool",name}）
//   - 同名字段透传：model/temperature/top_p/stream/metadata
//   - anthropic 无对应参数（top_logprobs/seed/store/parallel_tool_calls/
//     reasoning/text/include/truncation 等）→ 按规范丢弃
//
// messToRespResponse anthropic message 对象 → resp 响应对象（非流式）：
// content text 块 → message 项（output_text）；tool_use 块 → function_call
// 项（input 对象 → arguments JSON 字符串）；usage → input/output/total +
// cache_read → input_tokens_details.cached_tokens + cache_creation →
// input_tokens_details.cache_write_tokens。
func messToRespResponse(body []byte) ([]byte, error) {
	msg, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(msg, "id")
	model, _ := str(msg, "model")
	output, it, ot := messContentToRespOutput(msg)
	out := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": 0, // anthropic message 无时间戳（转换器纯函数，不发明）
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage":      messUsageToResp(msg, it, ot),
	}
	return json.Marshal(out)
}

// messContentToRespOutput anthropic content → resp output 项
// （reasoning 项 + message 项 + function_call 项），返回 (output, inputTokens,
// outputTokens)。
//
// thinking / redacted_thinking 块 → 独立 reasoning item，**排在其余项之前**
// （保持 Claude 的块顺序：thinking 恒在最前）。Anthropic 签名无处直接表达，
// 以信封形式放进 encrypted_content，客户端（codex 等）原样回传后即可解回原块
// （resp_to_mess 请求侧还原，spec §2.1）。无签名无文本的块丢弃。
func messContentToRespOutput(msg map[string]any) ([]any, int64, int64) {
	var thinks, rest []any
	var textParts []any
	var fcs []any
	msgID, _ := str(msg, "id")
	content, _ := arr(msg, "content")
	for _, blk := range content {
		bm, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "thinking":
			t, _ := str(bm, "thinking")
			sig, _ := str(bm, "signature")
			if t == "" && sig == "" {
				continue
			}
			item := map[string]any{
				"type": "reasoning", "id": "rs_" + msgID + "_" + strconv.Itoa(len(thinks)),
			}
			if t != "" {
				item["summary"] = []any{map[string]any{"type": "summary_text", "text": t}}
			} else {
				item["summary"] = []any{}
			}
			if sig != "" {
				item["encrypted_content"] = sealThinking(thinkingBlock{Type: "thinking", Thinking: t, Signature: sig})
			}
			thinks = append(thinks, item)
		case "redacted_thinking":
			data, _ := str(bm, "data")
			if data == "" {
				continue
			}
			thinks = append(thinks, map[string]any{
				"type": "reasoning", "id": "rs_" + msgID + "_" + strconv.Itoa(len(thinks)),
				"summary":           []any{},
				"encrypted_content": sealThinking(thinkingBlock{Type: "redacted_thinking", Data: data}),
			})
		case "text":
			if t, ok := str(bm, "text"); ok {
				textParts = append(textParts, map[string]any{"type": "output_text", "text": t, "annotations": []any{}})
			}
		case "tool_use":
			id, _ := str(bm, "id")
			name, _ := str(bm, "name")
			fcs = append(fcs, map[string]any{
				"type": "function_call", "id": id, "call_id": id,
				"name": name, "arguments": marshalAny(bm["input"]), "status": "completed",
			})
		}
	}
	if len(textParts) > 0 {
		rest = append(rest, map[string]any{
			"id": msgID, "type": "message", "status": "completed", "role": "assistant",
			"content": textParts,
		})
	}
	rest = append(rest, fcs...)
	output := append(thinks, rest...)
	if len(output) == 0 {
		output = []any{}
	}
	it, ot := int64(0), int64(0)
	if u, ok := msg["usage"].(map[string]any); ok {
		it = respInputGross(intOr0(u, "input_tokens"), intOr0(u, "cache_read_input_tokens"), intOr0(u, "cache_creation_input_tokens"))
		ot = intOr0(u, "output_tokens")
	}
	return output, it, ot
}

// messUsageToResp anthropic usage → resp usage。anthropic input_tokens 是 net，
// resp 侧 input_tokens 恒为 gross（口径见 protoconv.go 顶部）：it 由
// messContentToRespOutput 按 input + cache_creation + cache_read 重建；
// cached_tokens 取 cache_read，cache_write_tokens 取 cache_creation。
func messUsageToResp(msg map[string]any, it, ot int64) map[string]any {
	cached, write := int64(0), int64(0)
	if u, ok := msg["usage"].(map[string]any); ok {
		cached = intOr0(u, "cache_read_input_tokens")
		write = intOr0(u, "cache_creation_input_tokens")
	}
	return map[string]any{
		"input_tokens":         it,
		"output_tokens":        ot,
		"total_tokens":         it + ot,
		"input_tokens_details": respInputDetails(cached, write),
	}
}

// respInputDetails resp usage 的 input_tokens_details（cached_tokens 恒含；
// cache_write_tokens 仅在非 0 时出现，避免无缓存写入的响应多出零值键）。
func respInputDetails(cached, write int64) map[string]any {
	d := map[string]any{"cached_tokens": cached}
	if write > 0 {
		d["cache_write_tokens"] = write
	}
	return d
}

// mapMessToResp 流式：anthropic messages SSE 事件 → resp 流。事件映射表：
//
//	message_start           → response.created（id/model/input 用量入状态）
//	content_block_start     → response.output_item.added（text → message 项 /
//	                          tool_use → function_call 项）
//	content_block_delta     → response.output_text.delta（text）/
//	                          response.function_call_arguments.delta（json）
//	message_delta           → 状态吸收（output_tokens/stop_reason），不产帧
//	message_stop            → response.completed（输出项累积 + 用量）
//	error                   → resp 错误帧形态
//	其余 → 丢弃
func (m *StreamMapper) mapMessToResp(name string, data []byte) ([]byte, bool) {
	m.ensureBlocks() // 块级累积 map 懒初始化
	ev, err := decodeObj(data)
	if err != nil {
		return nil, true
	}
	switch name {
	case "message_start":
		if m.started {
			return nil, true
		}
		m.started = true
		if msg, ok := ev["message"].(map[string]any); ok {
			m.id, _ = str(msg, "id")
			m.model, _ = str(msg, "model")
			if u, ok := msg["usage"].(map[string]any); ok {
				m.setMessUsage(u)
			}
		}
		return EncodeFrame("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": m.id, "object": "response", "created_at": 0, "status": "in_progress",
				"model": m.model, "output": []any{}, "usage": nil,
			},
		}), false
	case "content_block_start":
		index := intOr0(ev, "index")
		block, ok := ev["content_block"].(map[string]any)
		if !ok {
			return nil, true
		}
		switch block["type"] {
		case "thinking":
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.blockOrder = append(m.blockOrder, index)
			m.thinkBlocks[index] = &thinkingBlock{Type: "thinking"}
			return EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index,
				"item": map[string]any{
					"id": m.thinkID(index), "type": "reasoning", "status": "in_progress",
					"summary": []any{},
				},
			}), false
		case "redacted_thinking":
			// 不透明块没有 delta：added + done 一次发完，done 的 item 带信封
			// （客户端原样回传，resp_to_mess 请求侧可解回原块）。
			data, _ := str(block, "data")
			if data == "" || m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.blockStopped[index] = true
			m.blockOrder = append(m.blockOrder, index)
			blk := &thinkingBlock{Type: "redacted_thinking", Data: data}
			m.thinkBlocks[index] = blk
			item := m.reasoningItem(index, blk)
			f := EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index,
				"item": map[string]any{
					"id": m.thinkID(index), "type": "reasoning", "status": "in_progress",
					"summary": []any{},
				},
			})
			return append(f, EncodeFrame("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": index, "item": item,
			})...), false
		case "text":
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.blockOrder = append(m.blockOrder, index)
			item := map[string]any{
				"id": m.itemID(index), "type": "message", "status": "in_progress",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			}
			return EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index, "item": item,
			}), false
		case "tool_use":
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.blockOrder = append(m.blockOrder, index)
			id, _ := str(block, "id")
			name, _ := str(block, "name")
			m.fcIDs[index] = id
			m.fcNames[index] = name
			item := map[string]any{
				"id": "fc_" + id, "type": "function_call", "call_id": id,
				"name": name, "arguments": "", "status": "in_progress",
			}
			return EncodeFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": index, "item": item,
			}), false
		}
		return nil, true
	case "content_block_delta":
		index := intOr0(ev, "index")
		delta, ok := ev["delta"].(map[string]any)
		if !ok {
			return nil, true
		}
		switch delta["type"] {
		case "thinking_delta":
			t, _ := str(delta, "thinking")
			if b := m.thinkBlocks[index]; b != nil {
				b.Thinking += t
			}
			return EncodeFrame("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "item_id": m.thinkID(index),
				"output_index": index, "summary_index": 0, "delta": t,
			}), false
		case "signature_delta":
			// Anthropic 签名是「块级收尾」信息，没有对应的 resp 流事件：累积到
			// message_stop 的 output item 信封里回给客户端。
			if b := m.thinkBlocks[index]; b != nil {
				if s, ok := str(delta, "signature"); ok {
					b.Signature += s
				}
			}
			return nil, true
		case "text_delta":
			text, _ := str(delta, "text")
			m.textByIndex[index] += text
			return EncodeFrame("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": m.itemID(index),
				"output_index": index, "content_index": 0, "delta": text,
			}), false
		case "input_json_delta":
			partial, _ := str(delta, "partial_json")
			m.argsByIndex[index] += partial
			return EncodeFrame("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": "fc_" + m.fcIDs[index],
				"output_index": index, "delta": partial,
			}), false
		}
		return nil, true
	case "message_delta":
		if u, ok := ev["usage"].(map[string]any); ok {
			m.ot = intOr0(u, "output_tokens")
			m.mergeMessUsage(u)
		}
		if d, ok := ev["delta"].(map[string]any); ok {
			m.reason, _ = str(d, "stop_reason")
		}
		return nil, true
	case "message_stop":
		if m.done {
			return nil, true
		}
		m.done = true
		input := m.messInputTotal()
		return EncodeFrame("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": m.id, "object": "response", "created_at": 0, "status": "completed",
				"model": m.model, "output": m.messOutputItems(),
				"usage": map[string]any{
					"input_tokens": input, "output_tokens": m.ot, "total_tokens": input + m.ot,
					"input_tokens_details": respInputDetails(m.cached, m.cacheCreate),
				},
			},
		}), false
	case "error":
		if e, ok := ev["error"].(map[string]any); ok {
			msg, _ := str(e, "message")
			typ, _ := str(e, "type")
			return EncodeFrame("error", map[string]any{"code": typ, "message": msg}), false
		}
		return nil, true
	}
	return nil, true
}

// reasoningItem thinking 块累积状态 → reasoning item（summary 文本 + 信封
// encrypted_content）。流式 added/done 与 message_stop 的 output 项共用。
func (m *StreamMapper) reasoningItem(index int64, b *thinkingBlock) map[string]any {
	item := map[string]any{
		"id": m.thinkID(index), "type": "reasoning", "status": "completed",
	}
	if b.Thinking != "" {
		item["summary"] = []any{map[string]any{"type": "summary_text", "text": b.Thinking}}
	} else {
		item["summary"] = []any{}
	}
	// 信封只对可回放的块有意义：thinking 需签名，redacted 需 data。
	if b.Type == "redacted_thinking" && b.Data != "" {
		item["encrypted_content"] = sealThinking(*b)
	} else if b.Type == "thinking" && b.Signature != "" {
		item["encrypted_content"] = sealThinking(*b)
	}
	return item
}

// messOutputItems 累积的块级输出 → response.completed 的 output 数组
// （reasoning 项补全 summary + 信封、message 项补全文本、function_call 项补全
// arguments + completed 状态）。
func (m *StreamMapper) messOutputItems() []any {
	var output []any
	for _, index := range m.blockOrder {
		if b := m.thinkBlocks[index]; b != nil {
			output = append(output, m.reasoningItem(index, b))
			continue
		}
		if name, ok := m.fcNames[index]; ok {
			output = append(output, map[string]any{
				"id": "fc_" + m.fcIDs[index], "type": "function_call", "call_id": m.fcIDs[index],
				"name": name, "arguments": m.argsByIndex[index], "status": "completed",
			})
			continue
		}
		output = append(output, map[string]any{
			"id": m.itemID(index), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": m.textByIndex[index], "annotations": []any{}}},
		})
	}
	if output == nil {
		return []any{}
	}
	return output
}
