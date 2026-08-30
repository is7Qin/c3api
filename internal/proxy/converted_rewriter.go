// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"

	"github.com/is7qin/c3api/pkg/sserelay"
)

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
		frameEnd := -1
		pos := start
		for pos < len(src) {
			nl := bytes.IndexByte(src[pos:], '\n')
			if nl < 0 {
				break
			}
			lineEnd := pos + nl + 1
			contentEnd := lineEnd
			if contentEnd > pos && src[contentEnd-1] == '\n' {
				contentEnd--
			}
			if contentEnd > pos && src[contentEnd-1] == '\r' {
				contentEnd--
			}
			if contentEnd == pos {
				frameEnd = lineEnd
				break
			}
			pos = lineEnd
		}
		var frame []byte
		var next int
		if frameEnd < 0 {
			frame = src[start:]
			next = len(src)
		} else {
			frame = src[start:frameEnd]
			next = frameEnd
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
		if frameEnd < 0 {
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
