// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"testing"

	"github.com/is7qin/c3api/internal/domain"
)

// 基准载荷：与功能测试同构的典型 chat→resp 主路径形态（压测 方向）。
const benchChatReqBody = `{
	"model": "gpt-4o",
	"messages": [
		{"role": "system", "content": "you are helpful"},
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "hello", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"x\"}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "content": "{\"temp\": 20}"}
	],
	"max_completion_tokens": 200,
	"temperature": 0.5,
	"stream": true,
	"stream_options": {"include_usage": true},
	"tools": [{"type": "function", "function": {"name": "get_weather", "description": "d", "parameters": {"type": "object"}, "strict": true}}],
	"tool_choice": {"type": "function", "function": {"name": "get_weather"}},
	"stop": ["x"],
	"frequency_penalty": 0.3
}`

const benchRespBody = `{
	"id": "rsp_1", "object": "response", "created_at": 1750000000, "status": "completed", "model": "gpt-4o",
	"output": [
		{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
		 "content": [{"type": "output_text", "text": "hello", "annotations": []}]},
		{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"x\"}", "status": "completed"}
	],
	"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15, "input_tokens_details": {"cached_tokens": 3}}
}`

// benchSink 承接基准输出，防 DCE。
var benchSink []byte

func BenchmarkConvertRequestChatToResp(b *testing.B) {
	body := []byte(benchChatReqBody)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		out, err := ConvertRequest(body, domain.ProtocolConvertChatToResp)
		if err != nil {
			b.Fatal(err)
		}
		benchSink = out
	}
}

func BenchmarkConvertResponseRespToChat(b *testing.B) {
	body := []byte(benchRespBody)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		out, err := ConvertResponse(body, domain.ProtocolConvertChatToResp)
		if err != nil {
			b.Fatal(err)
		}
		benchSink = out
	}
}

// benchEv 流事件（name + data 原始字节，与 sserelay Event 形态一致——避免
// 基准内重复 string→[]byte 转换的测量噪声）。
type benchEv struct {
	name string
	data []byte
}

// benchStreamEvents 构造典型 resp→chat 流（压测形态：created + n 个 delta +
// output_item.added(FC) + fc 参数 delta + completed）。
func benchStreamEvents(n int) []benchEv {
	evs := []benchEv{
		{"response.created", []byte(`{"type":"response.created","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"in_progress","model":"gpt-4o","output":[],"usage":null}}`)},
		{"response.output_item.added", []byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}`)},
	}
	for i := 0; i < n; i++ {
		evs = append(evs, benchEv{"response.output_text.delta", []byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"chunk token text "}`)})
	}
	evs = append(evs, benchEv{"response.function_call_arguments.delta", []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"city\": \"x\"}"}`)})
	evs = append(evs, benchEv{"response.completed", []byte(`{"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"completed"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)})
	return evs
}

func BenchmarkStreamRespToChat(b *testing.B) {
	events := benchStreamEvents(100)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m := NewStreamMapper(domain.ProtocolConvertChatToResp)
		for _, ev := range events {
			frame, _ := m.Map(ev.name, ev.data)
			benchSink = frame
		}
	}
}
