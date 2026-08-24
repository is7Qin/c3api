// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// chat→resp 方向（压测主路径，W5 优化）：请求/响应/流式帧转换均为字节级——
// 不 Unmarshal 到 map[string]any/结构体中间对象，直接对源 JSON 做字段级
// 原始字节提取（gjson，SDK FilterCodexPayload 模式：预筛 + 提取 + 拼接），
// 无需改写的值原字节透传（转义/格式原样保留，解析结果与 map 重排重编码
// 等价）；仅 messages→input、tools、tool_choice 等结构差异字段做消息级/
// 工具级局部处理。输出单缓冲直写（请求/响应按输入长度预分配一次；流式帧
// 复用 StreamMapper 缓冲，逐帧零分配）。语义与 map 版逐字段一致（测试钉住）。

import (
	"encoding/json"
	"errors"

	"github.com/tidwall/gjson"
)

// chatToRespRequest 客户端 chat 请求体 → resp 请求体。字段映射按 Responses
// API 规范（语义与 map 版完全一致）：
//   - messages → input：system → developer 消息项；user/assistant → message
//     项（assistant 文本 → output_text 部件）；assistant tool_calls → 独立
//     function_call 项；tool 消息 → function_call_output 项
//   - max_tokens / max_completion_tokens → max_output_tokens（两者都给时以
//     max_completion_tokens 为准）
//   - tools → tools（{type:"function"} 内嵌扁平化，strict 透传）；
//     tool_choice → tool_choice（{type:"function",function:{name}} 扁平化）
//   - 同名字段透传：model/temperature/top_p/stream/parallel_tool_calls/
//     user/metadata/store（null 值省略，pass 语义）
//   - resp 无对应参数 → 按规范丢弃
//
// 输出顶层键序与 map marshal 的排序键序一致（评审 I-5：透传值保留源格式
// 而非重排重编码——如 parameters 内空白/键序原样，JSON 语义等价非逐字节）。
func chatToRespRequest(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errors.New("invalid JSON")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		// null 体与 map 版对齐（decodeObj 解码 null → nil map → 空输出 {}；
		// 评审 I-2）；其余非对象顶层拒绝。
		if root.Type != gjson.Null {
			return nil, errors.New("invalid JSON: top-level must be an object")
		}
	}
	// 预筛：顶层单遍 ForEach 提取各字段原始文本（Raw 零拷贝切片；重复键
	// 后者覆盖——与 map 解码 last-wins 语义一致）。
	var (
		msgs, maxCT, maxT, toolChoice, tools                              gjson.Result
		model, temperature, topP, stream, parallel, user, metadata, store string
	)
	root.ForEach(func(k, v gjson.Result) bool {
		switch {
		case gjsonKeyEq(k, "messages"):
			msgs = v
		case gjsonKeyEq(k, "max_completion_tokens"):
			maxCT = v
		case gjsonKeyEq(k, "max_tokens"):
			maxT = v
		case gjsonKeyEq(k, "tool_choice"):
			toolChoice = v
		case gjsonKeyEq(k, "tools"):
			tools = v
		case gjsonKeyEq(k, "model"):
			model = v.Raw
		case gjsonKeyEq(k, "temperature"):
			temperature = v.Raw
		case gjsonKeyEq(k, "top_p"):
			topP = v.Raw
		case gjsonKeyEq(k, "stream"):
			stream = v.Raw
		case gjsonKeyEq(k, "parallel_tool_calls"):
			parallel = v.Raw
		case gjsonKeyEq(k, "user"):
			user = v.Raw
		case gjsonKeyEq(k, "metadata"):
			metadata = v.Raw
		case gjsonKeyEq(k, "store"):
			store = v.Raw
		}
		return true
	})
	// 输出：按排序键序单缓冲直写（预分配一次，无中间结构）。
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	if msgs.Exists() && msgs.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"input":`...)
		out = appendChatInputItems(out, msgs)
	}
	if maxCT.Exists() {
		out = appendField(out, &first, "max_output_tokens", maxCT.Raw)
	} else if maxT.Exists() {
		out = appendField(out, &first, "max_output_tokens", maxT.Raw)
	}
	if rawNotNull(metadata) {
		out = appendField(out, &first, "metadata", metadata)
	}
	if rawNotNull(model) {
		out = appendField(out, &first, "model", model)
	}
	if rawNotNull(parallel) {
		out = appendField(out, &first, "parallel_tool_calls", parallel)
	}
	if rawNotNull(store) {
		out = appendField(out, &first, "store", store)
	}
	if rawNotNull(stream) {
		out = appendField(out, &first, "stream", stream)
	}
	if rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if tc := chatToolChoiceRaw(toolChoice); tc != "" {
		out = appendField(out, &first, "tool_choice", tc)
	}
	if tools.Exists() && tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendRespTools(out, tools)
		out = append(out, ']')
	}
	if rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	if rawNotNull(user) {
		out = appendField(out, &first, "user", user)
	}
	out = append(out, '}')
	return out, nil
}

// appendChatInputItems messages 数组 → resp input 项数组（逐消息局部处理：
// 角色/内容/工具调用按语义映射，内容值原字节透传）。
func appendChatInputItems(out []byte, msgs gjson.Result) []byte {
	out = append(out, '[')
	n := 0
	msgs.ForEach(func(_, mv gjson.Result) bool {
		m := mv
		if !m.IsObject() {
			return true
		}
		role := m.Get("role")
		content := m.Get("content")
		switch {
		case rawStrEq(role.Raw, "system"):
			// system → developer 消息项（文本非空才产生，contentText 语义）
			if t, ok, nonEmpty := contentTextRaw(content); ok && nonEmpty {
				if n > 0 {
					out = append(out, ',')
				}
				n++
				out = append(out, `{"content":[{"text":`...)
				out = append(out, t...)
				out = append(out, `,"type":"input_text"}],"role":"developer","type":"message"}`...)
			}
		case rawStrEq(role.Raw, "user"), rawStrEq(role.Raw, "assistant"):
			// user/assistant → message 项（部件原字节映射；无部件 → 回退）
			start := len(out)
			if n > 0 {
				out = append(out, ',')
			}
			out = append(out, `{"content":[`...)
			var parts int
			out, parts = appendContentParts(out, content)
			if parts == 0 {
				out = out[:start]
			} else {
				out = append(out, `],"role":`...)
				out = append(out, role.Raw...)
				out = append(out, `,"type":"message"}`...)
				n++
			}
			// assistant tool_calls → 独立 function_call 项（保持原顺序）
			if rawStrEq(role.Raw, "assistant") {
				if tcs := m.Get("tool_calls"); tcs.IsArray() {
					tcs.ForEach(func(_, tc gjson.Result) bool {
						if !tc.IsObject() {
							return true
						}
						fn := tc.Get("function")
						if !fn.IsObject() {
							return true
						}
						if n > 0 {
							out = append(out, ',')
						}
						n++
						// 请求方向取 id（与 map 版一致：chat tool_calls 仅 id；
						// fcIDRaw 的 call_id 优先仅响应方向——评审 I-3）
						idRaw := strOrEmpty(tc.Get("id"))
						out = append(out, `{"arguments":`...)
						out = append(out, strOrEmpty(fn.Get("arguments"))...)
						out = append(out, `,"call_id":`...)
						out = append(out, idRaw...)
						out = append(out, `,"id":`...)
						out = append(out, idRaw...)
						out = append(out, `,"name":`...)
						out = append(out, strOrEmpty(fn.Get("name"))...)
						out = append(out, `,"type":"function_call"}`...)
						return true
					})
				}
			}
		case rawStrEq(role.Raw, "tool"), rawStrEq(role.Raw, "function"):
			// tool/function 消息 → function_call_output 项（call_id 取
			// tool_call_id / name，与 map 版一致）
			if t, ok, _ := contentTextRaw(content); ok {
				if n > 0 {
					out = append(out, ',')
				}
				n++
				callID := strOrEmpty(m.Get("tool_call_id"))
				if rawStrEq(role.Raw, "function") {
					callID = strOrEmpty(m.Get("name"))
				}
				out = append(out, `{"call_id":`...)
				out = append(out, callID...)
				out = append(out, `,"output":`...)
				out = append(out, t...)
				out = append(out, `,"type":"function_call_output"}`...)
			}
		}
		return true
	})
	out = append(out, ']')
	return out
}

// contentTextRaw 返回 content 的文本 JSON 字符串字面量：字符串 → 原字节透传
// （零拷贝）；text 块数组 → 各块 text 剥离首尾引号后以 \n 拼接再整体包裹
// （joinStrings("\n") 语义，转义逐字符保持、concat 即等价转义——评审 M-3 修复：
// raw 自带引号直接拼接会产出 ""a"" 非法 JSON；重建仅发生在多部件场景）。
// 返回 (字面量, 是否有文本, 文本是否非空)。字符串形态零分配。
func contentTextRaw(content gjson.Result) (string, bool, bool) {
	if content.Type == gjson.String {
		return content.Raw, true, len(content.Raw) > 2
	}
	if content.IsArray() {
		var joined []byte
		hasText := false
		nonEmpty := false
		content.ForEach(func(_, p gjson.Result) bool {
			if !p.IsObject() {
				return true
			}
			if rawStrEq(p.Get("type").Raw, "text") {
				if t := p.Get("text"); t.Type == gjson.String {
					// 分隔符按部件计数而非 joined 长度（评审 I-2：空字符串
					// 首部件剥引号后 0 字节，按长度判空会丢前导 \n）
					if hasText {
						joined = append(joined, '\\', 'n')
					}
					joined = append(joined, t.Raw[1:len(t.Raw)-1]...)
					hasText = true
					if len(t.Raw) > 2 {
						nonEmpty = true
					}
				}
			}
			return true
		})
		if !hasText {
			return "", false, false
		}
		buf := make([]byte, 0, len(joined)+2)
		buf = append(buf, '"')
		buf = append(buf, joined...)
		buf = append(buf, '"')
		return string(buf), true, nonEmpty
	}
	return "", false, false
}

// appendContentParts content → resp 内容部件数组（直接写入 out）：字符串 →
// 单 input_text（原字节）；text 部件 → input_text；image_url 部件 →
// input_image（url 原字节，string 或 {url} 两种形态）；其余部件按规范丢弃。
// 返回 (out, 部件数)。
func appendContentParts(out []byte, content gjson.Result) ([]byte, int) {
	if content.Type == gjson.String {
		out = append(out, `{"text":`...)
		out = append(out, content.Raw...)
		out = append(out, `,"type":"input_text"}`...)
		return out, 1
	}
	if !content.IsArray() {
		return out, 0
	}
	n := 0
	content.ForEach(func(_, p gjson.Result) bool {
		if !p.IsObject() {
			return true
		}
		switch {
		case rawStrEq(p.Get("type").Raw, "text"):
			if t := p.Get("text"); t.Type == gjson.String {
				if n > 0 {
					out = append(out, ',')
				}
				n++
				out = append(out, `{"text":`...)
				out = append(out, t.Raw...)
				out = append(out, `,"type":"input_text"}`...)
			}
		case rawStrEq(p.Get("type").Raw, "image_url"):
			if u, ok := imageURLRaw(p); ok {
				if n > 0 {
					out = append(out, ',')
				}
				n++
				out = append(out, `{"image_url":`...)
				out = append(out, u...)
				out = append(out, `,"type":"input_image"}`...)
			}
		}
		return true
	})
	return out, n
}

// imageURLRaw image_url 部件 → url JSON 字符串字面量：字符串形态原样透传；
// {url:...} 形态取 url 值。不可得 → false。
func imageURLRaw(p gjson.Result) (string, bool) {
	u := p.Get("image_url")
	if u.Type == gjson.String {
		return u.Raw, true
	}
	if u.IsObject() {
		if v := u.Get("url"); v.Type == gjson.String {
			return v.Raw, true
		}
	}
	return "", false
}

// appendRespTools resp tools 数组（转换后扁平化 function 工具）：非 function
// 工具按规范丢弃；输出 {type:"function", name?, description?, parameters?,
// strict?}（原字节透传）。
func appendRespTools(out []byte, tools gjson.Result) []byte {
	n := 0
	tools.ForEach(func(_, tv gjson.Result) bool {
		if !tv.IsObject() {
			return true
		}
		fn := tv.Get("function")
		if !fn.IsObject() {
			return true
		}
		if n > 0 {
			out = append(out, ',')
		}
		n++
		w := 0
		out = append(out, '{')
		if d := fn.Get("description"); d.Type == gjson.String {
			out = append(out, `"description":`...)
			out = append(out, d.Raw...)
			w++
		}
		if nm := fn.Get("name"); nm.Type == gjson.String {
			if w > 0 {
				out = append(out, ',')
			}
			out = append(out, `"name":`...)
			out = append(out, nm.Raw...)
			w++
		}
		if p := fn.Get("parameters"); p.Exists() && p.Type != gjson.Null {
			if w > 0 {
				out = append(out, ',')
			}
			out = append(out, `"parameters":`...)
			out = append(out, p.Raw...)
			w++
		}
		if s := fn.Get("strict"); s.Type == gjson.True || s.Type == gjson.False {
			if w > 0 {
				out = append(out, ',')
			}
			out = append(out, `"strict":`...)
			out = append(out, s.Raw...)
			w++
		}
		if w > 0 {
			out = append(out, ',')
		}
		out = append(out, `"type":"function"}`...)
		return true
	})
	return out
}

// chatToolChoiceRaw tool_choice 转换（字节级）：字符串 → 原样透传；
// {type:"function", function:{name}} → {name, type:"function"}（扁平化）；
// 其余 → nil（字段省略，与 map 版一致）。
func chatToolChoiceRaw(tc gjson.Result) string {
	if tc.Type == gjson.String {
		return tc.Raw
	}
	if tc.IsObject() && rawStrEq(tc.Get("type").Raw, "function") {
		if fn := tc.Get("function"); fn.IsObject() {
			if name := fn.Get("name"); name.Type == gjson.String {
				buf := make([]byte, 0, len(name.Raw)+16)
				buf = append(buf, `{"name":`...)
				buf = append(buf, name.Raw...)
				buf = append(buf, `,"type":"function"}`...)
				return string(buf)
			}
		}
	}
	return ""
}

// respToChatResponse resp 响应对象 → chat completion 对象（非流式，字节级）：
// output message 项文本拼接 content、function_call 项 → tool_calls（call_id
// 优先，M-1）；status/输出 → finish_reason；usage 同构映射。输出键序与 map
// marshal 排序一致。
func respToChatResponse(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errors.New("invalid JSON")
	}
	r := gjson.ParseBytes(body)
	if !r.IsObject() {
		// null 体与 map 版对齐（评审 I-2，同 chatToRespRequest）
		if r.Type != gjson.Null {
			return nil, errors.New("invalid JSON: top-level must be an object")
		}
	}
	id := r.Get("id")
	model := r.Get("model")
	created := gjsonNumInt(r.Get("created_at"))
	// finish_reason：incomplete → "length"；含 function_call → "tool_calls"；
	// 其余 "stop"。
	finish := `"stop"`
	if rawStrEq(r.Get("status").Raw, "incomplete") {
		finish = `"length"`
	} else if hasRespFunctionCall(r.Get("output")) {
		finish = `"tool_calls"`
	}
	out := make([]byte, 0, len(body)+64)
	out = append(out, `{"choices":[{"finish_reason":`...)
	out = append(out, finish...)
	out = append(out, `,"index":0,"message":{"content":`...)
	var tcs []byte
	out, tcs = appendChatMessageBody(out, tcs, r.Get("output"))
	out = append(out, `,"role":"assistant"`...)
	if tcs != nil {
		out = append(out, `,"tool_calls":[`...)
		out = append(out, tcs...)
		out = append(out, ']')
	}
	out = append(out, `}}],"created":`...)
	out = appendInt64(out, created)
	out = append(out, `,"id":`...)
	out = append(out, strOrEmpty(id)...)
	out = append(out, `,"model":`...)
	out = append(out, strOrEmpty(model)...)
	out = append(out, `,"object":"chat.completion"`...)
	if u := r.Get("usage"); u.IsObject() {
		out = append(out, `,"usage":`...)
		out = appendRespUsage(out, u)
	}
	out = append(out, '}')
	return out, nil
}

// appendChatMessageBody resp output → chat assistant message 的 content 文本
// 拼接（message 项 text 部件 join ""，恒为合法 JSON 字符串——评审 M-2 修复：
// 部件 raw 剥离首尾引号拼接，转义逐字符保持，concat 即等价转义）与 tool_calls
// 数组字节。无文本部件 → ""。返回 (out, tcs)。
func appendChatMessageBody(out, tcs []byte, output gjson.Result) ([]byte, []byte) {
	hasText := false
	output.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		switch {
		case rawStrEq(item.Get("type").Raw, "message"):
			if content := item.Get("content"); content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if !part.IsObject() {
						return true
					}
					if t := part.Get("text"); t.Type == gjson.String {
						if !hasText {
							out = append(out, '"')
							hasText = true
						}
						out = append(out, t.Raw[1:len(t.Raw)-1]...)
					}
					return true
				})
			}
		case rawStrEq(item.Get("type").Raw, "function_call"):
			if tcs == nil {
				tcs = make([]byte, 0, 96)
			} else {
				tcs = append(tcs, ',')
			}
			tcs = append(tcs, `{"function":{"arguments":`...)
			tcs = append(tcs, strOrEmpty(item.Get("arguments"))...)
			tcs = append(tcs, `,"name":`...)
			tcs = append(tcs, strOrEmpty(item.Get("name"))...)
			tcs = append(tcs, `},"id":`...)
			tcs = append(tcs, fcIDRaw(item)...)
			tcs = append(tcs, `,"type":"function"}`...)
		}
		return true
	})
	if !hasText {
		out = append(out, `""`...)
	} else {
		out = append(out, '"')
	}
	return out, tcs
}

// hasRespFunctionCall output 数组是否含 function_call 项。
func hasRespFunctionCall(output gjson.Result) bool {
	if !output.IsArray() {
		return false
	}
	found := false
	output.ForEach(func(_, item gjson.Result) bool {
		if item.IsObject() && rawStrEq(item.Get("type").Raw, "function_call") {
			found = true
			return false
		}
		return true
	})
	return found
}

// appendRespUsage resp usage → chat usage（input/output/total 同构；
// input_tokens_details.cached_tokens > 0 → prompt_tokens_details.cached_tokens）。
func appendRespUsage(out []byte, u gjson.Result) []byte {
	it := gjsonNumInt(u.Get("input_tokens"))
	ot := gjsonNumInt(u.Get("output_tokens"))
	tt := gjsonNumInt(u.Get("total_tokens"))
	out = append(out, `{"completion_tokens":`...)
	out = appendInt64(out, ot)
	out = append(out, `,"prompt_tokens":`...)
	out = appendInt64(out, it)
	if d := u.Get("input_tokens_details"); d.IsObject() {
		if c := gjsonNumInt(d.Get("cached_tokens")); c > 0 {
			out = append(out, `,"prompt_tokens_details":{"cached_tokens":`...)
			out = appendInt64(out, c)
			out = append(out, '}')
		}
	}
	out = append(out, `,"total_tokens":`...)
	out = appendInt64(out, tt)
	out = append(out, '}')
	return out
}

// contentText chat 消息 content（string 或 text 部件数组）→ 拼接文本（map 版
// 助手，chat_mess.go 共用；字节级路径用 contentTextRaw）。
func contentText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
				if t, ok := str(pm, "text"); ok {
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

// mapRespToChat 流式：resp SSE 事件 → chat 流（字节级：事件字段 gjson 原始
// 字节提取 + 帧复用缓冲组装，逐帧零分配）。事件映射表：
//
//	response.created                  → 角色前导 chunk（delta.role=assistant）
//	response.output_text.delta        → content delta chunk
//	response.output_item.added(FC)    → tool_calls 前导 chunk（id+name）
//	response.function_call_arguments.delta → tool_calls arguments delta
//	response.completed                → 收尾 chunk（finish_reason+usage）+ [DONE]
//	response.failed                   → data-only {"error":{...}} 帧（chat 流式错误约定）
//	其余（in_progress/output_item.done/content_part.* 等）→ 丢弃
func (m *StreamMapper) mapRespToChat(name string, data []byte) ([]byte, bool) {
	if !json.Valid(data) {
		return nil, true
	}
	ev := gjson.ParseBytes(data)
	if !ev.IsObject() {
		return nil, true
	}
	switch name {
	case "response.created":
		if m.started {
			return nil, true
		}
		m.started = true
		if resp := ev.Get("response"); resp.IsObject() {
			if v := resp.Get("id"); v.Type == gjson.String {
				m.id = v.String()
			}
			if v := resp.Get("model"); v.Type == gjson.String {
				m.model = v.String()
			}
			m.created = gjsonNumInt(resp.Get("created_at"))
		}
		m.dbuf = append(m.dbuf[:0], `{"content":"","role":"assistant"}`...)
		return m.chatChunkFrame(m.dbuf, nil, nil), false
	case "response.output_text.delta":
		m.dbuf = append(m.dbuf[:0], `{"content":`...)
		m.dbuf = append(m.dbuf, strOrEmpty(ev.Get("delta"))...)
		m.dbuf = append(m.dbuf, '}')
		return m.chatChunkFrame(m.dbuf, nil, nil), false
	case "response.output_item.added":
		item := ev.Get("item")
		if !item.IsObject() || !rawStrEq(item.Get("type").Raw, "function_call") {
			return nil, true
		}
		idRaw := fcIDRaw(item) // call_id 优先（客户端回传匹配键，M-1）
		index := gjsonNumInt(ev.Get("output_index"))
		m.dbuf = append(m.dbuf[:0], `{"tool_calls":[{"function":{"arguments":"","name":`...)
		m.dbuf = append(m.dbuf, strOrEmpty(item.Get("name"))...)
		m.dbuf = append(m.dbuf, `},"id":`...)
		m.dbuf = append(m.dbuf, idRaw...)
		m.dbuf = append(m.dbuf, `,"index":`...)
		m.dbuf = appendInt64(m.dbuf, index)
		m.dbuf = append(m.dbuf, `,"type":"function"}]}`...)
		return m.chatChunkFrame(m.dbuf, nil, nil), false
	case "response.function_call_arguments.delta":
		index := gjsonNumInt(ev.Get("output_index"))
		m.dbuf = append(m.dbuf[:0], `{"tool_calls":[{"function":{"arguments":`...)
		m.dbuf = append(m.dbuf, strOrEmpty(ev.Get("delta"))...)
		m.dbuf = append(m.dbuf, `},"index":`...)
		m.dbuf = appendInt64(m.dbuf, index)
		m.dbuf = append(m.dbuf, `}]}`...)
		return m.chatChunkFrame(m.dbuf, nil, nil), false
	case "response.completed":
		if m.done {
			return nil, true
		}
		m.done = true
		finish := []byte(`"stop"`)
		var usage []byte
		if resp := ev.Get("response"); resp.IsObject() {
			if rawStrEq(resp.Get("status").Raw, "incomplete") {
				finish = []byte(`"length"`)
			} else if hasRespFunctionCall(resp.Get("output")) {
				finish = []byte(`"tool_calls"`)
			}
			// resp 无 usage（或非对象）→ 收尾 chunk 省略 "usage" 字段而非
			// 写 "usage":null——与 map 版一致（usage 提取失败 → nil → 省略；
			// 评审 I-3 接受并注释）
			if u := resp.Get("usage"); u.IsObject() {
				usage = m.appendUsageToBuf(u)
			}
		}
		// 收尾 chunk：finish_reason + 内联 usage（chat 流式 include_usage 语义）
		m.buf = m.chatChunkFrame([]byte(`{}`), finish, usage)
		m.buf = append(m.buf, `data: [DONE]`...)
		m.buf = append(m.buf, '\n', '\n')
		return m.buf, false
	case "response.failed":
		if m.done {
			return nil, true
		}
		m.done = true
		if resp := ev.Get("response"); resp.IsObject() {
			if e := resp.Get("error"); e.IsObject() {
				m.buf = append(m.buf[:0], `data: {"error":{"message":`...)
				m.buf = append(m.buf, strOrEmpty(e.Get("message"))...)
				m.buf = append(m.buf, '}', '}', '\n', '\n')
				return m.buf, false
			}
		}
		m.buf = append(m.buf[:0], `data: [DONE]`...)
		m.buf = append(m.buf, '\n', '\n')
		return m.buf, false
	}
	return nil, true
}

// appendUsageToBuf resp usage → chat usage 字节（写入复用缓冲 m.dbuf）。
func (m *StreamMapper) appendUsageToBuf(u gjson.Result) []byte {
	it := gjsonNumInt(u.Get("input_tokens"))
	ot := gjsonNumInt(u.Get("output_tokens"))
	tt := gjsonNumInt(u.Get("total_tokens"))
	m.dbuf = append(m.dbuf[:0], `{"completion_tokens":`...)
	m.dbuf = appendInt64(m.dbuf, ot)
	m.dbuf = append(m.dbuf, `,"prompt_tokens":`...)
	m.dbuf = appendInt64(m.dbuf, it)
	if d := u.Get("input_tokens_details"); d.IsObject() {
		if c := gjsonNumInt(d.Get("cached_tokens")); c > 0 {
			m.dbuf = append(m.dbuf, `,"prompt_tokens_details":{"cached_tokens":`...)
			m.dbuf = appendInt64(m.dbuf, c)
			m.dbuf = append(m.dbuf, '}')
		}
	}
	m.dbuf = append(m.dbuf, `,"total_tokens":`...)
	m.dbuf = appendInt64(m.dbuf, tt)
	m.dbuf = append(m.dbuf, '}')
	return m.dbuf
}

// chatChunkFrame 组装 chat 流式 chunk 帧（字节级，写入复用缓冲 m.buf）：
// delta/finish/usage 为预组装 JSON 值字节（nil → null）。帧为完整 SSE 形态：
// `data: ` 前缀 + 载荷 + 空行终止（评审 M-1 修复）。返回 m.buf 当前字节——
// 生命周期仅限本次 Map 调用（sserelay 契约：Mapper 返回后立即写出，调用方
// 可复用缓冲）。
func (m *StreamMapper) chatChunkFrame(delta, finish, usage []byte) []byte {
	m.buf = append(m.buf[:0],
		`data: {"choices":[{"delta":`...)
	m.buf = append(m.buf, delta...)
	m.buf = append(m.buf, `,"finish_reason":`...)
	if finish == nil {
		m.buf = append(m.buf, `null`...)
	} else {
		m.buf = append(m.buf, finish...)
	}
	m.buf = append(m.buf, `,"index":0}],"created":`...)
	m.buf = appendInt64(m.buf, m.created)
	m.buf = append(m.buf, `,"id":`...)
	m.buf = appendJSONString(m.buf, m.id)
	m.buf = append(m.buf, `,"model":`...)
	m.buf = appendJSONString(m.buf, m.model)
	m.buf = append(m.buf, `,"object":"chat.completion.chunk"`...)
	if usage != nil {
		m.buf = append(m.buf, `,"usage":`...)
		m.buf = append(m.buf, usage...)
	}
	m.buf = append(m.buf, '}', '\n', '\n')
	return m.buf
}

// chatFrame 组装 chat 流式 chunk 帧（map 版，chat_mess.go 共用——非字节级
// 方向仍走 map 组装）。
func (m *StreamMapper) chatFrame(delta map[string]any, finish, usage any) []byte {
	c := map[string]any{
		"id":      m.id,
		"object":  "chat.completion.chunk",
		"created": m.created,
		"model":   m.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		c["usage"] = usage
	}
	return EncodeFrame("", c)
}
