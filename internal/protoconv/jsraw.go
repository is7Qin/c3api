// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 字节级转换的原始字节助手（chat→resp 主路径 优化）：不解析值、不构造
// 中间对象，直接取源 JSON 值/键的原始字节做透传拼接（SDK 字节级白名单过滤
// 模式：预筛 + 提取 + 拼接三步）。gjson Result.Raw 是值文本的零拷贝
// 切片，值本身无需改写时直接拼入输出（转义原样保留，语义等价——客户端/上游
// 解析后与 map 重排重编码的值相同）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

var (
	errInvalidJSON = errors.New("invalid JSON")
	errNotObject   = errors.New("invalid JSON: top-level must be an object")
)

// gjsonKeyEq 判定 gjson ForEach 键原始字节是否为指定名字（调用点传字符串
// 字面量）。无转义 → 长度校验 + 逐字节比较（零分配）。含 \uXXXX 等转义
// （合法 JSON，解码键 = 名字；转义形式恒比字面量长，长度前置检查会短路
// 转义场景——转义检测必须在长度判定之前，重做）→ 解码后比较
// （极低概率路径，一次分配可接受）。
func gjsonKeyEq(k gjson.Result, name string) bool {
	r := k.Raw
	if len(r) < 2 || r[0] != '"' || r[len(r)-1] != '"' {
		return false
	}
	if len(r) == len(name)+2 {
		// 无转义（含转义的 raw 恒更长）→ 逐字节比较
		for i := 1; i < len(r)-1; i++ {
			if r[i] != name[i-1] {
				return false
			}
		}
		return true
	}
	// 长度不等：含转义 → 解码后比较（gjson.Parse 还原 \uXXXX 等）
	for i := 1; i < len(r)-1; i++ {
		if r[i] == '\\' {
			return gjson.Parse(r).Str == name
		}
	}
	return false
}

// rawStrEq 判定字符串值原始文本是否等于字面量（"system" 等）。无转义 →
// 长度校验 + 逐字节比较（零分配）；含转义（恒更长，检测先于长度判定）→
// 解码后比较（重做）。非字符串值 → false。
func rawStrEq(v string, lit string) bool {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return false
	}
	if len(v) == len(lit)+2 {
		for i := 1; i < len(v)-1; i++ {
			if v[i] != lit[i-1] {
				return false
			}
		}
		return true
	}
	for i := 1; i < len(v)-1; i++ {
		if v[i] == '\\' {
			return gjson.Parse(v).Str == lit
		}
	}
	return false
}

// gjsonNumInt gjson 数字 → int64（截断，与 map 版 intOr0 的 float64→int64 同
// 语义）。非 Number 类型 → 0——需类型守卫：gjson 的 Int() 会解析字符串数字，
// 与 str() 的类型拒绝语义不符。超出 float64/int64 范围（如 1e400）→ 0
// （map 版对越界数字解码报错、字节级无错误通道，钳 0 避免
// int64(+Inf) 垃圾值；不可达真实流量）。
func gjsonNumInt(v gjson.Result) int64 {
	if v.Type != gjson.Number {
		return 0
	}
	f := v.Float()
	if f > 9223372036854775807.0 || f < -9223372036854775808.0 {
		return 0
	}
	return v.Int()
}

// emptyStr 空 JSON 字符串字面量（strOrEmpty 的缺省值，包级常量零分配）。
const emptyStr = `""`

// strOrEmpty 字符串值原始文本；缺失/非字符串 → ""（与 str() 缺失→"" 同语义）。
func strOrEmpty(v gjson.Result) string {
	if v.Type == gjson.String {
		return v.Raw
	}
	return emptyStr
}

// fcIDRaw function_call 项的匹配键原始文本：call_id 非空优先、id 兜底，均
// 缺失 → ""（同语义：toolCallID——客户端回传匹配键必须是 call_id）。
// 空字符串判定用原始长度（`""` 恰 2 字节）。
func fcIDRaw(item gjson.Result) string {
	if c := item.Get("call_id"); c.Type == gjson.String && len(c.Raw) > 2 {
		return c.Raw
	}
	if id := item.Get("id"); id.Type == gjson.String {
		return id.Raw
	}
	return emptyStr
}

// rawNotNull 值原始文本非空且非 null 字面量（pass() 的 v != nil 语义；
// json.Valid 已保证 'n' 开头的值恰为 null）。
func rawNotNull(v string) bool {
	return len(v) > 0 && v[0] != 'n'
}

// appendJSONString 按 encoding/json 规则把字符串写入 out（与 json.Marshal
// 逐字节一致：\" \\、\n\r\t\b\f 快捷转义、其余控制字符 \u00XX、& < > 转义
// \uXXXX、非法 UTF-8 → �）。用于需要重写/重排的字符串值（流式帧的
// id/model 来自 mapper 状态，非源字节切片）。
func appendJSONString(out []byte, s string) []byte {
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			out = append(out, '\\', '"')
		case c == '\\':
			out = append(out, '\\', '\\')
		case c < 0x20:
			switch c {
			case '\n':
				out = append(out, '\\', 'n')
			case '\r':
				out = append(out, '\\', 'r')
			case '\t':
				out = append(out, '\\', 't')
			case '\b':
				out = append(out, '\\', 'b')
			case '\f':
				out = append(out, '\\', 'f')
			default:
				out = append(out, '\\', 'u', '0', '0', hexDigit(c>>4), hexDigit(c&0xf))
			}
		case c == '&':
			out = append(out, '\\', 'u', '0', '0', '2', '6')
		case c == '<':
			out = append(out, '\\', 'u', '0', '0', '3', 'c')
		case c == '>':
			out = append(out, '\\', 'u', '0', '0', '3', 'e')
		case c >= 0x80:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				out = append(out, '\\', 'u', 'f', 'f', 'f', 'd')
			} else {
				out = append(out, s[i:i+size]...)
				i += size - 1
			}
		default:
			out = append(out, c)
		}
	}
	return append(out, '"')
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

// appendField 组装顶层 "key":value（首个字段无前导逗号；key 传字符串字面量）。
func appendField(out []byte, first *bool, key, val string) []byte {
	if !*first {
		out = append(out, ',')
	}
	*first = false
	out = append(out, '"')
	out = append(out, key...)
	out = append(out, `":`...)
	out = append(out, val...)
	return out
}

// appendInt64 以十进制追加 int64（strconv.AppendInt，零分配）。
func appendInt64(out []byte, n int64) []byte {
	return strconv.AppendInt(out, n, 10)
}

// parseRoot 合法对象只解析一次。结构不闭合直接 invalid JSON；闭合但不是对象时
// 再 json.Valid，区分非法与顶层 null（null 与 map 版对齐，输出 {}）。
func parseRoot(body []byte) (gjson.Result, error) {
	if !jsonShapeOK(body) {
		return gjson.Result{}, errInvalidJSON
	}
	root := gjson.ParseBytes(body)
	if root.IsObject() {
		return root, nil
	}
	if root.Type == gjson.Null && json.Valid(body) {
		return root, nil
	}
	if !json.Valid(body) {
		return gjson.Result{}, errInvalidJSON
	}
	return gjson.Result{}, errNotObject
}

func jsonShapeOK(b []byte) bool {
	i := 0
	for i < len(b) && b[i] <= ' ' {
		i++
	}
	if i >= len(b) {
		return false
	}
	var stack [64]byte
	sp := 0
	for i < len(b) {
		c := b[i]
		if c == '"' {
			i++
			for i < len(b) {
				if b[i] == '\\' {
					if i+1 >= len(b) {
						return false
					}
					i += 2
					continue
				}
				if b[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		}
		switch c {
		case '{', '[':
			if sp == len(stack) {
				return false
			}
			stack[sp] = c
			sp++
		case '}', ']':
			if sp == 0 {
				return false
			}
			sp--
			if (c == '}') != (stack[sp] == '{') {
				return false
			}
		}
		i++
		if sp == 0 {
			for i < len(b) && b[i] <= ' ' {
				i++
			}
			return i == len(b)
		}
	}
	return false
}

func appendComma(out []byte, n int) ([]byte, int) {
	if n > 0 {
		out = append(out, ',')
	}
	return out, n + 1
}

func appendJoinedTexts(out []byte, parts []string, sep string) []byte {
	if len(parts) == 0 {
		return out
	}
	if len(parts) == 1 {
		return append(out, parts[0]...)
	}
	var b []byte
	for i, p := range parts {
		if i > 0 {
			b = append(b, sep...)
		}
		b = append(b, gjson.Parse(p).Str...)
	}
	return appendJSONString(out, string(b))
}

func appendParsedJSON(out []byte, v gjson.Result) []byte {
	if v.Type == gjson.String && len(v.Raw) > 2 && json.Valid([]byte(v.Str)) {
		return append(out, v.Str...)
	}
	return append(out, '{', '}')
}

func appendImageSource(out []byte, urlRaw string) ([]byte, bool) {
	if len(urlRaw) < 2 || urlRaw[0] != '"' {
		return out, false
	}
	s := gjson.Parse(urlRaw).Str
	if len(s) >= 8 && (s[:8] == "https://" || (len(s) >= 7 && s[:7] == "http://")) {
		out = append(out, `{"source":{"type":"url","url":`...)
		out = append(out, urlRaw...)
		out = append(out, `},"type":"image"}`...)
		return out, true
	}
	const pfx = "data:image/"
	if len(s) < len(pfx) || s[:len(pfx)] != pfx {
		return out, false
	}
	rest := s[len(pfx):]
	semi := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == ';' {
			semi = i
			break
		}
	}
	if semi <= 0 {
		return out, false
	}
	media := rest[:semi]
	switch media {
	case "jpeg", "png", "gif", "webp":
	default:
		return out, false
	}
	const b64 = ";base64,"
	if len(rest) < semi+len(b64) || rest[semi:semi+len(b64)] != b64 {
		return out, false
	}
	out = append(out, `{"source":{"data":`...)
	out = appendJSONString(out, rest[semi+len(b64):])
	out = append(out, `,"media_type":"image/`...)
	out = append(out, media...)
	out = append(out, `","type":"base64"},"type":"image"}`...)
	return out, true
}

func textPartsRaw(content gjson.Result, types ...string) []string {
	if !content.IsArray() {
		return nil
	}
	var parts []string
	content.ForEach(func(_, p gjson.Result) bool {
		if !p.IsObject() {
			return true
		}
		typ := p.Get("type")
		ok := false
		for _, t := range types {
			if rawStrEq(typ.Raw, t) {
				ok = true
				break
			}
		}
		if !ok {
			return true
		}
		if tx := p.Get("text"); tx.Type == gjson.String {
			parts = append(parts, tx.Raw)
		}
		return true
	})
	return parts
}

func blockTextRaw(content gjson.Result, types ...string) (string, bool) {
	if content.Type == gjson.String {
		return content.Raw, true
	}
	parts := textPartsRaw(content, types...)
	if len(parts) == 0 {
		return "", false
	}
	buf := make([]byte, 0, 32)
	buf = appendJoinedTexts(buf, parts, "\n")
	return string(buf), true
}

// literalTop 在完整顶层对象里取未转义键的原始字符串（含引号）。
// numKey 非空时同时取该键的十进制整数（缺失为 0）。键含转义、值类型不符、
// 结构不闭合或尾随垃圾 → false，调用方回退 gjson，转义语义保持不变。
func literalTop(data []byte, key, numKey string) ([]byte, int64, bool) {
	i := skipSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, 0, false
	}
	i++
	var str []byte
	var n int64
	gotStr := false
	for {
		i = skipSpace(data, i)
		if i >= len(data) {
			return nil, 0, false
		}
		if data[i] == '}' {
			i = skipSpace(data, i+1)
			if i != len(data) || !gotStr {
				return nil, 0, false
			}
			return str, n, true
		}
		if data[i] != '"' {
			return nil, 0, false
		}
		content, next, ok := rawString(data, i)
		if !ok {
			return nil, 0, false
		}
		i = skipSpace(data, next)
		if i >= len(data) || data[i] != ':' {
			return nil, 0, false
		}
		i = skipSpace(data, i+1)
		if i >= len(data) {
			return nil, 0, false
		}
		switch {
		case bytesEq(content, key):
			if data[i] != '"' {
				return nil, 0, false
			}
			end, ok := rawStringEnd(data, i)
			if !ok {
				return nil, 0, false
			}
			str = data[i:end]
			gotStr = true
			i = end
		case numKey != "" && bytesEq(content, numKey):
			var ok bool
			n, i, ok = rawInt(data, i)
			if !ok {
				return nil, 0, false
			}
		default:
			var ok bool
			i, ok = skipValue(data, i)
			if !ok {
				return nil, 0, false
			}
		}
		i = skipSpace(data, i)
		if i < len(data) && data[i] == ',' {
			i++
			continue
		}
	}
}

func bytesEq(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(b); i++ {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

func skipSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func rawString(data []byte, i int) ([]byte, int, bool) {
	end, ok := rawStringEnd(data, i)
	if !ok {
		return nil, i, false
	}
	return data[i+1 : end-1], end, true
}

func rawStringEnd(data []byte, i int) (int, bool) {
	if i >= len(data) || data[i] != '"' {
		return i, false
	}
	i++
	for i < len(data) {
		if data[i] == '\\' {
			if i+1 >= len(data) {
				return i, false
			}
			i += 2
			continue
		}
		if data[i] == '"' {
			return i + 1, true
		}
		i++
	}
	return i, false
}

func rawInt(data []byte, i int) (int64, int, bool) {
	if i >= len(data) || data[i] == '-' || data[i] < '0' || data[i] > '9' {
		return 0, i, false
	}
	var n int64
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		n = n*10 + int64(data[i]-'0')
		i++
	}
	if i < len(data) && (data[i] == '.' || data[i] == 'e' || data[i] == 'E') {
		return 0, i, false
	}
	return n, i, true
}

func skipValue(data []byte, i int) (int, bool) {
	if i >= len(data) {
		return i, false
	}
	switch data[i] {
	case '"':
		return rawStringEnd(data, i)
	case '{', '[':
		open := data[i]
		close := byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 1
		i++
		for i < len(data) && depth > 0 {
			if data[i] == '"' {
				var ok bool
				i, ok = rawStringEnd(data, i)
				if !ok {
					return i, false
				}
				continue
			}
			switch data[i] {
			case open:
				depth++
			case close:
				depth--
			}
			i++
		}
		return i, depth == 0
	case 't':
		if i+4 <= len(data) && string(data[i:i+4]) == "true" {
			return i + 4, true
		}
	case 'f':
		if i+5 <= len(data) && string(data[i:i+5]) == "false" {
			return i + 5, true
		}
	case 'n':
		if i+4 <= len(data) && string(data[i:i+4]) == "null" {
			return i + 4, true
		}
	default:
		if data[i] == '-' || (data[i] >= '0' && data[i] <= '9') {
			i++
			for i < len(data) {
				c := data[i]
				if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
					i++
					continue
				}
				break
			}
			return i, true
		}
	}
	return i, false
}

func marshalRaw(v gjson.Result) string {
	if !v.Exists() || v.Raw == "" {
		return `"{}"`
	}
	if !json.Valid([]byte(v.Raw)) {
		return `"{}"`
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(v.Raw)); err != nil {
		return `"{}"`
	}
	out := make([]byte, 0, buf.Len()+8)
	out = appendJSONString(out, buf.String())
	return string(out)
}
