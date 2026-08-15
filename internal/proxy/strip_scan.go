// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"iter"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 手写 JSON 扫描器（strip_image_tools 优化，2026-08-12-strip-optimize-spec v5，
// 评审 PASS）：单遍字节扫描 + 零序列化 splice 改写。不做 cgo / gjson 依赖
// （热路径红线，spec 裁决）。
//
// 本文件组件：
//   - scanTopLevelKeys：顶层键区间定位（splice 地基——tools / tool_choice
//     键名区间 + 值区间 + 相邻逗号）
//   - scanTools：tools 数组元素迭代器（range-over-func iter.Seq，Go 1.23+；
//     工具级层级预筛 + 按需键提取 + \uXXXX 解码）
//   - 字节原语：skipValue / skipJSONString / parseJSONString /
//     decodeUnicodeEscapes 等（全部零分配——合法 JSON 前置：
//     handleFormat 已 json.Valid，异常分支仅防御性兜底）
//
// 热路径纪律：零锁零全局可变状态（仅不可变字节常量，编译期静态引用零分配）；
// 精确预分配；预筛短路保持。
// ---------------------------------------------------------------------------

// ToolView 工具视图：Raw 为 body 子切片（真零拷贝——引用 tools 值区间字节）；
// Type/Name/Namespace 为顶层键值（去引号、\uXXXX 已解码；未提取 = 空）。
// Result/ID 为 resp 响应检测旁路（spec §6）复用提取：ID = id 字符串值；
// Result = result 值裸字节（字符串值去引号/\u 解码；非字符串值——数组/null/
// 数字——取原始值区间，空语义在判定处 imageResultNonEmpty 处理）。
type ToolView struct {
	Raw       []byte
	Type      []byte
	Name      []byte
	Namespace []byte
	Result    []byte
	ID        []byte
}

// topLevelRange 顶层键-值区间定位结果（splice 改写的地基）：
//   - keyStart：键名开引号位置
//   - valStart：值首非空白字节（数组形态下即 '['）
//   - valEnd：值末字节后一位置（不含尾随空白）
//   - commaBefore / commaAfter：前导/后随逗号位置（-1 = 无）
type topLevelRange struct {
	keyStart, valStart, valEnd int
	commaBefore, commaAfter    int
}

// delRange 删键区间（含逗号清理——三位置规则：唯一键无逗号直接删键；
// 首键删后随逗号；末键/中间键删前导逗号）。删键含前导逗号避免产生悬挂逗号；
// 首键无前导逗号则带上后随逗号。
func (r topLevelRange) delRange() (start, end int) {
	if r.commaBefore >= 0 {
		return r.commaBefore, r.valEnd
	}
	end = r.valEnd
	if r.commaAfter >= 0 {
		end = r.commaAfter + 1 // 含逗号
	}
	return r.keyStart, end
}

// scanTopLevelKeys 从 body 扫顶层键，定位 "tools" 与 "tool_choice" 键的
// 键名区间 + 值区间 + 相邻逗号。键名精确匹配（字符串后跳过空白再冒号——
// `"tools" : [` 变体命中）；字符串内容含 "tools" 字样不误定位；嵌套值正确
// 跳过；同名重复键取最后一次出现（对齐 json.Unmarshal map 语义——该对齐
// 仅对值选择成立：剥离仅删末次出现区间，首次残留数组不含图像工具——
// 方向 benign）。顶层非对象（数组/标量）或结构非法 → 双键缺省（原样转发
// 语义，行为定义）。
func scanTopLevelKeys(body []byte) (tools, toolChoice topLevelRange, toolsOK, toolChoiceOK bool) {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return // 顶层非对象：无键（原样转发）
	}
	i++         // '{'
	comma := -1 // 上一个值后的逗号（当前键的前导逗号）
	for {
		i = skipSpace(body, i)
		if i >= len(body) {
			return
		}
		switch body[i] {
		case '}':
			return // 对象结束
		case ',':
			comma = i
			i++
		case '"':
			keyStart := i
			key, next := parseJSONString(body, i)
			if next < 0 {
				return
			}
			j := skipSpace(body, next)
			if j >= len(body) || body[j] != ':' {
				return
			}
			j = skipSpace(body, j+1)
			if j >= len(body) {
				return
			}
			valEnd := skipValue(body, j)
			if valEnd < 0 {
				return
			}
			r := topLevelRange{keyStart: keyStart, valStart: j, valEnd: valEnd, commaBefore: comma, commaAfter: -1}
			if t := skipSpace(body, valEnd); t < len(body) && body[t] == ',' {
				r.commaAfter = t
			}
			switch {
			case bytes.Equal(key, toolsKeyBytes):
				tools, toolsOK = r, true
			case bytes.Equal(key, toolChoiceKeyBytes):
				toolChoice, toolChoiceOK = r, true
			}
			comma = -1
			i = valEnd
		default:
			return // 结构非法（json.Valid 前置，理论上不可达）
		}
	}
}

// scanTools 迭代 tools 数组元素（range-over-func 标准迭代器形态，Go 1.23+）。
// 前置：tools 值区间字节且 bytes.HasPrefix(toolsRaw, '[') 数组守卫预检通过
// （object/null/标量挡回原样转发等价）。
//
// 每元素工具级预筛（bytes.Contains SIMD，最多 2 次）：
//   - 命中 "image" → 提取顶层键 + 判定（正常路径）
//   - 不命中 "image" 但命中 `\u` → 提取键 + 键值 \uXXXX 解码后判定
//     （转义形态逃逸检查——"image_generation_tool" 字节无 "image"
//     子串但语义是图像工具；解码保持与现状 gjson String() 等价）
//   - 双不命中 → yield 仅 Raw（零键提取——绝大多数 function 工具走这条；
//     判定目标为纯字面，"image 与 \u 双不命中"工具必非图像工具）
//
// break 提前终止自动停止（iter.Seq 标准语义，无泄漏）；Raw 引用 raw 子切片
// （迭代器生命周期内 raw 不变——stripImageTools 内无并发）。
//
// 实现注：外层为纯适配闭包（体仅一次调用，编译器可内联到调用方——热路径
// 闭包零分配），完整状态机在 scanToolsIter。
func scanTools(raw []byte) iter.Seq[ToolView] {
	return func(yield func(ToolView) bool) {
		scanToolsIter(raw, yield)
	}
}

// scanToolsIter tools 数组元素扫描状态机：跳过字符串（\ 分支即可正确找字符串
// 结尾——\" 跳 "、\\ 跳 \、\uXXXX 的 4 hex 无引号/反斜杠）、嵌套 {}[]、
// 顶层逗号元素边界；元素 Raw 去除前导/尾随空白（拼接紧凑合法、精确预分配
// 可算）。
func scanToolsIter(raw []byte, yield func(ToolView) bool) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return // 数组守卫（调用方已预检；防御性兜底）
	}
	i = skipSpace(raw, i+1)
	for {
		i = skipSpace(raw, i)
		if i >= len(raw) {
			return
		}
		if raw[i] == ']' {
			return // 空数组 / 数组结束
		}
		elemStart := i
		j := skipElement(raw, i) // 元素末字节后一位置（跳过空白后，指向 ',' 或 ']'）
		if j < 0 {
			return
		}
		s, e := trimWS(raw, elemStart, j)
		tv := ToolView{Raw: raw[s:e]}
		if bytes.Contains(tv.Raw, imageBytes) || bytes.Contains(tv.Raw, backslashUBytes) {
			extractKeys(tv.Raw, &tv)
		}
		if !yield(tv) {
			return // break 提前终止
		}
		i = j
		if i >= len(raw) || raw[i] == ']' {
			return
		}
		i++ // 跳过顶层逗号（skipElement 保证此处为 ','）
	}
}

// extractKeys 提取元素顶层键（单遍扫描 "type"/"name"/"namespace" + resp 检测
// 旁路 "result"/"id"）：键名精确匹配后跳过空白冒号（键名干扰 "type2"/"type "
// 不误命中）；值取裸字节去引号；含 \u 的值解码 \uXXXX（最小实现，对齐现状
// gjson String() 对判定目标的比较语义——目标为纯 ASCII 小写字面，其他转义
// 形态出现必 ≠ 目标）。非对象元素跳过；非字符串值仅 result 捕获原始值区间
// （数组/null 形态——检测侧空语义判定），其余跳过。
func extractKeys(elem []byte, tv *ToolView) {
	i := skipSpace(elem, 0)
	if i >= len(elem) || elem[i] != '{' {
		return // 非对象元素：无键可提取
	}
	i = skipSpace(elem, i+1)
	for {
		i = skipSpace(elem, i)
		if i >= len(elem) || elem[i] != '"' {
			return // 对象结束（'}'）或结构异常：提取完毕
		}
		key, next := parseJSONString(elem, i)
		if next < 0 {
			return
		}
		j := skipSpace(elem, next)
		if j >= len(elem) || elem[j] != ':' {
			return
		}
		j = skipSpace(elem, j+1)
		if j >= len(elem) {
			return
		}
		if elem[j] != '"' {
			// 非字符串值（判定字段恒为字符串；result 需捕获原始值区间——
			// 数组/null 形态的计数判定）
			end := skipValue(elem, j)
			if end < 0 {
				return
			}
			if bytes.Equal(key, resultKeyBytes) {
				tv.Result = elem[j:end]
			}
			i = end
		} else {
			val, end := parseJSONString(elem, j)
			if end < 0 {
				return
			}
			if bytes.Contains(val, backslashUBytes) {
				val = decodeUnicodeEscapes(val)
			}
			switch {
			case bytes.Equal(key, typeKeyBytes):
				tv.Type = val
			case bytes.Equal(key, nameKeyBytes):
				tv.Name = val
			case bytes.Equal(key, namespaceKeyBytes):
				tv.Namespace = val
			case bytes.Equal(key, idKeyBytes):
				tv.ID = val
			case bytes.Equal(key, resultKeyBytes):
				tv.Result = val
			}
			i = end
		}
		// 跳过分隔逗号
		if k := skipSpace(elem, i); k < len(elem) && elem[k] == ',' {
			i = k + 1
		}
	}
}

// scanKeyValue 定位对象顶层键的键值区间 [start, end)（值首字节 → 值末字节
// 后一位置；嵌套值正确跳过——对象/数组内同名键不误定位）。首次命中即返回
// （重复键病态，值语义取首/末对计数无实质影响）。顶层非对象/结构非法 →
// ok=false（调用方按无该键处理；json.Valid 前置下不可达，防御性兜底）。
// resp 响应检测旁路（spec §6）定位 output 数组用（completed 帧先定位
// "response" 对象再定位其内 "output"；非流式体直接定位顶层 "output"）。
func scanKeyValue(body []byte, key []byte) (start, end int, ok bool) {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return // 顶层非对象：无键
	}
	i++ // '{'
	for {
		i = skipSpace(body, i)
		if i >= len(body) {
			return
		}
		switch body[i] {
		case '}':
			return // 对象结束
		case ',':
			i++
		case '"':
			k, next := parseJSONString(body, i)
			if next < 0 {
				return
			}
			j := skipSpace(body, next)
			if j >= len(body) || body[j] != ':' {
				return
			}
			j = skipSpace(body, j+1)
			if j >= len(body) {
				return
			}
			e := skipValue(body, j)
			if e < 0 {
				return
			}
			if bytes.Equal(k, key) {
				return j, e, true
			}
			i = e
		default:
			return // 结构非法（json.Valid 前置，理论上不可达）
		}
	}
}

// decodeUnicodeEscapes 解码字节串中的 \uXXXX（\\ 前缀感知：\\u0069 是字面
// 反斜杠 + u0069，不解码）。最小实现：仅解码 \uXXXX——判定目标为纯 ASCII
// 小写字面，含其他转义形态的串在 == 比较上必 ≠ 目标（与现状 gjson String()
// 全解码等价）；无 \u 时返回原切片（零分配）。
func decodeUnicodeEscapes(v []byte) []byte {
	if !bytes.Contains(v, backslashUBytes) {
		return v // 无转义：原切片直返（零分配）
	}
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); {
		if v[i] != '\\' {
			out = append(out, v[i])
			i++
			continue
		}
		if i+1 < len(v) && v[i+1] == '\\' {
			out = append(out, '\\') // \\：字面反斜杠（其后 u 不解码）
			i += 2
			continue
		}
		if i+1 < len(v) && v[i+1] == 'u' && i+6 <= len(v) && isHex4(v[i+2:i+6]) {
			out = utf8.AppendRune(out, decodeHex4(v[i+2:i+6]))
			i += 6
			continue
		}
		out = append(out, v[i]) // 其他转义（\" \n 等）保留原样
		i++
	}
	return out
}

// isHex4 判定 4 字节均为十六进制数字。
func isHex4(b []byte) bool {
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodeHex4 解码 4 位十六进制 \uXXXX。单代理对按 RuneError 输出（JSON 语义
// 代理对应成对出现；不影响判定——目标为 ASCII 字面）。
func decodeHex4(b []byte) rune {
	var r rune
	for _, c := range b {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r += rune(c - '0')
		case c >= 'a' && c <= 'f':
			r += rune(c-'a') + 10
		default:
			r += rune(c-'A') + 10
		}
	}
	return r
}

// skipValue 跳过从 i 开始的 JSON 值（body[i] 为值首字节），返回值末字节后
// 一位置（不含尾随空白）；结构非法返回 -1。字符串转义正确跳过（\ 跳 1
// 字符——\uXXXX 的 4 hex 无引号/反斜杠，评审实证）；嵌套 {}[] 计数。
func skipValue(body []byte, i int) int {
	if i >= len(body) {
		return -1
	}
	switch body[i] {
	case '"':
		return skipJSONString(body, i)
	case '{', '[':
		depth := 1
		for i++; i < len(body); i++ {
			switch body[i] {
			case '"':
				end := skipJSONString(body, i)
				if end < 0 {
					return -1
				}
				i = end - 1 // 循环 i++ 抵消
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return -1
	case 't':
		if i+4 <= len(body) && bytes.Equal(body[i:i+4], trueBytes) {
			return i + 4
		}
		return -1
	case 'f':
		if i+5 <= len(body) && bytes.Equal(body[i:i+5], falseBytes) {
			return i + 5
		}
		return -1
	case 'n':
		if i+4 <= len(body) && bytes.Equal(body[i:i+4], nullBytes) {
			return i + 4
		}
		return -1
	default:
		// 数字：跳过至空白/逗号/括号（json.Valid 前置保证合法终止）
		for i < len(body) && !isWS(body[i]) && body[i] != ',' && body[i] != '}' && body[i] != ']' {
			i++
		}
		return i
	}
}

// skipElement 跳过数组元素（字符串/嵌套 {}[]/标量），返回元素末字节后一
// 位置（跳过尾随空白后，指向顶层 ',' 或 ']'）；结构非法返回 -1。
func skipElement(raw []byte, i int) int {
	end := skipValue(raw, i)
	if end < 0 {
		return -1
	}
	return skipSpace(raw, end)
}

// skipJSONString 跳过 JSON 字符串（body[i] 为开引号），返回闭引号后一位置；
// 未闭合返回 -1。\ 转义分支跳一个字符即足够（\"、\\、\uXXXX 的 4 hex 无
// 引号/反斜杠——评审实证）。
func skipJSONString(body []byte, i int) int {
	for i++; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++ // 跳过转义目标
		case '"':
			return i + 1
		}
	}
	return -1
}

// parseJSONString 解析 JSON 字符串（body[i] 为开引号），返回裸内容字节
// [i+1, 闭引号)（转义原样保留）与闭引号后一位置；未闭合返回 (nil, -1)。
func parseJSONString(body []byte, i int) ([]byte, int) {
	end := skipJSONString(body, i)
	if end < 0 {
		return nil, -1
	}
	return body[i+1 : end-1], end
}

// isWS 判定 JSON 空白字符。
func isWS(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// skipSpace 跳过空白，返回首个非空白位置。
func skipSpace(b []byte, i int) int {
	for i < len(b) && isWS(b[i]) {
		i++
	}
	return i
}

// trimWS 去除 [s, e) 区间的首尾空白，返回裸字节区间。
func trimWS(b []byte, s, e int) (int, int) {
	for s < e && isWS(b[s]) {
		s++
	}
	for e > s && isWS(b[e-1]) {
		e--
	}
	return s, e
}

// 不可变字节常量（零锁零全局状态——编译期静态引用，零分配）。
var (
	imageBytes         = []byte("image")
	backslashUBytes    = []byte(`\u`)
	toolsKeyBytes      = []byte("tools")
	toolChoiceKeyBytes = []byte("tool_choice")
	typeKeyBytes       = []byte("type")
	nameKeyBytes       = []byte("name")
	namespaceKeyBytes  = []byte("namespace")
	idKeyBytes         = []byte("id")
	resultKeyBytes     = []byte("result")
	responseKeyBytes   = []byte("response")
	outputKeyBytes     = []byte("output")
	// usage 提取键（spec 2026-08-15-gc-opt-ab A-1：scanKeyValue 键名精确匹配
	// 用——chat/responses/anthropic 三协议字段名；对齐 imageGenCallBytes 惯例）
	usageKeyBytes                 = []byte("usage")
	promptTokensKeyBytes          = []byte("prompt_tokens")
	completionTokensKeyBytes      = []byte("completion_tokens")
	totalTokensKeyBytes           = []byte("total_tokens")
	promptTokensDetailsKeyBytes   = []byte("prompt_tokens_details")
	cachedTokensKeyBytes          = []byte("cached_tokens")
	cacheCreationKeyBytes         = []byte("cache_creation")
	ephemeral5mKeyBytes           = []byte("ephemeral_5m_input_tokens")
	ephemeral1hKeyBytes           = []byte("ephemeral_1h_input_tokens")
	inputTokensKeyBytes           = []byte("input_tokens")
	outputTokensKeyBytes          = []byte("output_tokens")
	inputTokensDetailsKeyBytes    = []byte("input_tokens_details")
	cacheReadInputTokensKeyBytes  = []byte("cache_read_input_tokens")
	cacheCreationInputTokensKeyBytes = []byte("cache_creation_input_tokens")
	messageKeyBytes               = []byte("message")
	completedTypeBytes            = []byte("response.completed")
	imageGenCallBytes             = []byte("image_generation_call")
	typeImageGenTool   = []byte("image_generation_tool")
	typeImageEdits     = []byte("image_edits")
	typeNamespace      = []byte("namespace")
	nameImageGen       = []byte("image_gen")
	trueBytes          = []byte("true")
	falseBytes         = []byte("false")
	nullBytes          = []byte("null")
)
