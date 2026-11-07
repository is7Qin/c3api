// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"errors"
	"fmt"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/tidwall/gjson"
)

// ErrStatefulFields Responses 的**有状态**字段（previous_response_id /
// conversation / prompt / context_management）在 chat 目标下不可承载。它们不像
// 图片或文档那样"丢了就少一块内容"，而是**整段多轮历史**：静默丢弃会让客户端
// 只看到"模型突然变笨"，且不可逆。故转换入口直接拒绝；调用方据此回 400 +
// invalid_request_error（与"请求体非法"区分开）。
var ErrStatefulFields = errors.New("responses to chat conversion does not support stateful fields")

// respChatStatefulFields 触发 ErrStatefulFields 的字段（顺序即报错里的列举顺序）。
var respChatStatefulFields = []string{
	"previous_response_id", "conversation", "prompt", "context_management",
}

// respChatDroppedFields 有明确语义但 chat 无承载位的顶层字段：整段丢弃并逐项
// 计数（`metadata` 整段只计一次）。
var respChatDroppedFields = []string{
	"store", "include", "truncation", "prompt_cache_key", "max_tool_calls", "metadata",
}

// respToChatRequest Responses 请求 → Chat Completions 请求（客户端说 resp、
// 上游只说 chat 时使用）。
//
// 与 mess→chat 同口径：字节级拼装、键按字母序、不建 map。差异集中在三处结构
// 重排（F19，官方字段形状不同，直译必 400）：
//   - `tools[]`：源是**扁平** `{type:"function",name,parameters}`，chat 要嵌套
//     `{"function":{…},"type":"function"}`；
//   - `tool_choice`：源 `{"type":"function","name":"f"}` → 目标
//     `{"function":{"name":"f"},"type":"function"}`；
//   - `text.format`：源**扁平** `{"type":"json_schema","name","schema","strict"}`
//     → 目标**嵌套** `{"type":"json_schema","json_schema":{…}}`。
func respToChatRequest(body []byte) ([]byte, error) {
	root, err := parseRoot(body)
	if err != nil {
		return nil, err
	}
	if err := rejectRespStateful(root); err != nil {
		return nil, err
	}
	var (
		input, maxOut, tools, toolChoice, reasoning, textFmt gjson.Result
		instructions, model, temperature, topP, parallel     string
		stream, user                                         string
	)
	if root.IsObject() {
		root.ForEach(func(k, v gjson.Result) bool {
			switch {
			case gjsonKeyEq(k, "input"):
				input = v
			case gjsonKeyEq(k, "instructions"):
				instructions = v.Raw
			case gjsonKeyEq(k, "max_output_tokens"):
				maxOut = v
			case gjsonKeyEq(k, "tools"):
				tools = v
			case gjsonKeyEq(k, "tool_choice"):
				toolChoice = v
			case gjsonKeyEq(k, "model"):
				model = v.Raw
			case gjsonKeyEq(k, "parallel_tool_calls"):
				parallel = v.Raw
			case gjsonKeyEq(k, "temperature"):
				temperature = v.Raw
			case gjsonKeyEq(k, "top_p"):
				topP = v.Raw
			case gjsonKeyEq(k, "stream"):
				stream = v.Raw
			case gjsonKeyEq(k, "user"):
				user = v.Raw
			case gjsonKeyEq(k, "reasoning"):
				reasoning = v
			case gjsonKeyEq(k, "text"):
				textFmt = v.Get("format")
			case gjsonKeyEq(k, "stream_options"):
				// chat 的 stream_options 由本方向自己生成（强制 include_usage），
				// 源里的同名键不参与（resp 协议里没有它，出现即是野字段）。
				noteDrop(domain.ProtocolConvertRespToChat, "stream_options")
			default:
				for _, f := range respChatDroppedFields {
					if gjsonKeyEq(k, f) {
						noteDrop(domain.ProtocolConvertRespToChat, f)
						return true
					}
				}
			}
			return true
		})
	}
	if tools.IsArray() {
		noteNonFunctionTools(tools)
	}
	effort := respChatEffort(reasoning)

	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	if maxOut.Exists() && maxOut.Type == gjson.Number {
		out = appendField(out, &first, "max_completion_tokens", maxOut.Raw)
	}
	// messages 是 chat 必填：**恒写**。
	if !first {
		out = append(out, ',')
	}
	first = false
	out = append(out, `"messages":[`...)
	var nMsgs int
	out, nMsgs = appendRespChatMessages(out, instructions, input)
	out = append(out, ']')
	if nMsgs == 0 {
		noteDrop(domain.ProtocolConvertRespToChat, "messages_empty")
	}
	if rawNotNull(model) {
		out = appendField(out, &first, "model", model)
	}
	if parallel == "true" || parallel == "false" {
		out = appendField(out, &first, "parallel_tool_calls", parallel)
	}
	if effort != "" {
		out = appendField(out, &first, "reasoning_effort", effort)
	}
	if rf := respChatResponseFormat(textFmt); rf != "" {
		out = appendField(out, &first, "response_format", rf)
	}
	if rawNotNull(stream) {
		out = appendField(out, &first, "stream", stream)
	}
	if stream == "true" {
		out = appendStreamOptionsMerged(out, &first, gjson.Result{})
	}
	// 与其余方向同口径：思考开启时丢弃 temperature/top_p（上游只接受默认采样）。
	if effort == "" && rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if raw := respChatToolChoice(toolChoice); raw != "" {
		out = appendField(out, &first, "tool_choice", raw)
	}
	if tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendToolsMapped(out, tools, toolsChatFromResp)
		out = append(out, ']')
	}
	if effort == "" && rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	if rawNotNull(user) && user[0] == '"' {
		out = appendField(out, &first, "user", user)
	}
	return append(out, '}'), nil
}

// rejectRespStateful 有状态字段 → ErrStatefulFields（列举全部命中项，避免客户端
// 改一个再被拒一个）。
func rejectRespStateful(root gjson.Result) error {
	var hit []string
	for _, f := range respChatStatefulFields {
		if v := root.Get(f); v.Exists() && v.Type != gjson.Null {
			hit = append(hit, f)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrStatefulFields, strings.Join(hit, ", "))
}

// respChatEffort reasoning.effort → reasoning_effort（白名单原样直传）。
// 枚举外或非字符串 → 丢弃并计数。
func respChatEffort(reasoning gjson.Result) string {
	if !reasoning.IsObject() {
		return ""
	}
	v := reasoning.Get("effort")
	if !v.Exists() || v.Type == gjson.Null {
		return ""
	}
	// reasoningEffortRaw 返回的是**已带引号**的 Raw（它的出口直接拼进 JSON），
	// 故调用点不再编码一次。
	if raw := reasoningEffortRaw(v); raw != "" {
		return raw
	}
	noteDrop(domain.ProtocolConvertRespToChat, "reasoning_effort")
	return ""
}

// respChatResponseFormat `text.format` → chat `response_format`。
//
// 三态（F19）：`text` → 省略（chat 默认即文本）；`json_object` → 原样；`json_schema`
// → **结构重排**（源的 name/schema/strict 是扁平的，目标要包进 `json_schema`）。
// name/schema 缺失 ⇒ 整段丢弃并计数：写出去上游必 400，而"没有 response_format"
// 至多是客户端自己解析失败。
func respChatResponseFormat(f gjson.Result) string {
	if !f.Exists() || !f.IsObject() {
		return ""
	}
	switch f.Get("type").Str {
	case "", "text":
		return ""
	case "json_object":
		return `{"type":"json_object"}`
	case "json_schema":
		name := f.Get("name")
		schema := f.Get("schema")
		if name.Type != gjson.String || name.Str == "" || !schema.IsObject() {
			noteDrop(domain.ProtocolConvertRespToChat, "text_format_invalid")
			return ""
		}
		buf := make([]byte, 0, len(name.Raw)+len(schema.Raw)+64)
		buf = append(buf, `{"json_schema":{"name":`...)
		buf = append(buf, name.Raw...)
		buf = append(buf, `,"schema":`...)
		buf = append(buf, schema.Raw...)
		if s := f.Get("strict"); s.Type == gjson.True || s.Type == gjson.False {
			buf = append(buf, `,"strict":`...)
			buf = append(buf, s.Raw...)
		}
		buf = append(buf, `},"type":"json_schema"}`...)
		return string(buf)
	}
	noteDrop(domain.ProtocolConvertRespToChat, "text_format_invalid")
	return ""
}

// respChatToolChoice Responses 的 tool_choice → chat 形态。
//
// **源扁平、目标嵌套**（F19）：`{"type":"function","name":"f"}` →
// `{"function":{"name":"f"},"type":"function"}`。字符串 auto/none/required 原样。
// `allowed_tools`/`custom`/内置工具等 → 丢弃并计数（chat 侧无对应表达）。
//
// 注意：**不要**复用 respToolChoiceToMess——那个函数产出的是 mess 目标形态。
func respChatToolChoice(v gjson.Result) string {
	if !v.Exists() || v.Type == gjson.Null {
		return ""
	}
	if v.Type == gjson.String {
		switch v.Str {
		case "auto", "none", "required":
			return v.Raw
		}
		noteDrop(domain.ProtocolConvertRespToChat, "tool_choice_unsupported")
		return ""
	}
	if v.Get("type").Str == "function" {
		name := v.Get("name")
		if name.Type != gjson.String || name.Str == "" {
			noteDrop(domain.ProtocolConvertRespToChat, "tool_choice_unsupported")
			return ""
		}
		return `{"function":{"name":` + name.Raw + `},"type":"function"}`
	}
	noteDrop(domain.ProtocolConvertRespToChat, "tool_choice_unsupported")
	return ""
}

// noteNonFunctionTools tools[] 里 type != "function" 的项（custom /
// web_search_preview / computer_use_preview / image_generation / code_interpreter
// 等内置工具）在 chat 侧没有对应的 function 定义 ⇒ 丢弃并计数。
// 过滤本身由 appendToolsMapped 的 typeFunctionGate 完成，这里只负责让丢弃可见。
func noteNonFunctionTools(tools gjson.Result) {
	tools.ForEach(func(_, tv gjson.Result) bool {
		if !tv.IsObject() {
			return true
		}
		if typ, ok := lastField(tv, "type"); ok && rawStrEq(typ.Raw, "function") {
			return true
		}
		noteDrop(domain.ProtocolConvertRespToChat, "tool_custom")
		return true
	})
}

// respChatAsst resp 侧 assistant 面的累积器：reasoning / 文本 / tool_calls 在源
// 里是**并列的三种 item**，在 chat 里必须落进**同一条** assistant 消息（连续
// 多条 assistant 消息会让上游把 tool_calls 与前面的内容割开）。
type respChatAsst struct {
	text   []byte
	nText  int
	reason []byte
	nReas  int
	calls  []byte
	nCalls int
	open   bool
}

// appendRespChatMessages instructions + input → chat messages[]。
func appendRespChatMessages(out []byte, instructions string, input gjson.Result) ([]byte, int) {
	n := 0
	var asst respChatAsst
	flush := func() {
		if !asst.open {
			return
		}
		out, n = appendComma(out, n)
		out = append(out, `{"content":`...)
		out = append(out, string(appendJSONString(nil, string(asst.text)))...)
		if asst.nReas > 0 {
			out = append(out, `,"reasoning_content":`...)
			out = append(out, string(appendJSONString(nil, string(asst.reason)))...)
		}
		out = append(out, `,"role":"assistant"`...)
		if asst.nCalls > 0 {
			out = append(out, `,"tool_calls":[`...)
			out = append(out, asst.calls...)
			out = append(out, ']')
		}
		out = append(out, '}')
		asst = respChatAsst{}
	}
	// instructions → 首条 system 消息。
	if len(instructions) > 2 && instructions[0] == '"' {
		out, n = appendComma(out, n)
		out = append(out, `{"content":`...)
		out = append(out, instructions...)
		out = append(out, `,"role":"system"}`...)
	}
	switch {
	case input.Type == gjson.String:
		out, n = appendComma(out, n)
		out = append(out, `{"content":`...)
		out = append(out, input.Raw...)
		out = append(out, `,"role":"user"}`...)
	case input.IsArray():
		var declared []string
		input.ForEach(func(_, item gjson.Result) bool {
			if !item.IsObject() {
				noteDrop(domain.ProtocolConvertRespToChat, "item_unknown")
				return true
			}
			switch item.Get("type").Str {
			case "message":
				r := item.Get("role").Str
				if r == "assistant" {
					asst.open = true
					asst.text, asst.nText = respChatTextParts(asst.text, asst.nText, item.Get("content"))
					return true
				}
				flush()
				out, n = appendRespChatRoleMessage(out, n, r, item.Get("content"))
			case "function_call":
				id := item.Get("call_id")
				name := item.Get("name")
				if id.Type != gjson.String || name.Type != gjson.String {
					noteDrop(domain.ProtocolConvertRespToChat, "item_unknown")
					return true
				}
				asst.open = true
				// 两侧的 arguments 都是「装着 JSON 的字符串」：resp 源已经是那个
				// 字符串字面量，**原样搬**（再包一层会把 `{"a":1}` 变成
				// `"{\"a\":1}"`，上游解析出来是个字符串而不是对象）。
				args := []byte(`"{}"`)
				if a := item.Get("arguments"); a.Type == gjson.String {
					args = append([]byte(nil), a.Raw...)
				}
				asst.calls = appendToolCallComma(asst.calls, asst.nCalls)
				asst.nCalls++
				asst.calls = append(asst.calls, `{"function":{"arguments":`...)
				asst.calls = append(asst.calls, args...)
				asst.calls = append(asst.calls, `,"name":`...)
				asst.calls = append(asst.calls, name.Raw...)
				asst.calls = append(asst.calls, `},"id":`...)
				asst.calls = append(asst.calls, id.Raw...)
				asst.calls = append(asst.calls, `,"type":"function"}`...)
				declared = append(declared, id.Str)
			case "function_call_output":
				flush()
				id := item.Get("call_id")
				if id.Type != gjson.String || id.Str == "" || !chatCallDeclared(declared, id.Str) {
					// 不合成对应 call（J11）：孤儿 tool 消息上游必 400。
					noteDrop(domain.ProtocolConvertRespToChat, "tool_result_orphan")
					return true
				}
				out, n = appendComma(out, n)
				out = append(out, `{"content":`...)
				out = append(out, respChatOutputString(item.Get("output"))...)
				out = append(out, `,"role":"tool","tool_call_id":`...)
				out = append(out, id.Raw...)
				out = append(out, '}')
			case "reasoning":
				asst.open = true
				if ps := respChatReasoningTexts(item); len(ps) > 0 {
					if asst.nReas > 0 {
						asst.reason = append(asst.reason, '\n')
					}
					asst.reason = append(asst.reason, strings.Join(ps, "\n")...)
					asst.nReas++
				}
				if enc := item.Get("encrypted_content"); enc.Type == gjson.String && enc.Str != "" {
					// chat 没有签名/加密思考通道：丢内容是必然的，但必须可见。
					noteDrop(domain.ProtocolConvertRespToChat, "encrypted_content")
				}
			default:
				// Responses 的 item 是 20+ 成员开放联合（web_search_call /
				// computer_call / mcp_call / image_generation_call …）：chat 无载体。
				noteDrop(domain.ProtocolConvertRespToChat, "item_unknown")
			}
			return true
		})
		flush()
	}
	return out, n
}

// appendRespChatRoleMessage 非 assistant 的 message item（user / system /
// developer）→ 对应 role 的 chat 消息。文本走 parts 数组（resp 的 input_text /
// output_text 都是文本），图片走 image_url，文件无载体 ⇒ 丢弃并计数。
func appendRespChatRoleMessage(out []byte, n int, r string, content gjson.Result) ([]byte, int) {
	if r != "user" && r != "system" && r != "developer" {
		noteDrop(domain.ProtocolConvertRespToChat, "role")
		return out, n
	}
	if content.Type == gjson.String {
		out, n = appendComma(out, n)
		out = append(out, `{"content":`...)
		out = append(out, content.Raw...)
		out = append(out, `,"role":`...)
		out = append(out, string(appendJSONString(nil, r))...)
		out = append(out, '}')
		return out, n
	}
	var parts []byte
	nParts := 0
	if content.IsArray() {
		content.ForEach(func(_, p gjson.Result) bool {
			switch p.Get("type").Str {
			case "input_text", "output_text", "text":
				if t := p.Get("text"); t.Type == gjson.String {
					parts = appendPartComma(parts, nParts)
					nParts++
					parts = append(parts, `{"text":`...)
					parts = append(parts, t.Raw...)
					parts = append(parts, `,"type":"text"}`...)
				}
			case "input_image":
				if url, ok := respChatImageURL(p); ok {
					parts = appendPartComma(parts, nParts)
					nParts++
					parts = append(parts, `{"image_url":{"url":`...)
					parts = append(parts, string(appendJSONString(nil, url))...)
					parts = append(parts, `},"type":"image_url"}`...)
				}
			case "input_file":
				noteDrop(domain.ProtocolConvertRespToChat, "input_file")
			default:
				noteDrop(domain.ProtocolConvertRespToChat, "item_unknown")
			}
			return true
		})
	}
	out, n = appendComma(out, n)
	if nParts == 0 {
		// 空 content / 全是被丢部件：仍写一条空消息（保持轮次）。
		out = append(out, `{"content":"","role":`...)
		out = append(out, string(appendJSONString(nil, r))...)
		return append(out, '}'), n
	}
	out = append(out, `{"content":[`...)
	out = append(out, parts...)
	out = append(out, `],"role":`...)
	out = append(out, string(appendJSONString(nil, r))...)
	return append(out, '}'), n
}

// respChatTextParts assistant message 的文本部件累加进 content。
func respChatTextParts(buf []byte, n int, content gjson.Result) ([]byte, int) {
	if content.Type == gjson.String {
		s := content.Str
		if len(s) > 0 {
			n++
		}
		return append(buf, s...), n
	}
	if content.IsArray() {
		content.ForEach(func(_, p gjson.Result) bool {
			switch p.Get("type").Str {
			case "output_text", "input_text", "text":
				if t := p.Get("text"); t.Type == gjson.String {
					buf = append(buf, t.Str...)
					n++
				}
			default:
				noteDrop(domain.ProtocolConvertRespToChat, "item_unknown")
			}
			return true
		})
	}
	return buf, n
}

// respChatImageURL input_image → chat image_url.url。`image_url` 是字符串（http(s)
// 或 data URL）时原样；`file_id` 形态 chat 无载体 ⇒ 丢弃并计数。
func respChatImageURL(p gjson.Result) (string, bool) {
	if u := p.Get("image_url"); u.Type == gjson.String && u.Str != "" {
		return u.Str, true
	}
	if id := p.Get("file_id"); id.Type == gjson.String && id.Str != "" {
		noteDrop(domain.ProtocolConvertRespToChat, "image_file")
		return "", false
	}
	noteDrop(domain.ProtocolConvertRespToChat, "image_file")
	return "", false
}

// respChatReasoningTexts reasoning item → 文本片段（**已反转义**，可直接回写）。
// summary[].text 优先；空则 content[].type=="reasoning_text"。与
// appendReasoningStripes 的区别：那个按"摘引号字节"处理（保留转义），只适合把
// 源字节直接拼进源字节；这里要再经一次 JSON 编码，必须用反转义后的字符串。
func respChatReasoningTexts(item gjson.Result) []string {
	var out []string
	if s := item.Get("summary"); s.IsArray() {
		s.ForEach(func(_, p gjson.Result) bool {
			if t := p.Get("text"); t.Type == gjson.String && t.Str != "" {
				out = append(out, t.Str)
			}
			return true
		})
	}
	if len(out) == 0 {
		if c := item.Get("content"); c.IsArray() {
			c.ForEach(func(_, p gjson.Result) bool {
				if !rawStrEq(p.Get("type").Raw, "reasoning_text") {
					return true
				}
				if t := p.Get("text"); t.Type == gjson.String && t.Str != "" {
					out = append(out, t.Str)
				}
				return true
			})
		}
	}
	return out
}

// respChatOutputString function_call_output.output → chat tool 消息的 content
// 字符串。字符串原样（用反转义后的值）；对象/数组/数字 → 序列化成串（chat 的
// tool content 只能是字符串，丢掉会让模型看不到工具结果）。
func respChatOutputString(v gjson.Result) []byte {
	switch {
	case v.Type == gjson.String:
		return appendJSONString(nil, v.Str)
	case v.Exists() && v.Type != gjson.Null:
		return appendJSONString(nil, v.Raw)
	}
	return appendJSONString(nil, "")
}

// chatCallDeclared 该 call_id 是否已由前面的 assistant.function_call 声明。
// 线性扫描（工具调用数是个位数）——比每次请求建一个 map 便宜。
func chatCallDeclared(declared []string, id string) bool {
	for _, d := range declared {
		if d == id {
			return true
		}
	}
	return false
}
