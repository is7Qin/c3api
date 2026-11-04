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
//
// respToMessResponse resp 响应对象 → anthropic message 对象（非流式）：
// output reasoning 项 → thinking 块（encrypted_content 封进 signature 的
// reasoning_ref 臂，spec §2.5）；message 项 → text 块；function_call →
// tool_use 块（arguments JSON 字符串 → input 对象）；usage → input/output/cache
// 字段。
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

// respOutputToMessBlocks resp output → anthropic 内容块（thinking → thinking 块，
// text → text 块，function_call → tool_use 块）。
//
// thinking 块**恒排在其余块之前**（Anthropic 要求 assistant turn 里 thinking 在
// 最前，否则回放时被拒），多个 reasoning item 之间保持上游顺序。
func respOutputToMessBlocks(r map[string]any) []any {
	var think, rest []any
	if output, ok := arr(r, "output"); ok {
		for _, item := range output {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch im["type"] {
			case "reasoning":
				// reasoning item → thinking 块（spec §4.2 / §2.5）：encrypted_content
				// 非空 → signature 放 reasoning_ref 臂（放内容原文会与 Anthropic
				// 签名混淆），客户端原样回传后 mess_to_resp 请求侧即可还原同一个
				// item；空/缺失 → 按约束 B 输出 signature:""（字段必须在，缺失才被
				// 拒）。文本 summary[].text 优先、content[].reasoning_text 兜底；
				// 文本与臂都没有 → 该块无可承载的内容，丢。
				text := respReasoningTextOf(im)
				sig := ""
				if enc, _ := str(im, "encrypted_content"); enc != "" {
					id, _ := str(im, "id")
					sig = sealReasoningRef(reasoningRef{EncryptedContent: enc, ID: id})
				}
				if text == "" && sig == "" {
					continue
				}
				think = append(think, thinkingBlockMap(thinkingBlock{
					Type: "thinking", Thinking: text, Signature: sig,
				}))
			case "message":
				if content, ok := arr(im, "content"); ok {
					for _, part := range content {
						if pm, ok := part.(map[string]any); ok {
							if t, ok := str(pm, "text"); ok {
								rest = append(rest, map[string]any{"type": "text", "text": t})
							}
						}
					}
				}
			case "function_call":
				id := toolCallID(im) // call_id 优先（tool_result.tool_use_id 匹配键）
				name, _ := str(im, "name")
				rest = append(rest, map[string]any{
					"type": "tool_use", "id": id, "name": name, "input": parseJSON(im["arguments"]),
				})
			}
		}
	}
	blocks := append(think, rest...)
	if len(blocks) == 0 {
		return []any{}
	}
	return blocks
}

// respToMessStopReason resp 状态/输出 → anthropic stop_reason。status 已经是
// incomplete 时只看 incomplete_details.reason，不再看 function_call。
func respToMessStopReason(r map[string]any) string {
	if status, _ := str(r, "status"); status == "incomplete" {
		reason, _ := str(mapOrEmpty(r["incomplete_details"]), "reason")
		return messStopFromIncomplete(reason)
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

func mapOrEmpty(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// respUsageToMess resp usage → anthropic usage。resp 侧 input_tokens 是 gross
// （含缓存，口径见 protoconv.go 顶部）；anthropic 侧 input_tokens 是 net：
// input_tokens = it − cache_read − cache_write，cache_read 取 cached_tokens、
// cache_creation 取 cache_write_tokens（anthropic 响应四字段全含）。
func respUsageToMess(r map[string]any) map[string]any {
	it, ot := int64(0), int64(0)
	cached, write := int64(0), int64(0)
	if u, ok := r["usage"].(map[string]any); ok {
		it = intOr0(u, "input_tokens")
		ot = intOr0(u, "output_tokens")
		if d, ok := u["input_tokens_details"].(map[string]any); ok {
			cached = intOr0(d, "cached_tokens")
			write = intOr0(d, "cache_write_tokens")
		}
	}
	return map[string]any{
		"input_tokens":                respInputNet(it, cached, write),
		"output_tokens":               ot,
		"cache_creation_input_tokens": write,
		"cache_read_input_tokens":     cached,
	}
}

// eventBlockIndex resp 事件 → content index：跨事件身份优先用 item_id（spec
// §4.5 第 2 条），output_index 只作分配基值。
func (m *StreamMapper) eventBlockIndex(ev map[string]any, kind string) int64 {
	oi := intOr0(ev, "output_index")
	itemID, _ := str(ev, "item_id")
	return m.ownBlockIndex(blockKeyOf(itemID, oi, kind), oi, kind)
}

// thinkBlockIndex reasoning 事件 → content index。ok=false 表示该块被 thinking
// 门丢弃（迟到的 thinking 块整块丢弃，spec §4.5 第 4 条）。
//
// 丢弃**不写** itemIndex：键是 (id|output_index, kind)，正文事件可能与 reasoning
// 共用 id，写负索引占位会让正文落到非法 index 上（负 index 是协议外值）。重复
// 丢弃由 nonThinkSeen 门 + 调用方的 thinkBlocks 归属判据共同保证。
func (m *StreamMapper) thinkBlockIndex(key blockKey, oi int64) (int64, bool) {
	if idx, ok := m.itemIndex[key]; ok {
		return idx, true
	}
	if m.nonThinkSeen {
		return 0, false
	}
	index := m.blockIndex(key, oi, kindThinking)
	if m.thinkBlocks[index] == nil {
		m.thinkBlocks[index] = &thinkingBlock{Type: "thinking"}
	}
	return index, true
}

// stopBlock content_block_stop 帧：已 start 且未 stop 才发，置位防二次 stop
// （spec §4.5 第 5 条）。调用方据 nil 判定「无帧可发」。
func (m *StreamMapper) stopBlock(index int64) []byte {
	if !m.blockStarted[index] || m.blockStopped[index] {
		return nil
	}
	m.blockStopped[index] = true
	return EncodeFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

// mapRespToMess 流式：resp SSE 事件 → anthropic messages 流。事件映射表：
//
//	response.created                 → message_start（usage.input_tokens 在响应
//	                                  完成前不可知 → 0，补差映射已知取舍；网关
//	                                  计费独立于客户端用量展示）
//	response.output_item.added(reasoning) → content_block_start(thinking)，受
//	                                  thinking 门约束（§4.5 第 4 条）
//	response.reasoning_summary_text.delta / reasoning_text.delta
//	                                 → content_block_delta(thinking_delta)
//	response.output_item.done(reasoning) → signature_delta（= reasoning_ref 臂，
//	                                  仅 encrypted_content 非空）++ content_block_stop
//	response.output_text.delta       → content_block_start(按 item id / output_index
//	                                  分配) 惰性 + content_block_delta(text_delta)
//	response.output_text.done        → content_block_stop(同一 content index)
//	response.output_item.added(FC)   → content_block_start(tool_use)
//	response.function_call_arguments.delta → content_block_delta(input_json_delta)
//	response.function_call_arguments.done  → content_block_stop(同一 content index)
//	response.completed               → message_delta（stop_reason + 按 P1-3 换算的 usage）
//	                                  + message_stop；未收尾的 thinking 块补 stop
//	response.failed                  → error 事件（无 error 时 message 为空字符串）
//	其余 → 丢弃
//
// 跨事件身份一律用 item id 查表（§4.5 第 2 条），output_index 只作 content
// index 的候选基值——上游省略/错配 output_index 时 text_delta 不会落进 thinking 块。
func (m *StreamMapper) mapRespToMess(name string, data []byte) ([]byte, bool) {
	m.ensureBlocks() // 块级累积 map 懒初始化
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
		index := m.eventBlockIndex(ev, kindText)
		m.nonThinkSeen = true // 文本块开始 → thinking 门关闭
		if m.blockStopped[index] {
			return nil, true
		}
		var f []byte
		if !m.blockStarted[index] {
			m.blockStarted[index] = true
			f = EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		f = append(f, EncodeFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		})...)
		return f, false
	case "response.output_text.done":
		m.nonThinkSeen = true
		if f := m.stopBlock(m.eventBlockIndex(ev, kindText)); f != nil {
			return f, false
		}
		return nil, true
	case "response.output_item.added":
		item, ok := ev["item"].(map[string]any)
		if !ok {
			return nil, true
		}
		oi := intOr0(ev, "output_index")
		itemID, _ := str(item, "id")
		switch item["type"] {
		case "reasoning":
			key := blockKeyOf(itemID, oi, kindThinking)
			if m.nonThinkSeen {
				// thinking 门：该响应已有非 thinking 块 → 整块丢弃（spec §4.5 第 4
				// 条）。**不写** itemIndex：键与正文共享 id 空间，占位值会让正文事件
				// 解到非法/别的块的 index 上；后续 delta / done 由同一扇门与归属判据
				// 拦下。
				return nil, true
			}
			index := m.blockIndex(key, oi, kindThinking)
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			m.thinkBlocks[index] = &thinkingBlock{Type: "thinking"}
			// added 是 in-progress 展示项，客户端不写历史 → R1 不适用（§2.4）。
			return EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			}), false
		case "message":
			// 登记身份映射（后续 text delta 用同一 index）；块 start 仍惰性在首个
			// delta（保持既有帧序列）。message 项即文本块 → 门关闭。
			m.nonThinkSeen = true
			m.ownBlockIndex(blockKeyOf(itemID, oi, kindText), oi, kindText)
			return nil, true
		case "function_call":
			m.nonThinkSeen = true
			index := m.ownBlockIndex(blockKeyOf(itemID, oi, kindToolUse), oi, kindToolUse)
			if m.blockStarted[index] {
				return nil, true
			}
			m.blockStarted[index] = true
			id := toolCallID(item) // call_id 优先（tool_result.tool_use_id 匹配键）
			name, _ := str(item, "name")
			return EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
			}), false
		}
		return nil, true
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// resp 的思考文本 → Anthropic thinking_delta。块身份用 item_id 查表
		// （spec §4.5 第 2 条），块 start 惰性发出（兼容缺 output_item.added 的
		// 非规范上游）。
		oi := intOr0(ev, "output_index")
		itemID, _ := str(ev, "item_id")
		index, ok := m.thinkBlockIndex(blockKeyOf(itemID, oi, kindThinking), oi)
		if !ok || m.thinkBlocks[index] == nil || m.blockStopped[index] {
			// 与 done 分支同一条归属判据：id 形态的身份键不含 kind，reasoning 事件
			// 的 item_id 一旦与 message / function_call 撞车，解析出的 index 属于
			// text/tool_use 块——往里塞 thinking_delta 会污染该块。
			return nil, true
		}
		delta, _ := str(ev, "delta")
		var f []byte
		if !m.blockStarted[index] {
			m.blockStarted[index] = true
			f = EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			})
		}
		f = append(f, EncodeFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
		})...)
		return f, false
	case "response.output_item.done":
		// thinking 块收尾：encrypted_content 非空 → 先发 signature_delta（值 =
		// reasoning_ref 臂，spec §2.5）再 content_block_stop（mess.txt:8332：签名
		// 必须在块 stop 之前）；空/缺失 → 只 stop。已 stop 的 index 不再补。
		item, ok := ev["item"].(map[string]any)
		if !ok || item["type"] != "reasoning" {
			// 其余项的收尾走 output_text.done / function_call_arguments.done。
			return nil, true
		}
		oi := intOr0(ev, "output_index")
		itemID, _ := str(item, "id")
		index, ok := m.thinkBlockIndex(blockKeyOf(itemID, oi, kindThinking), oi)
		if !ok || m.thinkBlocks[index] == nil || m.blockStopped[index] {
			// 门丢弃 / 该 index 不是 thinking 块 / 已 stop（同一 index 只 stop 一次）。
			//
			// thinkBlocks 判据不可省：id 形态的身份键不含 kind，reasoning item 的 id
			// 一旦与某个 message / function_call 的 id 撞车（非规范中转会发生），
			// thinkBlockIndex 就返回 text/tool_use 块的 index——在这里发
			// signature_delta + stop 会把正文块提前关掉，后续正文 delta 全被丢弃，
			// 客户端只拿到半截正文。
			return nil, true
		}
		var f []byte
		if !m.blockStarted[index] {
			// 上游只发 done（无 added、无 delta）时必须补惰性 start：否则
			// signature_delta / stop 会落在从未 start 的 index 上，客户端组不出
			// 合法块（§4.5 第 5 条、门禁 §6.3 #28）。与 delta 路径同一口径。
			m.blockStarted[index] = true
			f = EncodeFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			})
		}
		if enc, _ := str(item, "encrypted_content"); enc != "" {
			if arm := sealReasoningRef(reasoningRef{EncryptedContent: enc, ID: itemID}); arm != "" {
				f = append(f, EncodeFrame("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": index,
					"delta": map[string]any{"type": "signature_delta", "signature": arm},
				})...)
			}
		}
		m.blockStopped[index] = true
		return append(f, EncodeFrame("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		})...), false
	case "response.function_call_arguments.delta":
		m.nonThinkSeen = true
		index := m.eventBlockIndex(ev, kindToolUse)
		if m.blockStopped[index] {
			return nil, true
		}
		delta, _ := str(ev, "delta")
		return EncodeFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
		}), false
	case "response.function_call_arguments.done":
		m.nonThinkSeen = true
		if f := m.stopBlock(m.eventBlockIndex(ev, kindToolUse)); f != nil {
			return f, false
		}
		return nil, true
	case "response.completed":
		if m.done {
			return nil, true
		}
		m.done = true
		var reason string
		usage := respUsageToMess(nil)
		if resp, ok := ev["response"].(map[string]any); ok {
			usage = respUsageToMess(resp)
			reason = respToMessStopReason(resp)
		}
		f := m.closeOpenThinkBlocks() // 兜底：上游未发 output_item.done 时补 stop
		f = append(f, EncodeFrame("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
			"usage": usage,
		})...)
		return append(f, EncodeFrame("message_stop", map[string]any{"type": "message_stop"})...), false
	case "response.failed":
		if m.done {
			return nil, true
		}
		m.done = true
		msg := ""
		if resp, ok := ev["response"].(map[string]any); ok {
			if e, ok := resp["error"].(map[string]any); ok {
				msg, _ = str(e, "message")
			}
		}
		return EncodeFrame("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg}}), false
	}
	return nil, true
}
