// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 三条请求转换的字节路径：gjson 单遍取字段，值原样拼进一个输出缓冲。
// 语义与原 map+json.Marshal 版一致（测试钉住）。chat→resp 仍在 chat_resp.go。

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

func chatToMessRequest(body []byte) ([]byte, error) {
	root, err := parseRoot(body)
	if err != nil {
		return nil, err
	}
	var (
		msgs, maxCT, maxT, stop, tools, toolChoice, effort gjson.Result
		model, temperature, topP, stream, metadata         string
	)
	if root.IsObject() {
		root.ForEach(func(k, v gjson.Result) bool {
			switch {
			case gjsonKeyEq(k, "messages"):
				msgs = v
			case gjsonKeyEq(k, "max_completion_tokens"):
				maxCT = v
			case gjsonKeyEq(k, "max_tokens"):
				maxT = v
			case gjsonKeyEq(k, "reasoning_effort"):
				effort = v
			case gjsonKeyEq(k, "stop"):
				stop = v
			case gjsonKeyEq(k, "tools"):
				tools = v
			case gjsonKeyEq(k, "tool_choice"):
				toolChoice = v
			case gjsonKeyEq(k, "model"):
				model = v.Raw
			case gjsonKeyEq(k, "temperature"):
				temperature = v.Raw
			case gjsonKeyEq(k, "top_p"):
				topP = v.Raw
			case gjsonKeyEq(k, "stream"):
				stream = v.Raw
			case gjsonKeyEq(k, "metadata"):
				metadata = v.Raw
			}
			return true
		})
	}
	// 上限归一化（spec §3.1）：写出值 = budget 基数（同一个变量）。逐字段判定：
	// 非法/缺失 → 继续下一字段；全部不合法 → 4096。
	maxTok, _ := maxTokensField(maxCT, maxT)
	// reasoning_effort → thinking（no_raise：只按上限压 budget，绝不抬高）。
	// chat 无签名通道 → 历史含 tool_calls 时不开启（否则「要求回放」的
	// Anthropic 端点该轮必 422）；强制 tool_choice 与 thinking 冲突时保留强制意图。
	think := ""
	if e := chatEffort(effort); e != "" && !chatHistoryHasToolCalls(msgs) {
		think = thinkingValue(e, maxTok)
	}
	if think != "" && messToolChoiceForces(chatToolChoiceToMess(toolChoice)) {
		think = ""
	}
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	out = appendField(out, &first, "max_tokens", strconv.FormatInt(maxTok, 10))
	if msgs.IsArray() {
		out = appendChatMessMessages(out, &first, msgs)
	}
	if rawNotNull(metadata) {
		out = appendField(out, &first, "metadata", metadata)
	}
	if rawNotNull(model) {
		out = appendField(out, &first, "model", model)
	}
	if stopRaw := chatStopRaw(stop); stopRaw != "" {
		out = appendField(out, &first, "stop_sequences", stopRaw)
	}
	if rawNotNull(stream) {
		out = appendField(out, &first, "stream", stream)
	}
	if sys := chatSystemRaw(msgs); sys != "" {
		out = appendField(out, &first, "system", sys)
	}
	// thinking 开启时 Anthropic 只接受默认采样参数 → 丢弃 temperature/top_p。
	if think == "" && rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if think != "" {
		out = appendField(out, &first, "thinking", think)
	}
	if tc := chatToolChoiceToMess(toolChoice); tc != "" {
		out = appendField(out, &first, "tool_choice", tc)
	}
	if tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendToolsMapped(out, tools, toolsMessFromChat)
		out = append(out, ']')
	}
	if think == "" && rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	return append(out, '}'), nil
}

func chatSystemRaw(msgs gjson.Result) string {
	if !msgs.IsArray() {
		return ""
	}
	var parts []string
	msgs.ForEach(func(_, mv gjson.Result) bool {
		if !mv.IsObject() {
			return true
		}
		role := mv.Get("role").Raw
		if !rawStrEq(role, "system") && !rawStrEq(role, "developer") {
			return true
		}
		if t, ok, _ := contentTextRaw(mv.Get("content")); ok {
			parts = append(parts, t)
		}
		return true
	})
	if len(parts) == 0 {
		return ""
	}
	buf := make([]byte, 0, 64)
	buf = appendJoinedTexts(buf, parts, "\n")
	return string(buf)
}

func appendChatMessMessages(out []byte, first *bool, msgs gjson.Result) []byte {
	if !*first {
		out = append(out, ',')
	}
	*first = false
	out = append(out, `"messages":[`...)
	n := 0
	msgs.ForEach(func(_, mv gjson.Result) bool {
		if !mv.IsObject() {
			return true
		}
		role := mv.Get("role")
		content := mv.Get("content")
		switch {
		case rawStrEq(role.Raw, "system"), rawStrEq(role.Raw, "developer"):
			return true
		case rawStrEq(role.Raw, "user"):
			if content.Type == gjson.String {
				out, n = appendComma(out, n)
				out = append(out, `{"content":`...)
				out = append(out, content.Raw...)
				out = append(out, `,"role":"user"}`...)
				return true
			}
			start := len(out)
			out, n = appendComma(out, n)
			out = append(out, `{"content":[`...)
			var blocks int
			out, blocks = appendChatPartsMess(out, content)
			if blocks == 0 {
				out = out[:start]
				n--
				return true
			}
			out = append(out, `],"role":"user"}`...)
		case rawStrEq(role.Raw, "assistant"):
			start := len(out)
			out, n = appendComma(out, n)
			out = append(out, `{"content":[`...)
			blocks := 0
			out, blocks = appendChatPartsMess(out, content)
			if content.Type == gjson.String && len(content.Raw) > 2 {
				if blocks > 0 {
					out = append(out, ',')
				}
				blocks++
				out = append(out, `{"text":`...)
				out = append(out, content.Raw...)
				out = append(out, `,"type":"text"}`...)
			}
			if tcs := mv.Get("tool_calls"); tcs.IsArray() {
				tcs.ForEach(func(_, tc gjson.Result) bool {
					if !tc.IsObject() {
						return true
					}
					fn := tc.Get("function")
					if !fn.IsObject() {
						return true
					}
					if blocks > 0 {
						out = append(out, ',')
					}
					blocks++
					out = append(out, `{"id":`...)
					out = append(out, strOrEmpty(tc.Get("id"))...)
					out = append(out, `,"input":`...)
					out = appendParsedJSON(out, fn.Get("arguments"))
					out = append(out, `,"name":`...)
					out = append(out, strOrEmpty(fn.Get("name"))...)
					out = append(out, `,"type":"tool_use"}`...)
					return true
				})
			}
			if blocks == 0 {
				out = out[:start]
				n--
				return true
			}
			out = append(out, `],"role":"assistant"}`...)
		case rawStrEq(role.Raw, "tool"), rawStrEq(role.Raw, "function"):
			t, ok, _ := contentTextRaw(content)
			if !ok {
				return true
			}
			id := strOrEmpty(mv.Get("tool_call_id"))
			if rawStrEq(role.Raw, "function") {
				id = strOrEmpty(mv.Get("name"))
			}
			out, n = appendComma(out, n)
			out = append(out, `{"content":[{"content":`...)
			out = append(out, t...)
			out = append(out, `,"tool_use_id":`...)
			out = append(out, id...)
			out = append(out, `,"type":"tool_result"}],"role":"user"}`...)
		}
		return true
	})
	return append(out, ']')
}

func appendChatPartsMess(out []byte, content gjson.Result) ([]byte, int) {
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
				out, n = appendComma(out, n)
				out = append(out, `{"text":`...)
				out = append(out, t.Raw...)
				out = append(out, `,"type":"text"}`...)
			}
		case rawStrEq(p.Get("type").Raw, "image_url"):
			if u, ok := imageURLRaw(p); ok {
				start := len(out)
				out, n = appendComma(out, n)
				wrote := false
				out, wrote = appendImageSource(out, u)
				if !wrote {
					out = out[:start]
					n--
				}
			}
		}
		return true
	})
	return out, n
}

func chatStopRaw(v gjson.Result) string {
	if !v.Exists() || v.Type == gjson.Null {
		return ""
	}
	if v.Type == gjson.String {
		return "[" + v.Raw + "]"
	}
	if v.IsArray() {
		return v.Raw
	}
	return ""
}

// toolsFieldMap 描述一次 tools 数组转换的源/目标字段差异（§4 流 E2：四个
// append*Tools* 收敛为 appendToolsMapped 的映射表）。源一律读
// description/name，参数 schema 源键 inSchemaKey、目标片段 outSchemaFrag；
// schemaBeforeName 决定参数与 name 的输出顺序（Mess 目标 schema 在前、resp
// 目标 name 在前——逐字节保持原输出）。
type toolsFieldMap struct {
	nestedInput      bool   // 源工具嵌套在 "function" 下（OpenAI chat 形态）
	typeFunctionGate bool   // 源工具须 type=="function" 才转换（resp 源）
	inSchemaKey      string // 源参数 schema 字段名（parameters / input_schema）
	outSchemaFrag    string // 目标参数 schema 片段（含引号冒号，如 `"input_schema":`）
	schemaObjectOnly bool   // schema 门控：true=IsObject；false=Exists 且非 null
	schemaBeforeName bool   // 参数 schema 先于 name 输出
	emitType         bool   // 目标追加 "type":"function"
	copyStrict       bool   // 目标透传 strict
}

var (
	// chat tools（{function:{...}}）→ Mess tools（{name,description,input_schema}）。
	toolsMessFromChat = toolsFieldMap{nestedInput: true, inSchemaKey: "parameters", outSchemaFrag: `"input_schema":`, schemaObjectOnly: true, schemaBeforeName: true}
	// Mess tools（{name,description,input_schema}）→ resp tools（{...,type:"function"}）。
	toolsRespFromMess = toolsFieldMap{inSchemaKey: "input_schema", outSchemaFrag: `"parameters":`, schemaObjectOnly: true, emitType: true}
	// chat tools（{function:{...}}）→ resp tools（strict 透传）。
	toolsRespFromChat = toolsFieldMap{nestedInput: true, inSchemaKey: "parameters", outSchemaFrag: `"parameters":`, emitType: true, copyStrict: true}
	// resp tools（{type:"function",...}）→ Mess tools。
	toolsMessFromResp = toolsFieldMap{typeFunctionGate: true, inSchemaKey: "parameters", outSchemaFrag: `"input_schema":`, schemaObjectOnly: true, schemaBeforeName: true}
)

// appendToolsMapped tools 数组转换（映射表驱动）：四个方向的 tools 处理收敛
// 于此，源/目标键差异全由 m 表达，输出逐字节等价于原各方向内联版本。
func appendToolsMapped(out []byte, tools gjson.Result, m toolsFieldMap) []byte {
	n := 0
	tools.ForEach(func(_, tv gjson.Result) bool {
		if !tv.IsObject() {
			return true
		}
		src := tv
		if m.nestedInput {
			if src = tv.Get("function"); !src.IsObject() {
				return true
			}
		} else if m.typeFunctionGate && !rawStrEq(tv.Get("type").Raw, "function") {
			return true
		}
		out, n = appendComma(out, n)
		out = append(out, '{')
		wrote := false
		if d := src.Get("description"); d.Type == gjson.String {
			out = append(out, `"description":`...)
			out = append(out, d.Raw...)
			wrote = true
		}
		writeName := func() {
			if name := src.Get("name"); name.Type == gjson.String {
				if wrote {
					out = append(out, ',')
				}
				out = append(out, `"name":`...)
				out = append(out, name.Raw...)
				wrote = true
			}
		}
		writeSchema := func() {
			schema := src.Get(m.inSchemaKey)
			ok := schema.IsObject()
			if !m.schemaObjectOnly {
				ok = schema.Exists() && schema.Type != gjson.Null
			}
			if ok {
				if wrote {
					out = append(out, ',')
				}
				out = append(out, m.outSchemaFrag...)
				out = append(out, schema.Raw...)
				wrote = true
			}
		}
		if m.schemaBeforeName {
			writeSchema()
			writeName()
		} else {
			writeName()
			writeSchema()
		}
		if m.copyStrict {
			if s := src.Get("strict"); s.Type == gjson.True || s.Type == gjson.False {
				if wrote {
					out = append(out, ',')
				}
				out = append(out, `"strict":`...)
				out = append(out, s.Raw...)
				wrote = true
			}
		}
		if m.emitType {
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"type":"function"}`...)
		} else {
			out = append(out, '}')
		}
		return true
	})
	return out
}

// messToolChoiceString tool_choice 字符串形态 → Messages 对象字面量；非字符串
// 或未知值 → ok=false（Messages 没有字符串形式）。Chat/Responses 同名映射。
func messToolChoiceString(v gjson.Result) (string, bool) {
	if !v.Exists() || v.Type == gjson.Null || v.Type != gjson.String {
		return "", false
	}
	switch v.Str {
	case "auto":
		return `{"type":"auto"}`, true
	case "none":
		return `{"type":"none"}`, true
	case "required", "any":
		return `{"type":"any"}`, true
	}
	return "", false
}

// messToolChoiceFromName 由已定位的 name 结果组装 Messages tool_choice 对象；
// name 非字符串 → 丢弃。
func messToolChoiceFromName(name gjson.Result) string {
	if name.Type != gjson.String {
		return ""
	}
	return `{"name":` + name.Raw + `,"type":"tool"}`
}

// chatToolChoiceToMess Chat tool_choice → Messages 对象：字符串同名映射；
// {type:"function",function:{name}} → {name,type:"tool"}（嵌套名）。
func chatToolChoiceToMess(v gjson.Result) string {
	if s, ok := messToolChoiceString(v); ok {
		return s
	}
	if !v.IsObject() || !rawStrEq(v.Get("type").Raw, "function") {
		return ""
	}
	return messToolChoiceFromName(v.Get("function.name"))
}

// respToolChoiceToMess Responses tool_choice → Messages 对象：字符串同名映射；
// {type:"function",name} → {name,type:"tool"}（扁平名）。
func respToolChoiceToMess(v gjson.Result) string {
	if s, ok := messToolChoiceString(v); ok {
		return s
	}
	if !v.IsObject() || !rawStrEq(v.Get("type").Raw, "function") {
		return ""
	}
	return messToolChoiceFromName(v.Get("name"))
}

func messToRespRequest(body []byte) ([]byte, error) {
	root, err := parseRoot(body)
	if err != nil {
		return nil, err
	}
	var (
		sys, msgs, maxT, tools, toolChoice, thinking, outputConfig gjson.Result
		clientInclude                                              gjson.Result
		model, temperature, topP                                   string
		stream, metadata                                           string
	)
	if root.IsObject() {
		root.ForEach(func(k, v gjson.Result) bool {
			switch {
			case gjsonKeyEq(k, "system"):
				sys = v
			case gjsonKeyEq(k, "messages"):
				msgs = v
			case gjsonKeyEq(k, "max_tokens"):
				maxT = v
			case gjsonKeyEq(k, "tools"):
				tools = v
			case gjsonKeyEq(k, "tool_choice"):
				toolChoice = v
			case gjsonKeyEq(k, "model"):
				model = v.Raw
			case gjsonKeyEq(k, "temperature"):
				temperature = v.Raw
			case gjsonKeyEq(k, "top_p"):
				topP = v.Raw
			case gjsonKeyEq(k, "stream"):
				stream = v.Raw
			case gjsonKeyEq(k, "metadata"):
				metadata = v.Raw
			case gjsonKeyEq(k, "thinking"):
				thinking = v
			case gjsonKeyEq(k, "output_config"):
				outputConfig = v
			case gjsonKeyEq(k, "include"):
				clientInclude = v
			}
			return true
		})
	}
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	// thinking{type,budget_tokens} → reasoning{effort,summary}：budget 反查
	// effort（无 budget 的 adaptive → 只发 summary），output_config.effort 优先。
	rv := reasoningValue(thinking, outputConfig)
	if rv != "" {
		// 要拿到上游的加密思考才能在 signature 里往返（spec §2.5）：合并去重
		// 追加 reasoning.encrypted_content（不能覆盖客户端已有元素）。
		out = appendIncludeMerged(out, &first, clientInclude)
		out = appendField(out, &first, "reasoning", rv)
	}
	if msgs.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"input":`...)
		out = appendMessInput(out, msgs)
	}
	if ins := anthropicSystemRaw(sys); ins != "" {
		out = appendField(out, &first, "instructions", ins)
	}
	if maxT.Exists() {
		out = appendField(out, &first, "max_output_tokens", maxT.Raw)
	}
	if rawNotNull(metadata) {
		out = appendField(out, &first, "metadata", metadata)
	}
	if rawNotNull(model) {
		out = appendField(out, &first, "model", model)
	}
	if fieldRaw(toolChoice, "disable_parallel_tool_use").Type == gjson.True {
		out = appendField(out, &first, "parallel_tool_calls", "false")
	}
	if rawNotNull(stream) {
		out = appendField(out, &first, "stream", stream)
	}
	if rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if tc := messToolChoiceRaw(toolChoice); tc != "" {
		out = appendField(out, &first, "tool_choice", tc)
	}
	if tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendToolsMapped(out, tools, toolsRespFromMess)
		out = append(out, ']')
	}
	if rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	return append(out, '}'), nil
}

// includeReasoningEncrypted 上层不透明思考的回传必须被显式索取：没有这个
// include，上游只回摘要，signature 里的 reasoning_ref 臂就成了空壳（spec §2.5）。
const includeReasoningEncrypted = `"reasoning.encrypted_content"`

// appendIncludeMerged 顶层 "include"：客户端原有元素在前（去重），末尾补
// reasoning.encrypted_content。合并而非覆盖——覆盖会凭空丢掉客户端明确要的
// message.output_text.logprobs 之类元素（spec §4.2）。调用方仅在请求开启
// thinking 时走到这里（否则上游的加密思考无处承载）。
func appendIncludeMerged(out []byte, first *bool, clientInclude gjson.Result) []byte {
	if !*first {
		out = append(out, ',')
	}
	*first = false
	out = append(out, `"include":[`...)
	var seen []string
	add := func(raw string) {
		for _, s := range seen {
			if s == raw {
				return
			}
		}
		seen = append(seen, raw)
	}
	if clientInclude.IsArray() {
		clientInclude.ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				add(v.Raw)
			}
			return true
		})
	}
	add(includeReasoningEncrypted)
	for i, s := range seen {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, s...)
	}
	return append(out, ']')
}

// quotedSealPrefix 信封在 JSON 原文里的前缀。快路径判定用：未命中就不解码
// （spec §5 性能约定：信封解码只在 c3a1: 命中时发生）。
const quotedSealPrefix = `"` + sealPrefix

// isThinkingKind 内容块是否 Anthropic 的思考类块（thinking / redacted_thinking）。
func isThinkingKind(blk gjson.Result) bool {
	t := blk.Get("type").Raw
	return rawStrEq(t, "thinking") || rawStrEq(t, "redacted_thinking")
}

func fieldRaw(obj gjson.Result, name string) gjson.Result {
	var found gjson.Result
	if !obj.IsObject() {
		return found
	}
	obj.ForEach(func(k, v gjson.Result) bool {
		if gjsonKeyEq(k, name) {
			found = v
		}
		return true
	})
	return found
}

func anthropicSystemRaw(v gjson.Result) string {
	if !v.Exists() || v.Type == gjson.Null {
		return ""
	}
	if v.Type == gjson.String {
		return v.Raw
	}
	if !v.IsArray() {
		return ""
	}
	var parts []string
	v.ForEach(func(_, p gjson.Result) bool {
		if p.Type == gjson.String {
			parts = append(parts, p.Raw)
			return true
		}
		if t := p.Get("text"); t.Type == gjson.String {
			parts = append(parts, t.Raw)
		}
		return true
	})
	if len(parts) == 0 {
		return ""
	}
	buf := make([]byte, 0, 64)
	buf = appendJoinedTexts(buf, parts, "\n")
	return string(buf)
}

func appendMessInput(out []byte, msgs gjson.Result) []byte {
	out = append(out, '[')
	n := 0
	msgs.ForEach(func(_, mv gjson.Result) bool {
		if !mv.IsObject() {
			return true
		}
		role := mv.Get("role")
		content := mv.Get("content")
		switch {
		case rawStrEq(role.Raw, "user"):
			var texts []string
			type toolOut struct{ id, output string }
			var toolsOut []toolOut
			if content.Type == gjson.String {
				texts = append(texts, content.Raw)
			} else if content.IsArray() {
				content.ForEach(func(_, blk gjson.Result) bool {
					if !blk.IsObject() {
						return true
					}
					switch {
					case rawStrEq(blk.Get("type").Raw, "text"):
						if t := blk.Get("text"); t.Type == gjson.String {
							texts = append(texts, t.Raw)
						}
					case rawStrEq(blk.Get("type").Raw, "tool_result"):
						t, ok := blockTextRaw(blk.Get("content"), "text")
						if !ok {
							t = `""`
						}
						toolsOut = append(toolsOut, toolOut{strOrEmpty(blk.Get("tool_use_id")), t})
					}
					return true
				})
			}
			if len(texts) > 0 {
				out, n = appendComma(out, n)
				out = append(out, `{"content":[`...)
				for i, t := range texts {
					if i > 0 {
						out = append(out, ',')
					}
					out = append(out, `{"text":`...)
					out = append(out, t...)
					out = append(out, `,"type":"input_text"}`...)
				}
				out = append(out, `],"role":"user","type":"message"}`...)
			}
			for _, tr := range toolsOut {
				out, n = appendComma(out, n)
				out = append(out, `{"call_id":`...)
				out = append(out, tr.id...)
				out = append(out, `,"output":`...)
				out = append(out, tr.output...)
				out = append(out, `,"type":"function_call_output"}`...)
			}
		case rawStrEq(role.Raw, "assistant"):
			if content.Type == gjson.String {
				if len(content.Raw) > 2 {
					out, n = appendComma(out, n)
					out = append(out, `{"content":[{"text":`...)
					out = append(out, content.Raw...)
					out = append(out, `,"type":"output_text"}],"role":"assistant","type":"message"}`...)
				}
				return true
			}
			if !content.IsArray() {
				return true
			}
			// thinking / redacted_thinking 块：signature 若是 reasoning_ref 臂，
			// 把上游不透明的 encrypted_content 还原成 reasoning item 排在本轮
			// 内容**之前**（spec §2.5 / §4.2）。真 Anthropic 签名 / 空串 / 其他
			// type → 丢（约束 A：既不伪造签名，也不把 Anthropic 签名当
			// encrypted_content 发给 Responses 上游）。逐块独立裁决——全有或全无
			// 只属于 resp_to_mess 方向。
			content.ForEach(func(_, blk gjson.Result) bool {
				if !isThinkingKind(blk) {
					return true
				}
				sig := blk.Get("signature")
				if !strings.HasPrefix(sig.Raw, quotedSealPrefix) {
					return true
				}
				r, ok := openReasoningRef(sig.Str)
				if !ok {
					return true
				}
				out, n = appendComma(out, n)
				out = appendReasoningRefItem(out, r)
				return true
			})
			textN := 0
			started := false
			start := 0
			content.ForEach(func(_, blk gjson.Result) bool {
				if !rawStrEq(blk.Get("type").Raw, "text") {
					return true
				}
				t := blk.Get("text")
				if t.Type != gjson.String {
					return true
				}
				if !started {
					out, n = appendComma(out, n)
					out = append(out, `{"content":[`...)
					started = true
					start = len(out)
				}
				if textN > 0 {
					out = append(out, ',')
				}
				textN++
				out = append(out, `{"text":`...)
				out = append(out, t.Raw...)
				out = append(out, `,"type":"output_text"}`...)
				return true
			})
			if started {
				if textN == 0 {
					out = out[:start]
					n--
				} else {
					out = append(out, `],"role":"assistant","type":"message"}`...)
				}
			}
			content.ForEach(func(_, blk gjson.Result) bool {
				if !rawStrEq(blk.Get("type").Raw, "tool_use") {
					return true
				}
				id := strOrEmpty(blk.Get("id"))
				out, n = appendComma(out, n)
				out = append(out, `{"arguments":`...)
				out = append(out, marshalRaw(blk.Get("input"))...)
				out = append(out, `,"call_id":`...)
				out = append(out, id...)
				out = append(out, `,"id":`...)
				out = append(out, id...)
				out = append(out, `,"name":`...)
				out = append(out, strOrEmpty(blk.Get("name"))...)
				out = append(out, `,"type":"function_call"}`...)
				return true
			})
		}
		return true
	})
	return append(out, ']')
}

func messToolChoiceRaw(v gjson.Result) string {
	if !v.Exists() || v.Type == gjson.Null {
		return ""
	}
	if v.Type == gjson.String {
		if rawStrEq(v.Raw, "any") {
			return `"required"`
		}
		return v.Raw
	}
	if !v.IsObject() {
		return ""
	}
	switch {
	case rawStrEq(v.Get("type").Raw, "tool"):
		name := v.Get("name")
		if name.Type != gjson.String {
			return ""
		}
		return `{"name":` + name.Raw + `,"type":"function"}`
	case rawStrEq(v.Get("type").Raw, "any"):
		return `"required"`
	case rawStrEq(v.Get("type").Raw, "auto"):
		return `"auto"`
	case rawStrEq(v.Get("type").Raw, "none"):
		return `"none"`
	}
	return ""
}

func respToMessRequest(body []byte) ([]byte, error) {
	root, err := parseRoot(body)
	if err != nil {
		return nil, err
	}
	var (
		input, maxOut, tools, toolChoice, reasoning gjson.Result
		instructions, model, temperature, topP      string
		stream, metadata                            string
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
			case gjsonKeyEq(k, "temperature"):
				temperature = v.Raw
			case gjsonKeyEq(k, "top_p"):
				topP = v.Raw
			case gjsonKeyEq(k, "stream"):
				stream = v.Raw
			case gjsonKeyEq(k, "metadata"):
				metadata = v.Raw
			case gjsonKeyEq(k, "reasoning"):
				reasoning = v
			}
			return true
		})
	}
	// 上限归一化（spec §3.1）：写出值与 budget 基数同源。
	maxTok, _ := maxTokensField(maxOut)
	// 回放裁决（spec §2.3）：input 里每一个 reasoning item 都能解出可往返的块、
	// 且都在所属轮正文/工具调用之前，才允许回放 thinking。否则整请求不带思考
	// （部分剥离会让 Anthropic 判 prefix_binding_mismatch → 该会话此后每轮 400）。
	replay := scanRespThinking(input)
	// reasoning.effort → thinking（no_raise）；none/minimal/缺失/未知 → 不开启。
	// 强制 tool_choice 与 thinking 冲突时保留强制意图（§3.2）。
	tc := respToolChoiceToMess(toolChoice)
	think := ""
	if e := reasoningEffortFromBody(reasoning); e != "" && replay {
		think = thinkingValue(e, maxTok)
	}
	if think != "" && messToolChoiceForces(tc) {
		think = ""
	}
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	out = appendField(out, &first, "max_tokens", strconv.FormatInt(maxTok, 10))
	switch {
	case input.IsArray():
		out = appendRespMessMessages(out, &first, input, replay)
	case input.Type == gjson.String:
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"messages":[{"content":`...)
		out = append(out, input.Raw...)
		out = append(out, `,"role":"user"}]`...)
	}
	if rawNotNull(metadata) {
		out = appendField(out, &first, "metadata", metadata)
	}
	if rawNotNull(model) {
		out = appendField(out, &first, "model", model)
	}
	if rawNotNull(stream) {
		out = appendField(out, &first, "stream", stream)
	}
	if sys := respSystemRaw(instructions, input); sys != "" {
		out = appendField(out, &first, "system", sys)
	}
	if think == "" && rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if think != "" {
		out = appendField(out, &first, "thinking", think)
	}
	if tc != "" {
		out = appendField(out, &first, "tool_choice", tc)
	}
	if tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendToolsMapped(out, tools, toolsMessFromResp)
		out = append(out, ']')
	}
	if think == "" && rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	return append(out, '}'), nil
}

func respSystemRaw(instructions string, input gjson.Result) string {
	var parts []string
	if len(instructions) > 2 && instructions[0] == '"' {
		parts = append(parts, instructions)
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			role := item.Get("role").Raw
			if !rawStrEq(role, "system") && !rawStrEq(role, "developer") {
				return true
			}
			if ps := textPartsRaw(item.Get("content"), "input_text", "output_text", "text"); len(ps) > 0 {
				buf := make([]byte, 0, 32)
				buf = appendJoinedTexts(buf, ps, "\n")
				parts = append(parts, string(buf))
			}
			return true
		})
	}
	if len(parts) == 0 {
		return ""
	}
	buf := make([]byte, 0, 64)
	buf = appendJoinedTexts(buf, parts, "\n")
	return string(buf)
}

// appendRespMessMessages resp input → Messages messages。replay=false（§2.3 判定
// 不可回放）时 thinking 块整体不写出——部分剥离会让 Anthropic 判会话前缀失配，
// 该会话此后每轮 400。
func appendRespMessMessages(out []byte, first *bool, input gjson.Result, replay bool) []byte {
	if !*first {
		out = append(out, ',')
	}
	*first = false
	out = append(out, `"messages":[`...)
	n := 0
	asstOpen := false
	closeAsst := func() {
		out = append(out, `],"role":"assistant"}`...)
		asstOpen = false
	}
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		typ := item.Get("type")
		if asstOpen && !rawStrEq(typ.Raw, "function_call") && !rawStrEq(typ.Raw, "reasoning") {
			closeAsst()
		}
		switch {
		case rawStrEq(typ.Raw, "message"):
			role := item.Get("role")
			if !rawStrEq(role.Raw, "user") && !rawStrEq(role.Raw, "assistant") {
				return true
			}
			start := len(out)
			out, n = appendComma(out, n)
			out = append(out, `{"content":[`...)
			blocks := 0
			content := item.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, p gjson.Result) bool {
					switch {
					case rawStrEq(p.Get("type").Raw, "input_text"), rawStrEq(p.Get("type").Raw, "output_text"):
						if t := p.Get("text"); t.Type == gjson.String {
							out, blocks = appendComma(out, blocks)
							out = append(out, `{"text":`...)
							out = append(out, t.Raw...)
							out = append(out, `,"type":"text"}`...)
						}
					case rawStrEq(p.Get("type").Raw, "input_image"):
						if u := p.Get("image_url"); u.Type == gjson.String && len(u.Raw) > 2 {
							mark := len(out)
							out, blocks = appendComma(out, blocks)
							wrote := false
							out, wrote = appendImageSource(out, u.Raw)
							if !wrote {
								out = out[:mark]
								blocks--
							}
						}
					}
					return true
				})
			}
			if blocks == 0 {
				out = out[:start]
				n--
				return true
			}
			if rawStrEq(role.Raw, "assistant") {
				asstOpen = true
				return true
			}
			out = append(out, `],"role":`...)
			out = append(out, role.Raw...)
			out = append(out, '}')
		case rawStrEq(typ.Raw, "function_call"):
			if !asstOpen {
				out, n = appendComma(out, n)
				out = append(out, `{"content":[`...)
				asstOpen = true
			} else if out[len(out)-1] != '[' {
				out = append(out, ',')
			}
			out = append(out, `{"id":`...)
			out = append(out, fcIDRaw(item)...)
			out = append(out, `,"input":`...)
			out = appendParsedJSON(out, item.Get("arguments"))
			out = append(out, `,"name":`...)
			out = append(out, strOrEmpty(item.Get("name"))...)
			out = append(out, `,"type":"tool_use"}`...)
		case rawStrEq(typ.Raw, "reasoning"):
			// reasoning item → thinking 块：只有本包信封（encrypted_content 内带
			// Anthropic 原始签名）才可回放；且 §2.3 判定为全有或全无（replay）时
			// 才整体回放，否则一个块都不写（不伪造无签名块）。
			if !replay {
				return true
			}
			enc := item.Get("encrypted_content")
			if enc.Type != gjson.String {
				return true
			}
			blk, ok := openThinking(enc.Str)
			if !ok {
				return true
			}
			if !asstOpen {
				out, n = appendComma(out, n)
				out = append(out, `{"content":[`...)
				asstOpen = true
			} else if out[len(out)-1] != '[' {
				out = append(out, ',')
			}
			out = appendThinkingBlock(out, blk)
		case rawStrEq(typ.Raw, "function_call_output"):
			t, ok := blockTextRaw(item.Get("output"), "input_text", "text")
			if !ok {
				t = `""`
			}
			out, n = appendComma(out, n)
			out = append(out, `{"content":[{"content":`...)
			out = append(out, t...)
			out = append(out, `,"tool_use_id":`...)
			out = append(out, strOrEmpty(item.Get("call_id"))...)
			out = append(out, `,"type":"tool_result"}],"role":"user"}`...)
		}
		return true
	})
	if asstOpen {
		closeAsst()
	}
	return append(out, ']')
}
