package proxy

import (
	"bytes"
	"encoding/json"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/is7qin/c3api/pkg/sserelay"
	"github.com/tidwall/sjson"
)

// rewriteResponseModelJSON 把响应体里三处既有 model 字段改写成客户端请求的模型名
// （隐式映射模式；`model == ""` 恒走空路径）。
//
// **两段式：先字节预筛，再全帧校验。** 三条改写路径（`model` / `response.model` /
// `message.model`）都要求帧内出现裸键字节 `model`：`scanKeyValue` 用
// `parseJSONString` 取引号之间的**原始字节**（不解 `\uXXXX`），再与 `modelKeyBytes`
// 做 `bytes.Equal`。所以"命中 model 键" ⇒ 帧内必含连续 5 字节 `model`，预筛是
// **真超集**，不存在漏判；预筛 miss 时原实现三个分支也全部 `continue`、`json.Valid`
// 也只原样返回 body，故输出逐字节相同。
//
// 代价对比（内容帧，无命中，Ryzen 7 7840H / go1.27）：
//
//	                    4 KiB      64 KiB     1 MiB
//	改前（json.Valid+3 遍）  7293 ns   112061 ns  1726057 ns
//	改后（预筛 miss）         90 ns      772 ns    12696 ns
//
// 即 81× / 145× / 136×——大帧上这是帧路径**最贵的一项**（判死嗅探同口径 12.4 µs/MiB）。
// 预筛命中（真出现 `model` 字样，含"正文里恰好有这个词"）只是回到原路径，无副作用。
func rewriteResponseModelJSON(body []byte, model string) []byte {
	if model == "" || !bytes.Contains(body, modelKeyBytes) {
		return body
	}
	if !json.Valid(body) {
		return body
	}
	original := body
	paths := [...]struct {
		outer []byte
		path  string
	}{
		{path: "model"},
		{outer: responseKeyBytes, path: "response.model"},
		{outer: messageKeyBytes, path: "message.model"},
	}
	for _, target := range paths {
		scope := body
		if target.outer != nil {
			start, end, ok := scanKeyValue(body, target.outer)
			if !ok {
				continue
			}
			scope = body[start:end]
		}
		start, end, ok := scanKeyValue(scope, modelKeyBytes)
		if !ok {
			continue
		}
		current := scope[start:end]
		if len(current) < 2 || current[0] != '"' || current[len(current)-1] != '"' || modelStringEquals(current, model) {
			continue
		}
		var err error
		body, err = sjson.SetBytes(body, target.path, model)
		if err != nil {
			return original
		}
	}
	return body
}

func modelStringEquals(raw []byte, model string) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	value := raw[1 : len(raw)-1]
	modelIndex := 0
	for valueIndex := 0; valueIndex < len(value); {
		if value[valueIndex] != '\\' {
			if modelIndex >= len(model) || value[valueIndex] != model[modelIndex] {
				return false
			}
			valueIndex++
			modelIndex++
			continue
		}

		valueIndex++
		if valueIndex >= len(value) {
			return false
		}
		escape := value[valueIndex]
		valueIndex++
		if escape == 'u' {
			if valueIndex+4 > len(value) {
				return false
			}
			r := decodeHex4(value[valueIndex : valueIndex+4])
			valueIndex += 4
			if r >= 0xd800 && r <= 0xdfff {
				if r <= 0xdbff && valueIndex+6 <= len(value) && value[valueIndex] == '\\' && value[valueIndex+1] == 'u' {
					low := decodeHex4(value[valueIndex+2 : valueIndex+6])
					if low >= 0xdc00 && low <= 0xdfff {
						r = utf16.DecodeRune(r, low)
						valueIndex += 6
					} else {
						r = utf8.RuneError
					}
				} else {
					r = utf8.RuneError
				}
			}
			var encoded [utf8.UTFMax]byte
			n := utf8.EncodeRune(encoded[:], r)
			if modelIndex+n > len(model) {
				return false
			}
			for i := range n {
				if encoded[i] != model[modelIndex+i] {
					return false
				}
			}
			modelIndex += n
			continue
		}

		decoded := escape
		switch escape {
		case 'b':
			decoded = '\b'
		case 'f':
			decoded = '\f'
		case 'n':
			decoded = '\n'
		case 'r':
			decoded = '\r'
		case 't':
			decoded = '\t'
		case '"', '\\', '/':
		default:
			return false
		}
		if modelIndex >= len(model) || decoded != model[modelIndex] {
			return false
		}
		modelIndex++
	}
	return modelIndex == len(model)
}

func rewriteResponseModelSSE(event sserelay.Event, model string) []byte {
	data := rewriteResponseModelJSON(event.Data, model)
	if bytes.Equal(data, event.Data) {
		return event.Raw
	}

	out := make([]byte, 0, len(event.Raw)+len(data))
	wroteData := false
	for lineStart := 0; lineStart < len(event.Raw); {
		lineEnd := bytes.IndexByte(event.Raw[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(event.Raw)
		} else {
			lineEnd += lineStart + 1
		}
		contentEnd := lineEnd
		if contentEnd > lineStart && event.Raw[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > lineStart && event.Raw[contentEnd-1] == '\r' {
			contentEnd--
		}

		line := event.Raw[lineStart:contentEnd]
		colon := bytes.IndexByte(line, ':')
		if colon >= 0 && bytes.Equal(line[:colon], []byte("data")) {
			if !wroteData {
				valueStart := lineStart + colon + 1
				if valueStart < contentEnd && event.Raw[valueStart] == ' ' {
					valueStart++
				}
				out = append(out, event.Raw[lineStart:valueStart]...)
				for _, b := range data {
					if b != '\n' {
						out = append(out, b)
					}
				}
				out = append(out, event.Raw[contentEnd:lineEnd]...)
				wroteData = true
			}
		} else {
			out = append(out, event.Raw[lineStart:lineEnd]...)
		}
		lineStart = lineEnd
	}
	if !wroteData {
		return event.Raw
	}
	return out
}

func newResponseModelRewriter(model string) func([]byte) []byte {
	if model == "" {
		return nil
	}
	return func(body []byte) []byte {
		return rewriteResponseModelJSON(body, model)
	}
}

func newResponseModelSSEMapper(model string) func(sserelay.Event) ([]byte, bool) {
	if model == "" {
		return nil
	}
	return func(ev sserelay.Event) ([]byte, bool) {
		return rewriteResponseModelSSE(ev, model), false
	}
}
