// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
)

// messToRespRequest 客户端 anthropic messages 请求体 → resp 请求体。字段映射
// 按 Responses API 规范：
//   - system → instructions（string 或 text 块数组 → 拼接文本）
//   - messages → input：文本 → message 项（user input_text / assistant
//     output_text）；tool_use → function_call 项；tool_result →
//     function_call_output 项
//   - max_tokens → max_output_tokens；temperature/top_p 透传（top_k 丢弃——
//     resp 无此参数）
//   - tools → tools（input_schema → parameters）；tool_choice 归一化
//     （any → required；{type:"tool",name} → {type:"function",name}）
//   - 同名字段透传：model/stream/metadata
//   - resp 无对应参数（stop_sequences/thinking 等）→ 按规范丢弃
func messToRespRequest(body []byte) ([]byte, error) {
	req, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, 8)
	if sys, ok := anthropicSystem(req); ok {
		out["instructions"] = sys
	}
	if items, ok := messMessagesToInput(req); ok {
		out["input"] = items
	}
	pass(out, req, "model", "temperature", "top_p", "stream", "metadata")
	if v, ok := req["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if tools, ok := arr(req, "tools"); ok {
		out["tools"] = messToolsToResp(tools)
	}
	if tc, ok := messToolChoice(req); ok {
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// anthropicSystem anthropic system（string 或 text 块/字符串数组）→ 拼接文本。
func anthropicSystem(req map[string]any) (string, bool) {
	v, ok := req["system"]
	if !ok || v == nil {
		return "", false
	}
	switch s := v.(type) {
	case string:
		return s, true
	case []any:
		var parts []string
		for _, p := range s {
			switch pt := p.(type) {
			case string:
				parts = append(parts, pt)
			case map[string]any:
				if t, ok := str(pt, "text"); ok {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return joinStrings(parts, "\n"), true
	}
	return "", false
}

// messMessagesToInput anthropic messages → resp input items：user 文本 →
// message 项（input_text）；user tool_result → function_call_output 项；
// assistant 文本 → message 项（output_text）；assistant tool_use →
// function_call 项（input 对象 → arguments JSON 字符串）。
func messMessagesToInput(req map[string]any) ([]any, bool) {
	msgs, ok := arr(req, "messages")
	if !ok {
		return nil, false
	}
	var items []any
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch role, _ := str(mm, "role"); role {
		case "user":
			var textParts []any
			var toolResults []any
			if c, ok := mm["content"].(string); ok {
				textParts = append(textParts, map[string]any{"type": "input_text", "text": c})
			} else if cs, ok := arr(mm, "content"); ok {
				for _, blk := range cs {
					bm, ok := blk.(map[string]any)
					if !ok {
						continue
					}
					switch bm["type"] {
					case "text":
						if t, ok := str(bm, "text"); ok {
							textParts = append(textParts, map[string]any{"type": "input_text", "text": t})
						}
					case "tool_result":
						id, _ := str(bm, "tool_use_id")
						outText, _ := blockText(bm["content"])
						toolResults = append(toolResults, map[string]any{"type": "function_call_output", "call_id": id, "output": outText})
					}
				}
			}
			if len(textParts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": textParts})
			}
			items = append(items, toolResults...)
		case "assistant":
			var textParts []any
			var fcs []any
			if cs, ok := arr(mm, "content"); ok {
				for _, blk := range cs {
					bm, ok := blk.(map[string]any)
					if !ok {
						continue
					}
					switch bm["type"] {
					case "text":
						if t, ok := str(bm, "text"); ok {
							textParts = append(textParts, map[string]any{"type": "output_text", "text": t})
						}
					case "tool_use":
						id, _ := str(bm, "id")
						name, _ := str(bm, "name")
						fcs = append(fcs, map[string]any{
							"type": "function_call", "id": id, "call_id": id,
							"name": name, "arguments": marshalAny(bm["input"]),
						})
					}
				}
			}
			if len(textParts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "assistant", "content": textParts})
			}
			items = append(items, fcs...)
		}
	}
	return items, true
}

// messToolsToResp anthropic tools → resp tools：{name, description, input_schema}
// → {type:"function", name, description, parameters}。
func messToolsToResp(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		tool := map[string]any{"type": "function"}
		if name, ok := str(tm, "name"); ok {
			tool["name"] = name
		}
		if desc, ok := str(tm, "description"); ok {
			tool["description"] = desc
		}
		if schema, ok := tm["input_schema"].(map[string]any); ok {
			tool["parameters"] = schema
		}
		out = append(out, tool)
	}
	return out
}

// messToolChoice anthropic tool_choice → resp tool_choice："auto"/"none" 透传；
// "any" → "required"；{type:"tool", name} → {type:"function", name}；
// 对象形态（disable_parallel_tool_use）按字符串语义映射。
func messToolChoice(req map[string]any) (any, bool) {
	v, ok := req["tool_choice"]
	if !ok || v == nil {
		return nil, false
	}
	if s, ok := v.(string); ok {
		if s == "any" {
			return "required", true
		}
		return s, true
	}
	if m, ok := v.(map[string]any); ok {
		switch t, _ := str(m, "type"); t {
		case "tool":
			if name, ok := str(m, "name"); ok {
				return map[string]any{"type": "function", "name": name}, true
			}
		case "auto", "any", "none":
			if t == "any" {
				return "required", true
			}
			return t, true
		}
	}
	return nil, false
}

// respToMessResponse resp 响应对象 → anthropic message 对象（非流式）：
// output message 项 → text 块；function_call → tool_use 块（arguments JSON
// 字符串 → input 对象）；usage → input/output/cache 字段。
func respToMessResponse(body []byte) ([]byte, error) {
	r, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(r, "id")
	model, _ := str(r, "model")
	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       respOutputToMessBlocks(r),
		"stop_reason":   respToMessStopReason(r),
		"stop_sequence": nil,
		"usage":         respUsageToMess(r),
	}
	return json.Marshal(out)
}

// respOutputToMessBlocks resp output → anthropic 内容块（text/tool_use）。
func respOutputToMessBlocks(r map[string]any) []any {
	var blocks []any
	if output, ok := arr(r, "output"); ok {
		for _, item := range output {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch im["type"] {
			case "message":
				if content, ok := arr(im, "content"); ok {
					for _, part := range content {
						if pm, ok := part.(map[string]any); ok {
							if t, ok := str(pm, "text"); ok {
								blocks = append(blocks, map[string]any{"type": "text", "text": t})
							}
						}
					}
				}
			case "function_call":
				id := toolCallID(im) // call_id 优先（tool_result.tool_use_id 匹配键，M-1）
				name, _ := str(im, "name")
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": id, "name": name, "input": parseJSON(im["arguments"]),
				})
			}
		}
	}
	if len(blocks) == 0 {
		return []any{}
	}
	return blocks
}

// respToMessStopReason resp 状态/输出 → anthropic stop_reason：incomplete →
// "max_tokens"；含 function_call → "tool_use"；其余 "end_turn"。
func respToMessStopReason(r map[string]any) string {
	if status, _ := str(r, "status"); status == "incomplete" {
		return "max_tokens"
	}
	if output, ok := arr(r, "output"); ok {
		for _, item := range output {
			if im, ok := item.(map[string]any); ok && im["type"] == "function_call" {
				return "tool_use"
			}
		}
	}
	return "end_turn"
}

// respUsageToMess resp usage → anthropic usage（cached_tokens →
// cache_read_input_tokens；anthropic 响应四字段全含）。
func respUsageToMess(r map[string]any) map[string]any {
	it, ot := int64(0), int64(0)
	cached := int64(0)
	if u, ok := r["usage"].(map[string]any); ok {
		it = intOr0(u, "input_tokens")
		ot = intOr0(u, "output_tokens")
		if d, ok := u["input_tokens_details"].(map[string]any); ok {
			cached = intOr0(d, "cached_tokens")
		}
	}
	return map[string]any{
		"input_tokens":                it,
		"output_tokens":               ot,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     cached,
	}
}

// mapRespToMess 流式：resp SSE 事件 → anthropic messages 流。事件映射表：
//
//	response.created                 → message_start（usage.input_tokens 在响应
//	                                  完成前不可知 → 0，补差映射已知取舍；网关
//	                                  计费独立于客户端用量展示）
//	response.output_text.delta       → content_block_start(0,text) 惰性 +
//	                                  content_block_delta(text_delta)
//	response.output_text.done        → content_block_stop(0)
//	response.output_item.added(FC)   → content_block_start(块索引, tool_use)
//	response.function_call_arguments.delta → content_block_delta(input_json_delta)
//	response.function_call_arguments.done  → content_block_stop(块索引)
//	response.completed               → message_delta（stop_reason+output_tokens）
//	                                  + message_stop
//	response.failed                  → error 事件（anthropic 错误帧形态）
//	其余 → 丢弃
func (m *StreamMapper) mapRespToMess(name string, data []byte) ([]byte, bool) {
	m.ensureBlocks() // 块级累积 map 懒初始化（评审 I-4）
	ev, err := decodeObj(data)
	if err != nil {
		return nil, true
	}
	switch name {
	case "response.created":
		if m.started {
			return nil, true
		}
		m.started = true
		var id, model string
		if resp, ok := ev["response"].(map[string]any); ok {
			id, _ = str(resp, "id")
			model, _ = str(resp, "model")
		}
		return EncodeFrame("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": id, "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}), false
	case "response.output_text.delta":
		delta, _ := str(ev, "delta")
		var f []byte
		if !m.blockStarted[0] {
			m.blockStarted[0] = true
			f = EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		f = append(f, EncodeFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		})...)
		return f, false
	case "response.output_text.done":
		if m.blockStarted[0] && !m.blockStopped[0] {
			m.blockStopped[0] = true
			return EncodeFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}), false
		}
		return nil, true
	case "response.output_item.added":
		item, ok := ev["item"].(map[string]any)
		if !ok || item["type"] != "function_call" {
			return nil, true
		}
		index := intOr0(ev, "output_index")
		id := toolCallID(item) // call_id 优先（tool_result.tool_use_id 匹配键，M-1）
		name, _ := str(item, "name")
		if !m.blockStarted[index] {
			m.blockStarted[index] = true
			return EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
			}), false
		}
		return nil, true
	case "response.function_call_arguments.delta":
		delta, _ := str(ev, "delta")
		index := intOr0(ev, "output_index")
		return EncodeFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
		}), false
	case "response.function_call_arguments.done":
		index := intOr0(ev, "output_index")
		if m.blockStarted[index] && !m.blockStopped[index] {
			m.blockStopped[index] = true
			return EncodeFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}), false
		}
		return nil, true
	case "response.completed":
		if m.done {
			return nil, true
		}
		m.done = true
		var ot int64
		var reason string
		if resp, ok := ev["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				ot = intOr0(u, "output_tokens")
			}
			reason = respToMessStopReason(resp)
		}
		f := EncodeFrame("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": ot},
		})
		return append(f, EncodeFrame("message_stop", map[string]any{"type": "message_stop"})...), false
	case "response.failed":
		if m.done {
			return nil, true
		}
		m.done = true
		if resp, ok := ev["response"].(map[string]any); ok {
			if e, ok := resp["error"].(map[string]any); ok {
				msg, _ := str(e, "message")
				return EncodeFrame("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg}}), false
			}
		}
		return nil, true
	}
	return nil, true
}
