// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 字节级转换的原始字节助手（chat→resp 主路径 优化）：不解析值、不构造
// 中间对象，直接取源 JSON 值/键的原始字节做透传拼接（SDK FilterCodexPayload
// 字节级模式：预筛 + 提取 + 拼接三步）。gjson Result.Raw 是值文本的零拷贝
// 切片，值本身无需改写时直接拼入输出（转义原样保留，语义等价——客户端/上游
// 解析后与 map 重排重编码的值相同）。

import (
	"strconv"
	"unicode/utf8"

	"github.com/tidwall/gjson"
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
