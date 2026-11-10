// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
	"strconv"

	"github.com/is7qin/c3api/internal/domain"
)

// chatToMessResponse Chat Completions 响应 → Anthropic Messages 响应（非流式）。
//
// 客户端说 Messages、上游只说 chat 时使用。与 chat→resp 的差别：mess 的停因是
// **必写**的 `stop_reason`（缺失/未知 → `end_turn`），`stop_sequence` 恒写 null
// （chat 不告知命中哪条 stop），内容装进 `content[]` 块数组。
func chatToMessResponse(body []byte) ([]byte, error) {
	msg, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(msg, "id")
	model, _ := str(msg, "model")
	choice := chatChoice0(msg, domain.ProtocolConvertMessToChat)
	inner := chatMessageOf(choice)
	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       chatToMessBlocks(inner),
		"stop_reason":   messStopFromChat(chatFinishReason(choice)),
		"stop_sequence": nil,
		"usage":         messUsageOf(chatUsageFrom(msg), domain.ProtocolConvertMessToChat),
	}
	return json.Marshal(out)
}

// chatToRespResponse Chat Completions 响应 → Responses 响应（非流式）。
//
// output 项顺序固定为 reasoning → message → function_call（与流式
// output_item.done 同序）：resp 客户端会按项类型累积，顺序换了不会错，但顺序
// 不稳会让"同一请求两次响应不同字节"，golden 也就不可比。
func chatToRespResponse(body []byte) ([]byte, error) {
	msg, err := decodeObj(body)
	if err != nil {
		return nil, err
	}
	id, _ := str(msg, "id")
	model, _ := str(msg, "model")
	choice := chatChoice0(msg, domain.ProtocolConvertRespToChat)
	inner := chatMessageOf(choice)
	out := map[string]any{
		"id":     id,
		"object": "response",
		"model":  model,
		"output": chatToRespOutput(id, inner),
		"usage":  respUsageOf(chatUsageFrom(msg), domain.ProtocolConvertRespToChat),
	}
	if created, ok := msg["created"]; ok && created != nil {
		out["created_at"] = created
	}
	status, incomplete := chatRespStatus(chatFinishReason(choice))
	out["status"] = status
	if incomplete != "" {
		out["incomplete_details"] = map[string]any{"reason": incomplete}
	}
	return json.Marshal(out)
}

// chatChoice0 取 choices[0]；`choices[1..]` 与 chat 独有但目标协议无载体的顶层
// 字段（system_fingerprint / service_tier / logprobs）丢弃并计数。
func chatChoice0(msg map[string]any, dir domain.ProtocolConvert) map[string]any {
	for _, f := range []string{"system_fingerprint", "service_tier"} {
		if v, ok := msg[f]; ok && v != nil {
			noteRespDrop(dir, f)
		}
	}
	choices, ok := arr(msg, "choices")
	if !ok || len(choices) == 0 {
		return map[string]any{}
	}
	if len(choices) > 1 {
		for i := 1; i < len(choices); i++ {
			noteRespDrop(dir, "choices_nonzero")
		}
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return map[string]any{}
	}
	if v, ok := c["logprobs"]; ok && v != nil {
		noteRespDrop(dir, "logprobs")
	}
	return c
}

// chatMessageOf choice.message（缺失 → 空对象，等价于"只有空内容"）。
func chatMessageOf(choice map[string]any) map[string]any {
	m, _ := choice["message"].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// chatFinishReason finish_reason 读字符串；缺失/非法 → ""（由各自的停因表兜底）。
func chatFinishReason(choice map[string]any) string {
	s, _ := str(choice, "finish_reason")
	return s
}

// chatToMessBlocks chat assistant message → Anthropic 内容块数组。
//
// 块序 thinking → text → tool_use：Anthropic 要求 assistant turn 里 thinking 在
// 最前（回放时否则被拒）；tool_use 在文本之后符合"先说再做"的常规形态。
// 空文本且无思考、无工具调用 → **仍写一个空 text 块**：部分 Messages 客户端与
// SDK 对 `content:[]` 报协议错，空串则能正常渲染出"无内容"。
func chatToMessBlocks(m map[string]any) []any {
	var think, rest []any
	if t, ok := str(m, "reasoning_content"); ok && t != "" {
		// chat 没有签名通道（reasoning_content 是第三方扩展，F5）：不发明
		// signature。客户端若回放该块，Anthropic 会 400 —— 这是 chat 源的既知
		// 限制，比编一个假签名让上游误判完整性要好。
		think = append(think, map[string]any{"type": "thinking", "thinking": t})
	}
	content, _ := str(m, "content")
	if content != "" {
		rest = append(rest, map[string]any{"type": "text", "text": content})
	}
	if tcs, ok := arr(m, "tool_calls"); ok {
		for _, tc := range tcs {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			rest = append(rest, chatToolCallToMessBlock(tcm))
		}
	}
	if len(rest) == 0 && len(think) == 0 {
		// 空响应：仍写一个空 text 块（不产 content:[]）。
		rest = append(rest, map[string]any{"type": "text", "text": ""})
	}
	return append(think, rest...)
}

// chatToolCallToMessBlock chat tool_call → Anthropic tool_use 块。
// arguments 是"装着 JSON 的字符串"：解析成对象；解析失败 → `input:{}` 并计
// bad_tool_arguments（不中断：一个坏工具调用不该让整条响应失败）。
func chatToolCallToMessBlock(tc map[string]any) map[string]any {
	id, _ := str(tc, "id")
	name := ""
	input := any(map[string]any{})
	if fn, ok := tc["function"].(map[string]any); ok && fn != nil {
		name, _ = str(fn, "name")
		args, ok := fn["arguments"]
		if ok {
			parsed, ok := parseArgsObject(args)
			if !ok {
				noteRespDrop(domain.ProtocolConvertMessToChat, "bad_tool_arguments")
			}
			input = parsed
		}
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// parseArgsObject tool_calls[].function.arguments → input 对象。
// 解析失败或非对象 → `{}`，第二返回值报告"是否走了兜底"。
func parseArgsObject(v any) (any, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return map[string]any{}, false
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]any{}, false
	}
	if _, ok := out.(map[string]any); !ok {
		// arguments 是 JSON 但不是对象（数组/标量）：Anthropic 的 input 必须是对象。
		return map[string]any{}, false
	}
	return out, true
}

// chatToRespOutput chat assistant message → Responses 的 output[]（顺序
// reasoning → message → function_call）。
//
// `output` 恒为**数组**（I6）：空响应（无文本、无 reasoning、无 tool_calls）
// 也必须是 `[]`，不是 `null` —— nil `[]any` 经 json.Marshal 得 `null`，而 spec
// 规定 Responses 的 `output` 是数组，客户端与官方 SDK 按数组反序列化，`null`
// 直接是协议错。故用 make 初始化成空切片，**不得**退回 `var out []any`。
func chatToRespOutput(id string, m map[string]any) []any {
	out := make([]any, 0, 3)
	if t, ok := str(m, "reasoning_content"); ok && t != "" {
		out = append(out, map[string]any{
			"type": "reasoning",
			// reasoning item 的 id 与流式 thinkID 同源（rs_<id>_<index>）。
			"id":      respItemID("rs_", id, 0),
			"summary": []any{map[string]any{"type": "summary_text", "text": t}},
		})
	}
	content, _ := str(m, "content")
	tcs, _ := arr(m, "tool_calls")
	// 空文本 → **不产** message 项：resp 的 output 允许空数组，而"空的 assistant
	// 项"反而是坏形态（不变量：assistant 项不得为空）。这一条与 mess 目标相反
	// （那边必须写空 text 块，否则 SDK 报协议错）。
	if content != "" {
		out = append(out, map[string]any{
			"type":   "message",
			"id":     respItemID("msg_", id, 0),
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": content, "annotations": []any{},
			}},
		})
	}
	for _, tc := range tcs {
		tcm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		callID, _ := str(tcm, "id")
		name := ""
		// arguments 是「装着 JSON 的字符串」，Responses 侧的 function_call
		// 要求**原样字符串**（spec §5.2）且不得编造（§7）：缺失/非字符串 →
		// 空串 ""，空串照样原样透传。绝不在这里补 `{}`（补出来的 `{}` 是
		// 客户端从未见过的参数，工具侧会按空参数执行）。
		args := ""
		if fn, ok := tcm["function"].(map[string]any); ok && fn != nil {
			name, _ = str(fn, "name")
			args, _ = str(fn, "arguments")
		}
		// 输出侧 item id 与流式同一规则（chatFCID = `fc_` + call_id）。已知且
		// 实测过的上游规则只有一条：`id` 一旦出现就必须以 `fc_` 开头——旧的
		// `call_…` 形态一旦被回放就会被 400。这里只是**对齐既有流式惯例**；
		// 合成出来的 `fc_` id 是否被上游接受（回放/存储引用）未经实测，不宣称。
		out = append(out, map[string]any{
			"type": "function_call", "id": chatFCID(callID), "call_id": callID,
			"name": name, "arguments": args, "status": "completed",
		})
	}
	return out
}

// respItemID 合成 Responses output item id（确定性：同一响应同一 index 恒同值，
// 与 StreamMapper.itemID/thinkID 同源格式）。
func respItemID(prefix, id string, index int64) string {
	return prefix + id + "_" + strconv.FormatInt(index, 10)
}

// messStopFromChat Chat finish_reason → Messages stop_reason（同一张表反查）。
//
// 表非单射（max_tokens 与 model_context_window_exceeded 都映到 length）⇒ 反查
// 只保证"对 chat 全集有定义"，不保证 round-trip 恒等（spec §5.3）。
// 未命中 → end_turn（**不得写空串**：空串不在 Anthropic 枚举内，SDK 会退化成
// "未知停因"）。
func messStopFromChat(chat string) string {
	for _, r := range messStopToChat {
		if r.chat == chat {
			return r.mess
		}
	}
	return "end_turn"
}

// chatRespStatus Chat finish_reason → Responses 的 (status, incomplete_details.reason)。
// 不由 messStopToChat 派生（那张表没有 status 列）。
func chatRespStatus(chat string) (string, string) {
	switch chat {
	case "length":
		return "incomplete", "max_output_tokens"
	}
	return "completed", ""
}

// chatUsage chat 的 usage 分量（gross / cached / write / output / reasoning）。
type chatUsage struct {
	gross, cached, write, out, reasoning int64
}

// chatUsageOf 读 chat 的 usage；没有 usage 对象 → ok=false（mess 目标仍要写
// usage，此时按全 0 写，见调用方）。
func chatUsageOf(m map[string]any) (chatUsage, bool) {
	u, ok := m["usage"].(map[string]any)
	if !ok || u == nil {
		return chatUsage{}, false
	}
	out := chatUsage{
		gross: intOr0(u, "prompt_tokens"),
		out:   intOr0(u, "completion_tokens"),
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok && d != nil {
		out.cached = intOr0(d, "cached_tokens")
		out.write = intOr0(d, "cache_write_tokens")
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok && d != nil {
		out.reasoning = intOr0(d, "reasoning_tokens")
	}
	return out, true
}

// net 由 gross 反推净额（口径见 protoconv.go 顶部）；病态上游（cached+write >
// gross）钳 0 并计数，避免负值进账。
func (u chatUsage) net(dir domain.ProtocolConvert) int64 {
	net := respInputNet(u.gross, u.cached, u.write)
	if u.gross < u.cached+u.write {
		noteRespDrop(dir, "usage_negative_clamped")
	}
	return net
}

// messUsageOf chat usage → Anthropic usage：三项恒写（含 0）。
// billing/cost.go 按 net + cache_read + cache_creation 分别计价，缺键即漏计。
//
// 这是 mess 目标**唯一**的用量构建器：流式（StreamMapper.chatMessUsage）与非流式
// （messUsageFromChat）都走它，于是 net 钳 0 的计数与 reasoning_tokens 的丢弃计数
// 只有一份判据（此前流式只在非零时计、非流式键存在就计 —— 同一份上游 payload
// 会得到两种计数）。
func messUsageOf(u chatUsage, dir domain.ProtocolConvert) map[string]any {
	if u.reasoning != 0 {
		// Messages 的 usage 没有推理 token 明细位；有**实际消耗**就必须可见
		// （值为 0 不构成信息丢失，不计数）。
		noteRespDrop(dir, "reasoning_tokens")
	}
	return map[string]any{
		"input_tokens":                u.net(dir),
		"cache_read_input_tokens":     u.cached,
		"cache_creation_input_tokens": u.write,
		"output_tokens":               u.out,
	}
}

// respUsageOf chat usage → Responses usage。
//
// `input_tokens_details` 的**两个键恒写（含 0）**：这是官方必填语义（F14），
// 与既有 respInputDetails（"write 仅 >0 时写"）故意不同 —— 旧方向不动，新方向
// 按官方来。Responses 有 reasoning_tokens 的承载位，故不丢弃、原值搬。
func respUsageOf(u chatUsage, dir domain.ProtocolConvert) map[string]any {
	net := u.net(dir)
	return map[string]any{
		"input_tokens": net,
		"input_tokens_details": map[string]any{
			"cached_tokens":      u.cached,
			"cache_write_tokens": u.write,
		},
		"output_tokens":         u.out,
		"total_tokens":          net + u.out,
		"output_tokens_details": map[string]any{"reasoning_tokens": u.reasoning},
	}
}

// chatUsageFrom 非流式：读 chat 响应的 usage 分量。上游没给 usage 时返回零值
// （三个键照样恒写 0，绝不留缺键 —— 缺键即漏计费）。
func chatUsageFrom(m map[string]any) chatUsage {
	u, _ := chatUsageOf(m)
	return u
}
