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

const benchMessReqBody = `{
	"model": "claude-3-5-sonnet",
	"max_tokens": 200,
	"system": "you are helpful",
	"messages": [
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": [{"type": "text", "text": "hello"}, {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "x"}}]},
		{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "{\"temp\": 20}"}]}
	],
	"temperature": 0.5,
	"stream": true,
	"tools": [{"name": "get_weather", "description": "d", "input_schema": {"type": "object"}}],
	"tool_choice": {"type": "auto"}
}`

const benchRespReqBody = `{
	"model": "gpt-4o",
	"instructions": "you are helpful",
	"input": [
		{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
		{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\": \"x\"}"},
		{"type": "function_call_output", "call_id": "call_1", "output": "{\"temp\": 20}"}
	],
	"max_output_tokens": 200,
	"temperature": 0.5,
	"stream": true,
	"tools": [{"type": "function", "name": "get_weather", "description": "d", "parameters": {"type": "object"}, "strict": true}],
	"tool_choice": "auto"
}`

func benchReq(b *testing.B, body []byte, dir domain.ProtocolConvert) {
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		out, err := ConvertRequest(body, dir)
		if err != nil {
			b.Fatal(err)
		}
		benchSink = out
	}
}

func BenchmarkConvertRequestChatToMess(b *testing.B) {
	benchReq(b, []byte(benchChatReqBody), domain.ProtocolConvertChatToMess)
}

func BenchmarkConvertRequestMessToResp(b *testing.B) {
	benchReq(b, []byte(benchMessReqBody), domain.ProtocolConvertMessToResp)
}

func BenchmarkConvertRequestRespToMess(b *testing.B) {
	benchReq(b, []byte(benchRespReqBody), domain.ProtocolConvertRespToMess)
}

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

// benchChatMultiTextBody chat→resp 的多 text 部件形态：一条用户消息含 3 个
// text 部件、一条 assistant 工具调用、一条 tool 消息（content 为数组）。内容
// 模型（行表分派）改动的分配/字节回归基线。
const benchChatMultiTextBody = `{
	"model": "gpt-4o",
	"messages": [
		{"role": "system", "content": "you are helpful"},
		{"role": "user", "content": [
			{"type": "text", "text": "first part"},
			{"type": "text", "text": "second part"},
			{"type": "text", "text": "third part"}
		]},
		{"role": "assistant", "content": "ok", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"x\"}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "content": [{"type": "text", "text": "{\"temp\": 20}"}]}
	],
	"max_completion_tokens": 200,
	"stream": true
}`

func BenchmarkConvertRequestChatToRespMultiText(b *testing.B) {
	benchReq(b, []byte(benchChatMultiTextBody), domain.ProtocolConvertChatToResp)
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
	for range n {
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

// --- reasoning 路径（issue 46）：请求侧 thinking 形态与流式思考 delta ---

// benchThinkingEnv 请求侧 thinking 形态用的信封（thinking 块 → encrypted_content）。
var benchThinkingEnv = sealThinking(thinkingBlock{
	Type: "thinking", Thinking: "我需要先列目录", Signature: "sig_bench",
})

// benchRespThinkingReqBody resp→mess 请求（reasoning item + effort + tools）：
// 压测 effort→budget 换算与信封解码（spec §5：换算 0 分配；解码只在 c3a1: 命中
// 时发生）。
func benchRespThinkingReqBody() []byte {
	return []byte(`{
		"model": "claude-3-5-sonnet",
		"max_output_tokens": 32000,
		"reasoning": {"effort": "high"},
		"stream": true,
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run ls"}]},
			{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + benchThinkingEnv + `"},
			{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"}
		],
		"tools": [{"type":"function","name":"exec_command","description":"d","parameters":{"type":"object"}}]
	}`)
}

func BenchmarkConvertRequestRespToMessThinking(b *testing.B) {
	benchReq(b, benchRespThinkingReqBody(), domain.ProtocolConvertRespToMess)
}

// benchMessStreamEvents 构造 n 个 delta 的 mess 流（kind = "thinking" / "text"）。
// 两条路径事件数相同，allocs/op 可直接对比（「流式无逐帧新增分配」）。
func benchMessStreamEvents(kind string, n int) []benchEv {
	evs := []benchEv{
		{"message_start", []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)},
	}
	if kind == "thinking" {
		evs = append(evs, benchEv{"content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)})
		for range n {
			evs = append(evs, benchEv{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"思考片段 "}}`)})
		}
		evs = append(evs,
			benchEv{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_bench"}}`)},
			benchEv{"content_block_stop", []byte(`{"type":"content_block_stop","index":0}`)},
		)
	} else {
		evs = append(evs, benchEv{"content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)})
		for range n {
			evs = append(evs, benchEv{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"正文片段 "}}`)})
		}
		evs = append(evs, benchEv{"content_block_stop", []byte(`{"type":"content_block_stop","index":0}`)})
	}
	evs = append(evs, benchEv{"message_stop", []byte(`{"type":"message_stop"}`)})
	return evs
}

// benchMessStream 跑一遍 mess→resp 流（每轮新建 mapper：模拟一条真实流）。
func benchMessStream(b *testing.B, events []benchEv) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m := NewStreamMapper(domain.ProtocolConvertRespToMess)
		for _, ev := range events {
			frame, _ := m.Map(ev.name, ev.data)
			benchSink = frame
		}
	}
}

// BenchmarkStreamMessToRespThinkingDelta 思考块（start + 100 thinking_delta +
// signature_delta + stop）；与下面的 text 对照用例比 allocs/op。
func BenchmarkStreamMessToRespThinkingDelta(b *testing.B) {
	benchMessStream(b, benchMessStreamEvents("thinking", 100))
}

// BenchmarkStreamMessToRespTextDelta 正文块（start + 100 text_delta + stop）：
// thinking 路径的对照基线。
func BenchmarkStreamMessToRespTextDelta(b *testing.B) {
	benchMessStream(b, benchMessStreamEvents("text", 100))
}
