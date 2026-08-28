package proxy

import (
	"bytes"
	"encoding/json"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/is7qin/c3api/pkg/sserelay"
	"github.com/tidwall/sjson"
)

func rewriteResponseModelJSON(body []byte, model string) []byte {
	if model == "" || !json.Valid(body) {
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

func rewriteConvertedFrames(src []byte, model string) []byte {
	if model == "" || len(src) == 0 {
		return src
	}
	if !bytes.Contains(src, []byte(`"model"`)) {
		return src
	}
	var out []byte
	changed := false
	start := 0
	for start < len(src) {
		idx := bytes.Index(src[start:], []byte("\n\n"))
		var frame []byte
		var next int
		if idx < 0 {
			frame = src[start:]
			next = len(src)
		} else {
			frame = src[start : start+idx+2]
			next = start + idx + 2
		}
		data := extractSSEData(frame)
		ev := sserelay.Event{Raw: frame, Data: data}
		rewritten := rewriteResponseModelSSE(ev, model)
		if !bytes.Equal(rewritten, frame) {
			if !changed {
				out = make([]byte, 0, len(src)+len(rewritten)-len(frame)+16)
				out = append(out, src[:start]...)
				changed = true
			}
			out = append(out, rewritten...)
		} else if changed {
			out = append(out, frame...)
		}
		if idx < 0 {
			break
		}
		start = next
	}
	if !changed {
		return src
	}
	return out
}

func extractSSEData(frame []byte) []byte {
	var data []byte
	for pos := 0; pos < len(frame); {
		nl := bytes.IndexByte(frame[pos:], '\n')
		var lineEnd int
		if nl < 0 {
			lineEnd = len(frame)
		} else {
			lineEnd = pos + nl + 1
		}
		contentEnd := lineEnd
		if contentEnd > pos && frame[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > pos && frame[contentEnd-1] == '\r' {
			contentEnd--
		}
		line := frame[pos:contentEnd]
		if colon := bytes.IndexByte(line, ':'); colon >= 0 {
			name := line[:colon]
			if len(name) > 0 && name[0] != ':' && bytes.Equal(name, []byte("data")) {
				val := line[colon+1:]
				if len(val) > 0 && val[0] == ' ' {
					val = val[1:]
				}
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, val...)
			}
		}
		if nl < 0 {
			break
		}
		pos = lineEnd
	}
	return data
}
