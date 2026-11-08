// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"sort"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/tidwall/gjson"
)

// maxChatStop chat 的 stop 官方上限（StopConfiguration.maxItems = 4）。超出时
// 截断而不 400：客户端多给几条的意图是"任意一条命中就停"，少几条比整单失败好，
// 但要留丢弃计数（stop_truncated）。
const maxChatStop = 4

// messToChatRequest Messages 请求 → Chat Completions 请求（客户端说 mess、
// 上游只说 chat 时使用）。
//
// 字节级拼装（与其余请求方向同口径）：不建 map、不 Marshal，输出键按字母序
// 写出（max_completion_tokens / messages / model / parallel_tool_calls /
// reasoning_effort / stop / stream / stream_options / temperature /
// tool_choice / tools / top_p / user），golden 逐字节锁住。
//
// 关键取舍：
//   - 目标协议无载体的块（document 的非 text 源、search_result、redacted_thinking、
//     server_tool_use 等）一律丢弃并计数，不拼伪值（不发明内容）；
//   - tool_result 的图片不进 tool 消息正文（chat 的 tool 消息只带文本，塞 base64
//     会喂给文本 tokenizer 且撑爆上下文），改放到紧随其后的 user 消息；
//   - cache_control 是 Anthropic 的服务端缓存断点，chat 无对应字段，整键丢弃且
//     不计数（它不是"内容"，计进丢弃统计只会噪声）。
func messToChatRequest(body []byte) ([]byte, error) {
	root, err := parseRoot(body)
	if err != nil {
		return nil, err
	}
	var (
		sys, msgs, maxT, tools, toolChoice, thinking, outputConfig gjson.Result
		clientSO                                                   gjson.Result
		model, temperature, topP, stream, metadata                 string
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
			case gjsonKeyEq(k, "stream_options"):
				clientSO = v
			case gjsonKeyEq(k, "metadata"):
				metadata = v.Raw
			case gjsonKeyEq(k, "thinking"):
				thinking = v
			case gjsonKeyEq(k, "output_config"):
				outputConfig = v
			// 以下五项 chat 侧无承载位（既没有同名字段，也没有可重排的等价结构），
			// 以前是**静默丢弃**：线上只会表现为"那几台 MCP 服务/那个容器/那个
			// 采样参数好像没生效"，无从排查（spec §7 I3）。
			case gjsonKeyEq(k, "mcp_servers"):
				noteReqDrop(domain.ProtocolConvertMessToChat, "mcp_servers")
			case gjsonKeyEq(k, "container"):
				noteReqDrop(domain.ProtocolConvertMessToChat, "container")
			case gjsonKeyEq(k, "service_tier"):
				// Anthropic 的 service_tier（auto/standard_only）与 chat 的同名字段
				// 是两套枚举 ⇒ 丢弃并计数。响应面也会丢 chat 响应里的 service_tier，
				// 但计数键带面维度（DropEvent），两者不会混成一个桶。
				noteReqDrop(domain.ProtocolConvertMessToChat, "service_tier")
			case gjsonKeyEq(k, "betas"):
				// Anthropic 的 beta 特性开关（如 fine-grained-tool-streaming）：chat
				// 没有 beta 通道，只能丢；丢了必须可见。
				noteReqDrop(domain.ProtocolConvertMessToChat, "betas")
			case gjsonKeyEq(k, "top_k"):
				// chat 的采样参数只有 temperature/top_p（没有 top_k）⇒ 真的被丢弃，
				// 计数。若将来给它找到承载位，这个 case 就该删掉（那时它不再被丢）。
				noteReqDrop(domain.ProtocolConvertMessToChat, "top_k")
			}
			return true
		})
	}
	effort := effortOf(thinking, outputConfig)
	out := make([]byte, 0, len(body)+64)
	out = append(out, '{')
	first := true
	if maxT.Exists() && maxT.Type != gjson.Null {
		out = appendField(out, &first, "max_completion_tokens", maxT.Raw)
	}
	// messages 是 chat 的必填字段：**恒写**（转换后为空也写空数组，不留缺键）。
	if !first {
		out = append(out, ',')
	}
	first = false
	out = append(out, `"messages":[`...)
	var nMsgs int
	out, nMsgs = appendMessChatMessages(out, sys, msgs)
	out = append(out, ']')
	if nMsgs == 0 {
		// system 折叠为空 + 全是被丢块：messages 仍是必填键（写了空数组），但
		// "这一单其实没有任何可发给模型的内容"必须可见（spec §4.1）。
		noteReqDrop(domain.ProtocolConvertMessToChat, "messages_empty")
	}
	if rawNotNull(model) {
		out = appendField(out, &first, "model", model)
	}
	if fieldRaw(toolChoice, "disable_parallel_tool_use").Type == gjson.True {
		out = appendField(out, &first, "parallel_tool_calls", "false")
	}
	if effort != "" {
		out = appendField(out, &first, "reasoning_effort", string(appendJSONString(nil, effort)))
	}
	if raw := messChatStop(root.Get("stop_sequences")); raw != "" {
		out = appendField(out, &first, "stop", raw)
	}
	if rawNotNull(stream) {
		out = appendField(out, &first, "stream", stream)
	}
	if stream == "true" {
		out = appendStreamOptionsMerged(out, &first, domain.ProtocolConvertMessToChat, clientSO)
	} else if clientSO.Exists() && clientSO.Type != gjson.Null {
		// 非流式请求里的 stream_options 没有承载位（chat 只在 stream=true 时读它）。
		noteReqDrop(domain.ProtocolConvertMessToChat, "stream_options")
	}
	// 思考与自定义采样不兼容（与其余方向同口径）：开启时丢弃 temperature/top_p。
	if effort == "" && rawNotNull(temperature) {
		out = appendField(out, &first, "temperature", temperature)
	}
	if raw := messChatToolChoice(toolChoice); raw != "" {
		out = appendField(out, &first, "tool_choice", raw)
	}
	if tools.IsArray() {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, `"tools":[`...)
		out = appendToolsMapped(out, tools, toolsChatFromMess)
		out = append(out, ']')
	}
	if effort == "" && rawNotNull(topP) {
		out = appendField(out, &first, "top_p", topP)
	}
	if uid := metadataUserID(metadata); uid != "" {
		out = appendField(out, &first, "user", uid)
	}
	return append(out, '}'), nil
}

// appendMessChatMessages Messages 的 messages[] → chat 的 messages[]。
//
// 顺序语义（chat 硬约束）：assistant.tool_calls 之后必须紧跟其 role:"tool"
// 消息。Mess 把工具结果放在**下一条 user 消息**的 content 里，所以遇到
// tool_result 块要**就地**把它写成独立的 tool 消息（先于该 user 消息自身的
// 文本/图片），否则会出现"tool 消息离它的 tool_calls 太远" → 上游 400。
func appendMessChatMessages(out []byte, sys, msgs gjson.Result) ([]byte, int) {
	n := 0
	// 顶层 system 折叠成首条 system 消息（foldSystemTexts 返回 JSON 字符串字面量；
	// 折叠后为空串则不写空 system）。非文本块（image/document …）chat 无载体 ⇒
	// 丢弃 + 计 `system_block`（resp 方向同样丢弃但不计数，见 anthropicSystemParts
	// 的注释：共享函数不改既有方向的行为）。
	texts := anthropicSystemParts(sys)
	noteSystemBlockDrops(sys)
	if s := foldSystemTexts(tgtChat, texts); s != "" {
		out, n = appendComma(out, n)
		out = append(out, `{"content":`...)
		out = append(out, s...)
		out = append(out, `,"role":"system"}`...)
	}
	if !msgs.IsArray() {
		return out, n
	}
	// prevAsst 是**紧邻的上一条 assistant 消息的 content 数组**，即 §4.3 配对
	// 不变量的作用域：下一条 user 的 tool_result 只允许回指它声明的 tool_use，
	// 且必须按声明顺序输出（chat 要求 tool 消息紧邻其 tool_calls 且保序）。
	// 换成"别的角色/别的形态"就闭合窗口（invalid gjson 表示"没有声明"）。
	var prevAsst gjson.Result
	msgs.ForEach(func(_, m gjson.Result) bool {
		if !m.IsObject() {
			noteReqDrop(domain.ProtocolConvertMessToChat, "message")
			prevAsst = gjson.Result{}
			return true
		}
		role := m.Get("role").Str
		if role != "user" && role != "assistant" {
			noteReqDrop(domain.ProtocolConvertMessToChat, "role")
			prevAsst = gjson.Result{}
			return true
		}
		content := m.Get("content")
		if content.Type == gjson.String {
			out, n = appendComma(out, n)
			out = append(out, `{"content":`...)
			out = append(out, content.Raw...)
			out = append(out, `,"role":`...)
			out = append(out, string(appendJSONString(nil, role))...)
			out = append(out, '}')
			prevAsst = gjson.Result{}
			return true
		}
		if !content.IsArray() {
			// 缺失/null/非法形态 → 该轮写空内容（不省略消息，保持轮次）。
			out, n = appendComma(out, n)
			out = append(out, `{"content":"","role":`...)
			out = append(out, string(appendJSONString(nil, role))...)
			out = append(out, '}')
			prevAsst = gjson.Result{}
			return true
		}
		if role == "user" {
			out, n = appendMessChatUserTurn(out, n, content, prevAsst)
			prevAsst = gjson.Result{}
			return true
		}
		out, n = appendMessChatAsstTurn(out, n, content)
		prevAsst = content
		return true
	})
	return out, n
}

// messToolUseEmitted 该 tool_use 会不会被写进 chat 的 tool_calls[]。判据与
// appendMessChatAsstTurn 一致：必须有 string name 与**非空** string id。
func messToolUseEmitted(blk gjson.Result) bool {
	return blk.Get("type").Str == "tool_use" &&
		blk.Get("name").Type == gjson.String &&
		blk.Get("id").Type == gjson.String && blk.Get("id").Str != ""
}

// messDeclaresToolUse 上一条 assistant 是否声明过该 tool_use_id。
func messDeclaresToolUse(prevAsst, id gjson.Result) bool {
	if id.Type != gjson.String || id.Str == "" {
		return false
	}
	found := false
	prevAsst.ForEach(func(_, blk gjson.Result) bool {
		if !messToolUseEmitted(blk) || blk.Get("id").Str != id.Str {
			return true
		}
		found = true
		return false
	})
	return found
}

// noteSystemBlockDrops 顶层 system 数组里**不是文本**的块逐块计数（chat 没有
// 承载位）。判据与 anthropicSystemParts 一致：字符串、或带 string `text` 的块
// 才算文本。
func noteSystemBlockDrops(sys gjson.Result) {
	if !sys.IsArray() {
		return
	}
	sys.ForEach(func(_, p gjson.Result) bool {
		if p.Type == gjson.String {
			return true
		}
		if t := p.Get("text"); t.Type == gjson.String {
			return true
		}
		noteReqDrop(domain.ProtocolConvertMessToChat, "system_block")
		return true
	})
}

// appendMessChatUserTurn user 轮：文本/图片 → content parts；tool_result →
// role:"tool" 消息（**按 prevAsst 的声明顺序**输出，不是源数组顺序）；
// tool_result 内的图片挪到紧随其后的 user 消息。
//
// prevAsst 是上一条 assistant 的 content 数组：不在其中声明的 tool_result 是
// 孤儿（上游必 400），同一 id 的第二个结果无从表达 ⇒ 都丢弃并计
// `tool_result_orphan`，不合成对应 call（assemble.go 的 J11 决策）。
//
// 为什么不先摘一份 id 清单：这是请求热路径，多一块切片就多一次堆分配
// （bench 门禁按 allocs/op 卡死），直接两遍扫 prevAsst 更省也更短。
func appendMessChatUserTurn(out []byte, n int, content, prevAsst gjson.Result) ([]byte, int) {
	var parts, toolMsgs, pendingImgs []byte
	nParts, nTools, nImgs := 0, 0, 0
	// ① 归属校验（只为计数，源顺序走一遍）。
	content.ForEach(func(_, blk gjson.Result) bool {
		if blk.Get("type").Str != "tool_result" {
			return true
		}
		if !messDeclaresToolUse(prevAsst, blk.Get("tool_use_id")) {
			noteReqDrop(domain.ProtocolConvertMessToChat, "tool_result_orphan")
		}
		return true
	})
	// ② 文本/图片/文档就地累积；tool_result 留给 ③。
	content.ForEach(func(_, blk gjson.Result) bool {
		switch blk.Get("type").Str {
		case "text":
			if t := blk.Get("text"); t.Type == gjson.String {
				parts, nParts = appendComma(parts, nParts)
				parts = append(parts, `{"text":`...)
				parts = append(parts, t.Raw...)
				parts = append(parts, `,"type":"text"}`...)
			}
		case "image":
			if url, ok := messChatImageURL(blk); ok {
				parts, nParts = appendComma(parts, nParts)
				parts = append(parts, `{"image_url":{"url":`...)
				parts = append(parts, string(appendJSONString(nil, url))...)
				parts = append(parts, `},"type":"image_url"}`...)
			}
		case "tool_result":
			// 见 ③：结果必须排在它声明的 call 之后，所以不在这里输出。
		case "document":
			// 纯文本文档降级为文本部件（它本来就是文本，不构成污染）；其余源
			// （base64 PDF / url PDF / content blocks / file）chat 无载体 → 丢弃。
			if blk.Get("source.type").Str == "text" {
				if d := blk.Get("source.data"); d.Type == gjson.String {
					parts, nParts = appendComma(parts, nParts)
					parts = append(parts, `{"text":`...)
					parts = append(parts, d.Raw...)
					parts = append(parts, `,"type":"text"}`...)
					return true
				}
			}
			noteReqDrop(domain.ProtocolConvertMessToChat, "document")
		case "thinking":
			// user 轮里的 thinking 无意义（Anthropic 只允许 assistant 携带）。
			noteReqDrop(domain.ProtocolConvertMessToChat, "thinking")
		default:
			noteReqDrop(domain.ProtocolConvertMessToChat, "block_unknown")
		}
		return true
	})
	// ③ 按声明顺序输出结果：源里 b,a 的两条 → 产物 a,b（chat 要求 tool 消息
	// 紧邻其 tool_calls 且保序）。每个 id 只认源里第一条，其余是重复结果。
	prevAsst.ForEach(func(_, decl gjson.Result) bool {
		if !messToolUseEmitted(decl) {
			return true
		}
		// 用声明里的**源字节**（含转义）而不是重新编码 id：tool_call_id 与
		// tool_calls[].id 必须是同一个字符串，且这里不该多两次分配。
		idJSON := decl.Get("id")
		id := idJSON.Str
		first := true
		content.ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").Str != "tool_result" || blk.Get("tool_use_id").Str != id {
				return true
			}
			if !first {
				noteReqDrop(domain.ProtocolConvertMessToChat, "tool_result_orphan")
				return true
			}
			first = false
			toolMsgs, nTools = appendComma(toolMsgs, nTools)
			toolMsgs = append(toolMsgs, `{"content":`...)
			toolMsgs = append(toolMsgs, messChatToolResultContent(blk, &pendingImgs, &nImgs)...)
			toolMsgs = append(toolMsgs, `,"role":"tool","tool_call_id":`...)
			toolMsgs = append(toolMsgs, idJSON.Raw...)
			toolMsgs = append(toolMsgs, '}')
			return true
		})
		return true
	})
	out = appendFrag(out, toolMsgs)
	n += nTools
	// 本轮的 user 消息：文本/图片部件在前，从 tool_result 里挪出来的图片统一附在
	// **同一条**消息的末尾（单一规则，可预测；拆成两条会在对话里凭空多出一轮）。
	switch {
	case nParts > 0:
		out, n = appendComma(out, n)
		out = append(out, `{"content":[`...)
		out = append(out, parts...)
		if nImgs > 0 {
			out = append(out, ',')
			out = append(out, pendingImgs...)
		}
		out = append(out, `],"role":"user"}`...)
	case nImgs > 0:
		out, n = appendComma(out, n)
		out = append(out, `{"content":[`...)
		out = append(out, pendingImgs...)
		out = append(out, `],"role":"user"}`...)
	case nTools == 0:
		// 空 content / 全是被丢块：仍写一条空 user 消息（保持轮次）。
		out, n = appendComma(out, n)
		out = append(out, `{"content":"","role":"user"}`...)
	}
	return out, n
}

// appendMessChatAsstTurn assistant 轮：文本折叠成 content 字符串、thinking →
// reasoning_content、tool_use → tool_calls[]（同一条消息内）。
func appendMessChatAsstTurn(out []byte, n int, content gjson.Result) ([]byte, int) {
	var text, think strings.Builder
	var calls []byte
	nCalls := 0
	content.ForEach(func(_, blk gjson.Result) bool {
		switch blk.Get("type").Str {
		case "text":
			if t := blk.Get("text"); t.Type == gjson.String {
				text.WriteString(t.Str)
			}
		case "thinking":
			// chat 无签名通道 → 只搬纯文本，signature 丢弃（不计数：它是
			// Anthropic 的完整性凭据，chat 侧本就没有承载位）。
			if t := blk.Get("thinking"); t.Type == gjson.String {
				if think.Len() > 0 {
					think.WriteString("\n")
				}
				think.WriteString(t.Str)
			}
		case "redacted_thinking":
			noteReqDrop(domain.ProtocolConvertMessToChat, "redacted_thinking")
		case "tool_use":
			name := blk.Get("name")
			id := blk.Get("id")
			// id 空串的结果永远配不上（tool_use_id 也是空串），一并丢掉：
			// 让"声明了什么"与 messDeclaresToolUse 的判据完全一致。
			if name.Type != gjson.String || id.Type != gjson.String || id.Str == "" {
				noteReqDrop(domain.ProtocolConvertMessToChat, "tool_use")
				return true
			}
			// arguments 用**源字节**而不是重排后的紧凑 JSON：chat 把 arguments
			// 当不透明字符串转给上游，重排只会多一条"中途解析失败"的路径，且会
			// 悄悄改掉重复键的语义（上游 json.loads 是末键胜，重排会先合并）。
			// 唯一要求是合法 JSON：input 缺失/null → "{}"；非 JSON（字符串等）
			// 按原字面量传递。
			// chat 的 function.arguments 是**字符串**（里面装着 JSON 文本）。缺省
			// 因此必须是字符串字面量 `"{}"`，写成对象 `{}` 会让上游收到
			// arguments:{} 而不是 arguments:"{}" —— 类型错，直接 400。
			args := []byte(`"{}"`)
			if in := blk.Get("input"); in.Exists() && in.Type != gjson.Null {
				args = appendJSONString(nil, in.Raw)
			}
			calls, nCalls = appendComma(calls, nCalls)
			calls = append(calls, `{"function":{"arguments":`...)
			calls = append(calls, args...)
			calls = append(calls, `,"name":`...)
			calls = append(calls, name.Raw...)
			calls = append(calls, `},"id":`...)
			calls = append(calls, id.Raw...)
			calls = append(calls, `,"type":"function"}`...)
		case "image":
			noteReqDrop(domain.ProtocolConvertMessToChat, "asst_image")
		case "document":
			noteReqDrop(domain.ProtocolConvertMessToChat, "document")
		default:
			noteReqDrop(domain.ProtocolConvertMessToChat, "block_unknown")
		}
		return true
	})
	out, n = appendComma(out, n)
	out = append(out, `{"content":`...)
	out = append(out, string(appendJSONString(nil, text.String()))...)
	if think.Len() > 0 {
		out = append(out, `,"reasoning_content":`...)
		out = append(out, string(appendJSONString(nil, think.String()))...)
	}
	out = append(out, `,"role":"assistant"`...)
	if nCalls > 0 {
		out = append(out, `,"tool_calls":[`...)
		out = append(out, calls...)
		out = append(out, ']')
	}
	out = append(out, '}')
	return out, n
}

// messChatToolResultContent tool_result 的 content → chat tool 消息的 content
// 字符串：文本块按 "\n" 拼接；图片收进 pendingImgs（由调用方写到紧随的 user
// 消息）；is_error 加 "Error: " 前缀。整块无文本但有图片 → "[image]" 占位
// （chat 上游拒绝空 tool content）。
func messChatToolResultContent(blk gjson.Result, pending *[]byte, nPending *int) []byte {
	// 只关心**本块**是否贡献了图片：pending 是整轮的累积槽，拿 nPending>0 判会
	// 让"前一个 tool_result 带图、本块纯空文本"也写出 [image] 占位。
	imgsBefore := *nPending
	c := blk.Get("content")
	isErr := blk.Get("is_error").Type == gjson.True
	var sb strings.Builder
	if c.Type == gjson.String {
		sb.WriteString(c.Str)
	} else if c.IsArray() {
		c.ForEach(func(_, p gjson.Result) bool {
			switch p.Get("type").Str {
			case "text":
				if t := p.Get("text"); t.Type == gjson.String {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t.Str)
				}
			case "image":
				if url, ok := p.Get("source.url").Str, p.Get("source.url").Type == gjson.String; ok && url != "" {
					*pending, *nPending = appendComma(*pending, *nPending)
					*pending = append(*pending, `{"image_url":{"url":`...)
					*pending = append(*pending, string(appendJSONString(nil, url))...)
					*pending = append(*pending, `},"type":"image_url"}`...)
				} else if d := p.Get("source.data"); d.Type == gjson.String && d.Str != "" {
					mt := p.Get("source.media_type").Str
					if mt == "" {
						mt = "image/png"
					}
					*pending, *nPending = appendComma(*pending, *nPending)
					*pending = append(*pending, `{"image_url":{"url":`...)
					*pending = append(*pending, string(appendJSONString(nil, "data:"+mt+";base64,"+d.Str))...)
					*pending = append(*pending, `},"type":"image_url"}`...)
				} else {
					noteReqDrop(domain.ProtocolConvertMessToChat, "image_file")
				}
			default:
				noteReqDrop(domain.ProtocolConvertMessToChat, "tool_result_block")
			}
			return true
		})
	} else if c.Exists() && c.Type != gjson.Null {
		noteReqDrop(domain.ProtocolConvertMessToChat, "tool_result_block")
	}
	text := sb.String()
	if text == "" && *nPending > imgsBefore {
		text = "[image]"
	}
	if isErr {
		text = "Error: " + text
	}
	return appendJSONString(nil, text)
}

// messChatImageURL Mess 的 image.source → chat 的 image_url.url。三种源：
// base64 → data URL；url → 原样；file（file_id）→ chat 无载体（丢弃 + 计数）。
func messChatImageURL(blk gjson.Result) (string, bool) {
	src := blk.Get("source")
	switch src.Get("type").Str {
	case "base64":
		d := src.Get("data")
		if d.Type != gjson.String || d.Str == "" {
			noteReqDrop(domain.ProtocolConvertMessToChat, "image_empty")
			return "", false
		}
		mt := src.Get("media_type").Str
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + d.Str, true
	case "url":
		if u := src.Get("url"); u.Type == gjson.String && u.Str != "" {
			return u.Str, true
		}
		noteReqDrop(domain.ProtocolConvertMessToChat, "image_empty")
		return "", false
	case "file":
		noteReqDrop(domain.ProtocolConvertMessToChat, "image_file")
		return "", false
	default:
		noteReqDrop(domain.ProtocolConvertMessToChat, "image_source")
		return "", false
	}
}

// messChatStop Mess 的 stop_sequences → chat 的 stop（单元素写字符串，多元素
// 写数组，最多 4 个——见 maxChatStop）。
func messChatStop(v gjson.Result) string {
	if !v.IsArray() {
		return ""
	}
	var items []string
	v.ForEach(func(_, e gjson.Result) bool {
		if e.Type == gjson.String {
			items = append(items, e.Raw)
		}
		return true
	})
	if len(items) == 0 {
		return ""
	}
	if len(items) > maxChatStop {
		noteReqDrop(domain.ProtocolConvertMessToChat, "stop_truncated")
		items = items[:maxChatStop]
	}
	if len(items) == 1 {
		return items[0]
	}
	return "[" + strings.Join(items, ",") + "]"
}

// messChatToolChoice Mess 的 tool_choice → chat 形态：
//
//	{type:auto} → "auto"；{type:any} → "required"；{type:none} → "none"；
//	{type:tool,name} → {"function":{"name":…},"type":"function"}。
//
// 字符串形态（非 Anthropic 规范但客户端有时会给）按同名映射，未知形态丢弃计数。
func messChatToolChoice(v gjson.Result) string {
	if !v.Exists() || v.Type == gjson.Null {
		return ""
	}
	if v.Type == gjson.String {
		switch v.Str {
		case "auto":
			return `"auto"`
		case "none":
			return `"none"`
		case "required", "any":
			return `"required"`
		}
		noteReqDrop(domain.ProtocolConvertMessToChat, "tool_choice")
		return ""
	}
	switch v.Get("type").Str {
	case "auto":
		return `"auto"`
	case "any":
		return `"required"`
	case "none":
		return `"none"`
	case "tool":
		name := v.Get("name")
		if name.Type != gjson.String || name.Str == "" {
			noteReqDrop(domain.ProtocolConvertMessToChat, "tool_choice")
			return ""
		}
		return `{"function":{"name":` + name.Raw + `},"type":"function"}`
	}
	noteReqDrop(domain.ProtocolConvertMessToChat, "tool_choice")
	return ""
}

// metadataUserID Mess metadata.user_id → chat 的 user；其余 metadata 键无载体
// （整段计一次，不按键展开：避免高基数 metrics）。
func metadataUserID(raw string) string {
	if !rawNotNull(raw) {
		return ""
	}
	obj := gjson.Parse(raw)
	if !obj.IsObject() {
		noteReqDrop(domain.ProtocolConvertMessToChat, "metadata")
		return ""
	}
	keys := 0
	obj.ForEach(func(gjson.Result, gjson.Result) bool {
		keys++
		return true
	})
	uid := obj.Get("user_id")
	if uid.Type == gjson.String && uid.Str != "" {
		if keys > 1 {
			noteReqDrop(domain.ProtocolConvertMessToChat, "metadata")
		}
		return uid.Raw
	}
	noteReqDrop(domain.ProtocolConvertMessToChat, "metadata")
	return ""
}

// streamOptKV 客户端 stream_options 的一个键值对（raw 保留源字节，不重编码）。
type streamOptKV struct {
	key string
	raw string
}

// appendStreamOptionsMerged chat 的 stream_options：**对象合并**而不是整段覆盖——
// 客户端已有的键（include_obfuscation 等）原样保留，只把 include_usage 强制置
// true（否则上游流里根本没有 usage 帧 → 客户端看到 0 token、服务端也计 0）。
//
// 键序：客户端的键按**字母序**写出，`include_usage` 恒在最后（它是本方向的强制
// 项，与客户端是否给了无关）。同一键在源里出现多次时**末键胜**——与上游
// json.loads 的语义一致，不在这里改掉重复键的含义。
//
// 非对象形态（字符串/数组/数字）：chat 无法承载 ⇒ 整段丢弃并计 `stream_options`，
// 但仍写强制项（不因为客户端给错形态就连 usage 观测一起丢）。
//
// 分配：只有客户端真给了 stream_options 对象才走收集+排序分支（一次切片），
// 没有该键的请求（含基准载荷）只写一个常量串，零分配。
func appendStreamOptionsMerged(out []byte, first *bool, dir domain.ProtocolConvert, client gjson.Result) []byte {
	if !*first {
		out = append(out, ',')
	}
	*first = false
	out = append(out, `"stream_options":{`...)
	if client.Type != gjson.Null && client.Exists() && !client.IsObject() {
		noteReqDrop(dir, "stream_options")
		return append(out, `"include_usage":true}`...)
	}
	if client.IsObject() {
		kvs := make([]streamOptKV, 0, 4)
		client.ForEach(func(k, v gjson.Result) bool {
			if k.Type != gjson.String || k.Str == "include_usage" {
				// include_usage 的取值由本方向决定（强制 true），客户端的值不参与。
				return true
			}
			for i := range kvs {
				if kvs[i].key == k.Str {
					kvs[i].raw = v.Raw
					return true
				}
			}
			kvs = append(kvs, streamOptKV{key: k.Str, raw: v.Raw})
			return true
		})
		sort.Slice(kvs, func(i, j int) bool { return kvs[i].key < kvs[j].key })
		for _, kv := range kvs {
			out = append(out, string(appendJSONString(nil, kv.key))...)
			out = append(out, ':')
			out = append(out, kv.raw...)
			out = append(out, ',')
		}
	}
	return append(out, `"include_usage":true}`...)
}

// appendFrag 把已拼好的消息片段接到 out 上（片段内部已含前导逗号控制）。
func appendFrag(out, frag []byte) []byte {
	if len(frag) == 0 {
		return out
	}
	if len(out) > 0 && out[len(out)-1] != '[' {
		out = append(out, ',')
	}
	return append(out, frag...)
}
