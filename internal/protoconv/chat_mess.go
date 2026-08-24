// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
)

// chatToMessRequest 客户端 chat 请求体 → anthropic messages 请求体。字段映射
// 按 Messages API 规范：
//   - system 消息 → 顶层 system（拼接）；user/assistant 文本 → 消息（文本块）；
//     assistant tool_calls → tool_use 块；tool 消息 → user 消息 tool_result 块
//   - max_completion_tokens / max_tokens → max_tokens（anthropic 必填，客户端
//     缺失则不补——补差值属策略决定，不由转换器发明）
//   - stop → stop_sequences（string 归一为数组）；tools → tools
//     （{type:"function"} 内嵌扁平化 → input_schema）；tool_choice 归一化
//     （required → any；{type:"function",name} → {type:"tool",name}）
//   - 同名字段透传：model/temperature/top_p/stream/metadata
//   - anthropic 无对应参数（n/seed/logprobs/frequency_penalty/
//     presence_penalty/stream_options/response_format/logit_bias/user 等）
//     → 按规范丢弃
func chatToMessRequest(body []byte) ([]byte, error) {
	req, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, 8)
	if sys, ok := chatSystem(req); ok {
		out["system"] = sys
	}
	if msgs, ok := chatMessagesToMess(req); ok {
		out["messages"] = msgs
	}
	pass(out, req, "model", "temperature", "top_p", "stream", "metadata")
	// max_completion_tokens / max_tokens → max_tokens（anthropic 必填，客户端
	// 缺失则不补——补差值属策略决定，不由转换器发明）。两者都给时以
	// max_completion_tokens 为准（与 chatToResp 同语义，M-2）。
	if v, ok := req["max_completion_tokens"]; ok {
		out["max_tokens"] = v
	} else if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if stop, ok := chatStop(req); ok {
		out["stop_sequences"] = stop
	}
	if tools, ok := arr(req, "tools"); ok {
		out["tools"] = chatToolsToMess(tools)
	}
	if tc, ok := chatToolChoiceMess(req); ok {
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// chatSystem chat system 消息（role=system）→ 顶层 system 拼接文本。
func chatSystem(req map[string]any) (string, bool) {
	msgs, ok := arr(req, "messages")
	if !ok {
		return "", false
	}
	var parts []string
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := str(mm, "role"); role == "system" {
			if t, ok := contentText(mm["content"]); ok {
				parts = append(parts, t)
			}
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return joinStrings(parts, "\n"), true
}

// chatMessagesToMess chat messages → anthropic messages（system 已并入顶层
// system，此处跳过）：user 文本 → 消息（单文本块 → string content）；
// assistant 文本 → 消息 + tool_calls → tool_use 块（arguments JSON 字符串 →
// input 对象）；tool 消息 → user 消息 tool_result 块；image_url 等部件按
// 规范丢弃（图像透传属 W4 范围）。
func chatMessagesToMess(req map[string]any) ([]any, bool) {
	msgs, ok := arr(req, "messages")
	if !ok {
		return nil, false
	}
	var out []any
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch role, _ := str(mm, "role"); role {
		case "system":
			continue // 已并入顶层 system
		case "user":
			content := mm["content"]
			if s, ok := content.(string); ok {
				out = append(out, map[string]any{"role": "user", "content": s})
				continue
			}
			blocks := chatPartsToMessBlocks(content)
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "user", "content": blocks})
			}
		case "assistant":
			blocks := chatPartsToMessBlocks(mm["content"])
			if s, ok := mm["content"].(string); ok && s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
			if tcs, ok := arr(mm, "tool_calls"); ok {
				for _, tc := range tcs {
					tcm, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					id, _ := str(tcm, "id")
					if fn, ok := tcm["function"].(map[string]any); ok {
						name, _ := str(fn, "name")
						blocks = append(blocks, map[string]any{
							"type": "tool_use", "id": id, "name": name, "input": parseJSON(fn["arguments"]),
						})
					}
				}
			}
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": blocks})
			}
		case "tool":
			if outText, ok := contentText(mm["content"]); ok {
				callID, _ := str(mm, "tool_call_id")
				out = append(out, map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": outText}},
				})
			}
		case "function":
			// 已废弃 chat function 消息 → tool_result（tool_use_id 用 name 近似）
			if outText, ok := contentText(mm["content"]); ok {
				name, _ := str(mm, "name")
				out = append(out, map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "tool_result", "tool_use_id": name, "content": outText}},
				})
			}
		}
	}
	return out, true
}

// chatPartsToMessBlocks chat 消息 content 部件 → anthropic 内容块（text →
// text 块；image_url 等按规范丢弃）。
func chatPartsToMessBlocks(content any) []any {
	cs, ok := content.([]any)
	if !ok {
		return nil
	}
	var blocks []any
	for _, p := range cs {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if pm["type"] == "text" {
			if t, ok := str(pm, "text"); ok {
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			}
		}
	}
	return blocks
}

// chatStop chat stop（string 或数组）→ anthropic stop_sequences（数组）。
func chatStop(req map[string]any) ([]any, bool) {
	v, ok := req["stop"]
	if !ok || v == nil {
		return nil, false
	}
	switch s := v.(type) {
	case string:
		return []any{s}, true
	case []any:
		return s, true
	}
	return nil, false
}

// chatToolsToMess chat tools → anthropic tools：{type:"function", function:
// {name, description, parameters}} → {name, description, input_schema}；
// 非 function 工具按规范丢弃。
func chatToolsToMess(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		tool := map[string]any{}
		if name, ok := str(fn, "name"); ok {
			tool["name"] = name
		}
		if desc, ok := str(fn, "description"); ok {
			tool["description"] = desc
		}
		if p, ok := fn["parameters"].(map[string]any); ok {
			tool["input_schema"] = p
		}
		out = append(out, tool)
	}
	return out
}

// chatToolChoiceMess chat tool_choice → anthropic tool_choice："auto"/"none"
// 透传；"required" → "any"；{type:"function", function:{name}} →
// {type:"tool", name}。
func chatToolChoiceMess(req map[string]any) (any, bool) {
	v, ok := req["tool_choice"]
	if !ok || v == nil {
		return nil, false
	}
	if s, ok := v.(string); ok {
		if s == "required" {
			return "any", true
		}
		return s, true
	}
	if m, ok := v.(map[string]any); ok && m["type"] == "function" {
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := str(fn, "name"); ok {
				return map[string]any{"type": "tool", "name": name}, true
			}
		}
	}
	return nil, false
}

// messToChatResponse anthropic message 对象 → chat completion 对象（非流式）：
// text 块拼接 content；tool_use → tool_calls（input 对象 → arguments JSON
// 字符串）；stop_reason → finish_reason；usage 同构映射。
func messToChatResponse(body []byte) ([]byte, error) {
	msg, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(msg, "id")
	model, _ := str(msg, "model")
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": 0, // anthropic message 无时间戳（转换器纯函数，不发明）
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       messToChatMessage(msg),
			"finish_reason": messToChatFinishReason(msg),
		}},
	}
	if u, ok := messUsageToChat(msg); ok {
		out["usage"] = u
	}
	return json.Marshal(out)
}

// messToChatMessage anthropic content → chat assistant message（text 块拼接
// content；tool_use → tool_calls）。
func messToChatMessage(msg map[string]any) map[string]any {
	m := map[string]any{"role": "assistant", "content": ""}
	var text []string
	var tcs []any
	if content, ok := arr(msg, "content"); ok {
		for _, blk := range content {
			bm, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			switch bm["type"] {
			case "text":
				if t, ok := str(bm, "text"); ok {
					text = append(text, t)
				}
			case "tool_use":
				id, _ := str(bm, "id")
				name, _ := str(bm, "name")
				tcs = append(tcs, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": marshalAny(bm["input"])},
				})
			}
		}
	}
	m["content"] = joinStrings(text, "")
	if len(tcs) > 0 {
		m["tool_calls"] = tcs
	}
	return m
}

// messToChatFinishReason anthropic stop_reason → chat finish_reason：
// end_turn → "stop"；tool_use → "tool_calls"；max_tokens → "length"；
// stop_sequence → "stop"。
func messToChatFinishReason(msg map[string]any) string {
	switch reason, _ := str(msg, "stop_reason"); reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// messUsageToChat anthropic usage → chat usage。
func messUsageToChat(msg map[string]any) (map[string]any, bool) {
	u, ok := msg["usage"].(map[string]any)
	if !ok || u == nil {
		return nil, false
	}
	it := intOr0(u, "input_tokens")
	ot := intOr0(u, "output_tokens")
	out := map[string]any{
		"prompt_tokens":     it,
		"completion_tokens": ot,
		"total_tokens":      it + ot,
	}
	if c := intOr0(u, "cache_read_input_tokens"); c > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": c}
	}
	return out, true
}

// mapMessToChat 流式：anthropic messages SSE 事件 → chat 流。事件映射表：
//
//	message_start             → 角色前导 chunk（delta.role=assistant）
//	content_block_start       → tool_use → tool_calls 前导 chunk（id+name）
//	content_block_delta       → text_delta → content delta chunk /
//	                            input_json_delta → tool_calls arguments delta
//	message_delta             → 收尾 chunk（finish_reason+usage）+ [DONE]
//	message_stop              → 丢弃（收尾已在 message_delta 发出）
//	error                     → data-only {"error":{...}} 帧（chat 流式错误约定）
//	其余 → 丢弃
func (m *StreamMapper) mapMessToChat(name string, data []byte) ([]byte, bool) {
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
				m.it = intOr0(u, "input_tokens")
				m.cached = intOr0(u, "cache_read_input_tokens")
			}
		}
		return m.chatFrame(map[string]any{"role": "assistant", "content": ""}, nil, nil), false
	case "content_block_start":
		block, ok := ev["content_block"].(map[string]any)
		if !ok || block["type"] != "tool_use" {
			return nil, true
		}
		id, _ := str(block, "id")
		name, _ := str(block, "name")
		index := intOr0(ev, "index")
		return m.chatFrame(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": ""},
		}}}, nil, nil), false
	case "content_block_delta":
		delta, ok := ev["delta"].(map[string]any)
		if !ok {
			return nil, true
		}
		switch delta["type"] {
		case "text_delta":
			text, _ := str(delta, "text")
			return m.chatFrame(map[string]any{"content": text}, nil, nil), false
		case "input_json_delta":
			partial, _ := str(delta, "partial_json")
			index := intOr0(ev, "index")
			return m.chatFrame(map[string]any{"tool_calls": []any{map[string]any{
				"index": index, "function": map[string]any{"arguments": partial},
			}}}, nil, nil), false
		}
		return nil, true
	case "message_delta":
		if m.done {
			return nil, true
		}
		m.done = true
		var reason any = "stop"
		if d, ok := ev["delta"].(map[string]any); ok {
			switch r, _ := str(d, "stop_reason"); r {
			case "tool_use":
				reason = "tool_calls"
			case "max_tokens":
				reason = "length"
			}
		}
		var ot int64
		if u, ok := ev["usage"].(map[string]any); ok {
			ot = intOr0(u, "output_tokens")
		}
		m.ot = ot
		usage := map[string]any{
			"prompt_tokens": m.it, "completion_tokens": m.ot, "total_tokens": m.it + m.ot,
		}
		if m.cached > 0 {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": m.cached}
		}
		return append(m.chatFrame(map[string]any{}, reason, usage), []byte("data: [DONE]\n\n")...), false
	case "error":
		if e, ok := ev["error"].(map[string]any); ok {
			msg, _ := str(e, "message")
			return EncodeFrame("", map[string]any{"error": map[string]any{"message": msg}}), false
		}
		return nil, true
	}
	return nil, true
}
