// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
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
// messToRespResponse anthropic message 对象 → resp 响应对象（非流式）：
// content text 块 → message 项（output_text）；tool_use 块 → function_call
// 项（input 对象 → arguments JSON 字符串）；usage → input/output/total +
// cache_read → input_tokens_details.cached_tokens。
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
// （message 项 + function_call 项），返回 (output, inputTokens, outputTokens)。
func messContentToRespOutput(msg map[string]any) ([]any, int64, int64) {
	var output []any
	var textParts []any
	var fcs []any
	content, _ := arr(msg, "content")
	for _, blk := range content {
		bm, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
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
		id, _ := str(msg, "id")
		output = append(output, map[string]any{
			"id": id, "type": "message", "status": "completed", "role": "assistant",
			"content": textParts,
		})
	}
	output = append(output, fcs...)
	if len(output) == 0 {
		output = []any{}
	}
	it, ot := int64(0), int64(0)
	if u, ok := msg["usage"].(map[string]any); ok {
		it = intOr0(u, "input_tokens") + intOr0(u, "cache_creation_input_tokens") + intOr0(u, "cache_read_input_tokens")
		ot = intOr0(u, "output_tokens")
	}
	return output, it, ot
}

// messUsageToResp anthropic usage → resp usage。input_tokens 含 cache_creation
// 与 cache_read；cached_tokens 只取 cache_read。不写 cache_write_tokens。
func messUsageToResp(msg map[string]any, it, ot int64) map[string]any {
	cached := int64(0)
	if u, ok := msg["usage"].(map[string]any); ok {
		cached = intOr0(u, "cache_read_input_tokens")
	}
	return map[string]any{
		"input_tokens":         it,
		"output_tokens":        ot,
		"total_tokens":         it + ot,
		"input_tokens_details": map[string]any{"cached_tokens": cached},
	}
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
				m.noteMessUsage(u, false)
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
			m.noteMessUsage(u, true)
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
					"input_tokens_details": map[string]any{"cached_tokens": m.cached},
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

// messOutputItems 累积的块级输出 → response.completed 的 output 数组
// （message 项补全文本、function_call 项补全 arguments + completed 状态）。
func (m *StreamMapper) messOutputItems() []any {
	var output []any
	for _, index := range m.blockOrder {
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
