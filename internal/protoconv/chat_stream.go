// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"fmt"
	"sort"

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
		noteRespDrop(m.dir, "frame_after_terminal")
		return nil, true
	}
	if string(data) == "[DONE]" { // chat 流的唯一显式终止标记
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

// chatChunk 一个 chat.completion.chunk → 客户端帧。
func (m *StreamMapper) chatChunk(ev map[string]any) ([]byte, bool) {
	if id, ok := str(ev, "id"); ok && m.id == "" {
		m.id = id
	}
	if model, ok := str(ev, "model"); ok && m.model == "" {
		m.model = model
	}
	// I7：created 的判据是**源帧有没有这个键**，不是值是否为 0 —— 存在才映射成
	// created_at，缺省就省略该字段（转换器不发明时间戳）。必须在这里、chatStart
	// 之前读：response.created 首帧就要用到（末帧/EOF 收尾时也已就位）。
	if !m.createdSet {
		if v, ok := num(ev, "created"); ok {
			m.created, m.createdSet = int64(v), true
		}
	}
	// choices 缺失/空 ⇒ 只在 usage 是**对象**时取用量（include_usage 的末帧形态）。
	// 注意（F17）：开了 include_usage 后其它帧都带 `"usage":null` —— 键存在、值
	// 为 null，绝不能当用量帧处理，否则每帧都会刷新一次用量。
	choices, hasChoices := arr(ev, "choices")
	if !hasChoices || len(choices) == 0 {
		if u, ok := ev["usage"].(map[string]any); ok && u != nil {
			m.absorbChatUsage(u)
		}
		return nil, true
	}
	if len(choices) > 1 {
		for i := 1; i < len(choices); i++ {
			noteRespDrop(m.dir, "choices_nonzero")
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
			"response": m.chatCreatedAt(map[string]any{
				"id": m.id, "object": "response", "status": "in_progress",
				"model": m.model, "output": []any{}, "usage": nil,
			}),
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

// chatFCID 由 call id 派生的 function_call 项 id（`fc_` + call_id，不带 index）。
// 正常情况（call id 随首片到达）下与非流式 chatToRespOutput 的写法一致；call id
// 迟到时见 chatToolItemID（item id 已提前定死，不再由 call id 派生）。
func chatFCID(callID string) string { return "fc_" + callID }

// chatToolBlock 一个 chat 工具调用块（内容 index + 元数据 + 已拼好的 arguments）。
type chatToolBlock struct {
	idx  int64  // 内容 index（chatAlloc 分配，块列表顺序即分配序）
	iid  string // 对客户端的块身份：resp 目标 = function_call item id；mess 目标 = tool_use.id
	id   string // call_id（迟到时回填；resp 目标用作 call_id 字段）
	name string // function.name（迟到时回填）
	args string // arguments 累积
}

// chatToolItemID 决定工具块对客户端的身份，**在 start 帧发出前一次定死**：
//
//   - resp 目标：正常由 call id 派生（fc_ + call_id，与非流式同源）。call id 迟到
//     （非规范上游）时改用响应 id + 内容 index 合成 —— item id 一旦发出就不能再
//     变（客户端按 item_id 归并增量），而 call_id 之后仍可回填。
//   - mess 目标：tool_use.id 是客户端回传 tool_result 的匹配键，同样必须从 start
//     起稳定；迟到时用同一确定性规则占位。上游 chat 只要求「同一请求内调用与结果
//     的 id 配对」，字符串本身不参与校验，故占位与后续回传一致即可。
func (m *StreamMapper) chatToolItemID(b *chatToolBlock, callID string) string {
	if m.dir == domain.ProtocolConvertRespToChat {
		if callID != "" {
			return chatFCID(callID)
		}
		return chatFCID(m.id + "_" + fmt.Sprint(b.idx))
	}
	if callID != "" {
		return callID
	}
	return "call_" + m.id + "_" + fmt.Sprint(b.idx)
}

// chatToolCall 工具调用增量。chat 的 tool_calls 分片到达：首片带 id/name，之后
// 只带 arguments 片段。
//
// 归并（I5）：块列表 + 一张 index 表，命中顺序 = 按 id 扫块列表 → 按
// tool_calls[i].index 查表 → 都没有才新开块：
//   - 同一工具的分片中途改了 index（非规范上游常见）时，按 id 落回原块，
//     绝不开新块（否则一个应答被切成两个 tool_use）；
//   - 只带 index、不带 id 的续片凭 index 表落回原块（第三片带的是**新**
//     index，所以命中分支必须把新 index 登记回原块）；
//   - 首片没带 id/name（迟到）时，后续分片的 id/name 写回块元数据；块身份
//     （chatToolItemID）在 start 帧发出前已定死，迟到只回填 call_id，不改 item id
//     —— 也不把 start 帧推迟（推迟会让后到的块先 start，Anthropic 客户端按
//     content_block_start 的顺序追加块，索引会错位）。
//
// 新块的内容 index 由 chatAlloc 单调分配 ⇒ 块列表顺序即首片到达序，start 帧与
// 终态 output 次序都不依赖 map 迭代顺序。
func (m *StreamMapper) chatToolCall(pos int, tcm map[string]any) []byte {
	key := int64(pos)
	if _, ok := tcm["index"]; ok {
		key = intOr0(tcm, "index")
	}
	id, _ := str(tcm, "id")
	name := ""
	args := ""
	if fn, ok := tcm["function"].(map[string]any); ok && fn != nil {
		name, _ = str(fn, "name")
		args, _ = str(fn, "arguments")
	}
	// 归并顺序：id 扫块列表 → index 表 → 都没有才新开块。空 id 的块不参与 id
	// 命中（判据 b.id != ""），否则后续同样不带 id 的分片会撞上这个空键。
	bi := -1
	if id != "" {
		for i, b := range m.tools {
			if b.id != "" && b.id == id {
				bi = i
				break
			}
		}
	}
	if bi < 0 {
		if i, ok := m.toolIdx[key]; ok {
			bi = i
		}
	}
	if bi >= 0 {
		b := m.tools[bi]
		// 同一块可能被多个 chat index 指着：新 index 也必须登记，只带新 index、
		// 不带 id 的续片才落得回原块。
		m.toolIdx[key] = bi
		// 迟到的 id/name 回填（终态 item 不能带空值）。**不动 b.iid**：start 帧
		// 已用当时的身份发出，流内身份必须自始至终一致。
		if b.id == "" {
			b.id = id
		}
		if b.name == "" {
			b.name = name
		}
		if args == "" {
			return nil
		}
		b.args += args
		return m.chatToolDelta(b, args)
	}
	b := &chatToolBlock{idx: m.chatAlloc(), iid: "", id: id, name: name, args: args}
	b.iid = m.chatToolItemID(b, id)
	m.tools = append(m.tools, b)
	m.toolIdx[key] = len(m.tools) - 1
	return m.chatToolStart(b, args)
}

// chatToolStart 发工具块的 start 帧（resp 目标 = output_item.added；mess 目标 =
// content_block_start）。身份取 b.iid，首片参数作为第一条增量一并写出。
func (m *StreamMapper) chatToolStart(b *chatToolBlock, args string) []byte {
	// mess 目标的 content_block_stop 由这张表驱动（closeOpenBlocks）。
	m.blockStarted[b.idx] = true
	if m.dir == domain.ProtocolConvertRespToChat {
		out := EncodeFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": b.idx,
			"item": map[string]any{
				"id": b.iid, "type": "function_call", "call_id": b.id,
				"name": b.name, "arguments": "", "status": "in_progress",
			},
		})
		if args == "" {
			return out
		}
		return append(out, m.chatToolDelta(b, args)...)
	}
	out := EncodeFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": b.idx,
		"content_block": map[string]any{"type": "tool_use", "id": b.iid, "name": b.name, "input": map[string]any{}},
	})
	if args == "" {
		return out
	}
	return append(out, m.chatToolDelta(b, args)...)
}

// chatToolDelta 工具参数增量帧（resp = function_call_arguments.delta；mess =
// input_json_delta）。item_id / 块 index 与 chatToolStart 同源。
func (m *StreamMapper) chatToolDelta(b *chatToolBlock, args string) []byte {
	if m.dir == domain.ProtocolConvertRespToChat {
		return EncodeFrame("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": b.iid,
			"output_index": b.idx, "delta": args,
		})
	}
	return EncodeFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.idx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
}

// chatCreatedAt 只在源帧携带过 created 时写 created_at（I7）。
func (m *StreamMapper) chatCreatedAt(obj map[string]any) map[string]any {
	if m.createdSet {
		obj["created_at"] = m.created
	}
	return obj
}

// absorbChatUsage 累加 chat 用量（覆盖语义：后到的末帧覆盖先到的值）。
//
// C15：completion_tokens_details.reasoning_tokens 必须真读真传 —— resp 客户端的
// output_tokens_details.reasoning_tokens 是计费/观测口径，恒写 0 等于把上游的
// 思考用量抹掉。Messages 协议**没有**这个承载位，故 chat→mess 方向改为丢弃计数
// （键 `reasoning_tokens`，方向取本 mapper 的实际方向常量，与 chat_upstream_resp.go
// 的非流式同向计数一致）。
func (m *StreamMapper) absorbChatUsage(u map[string]any) {
	m.it = intOr0(u, "prompt_tokens")
	m.ot = intOr0(u, "completion_tokens")
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok && d != nil {
		m.cached = intOr0(d, "cached_tokens")
		m.cacheCreate = intOr0(d, "cache_write_tokens")
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok && d != nil {
		if _, has := d["reasoning_tokens"]; has {
			// 只记录**值**。丢弃计数在终态（messUsageOf）做一次：逐帧计会把
			// 「上游在每个 usage 帧都重复该明细」重复计成多次。
			m.reasoningTokens = intOr0(d, "reasoning_tokens")
		}
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
	out = append(out, m.closeOpenBlocks()...)
	out = append(out, EncodeFrame("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": messStopFromChat(m.stopReason), "stop_sequence": nil},
		"usage": messUsageOf(m.chatUsage(), m.dir),
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
	resp := m.chatRespEnvelope(status, items)
	if incomplete != "" {
		resp["incomplete_details"] = map[string]any{"reason": incomplete}
	}
	return append(out, EncodeFrame("response.completed", map[string]any{
		"type": "response.completed", "response": resp,
	})...)
}

// chatRespEnvelope 终态 response 对象（completed / failed **同构**，I2）：output
// 为已累积项快照（调用方已剥离 __index）、usage 为累计值。created_at 只在源帧
// 携带过 created 时出现（I7：缺省不写、不编造）。
func (m *StreamMapper) chatRespEnvelope(status string, items []map[string]any) map[string]any {
	// output 必须是 JSON 数组（[]any），不能是 []map[string]any 的接口值。
	output := make([]any, 0, len(items))
	for _, it := range items {
		output = append(output, it)
	}
	return m.chatCreatedAt(map[string]any{
		"id": m.id, "object": "response", "status": status,
		"model": m.model, "output": output, "usage": respUsageOf(m.chatUsage(), m.dir),
	})
}

// chatRespItems 已累积的 resp output 项（顺序：reasoning → message → 工具块）。
// 工具块按块列表序（= 内容 index 分配序 = 首片到达序）：绝不依赖 map 迭代顺序
// （否则多个工具的输出次序每次跑都不同）。`__index` 是内部携带的 output_index
// （发帧时移除）。
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
	for _, b := range m.tools {
		out = append(out, map[string]any{
			"__index": b.idx, "id": b.iid, "type": "function_call",
			"call_id": b.id, "name": b.name, "arguments": b.args,
			"status": "completed",
		})
	}
	// chatRespItems 的固定次序是 reasoning → message → tools，但**输出索引是分配
	// 序**（工具先到、正文后到时分错）。终态 output / done 必须按 output_index 升序，
	// 否则客户端拿 completed.output 与流内 output_index 对不上。
	sort.SliceStable(out, func(i, j int) bool {
		ai, _ := out[i]["__index"].(int64)
		aj, _ := out[j]["__index"].(int64)
		return ai < aj
	})
	return out
}

// chatUsage 当前累计值 → chatUsage（非流式侧从 JSON 读，流式侧从这里读）。
// 口径（net 钳 0 计数、reasoning_tokens 丢弃计数）全部在 messUsageOf/respUsageOf
// 里，与非流式共用同一份判据。
func (m *StreamMapper) chatUsage() chatUsage {
	return chatUsage{
		gross: m.it, cached: m.cached, write: m.cacheCreate,
		out: m.ot, reasoning: m.reasoningTokens,
	}
}

// chatError 上游 error 帧 → 客户端错误帧（先关块再报，客户端才不会停在半截块）。
//
// 终态形态按目标协议（I2）：
//   - resp 目标：`response.failed`（response 与 completed **同构**：带累计 output /
//     usage / created_at，status:"failed"，error 里带 code+message）。只发这一条
//     错误事件，且 m.done 已置位 ⇒ 之后不会再有 response.completed（客户端拿得到
//     失败终态，而不是「一个通用 error + 静默结束」）；
//   - mess 目标：保持原样的 error 事件（`{type, error:{type, message}}`），位置在
//     关块之后（等价于 stop 帧之前）。
func (m *StreamMapper) chatError(e map[string]any) []byte {
	if m.done {
		noteRespDrop(m.dir, "frame_after_terminal")
		return nil
	}
	m.done = true
	m.ensureChat()
	msg, _ := str(e, "message")
	if m.dir == domain.ProtocolConvertRespToChat {
		items := m.chatRespItems()
		for _, it := range items {
			delete(it, "__index")
		}
		resp := m.chatRespEnvelope("failed", items)
		code, _ := str(e, "code")
		if code == "" {
			code = "upstream_error"
		}
		resp["error"] = map[string]any{"code": code, "message": msg}
		out := m.chatStart()
		return append(out, EncodeFrame("response.failed", map[string]any{
			"type": "response.failed", "response": resp,
		})...)
	}
	out := m.chatStart()
	out = append(out, m.closeOpenBlocks()...)
	return append(out, EncodeFrame("error", map[string]any{
		"type": "error", "error": map[string]any{"type": "api_error", "message": msg},
	})...)
}

// ensureChat chat 源方向的块状态懒初始化（与 ensureBlocks 同构）。
func (m *StreamMapper) ensureChat() {
	if m.toolIdx != nil {
		return
	}
	m.ensureBlocks()
	m.toolIdx = map[int64]int{}
	m.thinkIdx = -1
	m.textIdx = -1
}

// chatAlloc 单调分配内容 index（text / thinking / tool_use 共用一张表，与
// resp→mess 的 contentIndex 语义一致：index 只增不减、按出现顺序）。
func (m *StreamMapper) chatAlloc() int64 {
	idx := m.nextIndex
	m.nextIndex++
	return idx
}
