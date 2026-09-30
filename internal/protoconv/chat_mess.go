// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
)

// chatToMessRequest 客户端 chat 请求体 → anthropic messages 请求体。字段映射
// 按 Messages API 规范：
//   - system / developer 消息 → 顶层 system（按出现顺序以 \n 拼接）；
//     user/assistant 文本 → 消息（文本块）；assistant tool_calls → tool_use 块；
//     tool 消息 → user 消息 tool_result 块；image_url → image 块
//   - max_completion_tokens / max_tokens → max_tokens（anthropic 必填；
//     两者都缺时补常量 4096；两者都给时 max_completion_tokens 优先）
//   - stop → stop_sequences（string 归一为数组）；tools → tools
//     （{type:"function"} 内嵌扁平化 → input_schema）；tool_choice 归一化为
//     对象（auto/none 同名；required → any；{type:"function",name} →
//     {type:"tool",name}）
//   - 同名字段透传：model/temperature/top_p/stream/metadata
//   - anthropic 无对应参数（n/seed/logprobs/frequency_penalty/
//     presence_penalty/stream_options/response_format/logit_bias/user 等）
//     → 按规范丢弃
//
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
	m := map[string]any{"role": "assistant", "content": nil}
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
	if len(text) > 0 {
		m["content"] = joinStrings(text, "")
	}
	if len(tcs) > 0 {
		m["tool_calls"] = tcs
	}
	return m
}

// messToChatFinishReason anthropic stop_reason → chat finish_reason。
func messToChatFinishReason(msg map[string]any) string {
	reason, _ := str(msg, "stop_reason")
	return messStopToChatFinish(reason)
}

// messUsageToChat anthropic usage → chat usage。anthropic input_tokens 是 net，
// chat prompt_tokens 为 gross（口径见 protoconv.go 顶部）：prompt = input +
// cache_creation + cache_read；cached_tokens 只取 cache_read。
func messUsageToChat(msg map[string]any) (map[string]any, bool) {
	u, ok := msg["usage"].(map[string]any)
	if !ok || u == nil {
		return nil, false
	}
	it := intOr0(u, "input_tokens")
	ot := intOr0(u, "output_tokens")
	cr := intOr0(u, "cache_read_input_tokens")
	cc := intOr0(u, "cache_creation_input_tokens")
	prompt := respInputGross(it, cr, cc)
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": ot,
		"total_tokens":      prompt + ot,
	}
	if cr > 0 || cc > 0 {
		out["prompt_tokens_details"] = chatPromptDetails(cr, cc)
	}
	return out, true
}

// chatPromptDetails chat usage 的 prompt_tokens_details：cached_tokens 恒含；
// cache_write_tokens 仅在非 0 时出现（无缓存写入的响应不多出零值键——零值策略
// 与 resp_mess.go respInputDetails 一致）。
func chatPromptDetails(cr, cc int64) map[string]any {
	d := map[string]any{"cached_tokens": cr}
	if cc > 0 {
		d["cache_write_tokens"] = cc
	}
	return d
}

// mapMessToChat 流式：anthropic messages SSE 事件 → chat 流。事件映射表：
//
//	message_start             → 角色前导 chunk（delta.role=assistant）
//	content_block_start       → tool_use → tool_calls 前导 chunk（id+name）
//	content_block_delta       → text_delta → content delta chunk /
//	                            input_json_delta → tool_calls arguments delta
//	message_delta             → 收尾 chunk（finish_reason，不含 usage）
//	                            + 单独 usage 帧（choices 为空）+ [DONE]
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
				m.setMessUsage(u)
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
		reason := "stop"
		if d, ok := ev["delta"].(map[string]any); ok {
			r, _ := str(d, "stop_reason")
			reason = messStopToChatFinish(r)
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			m.ot = intOr0(u, "output_tokens")
			// message_delta 带累计字段时覆盖 message_start。
			m.mergeMessUsage(u)
		}
		prompt := m.messInputTotal()
		usage := map[string]any{
			"prompt_tokens": prompt, "completion_tokens": m.ot, "total_tokens": prompt + m.ot,
		}
		if m.cached > 0 || m.cacheCreate > 0 {
			usage["prompt_tokens_details"] = chatPromptDetails(m.cached, m.cacheCreate)
		}
		// finish 帧不含 usage；下一帧 choices 为空且只含 usage。
		finish := m.chatFrame(map[string]any{}, reason, nil)
		usageFrame := m.chatUsageOnlyFrame(usage)
		out := append(finish, usageFrame...)
		return append(out, []byte("data: [DONE]\n\n")...), false
	case "error":
		if e, ok := ev["error"].(map[string]any); ok {
			msg, _ := str(e, "message")
			return EncodeFrame("", map[string]any{"error": map[string]any{"message": msg}}), false
		}
		return nil, true
	}
	return nil, true
}
