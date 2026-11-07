// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 三条请求转换的字节路径：gjson 单遍取字段，值原样拼进一个输出缓冲。
// 语义与原 map+json.Marshal 版一致（测试钉住）。chat→resp 仍在 chat_resp.go。

import (
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
	out = appendIntField(out, &first, "max_tokens", maxTok)
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

// chatSystemRaw system / developer 消息折叠为顶层 system 字段（内容模型的
// cm_sysdev_text 行：item 级 polOmit，文本在此折叠写出）。
func chatSystemRaw(msgs gjson.Result) string {
	if !msgs.IsArray() {
		return ""
	}
	var parts []string
	msgs.ForEach(func(_, mv gjson.Result) bool {
		if !mv.IsObject() {
			return true
		}
		if _, ok := lookup(dirChatToMess, shCMSysdevText, roleOf(mv.Get("role").Raw)); !ok {
			return true
		}
		if t, hasText, _ := contentTextRaw(mv.Get("content")); hasText {
			parts = append(parts, t)
		}
		return true
	})
	return foldSystemTexts(tgtMess, parts)
}

// appendChatMessMessages messages → Messages 消息数组（dirChatToMess 表分派）。
// system/developer 已折叠进顶层 system（cm_sysdev_text = polOmit），此处不写。
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
		r := roleOf(role.Raw)
		switch {
		case r == roleSystem || r == roleDeveloper:
			return true
		case r == roleUser:
			if content.Type == gjson.String {
				if _, ok := mapsTo(dirChatToMess, shCMUserText, r); !ok {
					return true
				}
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
			out, blocks = appendChatPartsMess(out, content, r)
			if blocks == 0 {
				out = out[:start]
				n--
				return true
			}
			out = append(out, `],"role":"user"}`...)
		case r == roleAssistant:
			start := len(out)
			out, n = appendComma(out, n)
			out = append(out, `{"content":[`...)
			blocks := 0
			out, blocks = appendChatPartsMess(out, content, r)
			if content.Type == gjson.String && len(content.Raw) > 2 {
				if _, ok := mapsTo(dirChatToMess, shCMAsstText, r); ok {
					if blocks > 0 {
						out = append(out, ',')
					}
					blocks++
					out = append(out, `{"text":`...)
					out = append(out, content.Raw...)
					out = append(out, `,"type":"text"}`...)
				}
			}
			if tcs := mv.Get("tool_calls"); tcs.IsArray() {
				if _, ok := mapsTo(dirChatToMess, shCMAsstToolFunction, r); ok {
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
			}
			if blocks == 0 {
				out = out[:start]
				n--
				return true
			}
			out = appendAssistantTurn(out, roleAssistant, true)
		case r == roleTool:
			sh := shCMToolMsg
			if rawStrEq(role.Raw, "function") {
				sh = shCMFunctionMsg
			}
			if _, ok := mapsTo(dirChatToMess, sh, r); !ok {
				return true
			}
			t, hasText, _ := contentTextRaw(content)
			if !hasText {
				return true
			}
			id := strOrEmpty(mv.Get("tool_call_id"))
			if rawStrEq(role.Raw, "function") {
				id = strOrEmpty(mv.Get("name"))
			}
			if !chatDeclaresToolCall(msgs, id) {
				// 孤儿 tool_result：没有对应的 tool_use 就丢弃（不合成 call，J11）
				return true
			}
			out, n = appendComma(out, n)
			out = appendToolResult(out, tgtMess, id, t, true)
		}
		return true
	})
	return append(out, ']')
}

// appendChatPartsMess chat 内容部件 → Messages 内容块（dirChatToMess 表分派）。
// image_url 部件按 url 形态落到三个成员：http(s) → image{source:url}、
// data URL → image{source:base64}、裸 base64 → ∅（chat.txt:162 允许该写法，
// 但 Messages 的 image.source 没有裸 base64 形态）。assistant 的非文本部件
// 一律落 cm_asst_other_parts 丢弃（J2）。
func appendChatPartsMess(out []byte, content gjson.Result, r role) ([]byte, int) {
	if !content.IsArray() {
		return out, 0
	}
	n := 0
	content.ForEach(func(_, p gjson.Result) bool {
		if !p.IsObject() {
			return true
		}
		sh := chatMessPartShape(r, p)
		if _, ok := emits(dirChatToMess, sh, r); !ok {
			return true
		}
		switch sh {
		case shCMUserText, shCMAsstText:
			if t := p.Get("text"); t.Type == gjson.String {
				out, n = appendComma(out, n)
				out = append(out, `{"text":`...)
				out = append(out, t.Raw...)
				out = append(out, `,"type":"text"}`...)
			}
		case shCMAsstRefusal:
			// refusal 在 Messages 目标降级为 text（J6/L12）
			if rf := p.Get("refusal"); rf.Type == gjson.String {
				out, n = appendComma(out, n)
				out = append(out, `{"text":`...)
				out = append(out, rf.Raw...)
				out = append(out, `,"type":"text"}`...)
			}
		case shCMUserImgURL, shCMUserImgData:
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

// chatMessPartShape chat 内容部件 → chat→mess 的成员形状。
func chatMessPartShape(r role, p gjson.Result) shape {
	typ := p.Get("type")
	if r == roleAssistant {
		switch {
		case rawStrEq(typ.Raw, "text"):
			return shCMAsstText
		case rawStrEq(typ.Raw, "refusal"):
			return shCMAsstRefusal
		}
		return shCMAsstOtherParts
	}
	switch {
	case rawStrEq(typ.Raw, "text"):
		return shCMUserText
	case rawStrEq(typ.Raw, "image_url"):
		switch chatImgShape(p) {
		case shCUserImgURL:
			return shCMUserImgURL
		case shCUserImgData:
			return shCMUserImgData
		case shCUserImgBare64:
			return shCMUserImgBare64
		}
		return shCMTail
	case rawStrEq(typ.Raw, "input_audio"):
		return shCMUserAudio
	case rawStrEq(typ.Raw, "file"):
		if f := p.Get("file"); f.IsObject() {
			if id := f.Get("file_id"); id.Type == gjson.String && len(id.Raw) > 2 {
				return shCMUserFileID
			}
		}
		return shCMUserFileData
	}
	return shCMTail
}

// chatDeclaresToolCall 请求里是否有 assistant 消息声明了该 tool 调用
// （J11：不合成 call，孤儿 tool_result 丢弃）。消息数很小，逐条比对即可；
// gjson.Parse 只去引号，零分配。
func chatDeclaresToolCall(msgs gjson.Result, callID string) bool {
	if !msgs.IsArray() {
		return false
	}
	want := gjson.Parse(callID).Str
	found := false
	msgs.ForEach(func(_, mv gjson.Result) bool {
		if !rawStrEq(mv.Get("role").Raw, "assistant") {
			return true
		}
		tcs := mv.Get("tool_calls")
		if !tcs.IsArray() {
			return true
		}
		tcs.ForEach(func(_, tc gjson.Result) bool {
			if gjson.Parse(strOrEmpty(tc.Get("id"))).Str == want {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

// respDeclaresCall resp 请求的 input 里是否有 function_call 项声明了该调用
// （匹配键取 call_id 优先、id 兜底，与 fcIDRaw 同口径）。
func respDeclaresCall(input gjson.Result, callID string) bool {
	if !input.IsArray() {
		return false
	}
	want := gjson.Parse(callID).Str
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if !rawStrEq(item.Get("type").Raw, "function_call") {
			return true
		}
		if gjson.Parse(fcIDRaw(item)).Str == want {
			found = true
			return false
		}
		return true
	})
	return found
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
	dir              direction  // 方向（与 mode 同源，供 TestToolsMapsComplete 派生断言）
	nestedInput      bool       // 源工具嵌套在 "function" 下（OpenAI chat 形态）
	typeFunctionGate bool       // 源工具须 type=="function" 才转换（resp 源）
	inSchemaKey      string     // 源参数 schema 字段名（parameters / input_schema）
	outSchemaFrag    string     // 目标参数 schema 片段（含引号冒号，如 `"input_schema":`）
	mode             schemaMode // schema 处理模式（v18：取代 schemaObjectOnly）
	schemaBeforeName bool       // 参数 schema 先于 name 输出
	emitType         bool       // 目标追加 "type":"function"
	copyStrict       bool       // 目标透传 strict
	nestedOut        bool       // 目标把工具包进 {"function":{…},"type":"function"}（chat 形态）
}

var (
	// chat tools（{function:{...}}）→ Mess tools（{name,description,input_schema}）。
	toolsMessFromChat = toolsFieldMap{dir: dirChatToMess, nestedInput: true, inSchemaKey: "parameters", outSchemaFrag: `"input_schema":`, mode: schemaModeMessDialect, schemaBeforeName: true}
	// Mess tools（{name,description,input_schema}）→ resp tools（{...,type:"function"}）。
	toolsRespFromMess = toolsFieldMap{dir: dirMessToResp, inSchemaKey: "input_schema", outSchemaFrag: `"parameters":`, mode: schemaModeObjectOnly, emitType: true}
	// chat tools（{function:{...}}）→ resp tools（strict 透传）。
	toolsRespFromChat = toolsFieldMap{dir: dirChatToResp, nestedInput: true, inSchemaKey: "parameters", outSchemaFrag: `"parameters":`, mode: schemaModeExistence, emitType: true, copyStrict: true}
	// resp tools（{type:"function",...}）→ Mess tools。
	toolsMessFromResp = toolsFieldMap{dir: dirRespToMess, typeFunctionGate: true, inSchemaKey: "parameters", outSchemaFrag: `"input_schema":`, mode: schemaModeMessDialect, schemaBeforeName: true}
	// Mess tools（{name,description,input_schema}）→ chat tools（{type:"function",function:{…}}）。
	// chat 是**嵌套**形态（与 resp 的扁平相反）；schema 走 objectOnly（chat 把缺省
	// parameters 视为空对象，不发明默认 schema）。
	toolsChatFromMess = toolsFieldMap{dir: dirMessToChatReq, inSchemaKey: "input_schema", outSchemaFrag: `"parameters":`, mode: schemaModeObjectOnly, nestedOut: true, copyStrict: true}
	// resp tools（扁平 {type:"function",name,parameters}）→ chat tools（嵌套）。
	// typeFunctionGate：内置工具（custom / web_search_preview / computer_use_preview
	// / image_generation / code_interpreter）在 chat 侧没有 function 定义，读取侧
	// 必须先过滤（丢弃计数见 noteNonFunctionTools）。
	toolsChatFromResp = toolsFieldMap{dir: dirRespToChatReq, typeFunctionGate: true, inSchemaKey: "parameters", outSchemaFrag: `"parameters":`, mode: schemaModeObjectOnly, nestedOut: true, copyStrict: true}
)

// lastField 取工具对象里某个成员的**末个**取值（末个胜，零分配）。工具级重复键
// 必须与上游 json.loads 一致；gjson.Get 是首胜，会让"客户端与上游读的不是同一个
// 值"（metis-r2 C-3：重复 parameters 时产物会收窄）。
func lastField(obj gjson.Result, name string) (gjson.Result, bool) {
	sp, ok := fieldSpanStr(obj.Raw, name)
	if !ok {
		return gjson.Result{}, false
	}
	return gjson.Parse(obj.Raw[sp.lo:sp.hi]), true
}

// appendToolsMapped tools 数组转换（映射表驱动）：四个方向的 tools 处理收敛
// 于此，源/目标键差异全由 m 表达，输出逐字节等价于原各方向内联版本。
// mess 方言方向（m.mode == schemaModeMessDialect）先求值再写 description：
// 退化提示要写进 description，而退化只能由 schema 求值判定。
func appendToolsMapped(out []byte, tools gjson.Result, m toolsFieldMap) []byte {
	n := 0
	tools.ForEach(func(_, tv gjson.Result) bool {
		if !tv.IsObject() {
			return true
		}
		src := tv
		if m.nestedInput {
			fn, ok := lastField(tv, "function")
			if !ok || !fn.IsObject() {
				return true
			}
			src = fn
		} else if m.typeFunctionGate {
			typ, ok := lastField(tv, "type")
			if !ok || !rawStrEq(typ.Raw, "function") {
				return true
			}
		}
		// 先求值（mess 方言），供 description 的 hint 与后面的 schema 字段共用。
		schemaStr, schemaBytes, deg := evalSchemaField(m, src)
		out, n = appendComma(out, n)
		out = append(out, '{')
		if m.nestedOut {
			out = append(out, `"function":{`...)
		}
		wrote := false
		if d, ok := lastField(src, "description"); ok && d.Type == gjson.String {
			out = append(out, `"description":`...)
			if deg == degNone {
				out = append(out, d.Raw...)
			} else {
				out = append(out, encodeDescriptionWithHint(d.Str, deg)...)
			}
			wrote = true
		}
		writeName := func() {
			if name, ok := lastField(src, "name"); ok && name.Type == gjson.String {
				if wrote {
					out = append(out, ',')
				}
				out = append(out, `"name":`...)
				out = append(out, name.Raw...)
				wrote = true
			}
		}
		writeSchema := func() {
			if schemaStr == "" && schemaBytes == nil {
				return
			}
			if wrote {
				out = append(out, ',')
			}
			out = append(out, m.outSchemaFrag...)
			if schemaBytes == nil {
				out = append(out, schemaStr...)
			} else {
				out = append(out, schemaBytes...)
			}
			wrote = true
		}
		if m.schemaBeforeName {
			writeSchema()
			writeName()
		} else {
			writeName()
			writeSchema()
		}
		if m.copyStrict {
			if s, ok := lastField(src, "strict"); ok && (s.Type == gjson.True || s.Type == gjson.False) {
				if wrote {
					out = append(out, ',')
				}
				out = append(out, `"strict":`...)
				out = append(out, s.Raw...)
				wrote = true
			}
		}
		switch {
		case m.nestedOut:
			// 嵌套形态：先闭合 function 对象，再补 "type":"function"（键序字母序：
			// function < type），与 chat 官方 ChatCompletionTool 一致。
			out = append(out, '}')
			out = append(out, `,"type":"function"}`...)
		case m.emitType:
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"type":"function"}`...)
		default:
			out = append(out, '}')
		}
		return true
	})
	return out
}

// evalSchemaField 计算 schema 字段的值字节；nil 表示该字段不写出。
func evalSchemaField(m toolsFieldMap, src gjson.Result) (string, []byte, degenerateKind) {
	in, inOK := lastField(src, m.inSchemaKey)
	switch m.mode {
	case schemaModeMessDialect:
		res := evalMessSchema(in)
		if res.raw != "" {
			return res.raw, nil, res.deg // P_in 成立：不观测、不分配（I2）
		}
		observeSchema(m, src, in, res)
		return "", res.out, res.deg
	case schemaModeObjectOnly:
		if inOK && in.IsObject() {
			return in.Raw, nil, degNone
		}
		return "", nil, degNone
	case schemaModeExistence:
		if inOK && in.Exists() && in.Type != gjson.Null {
			return in.Raw, nil, degNone
		}
		return "", nil, degNone
	default:
		panic("protoconv: unknown schemaMode")
	}
}

// hintOf 退化种类 → 追加文案。
func hintOf(deg degenerateKind) string {
	switch deg {
	case degBail:
		return bailHint
	case degSimplify:
		return degenerateHint
	}
	return ""
}

// encodeDescriptionWithHint 幂等追加提示（按实际要追加的那一份判重；超长则原样输出）。
func encodeDescriptionWithHint(desc string, deg degenerateKind) []byte {
	hint := hintOf(deg)
	if hint == "" || strings.HasSuffix(desc, hint) || len(desc)+len(hint) > maxDescriptionBytes {
		var out []byte
		return appendJSONString(out, desc)
	}
	var out []byte
	return appendJSONString(out, desc+hint)
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
	if rw, ok := mapsTo(dirMessToResp, shMTopSystem, roleInvalid); ok {
		if ins := anthropicSystemRaw(sys); ins != "" {
			out = appendField(out, &first, rw.name, ins)
		}
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
	// reasoning 开启时丢弃 temperature / top_p（spec §3.2：所有目标方向同口径）。
	// 思考与自定义采样不兼容；OpenAI 侧虽不报错，但带上会让思考行为不可预期。
	if rv == "" && rawNotNull(temperature) {
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
	if rv == "" && rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	return append(out, '}'), nil
}

// includeReasoningEncrypted 上层不透明思考的回传必须被显式索取：没有这个
// include，上游只回摘要，signature 里的 reasoning_ref 臂就成了空壳（spec §2.5）。
const (
	// includeReasoningEncryptedValue include 元素的解码值（去重比较用）。
	includeReasoningEncryptedValue = "reasoning.encrypted_content"
	// includeReasoningEncrypted 同一元素的 JSON 字面量（写出用）。
	includeReasoningEncrypted = `"` + includeReasoningEncryptedValue + `"`
)

// appendIncludeMerged 顶层 "include"：客户端原有元素在前（去重），末尾补
// reasoning.encrypted_content。合并而非覆盖——覆盖会凭空丢掉客户端明确要的
// message.output_text.logprobs 之类元素（spec §4.2）。客户端把 include 写成
// 单个字符串（合法但少见）时按单元素并入；既非字符串也非数组则只发自己的
// 元素。调用方仅在请求开启 thinking 时走到这里（否则上游的加密思考无处承载）。
func appendIncludeMerged(out []byte, first *bool, clientInclude gjson.Result) []byte {
	if !*first {
		out = append(out, ',')
	}
	*first = false
	out = append(out, `"include":[`...)
	// 去重按**解码后**的元素比较（"reasoning\u002eencrypted_content" 与
	// reasoning.encrypted_content 是同一个值），写出仍用原始字节形态（保留客户端
	// 的转义写法）。
	var keys, raws []string
	add := func(decoded, raw string) {
		for _, k := range keys {
			if k == decoded {
				return
			}
		}
		keys = append(keys, decoded)
		raws = append(raws, raw)
	}
	switch {
	case clientInclude.IsArray():
		clientInclude.ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				add(v.Str, v.Raw)
			}
			return true
		})
	case clientInclude.Type == gjson.String:
		// 单字符串形态：按单元素并入，不能丢客户端原值。
		add(clientInclude.Str, clientInclude.Raw)
	}
	add(includeReasoningEncryptedValue, includeReasoningEncrypted)
	for i, s := range raws {
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

// anthropicSystemRaw Mess 顶层 system → resp instructions 字段值
// （内容模型的 m_top_system 行：字符串或 text 块数组都折叠成一条文本）。
func anthropicSystemRaw(v gjson.Result) string {
	return foldSystemTexts(tgtResp, anthropicSystemParts(v))
}

// anthropicSystemParts Mess 顶层 system → 各段文本的 **JSON 字符串字面量**（Raw）。
// 非文本块不返回（丢不丢、计不计由调用方决定：chat 目标要计 `system_block`，
// resp 目标沿用既有行为不计数）。与 anthropicSystemRaw 共用同一套"什么算文本"
// 的判据，避免两处各写一遍。
func anthropicSystemParts(v gjson.Result) []string {
	if !v.Exists() || v.Type == gjson.Null {
		return nil
	}
	if v.Type == gjson.String {
		return []string{v.Raw}
	}
	if !v.IsArray() {
		return nil
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
	return parts
}

// messInputPartShape Mess 内容块 → mess→resp 的成员形状。按 (role, 块类型,
// 源的子形态) 判定；未建模块落 tail（polDrop）。
func messInputPartShape(r role, blk gjson.Result) shape {
	typ := blk.Get("type")
	switch {
	case rawStrEq(typ.Raw, "text"):
		if r == roleAssistant {
			return shMAsstText
		}
		return shMUserText
	case rawStrEq(typ.Raw, "image"):
		src := blk.Get("source.type")
		switch {
		case rawStrEq(src.Raw, "file"):
			if r == roleAssistant {
				return shMAsstImgFileID
			}
			return shMUserImgFileID
		case rawStrEq(src.Raw, "url"):
			if r == roleAssistant {
				return shMAsstImgURL
			}
			return shMUserImgURL
		}
		if r == roleAssistant {
			return shMAsstImgBase64
		}
		return shMUserImgBase64
	case rawStrEq(typ.Raw, "document"):
		switch {
		case rawStrEq(blk.Get("source.type").Raw, "file"):
			return shMUserDocFileID
		case rawStrEq(blk.Get("source.type").Raw, "url"):
			return shMUserDocURL
		}
		return shMUserDocB64TextContent
	case rawStrEq(typ.Raw, "search_result"):
		if r == roleAssistant {
			return shMAsstSearchResult
		}
		return shMUserSearchResult
	case rawStrEq(typ.Raw, "thinking"):
		return shMThinking
	case rawStrEq(typ.Raw, "redacted_thinking"):
		return shMRedactedThinking
	}
	return shMTail
}

// messToolResultShape tool_result.content 的成员形状：缺席 / 字符串 / 数组含
// text / 数组无 text 且非空（嵌套媒体等不可搬运块）/ 空数组。
//
// hasText 由调用方一次算出（blockTextRaw 只会被调用一次）；「非空」用数组长度
// 判定而不重扫内容——重扫的闭包会逃逸成一次堆分配，热路径不允许。
func messToolResultShape(c gjson.Result, hasText bool) shape {
	switch {
	case c.Type == gjson.String:
		return shMToolResultString
	case c.IsArray():
		switch {
		case hasText:
			return shMToolResultArrayText
		case c.Get("#").Int() > 0:
			return shMToolResultNestedMedia
		}
		return shMToolResultArrayNoText
	}
	return shMToolResultNoContent
}

// messImageDataURL image{source:{type:"base64",media_type,data}} → 「data URL」
// 的 JSON 字符串字面量（resp 的 input_image.image_url 只接受 URL 或 data
// URL，resp.txt:187）。
func messImageDataURL(blk gjson.Result) (string, bool) {
	mt := blk.Get("source.media_type")
	data := blk.Get("source.data")
	if mt.Type != gjson.String || data.Type != gjson.String || len(mt.Raw) <= 2 || len(data.Raw) <= 2 {
		return "", false
	}
	buf := make([]byte, 0, len(mt.Raw)+len(data.Raw)+24)
	buf = append(buf, `"data:`...)
	buf = append(buf, mt.Raw[1:len(mt.Raw)-1]...)
	buf = append(buf, `;base64,`...)
	buf = append(buf, data.Raw[1:len(data.Raw)-1]...)
	buf = append(buf, '"')
	return string(buf), true
}

// appendMessInput Messages 消息 → resp input 项（dirMessToResp 表分派）。
func appendMessInput(out []byte, msgs gjson.Result) []byte {
	out = append(out, '[')
	n := 0
	msgs.ForEach(func(_, mv gjson.Result) bool {
		if !mv.IsObject() {
			return true
		}
		role := mv.Get("role")
		content := mv.Get("content")
		r := roleOf(role.Raw)
		switch {
		case r == roleUser:
			var texts, imgURLs, docURLs []string
			type toolOut struct{ id, output string }
			var toolsOut []toolOut
			if content.Type == gjson.String {
				if _, ok := mapsTo(dirMessToResp, shMUserText, r); ok {
					texts = append(texts, content.Raw)
				}
			} else if content.IsArray() {
				content.ForEach(func(_, blk gjson.Result) bool {
					if !blk.IsObject() {
						return true
					}
					if rawStrEq(blk.Get("type").Raw, "tool_result") {
						// tool_result.content 可缺席/字符串/数组（mess.txt:584）；
						// 无文本 → ""（function_call_output.output 必填，L16）。
						c := blk.Get("content")
						t, hasText := blockTextRaw(c, "text")
						sh := messToolResultShape(c, hasText)
						if _, ok := mapsTo(dirMessToResp, sh, r); !ok {
							return true
						}
						if !hasText {
							t = `""`
						}
						toolsOut = append(toolsOut, toolOut{strOrEmpty(blk.Get("tool_use_id")), t})
						return true
					}
					switch messInputPartShape(r, blk) {
					case shMUserText:
						if _, ok := mapsTo(dirMessToResp, shMUserText, r); !ok {
							return true
						}
						if t := blk.Get("text"); t.Type == gjson.String {
							texts = append(texts, t.Raw)
						}
					case shMUserImgBase64:
						// image{base64,media_type} → input_image{image_url: data URL}
						// （与 chat 的 data URL 形态逐字节相同，§5.1#7）
						if _, ok := mapsTo(dirMessToResp, shMUserImgBase64, r); !ok {
							return true
						}
						if u, ok := messImageDataURL(blk); ok {
							imgURLs = append(imgURLs, u)
						}
					case shMUserImgURL:
						if _, ok := mapsTo(dirMessToResp, shMUserImgURL, r); !ok {
							return true
						}
						if u := blk.Get("source.url"); u.Type == gjson.String && len(u.Raw) > 2 {
							imgURLs = append(imgURLs, u.Raw)
						}
					case shMUserDocURL:
						// J5：document{source:url} → input_file{file_url}
						if _, ok := mapsTo(dirMessToResp, shMUserDocURL, r); !ok {
							return true
						}
						if u := blk.Get("source.url"); u.Type == gjson.String && len(u.Raw) > 2 {
							docURLs = append(docURLs, u.Raw)
						}
					}
					return true
				})
			}
			if len(texts) > 0 || len(imgURLs) > 0 || len(docURLs) > 0 {
				sh := shMUserText
				if len(texts) == 0 {
					sh = shMUserImgURL
					if len(imgURLs) == 0 {
						sh = shMUserDocURL
					}
				}
				rw, ok := mapsTo(dirMessToResp, sh, r)
				if !ok {
					// 行策略不允许写 message 项（当前不可达：三个候选行均为
					// polMap + rmUser）——显式跳过，不写半成品。
					return true
				}
				out, n = appendComma(out, n)
				out = append(out, `{"content":[`...)
				parts := 0
				for _, t := range texts {
					out, parts = appendComma(out, parts)
					out = append(out, `{"text":`...)
					out = append(out, t...)
					out = append(out, `,"type":"input_text"}`...)
				}
				for _, u := range imgURLs {
					out, parts = appendComma(out, parts)
					out = append(out, `{"image_url":`...)
					out = append(out, u...)
					out = append(out, `,"type":"input_image"}`...)
				}
				for _, u := range docURLs {
					out, parts = appendComma(out, parts)
					out = append(out, `{"file_url":`...)
					out = append(out, u...)
					out = append(out, `,"type":"input_file"}`...)
				}
				out = append(out, `],"role":"user","type":`...)
				out = appendQuoted(out, rw.name)
				out = append(out, '}')
			}
			for _, tr := range toolsOut {
				out, n = appendComma(out, n)
				out = appendToolResult(out, tgtResp, tr.id, tr.output, true)
			}
		case r == roleAssistant:
			if content.Type == gjson.String {
				rw, ok := mapsTo(dirMessToResp, shMAsstText, r)
				if ok && len(content.Raw) > 2 {
					out, n = appendComma(out, n)
					out = append(out, `{"content":[{"text":`...)
					out = append(out, content.Raw...)
					out = append(out, `,"type":"output_text"}],"role":"assistant","type":`...)
					out = appendQuoted(out, rw.name)
					out = append(out, '}')
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
				if rawStrEq(blk.Get("type").Raw, "redacted_thinking") {
					// 不透明块的载荷在 data 里；只有本包信封才能往返（§2.5）
					sig = blk.Get("data")
				}
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
			rwText, textMapped := mapsTo(dirMessToResp, shMAsstText, r)
			textN := 0
			started := false
			start := 0
			content.ForEach(func(_, blk gjson.Result) bool {
				if !textMapped || !rawStrEq(blk.Get("type").Raw, "text") {
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
					out = append(out, `],"role":"assistant","type":`...)
					out = appendQuoted(out, rwText.name)
					out = append(out, '}')
				}
			}
			rwFC, fcMapped := mapsTo(dirMessToResp, shMAsstToolUse, r)
			content.ForEach(func(_, blk gjson.Result) bool {
				if !fcMapped || !rawStrEq(blk.Get("type").Raw, "tool_use") {
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
				out = append(out, `,"type":`...)
				out = appendQuoted(out, rwFC.name)
				out = append(out, '}')
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
	out = appendIntField(out, &first, "max_tokens", maxTok)
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

// respSystemRaw instructions + input 内 system/developer 消息项 → 目标顶层
// system（r_msg_sysdev_text 折叠；该行 item 级为 polOmit）。
func respSystemRaw(instructions string, input gjson.Result) string {
	var parts []string
	if len(instructions) > 2 && instructions[0] == '"' {
		parts = append(parts, instructions)
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			rw, ok := lookup(dirRespToMess, shRMsgSysdevText, roleOf(item.Get("role").Raw))
			if !ok || rw.pol != polOmit {
				// 该行必须是 polOmit（已折叠到顶层 system）：策略变了就不该在这里折叠。
				return true
			}
			if ps := textPartsRaw(item.Get("content"), "input_text", "output_text", "text"); len(ps) > 0 {
				buf := make([]byte, 0, 32)
				buf = appendJoinedTexts(buf, ps, systemFoldSep)
				parts = append(parts, string(buf))
			}
			return true
		})
	}
	return foldSystemTexts(tgtMess, parts)
}

// appendImageBlock resp message 的 input_image 部件 → mess image 块
// （source 由 appendImageSource 按 url 形态决定；写不出来则整体回滚）。
func appendImageBlock(out []byte, n int, p gjson.Result) ([]byte, int) {
	u := p.Get("image_url")
	if u.Type != gjson.String || len(u.Raw) <= 2 {
		return out, n
	}
	mark := len(out)
	out, n = appendComma(out, n)
	wrote := false
	out, wrote = appendImageSource(out, u.Raw)
	if !wrote {
		return out[:mark], n - 1
	}
	return out, n
}

// respMessPartShape resp input message 的内容部件 → resp→mess 的成员形状。
// 读宽容（R4）：input_text 与 output_text 都当文本搬运，role 只决定落哪一行。
func respMessPartShape(r role, p gjson.Result) shape {
	typ := p.Get("type")
	switch {
	case rawStrEq(typ.Raw, "input_text"), rawStrEq(typ.Raw, "output_text"):
		if r == roleAssistant {
			if rawStrEq(typ.Raw, "input_text") {
				return shRMsgAsstInputText
			}
			return shRMsgAsstOutputText
		}
		if rawStrEq(typ.Raw, "input_text") {
			return shRMsgUserInputText
		}
		return shRMsgUserOutputText
	case rawStrEq(typ.Raw, "input_image"):
		if r == roleAssistant {
			return shRMsgAsstMedia
		}
		return respImgShape(p)
	case rawStrEq(typ.Raw, "input_file"):
		if r == roleAssistant {
			return shRMsgAsstMedia
		}
		return respFileShape(p)
	case rawStrEq(typ.Raw, "input_audio"):
		if r == roleAssistant {
			return shRMsgAsstMedia
		}
		return shRMsgUserInputAudio
	case rawStrEq(typ.Raw, "refusal"):
		if r == roleAssistant {
			return shRMsgAsstRefusal
		}
	}
	return shRTail
}

// respImgShape resp input_image 的 source 形态：file_id → ∅，http(s) → url，
// data URL → data_url，其余 → ∅（jsraw.go:325 的两个分支之外一律写不出）。
func respImgShape(p gjson.Result) shape {
	if id := p.Get("file_id"); id.Type == gjson.String && len(id.Raw) > 2 {
		return shRMsgUserImgFileID
	}
	u := p.Get("image_url")
	if u.Type != gjson.String {
		return shRMsgUserImgOther
	}
	s := gjson.Parse(u.Raw).Str
	switch {
	case len(s) >= 8 && s[:8] == "https://", len(s) >= 7 && s[:7] == "http://":
		return shRMsgUserImgURL
	case len(s) >= 11 && s[:11] == "data:image/":
		return shRMsgUserImgDataURL
	}
	return shRMsgUserImgOther
}

// respFileShape resp input_file 的 source 形态（J10：file_id 永不跨协议）。
func respFileShape(p gjson.Result) shape {
	if id := p.Get("file_id"); id.Type == gjson.String && len(id.Raw) > 2 {
		return shRMsgUserFileID
	}
	if u := p.Get("file_url"); u.Type == gjson.String && len(u.Raw) > 2 {
		return shRMsgUserFileURL
	}
	return shRMsgUserFileData
}

// respFcoShape function_call_output.output 的成员形状（resp.txt:1082 的字符串 /
// 数组两形态）。hasText 由调用方一次算出（blockTextRaw 只调用一次）。
func respFcoShape(output gjson.Result, hasText bool) shape {
	if !output.IsArray() {
		return shRFcoString
	}
	if hasText {
		return shRFcoArrayText
	}
	return shRFcoArrayNoText
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
		out = appendAssistantTurn(out, roleAssistant, asstOpen)
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
			r := roleOf(role.Raw)
			if r != roleUser && r != roleAssistant {
				// r_msg_sysdev_text 折叠进顶层 system（polOmit），其余角色无成员
				return true
			}
			start := len(out)
			out, n = appendComma(out, n)
			out = append(out, `{"content":[`...)
			blocks := 0
			content := item.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, p gjson.Result) bool {
					sh := respMessPartShape(r, p)
					// r_msg_asst_media 是 polDrop：assistant 的 image/file/audio
					// 部件不进 Messages（J2，§5.1#3 两个 mess 方向同口径）。
					if _, ok := emits(dirRespToMess, sh, r); !ok {
						return true
					}
					switch sh {
					case shRMsgUserInputText, shRMsgUserOutputText, shRMsgAsstInputText, shRMsgAsstOutputText:
						if t := p.Get("text"); t.Type == gjson.String {
							out, blocks = appendComma(out, blocks)
							out = append(out, `{"text":`...)
							out = append(out, t.Raw...)
							out = append(out, `,"type":"text"}`...)
						}
					case shRMsgAsstRefusal:
						// resp 的 refusal 在 Messages 目标降级为 text 块（J6/L12）
						if rf := p.Get("refusal"); rf.Type == gjson.String {
							out, blocks = appendComma(out, blocks)
							out = append(out, `{"text":`...)
							out = append(out, rf.Raw...)
							out = append(out, `,"type":"text"}`...)
						}
					case shRMsgUserImgURL, shRMsgUserImgDataURL:
						out, blocks = appendImageBlock(out, blocks, p)
					case shRMsgUserFileURL:
						// J5：input_file{file_url} → document{source:{type:url}}
						if u := p.Get("file_url"); u.Type == gjson.String && len(u.Raw) > 2 {
							out, blocks = appendComma(out, blocks)
							out = append(out, `{"source":{"type":"url","url":`...)
							out = append(out, u.Raw...)
							out = append(out, `},"type":"document"}`...)
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
			if r == roleAssistant {
				asstOpen = true
				return true
			}
			out = append(out, `],"role":`...)
			out = append(out, role.Raw...)
			out = append(out, '}')
		case rawStrEq(typ.Raw, "function_call"):
			// r_function_call 的 name 是 resp 侧项类型（itemVocabIn）；Mess 侧的
			// 承载块类型由本方向的发射器决定（tool_use）。
			if _, ok := mapsTo(dirRespToMess, shRFunctionCall, roleAssistant); !ok {
				return true
			}
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
			if _, ok := mapsTo(dirRespToMess, shRReasoning, roleAssistant); !ok || !replay {
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
			// 文本与形状一次算出（blockTextRaw 只调用一次；形状不重扫数组——
			// 重扫的闭包会逃逸成一次堆分配）。
			output := item.Get("output")
			t, hasText := blockTextRaw(output, "input_text", "text")
			if _, ok := mapsTo(dirRespToMess, respFcoShape(output, hasText), roleUser); !ok {
				return true
			}
			if !respDeclaresCall(input, strOrEmpty(item.Get("call_id"))) {
				// 孤儿 tool_result：不合成 call（J11 / §3.3 不变量 1）
				return true
			}
			if !hasText {
				t = `""`
			}
			out, n = appendComma(out, n)
			out = appendToolResult(out, tgtMess, strOrEmpty(item.Get("call_id")), t, true)
		}
		return true
	})
	if asstOpen {
		closeAsst()
	}
	return append(out, ']')
}
