// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 三条请求转换的字节路径：gjson 单遍取字段，值原样拼进一个输出缓冲。
// 语义与原 map+json.Marshal 版一致（测试钉住）。chat→resp 仍在 chat_resp.go。

import "github.com/tidwall/gjson"

func chatToMessRequest(body []byte) ([]byte, error) {
	root, err := parseRoot(body)
	if err != nil {
		return nil, err
	}
	var (
		msgs, maxCT, maxT, stop, tools, toolChoice gjson.Result
		model, temperature, topP, stream, metadata string
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
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	if maxCT.Exists() {
		out = appendField(out, &first, "max_tokens", maxCT.Raw)
	} else if maxT.Exists() {
		out = appendField(out, &first, "max_tokens", maxT.Raw)
	} else {
		out = appendField(out, &first, "max_tokens", "4096")
	}
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
	if rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if tc := toolChoiceToMessRaw(toolChoice, true); tc != "" {
		out = appendField(out, &first, "tool_choice", tc)
	}
	if tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendChatToolsMess(out, tools)
		out = append(out, ']')
	}
	if rawNotNull(topP) {
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

func appendChatToolsMess(out []byte, tools gjson.Result) []byte {
	n := 0
	tools.ForEach(func(_, tv gjson.Result) bool {
		fn := tv.Get("function")
		if !fn.IsObject() {
			return true
		}
		out, n = appendComma(out, n)
		out = append(out, '{')
		wrote := false
		if d := fn.Get("description"); d.Type == gjson.String {
			out = append(out, `"description":`...)
			out = append(out, d.Raw...)
			wrote = true
		}
		if p := fn.Get("parameters"); p.IsObject() {
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"input_schema":`...)
			out = append(out, p.Raw...)
			wrote = true
		}
		if name := fn.Get("name"); name.Type == gjson.String {
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"name":`...)
			out = append(out, name.Raw...)
		}
		out = append(out, '}')
		return true
	})
	return out
}

func toolChoiceToMessRaw(v gjson.Result, nested bool) string {
	if !v.Exists() || v.Type == gjson.Null {
		return ""
	}
	if v.Type == gjson.String {
		switch v.Str {
		case "auto":
			return `{"type":"auto"}`
		case "none":
			return `{"type":"none"}`
		case "required", "any":
			return `{"type":"any"}`
		}
		return ""
	}
	if !v.IsObject() || !rawStrEq(v.Get("type").Raw, "function") {
		return ""
	}
	var name gjson.Result
	if nested {
		name = v.Get("function.name")
	} else {
		name = v.Get("name")
	}
	if name.Type != gjson.String {
		return ""
	}
	return `{"name":` + name.Raw + `,"type":"tool"}`
}

func messToRespRequest(body []byte) ([]byte, error) {
	root, err := parseRoot(body)
	if err != nil {
		return nil, err
	}
	var (
		sys, msgs, maxT, tools, toolChoice gjson.Result
		model, temperature, topP           string
		stream, metadata                   string
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
			}
			return true
		})
	}
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	if msgs.IsArray() {
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
		out = appendMessToolsResp(out, tools)
		out = append(out, ']')
	}
	if rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	return append(out, '}'), nil
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

func appendMessToolsResp(out []byte, tools gjson.Result) []byte {
	n := 0
	tools.ForEach(func(_, tv gjson.Result) bool {
		if !tv.IsObject() {
			return true
		}
		out, n = appendComma(out, n)
		out = append(out, '{')
		wrote := false
		if d := tv.Get("description"); d.Type == gjson.String {
			out = append(out, `"description":`...)
			out = append(out, d.Raw...)
			wrote = true
		}
		if name := tv.Get("name"); name.Type == gjson.String {
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"name":`...)
			out = append(out, name.Raw...)
			wrote = true
		}
		if schema := tv.Get("input_schema"); schema.IsObject() {
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"parameters":`...)
			out = append(out, schema.Raw...)
			wrote = true
		}
		if wrote {
			out = append(out, ',')
		}
		out = append(out, `"type":"function"}`...)
		return true
	})
	return out
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
		input, maxOut, tools, toolChoice       gjson.Result
		instructions, model, temperature, topP string
		stream, metadata                       string
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
			}
			return true
		})
	}
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	if maxOut.Exists() {
		out = appendField(out, &first, "max_tokens", maxOut.Raw)
	} else {
		out = appendField(out, &first, "max_tokens", "4096")
	}
	switch {
	case input.IsArray():
		out = appendRespMessMessages(out, &first, input)
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
	if rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if tc := toolChoiceToMessRaw(toolChoice, false); tc != "" {
		out = appendField(out, &first, "tool_choice", tc)
	}
	if tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendRespToolsMess(out, tools)
		out = append(out, ']')
	}
	if rawNotNull(topP) {
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

func appendRespMessMessages(out []byte, first *bool, input gjson.Result) []byte {
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
		if asstOpen && !rawStrEq(typ.Raw, "function_call") {
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

func appendRespToolsMess(out []byte, tools gjson.Result) []byte {
	n := 0
	tools.ForEach(func(_, tv gjson.Result) bool {
		if !rawStrEq(tv.Get("type").Raw, "function") {
			return true
		}
		out, n = appendComma(out, n)
		out = append(out, '{')
		wrote := false
		if d := tv.Get("description"); d.Type == gjson.String {
			out = append(out, `"description":`...)
			out = append(out, d.Raw...)
			wrote = true
		}
		if p := tv.Get("parameters"); p.IsObject() {
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"input_schema":`...)
			out = append(out, p.Raw...)
			wrote = true
		}
		if name := tv.Get("name"); name.Type == gjson.String {
			if wrote {
				out = append(out, ',')
			}
			out = append(out, `"name":`...)
			out = append(out, name.Raw...)
		}
		out = append(out, '}')
		return true
	})
	return out
}
