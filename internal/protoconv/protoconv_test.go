// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func obj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func arrOf(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	require.True(t, ok, "missing array key %s", key)
	return v
}

// --- chat→resp 请求 ---

func TestConvertRequestChatToResp(t *testing.T) {
	body := []byte(`{
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
	}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "gpt-4o", m["model"])
	require.Equal(t, true, m["stream"], "stream 透传（模板 resp 流式 raw 路径依赖）")
	require.Equal(t, float64(200), m["max_output_tokens"], "max_completion_tokens → max_output_tokens")
	require.Equal(t, float64(0.5), m["temperature"])
	require.NotContains(t, m, "stream_options", "resp 无 stream_options，按规范丢弃")
	require.NotContains(t, m, "stop", "resp 无 stop 参数，按规范丢弃")
	require.NotContains(t, m, "frequency_penalty", "resp 无 frequency_penalty，按规范丢弃")

	input := arrOf(t, m, "input")
	require.Len(t, input, 5, "system/user/assistant 各一消息项 + assistant tool_calls 独立 function_call 项 + tool 消息")
	sys := input[0].(map[string]any)
	require.Equal(t, "developer", sys["role"], "system → developer")
	user := input[1].(map[string]any)
	uc := arrOf(t, user, "content")[0].(map[string]any)
	require.Equal(t, "input_text", uc["type"])
	fc := input[3].(map[string]any)
	require.Equal(t, "function_call", fc["type"])
	require.Equal(t, "call_1", fc["call_id"])
	require.Equal(t, `{"city": "x"}`, fc["arguments"])
	fco := input[4].(map[string]any)
	require.Equal(t, "function_call_output", fco["type"])
	require.Equal(t, "call_1", fco["call_id"])

	tools := arrOf(t, m, "tools")
	require.Len(t, tools, 1)
	tl := tools[0].(map[string]any)
	require.Equal(t, "function", tl["type"])
	require.Equal(t, "get_weather", tl["name"])
	require.Equal(t, true, tl["strict"])
	tc := m["tool_choice"].(map[string]any)
	require.Equal(t, map[string]any{"type": "function", "name": "get_weather"}, tc, "嵌套 function 扁平化")
}

func TestConvertRequestChatToRespLegacyMaxTokensAndImages(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "look"}, {"type": "image_url", "image_url": {"url": "https://example.com/i.png"}}]}
		],
		"max_tokens": 100
	}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, float64(100), m["max_output_tokens"], "legacy max_tokens → max_output_tokens")
	input := arrOf(t, m, "input")
	content := arrOf(t, input[0].(map[string]any), "content")
	require.Len(t, content, 2)
	require.Equal(t, "input_text", content[0].(map[string]any)["type"])
	img := content[1].(map[string]any)
	require.Equal(t, "input_image", img["type"])
	require.Equal(t, "https://example.com/i.png", img["image_url"])
}

// --- mess→resp 请求 ---

func TestConvertRequestMessToResp(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"system": [{"type": "text", "text": "sys A"}, {"type": "text", "text": "sys B"}],
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "text", "text": "hello"}, {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "x"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "{\"temp\": 20}"}]}
		],
		"max_tokens": 500,
		"top_k": 40,
		"tools": [{"name": "get_weather", "description": "d", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "tool", "name": "get_weather"},
		"stop_sequences": ["END"]
	}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertMessToResp)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "sys A\nsys B", m["instructions"], "system 块数组 → 拼接 instructions")
	require.Equal(t, "claude-3-5-sonnet", m["model"])
	require.Equal(t, float64(500), m["max_output_tokens"])
	require.NotContains(t, m, "top_k", "resp 无 top_k，按规范丢弃")
	require.NotContains(t, m, "stop_sequences", "resp 无 stop_sequences，按规范丢弃")

	input := arrOf(t, m, "input")
	require.Len(t, input, 4)
	fc := input[2].(map[string]any)
	require.Equal(t, "function_call", fc["type"])
	require.Equal(t, "toolu_1", fc["call_id"])
	require.Equal(t, `{"city":"x"}`, fc["arguments"], "input 对象 → arguments JSON 字符串")
	fco := input[3].(map[string]any)
	require.Equal(t, "function_call_output", fco["type"])
	require.Equal(t, "toolu_1", fco["call_id"])

	tools := arrOf(t, m, "tools")
	require.Len(t, tools, 1)
	tl := tools[0].(map[string]any)
	require.Equal(t, "function", tl["type"])
	require.Equal(t, map[string]any{"type": "object"}, tl["parameters"], "input_schema → parameters")
	tc := m["tool_choice"].(map[string]any)
	require.Equal(t, map[string]any{"type": "function", "name": "get_weather"}, tc, "{type:tool} → {type:function}")
}

// --- resp→mess 请求 ---

func TestConvertRequestRespToMess(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"instructions": "you are helpful",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "let me check"}]},
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\": \"x\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "{\"temp\": 20}"}
		],
		"max_output_tokens": 300,
		"tools": [{"type": "function", "name": "get_weather", "description": "d", "parameters": {"type": "object"}}],
		"tool_choice": {"type": "function", "name": "get_weather"},
		"stream": true
	}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertRespToMess)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "you are helpful", m["system"], "instructions → system")
	require.Equal(t, "gpt-4o", m["model"])
	require.Equal(t, float64(300), m["max_tokens"], "max_output_tokens → max_tokens")
	require.Equal(t, true, m["stream"])

	msgs := arrOf(t, m, "messages")
	require.Len(t, msgs, 3)
	assistant := msgs[1].(map[string]any)
	content := arrOf(t, assistant, "content")
	require.Len(t, content, 2, "function_call 追加到最近 assistant 消息")
	require.Equal(t, "text", content[0].(map[string]any)["type"])
	tu := content[1].(map[string]any)
	require.Equal(t, "tool_use", tu["type"])
	require.Equal(t, map[string]any{"city": "x"}, tu["input"], "arguments JSON 字符串 → input 对象")
	tr := msgs[2].(map[string]any)
	require.Equal(t, "user", tr["role"])
	trc := arrOf(t, tr, "content")[0].(map[string]any)
	require.Equal(t, "tool_result", trc["type"])
	require.Equal(t, "call_1", trc["tool_use_id"])

	tools := arrOf(t, m, "tools")
	require.Len(t, tools, 1)
	tl := tools[0].(map[string]any)
	require.Equal(t, map[string]any{"type": "object"}, tl["input_schema"], "parameters → input_schema")
	require.NotContains(t, tl, "type", "anthropic tool 无 type 字段")
	tc := m["tool_choice"].(map[string]any)
	require.Equal(t, map[string]any{"type": "tool", "name": "get_weather"}, tc)
}

func TestConvertRequestRespToMessSystemInputItems(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "dev rule"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}
		]
	}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertRespToMess)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "dev rule", m["system"], "developer 消息项并入 system")
	msgs := arrOf(t, m, "messages")
	require.Len(t, msgs, 1, "developer 项不产生消息")
}

// --- chat→mess 请求 ---

func TestConvertRequestChatToMess(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "you are helpful"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "let me check", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"x\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "{\"temp\": 20}"}
		],
		"max_tokens": 200,
		"stop": ["END"],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "d", "parameters": {"type": "object"}}}],
		"tool_choice": "required",
		"stream": true
	}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertChatToMess)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "you are helpful", m["system"], "system 消息 → 顶层 system")
	require.Equal(t, "gpt-4o", m["model"])
	require.Equal(t, float64(200), m["max_tokens"], "max_tokens 同名字段")
	require.Equal(t, []any{"END"}, m["stop_sequences"], "stop → stop_sequences 数组")
	require.Equal(t, true, m["stream"])

	msgs := arrOf(t, m, "messages")
	require.Len(t, msgs, 3)
	assistant := msgs[1].(map[string]any)
	content := arrOf(t, assistant, "content")
	require.Len(t, content, 2)
	tu := content[1].(map[string]any)
	require.Equal(t, "tool_use", tu["type"])
	require.Equal(t, map[string]any{"city": "x"}, tu["input"])
	tr := msgs[2].(map[string]any)
	trc := arrOf(t, tr, "content")[0].(map[string]any)
	require.Equal(t, "tool_result", trc["type"])
	require.Equal(t, "call_1", trc["tool_use_id"])

	tools := arrOf(t, m, "tools")
	require.Len(t, tools, 1)
	tl := tools[0].(map[string]any)
	require.Equal(t, map[string]any{"type": "object"}, tl["input_schema"])
	require.Equal(t, "any", m["tool_choice"], "required → any")
}

func TestConvertRequestChatToMessMaxCompletionTokens(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":300}`), domain.ProtocolConvertChatToMess)
	require.NoError(t, err)
	require.Equal(t, float64(300), obj(t, out)["max_tokens"], "max_completion_tokens → max_tokens")
}

// --- 响应 JSON 转换 ---

func TestConvertResponseRespToChat(t *testing.T) {
	body := []byte(`{
		"id": "rsp_1", "object": "response", "created_at": 1750000000, "status": "completed", "model": "gpt-4o",
		"output": [
			{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
			 "content": [{"type": "output_text", "text": "hello", "annotations": []}]},
			{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"x\"}", "status": "completed"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15, "input_tokens_details": {"cached_tokens": 3}}
	}`)
	out, err := ConvertResponse(body, domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "chat.completion", m["object"])
	require.Equal(t, "rsp_1", m["id"])
	require.Equal(t, float64(1750000000), m["created"])
	choices := arrOf(t, m, "choices")
	ch := choices[0].(map[string]any)
	require.Equal(t, "tool_calls", ch["finish_reason"])
	msg := ch["message"].(map[string]any)
	require.Equal(t, "hello", msg["content"])
	tcs := arrOf(t, msg, "tool_calls")
	require.Len(t, tcs, 1)
	tc := tcs[0].(map[string]any)
	require.Equal(t, "get_weather", tc["function"].(map[string]any)["name"])
	require.Equal(t, `{"city":"x"}`, tc["function"].(map[string]any)["arguments"])
	u := m["usage"].(map[string]any)
	require.Equal(t, float64(10), u["prompt_tokens"])
	require.Equal(t, float64(5), u["completion_tokens"])
	require.Equal(t, float64(15), u["total_tokens"])
	require.Equal(t, float64(3), u["prompt_tokens_details"].(map[string]any)["cached_tokens"])
}

func TestConvertResponseRespToMess(t *testing.T) {
	body := []byte(`{
		"id": "rsp_1", "object": "response", "created_at": 1750000000, "status": "completed", "model": "gpt-4o",
		"output": [
			{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
			 "content": [{"type": "output_text", "text": "hello", "annotations": []}]},
			{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"x\"}", "status": "completed"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15, "input_tokens_details": {"cached_tokens": 3}}
	}`)
	out, err := ConvertResponse(body, domain.ProtocolConvertMessToResp)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "message", m["type"])
	require.Equal(t, "rsp_1", m["id"])
	require.Equal(t, "tool_use", m["stop_reason"])
	content := arrOf(t, m, "content")
	require.Len(t, content, 2)
	tu := content[1].(map[string]any)
	require.Equal(t, "tool_use", tu["type"])
	require.Equal(t, map[string]any{"city": "x"}, tu["input"], "arguments JSON 字符串 → input 对象")
	u := m["usage"].(map[string]any)
	require.Equal(t, float64(10), u["input_tokens"])
	require.Equal(t, float64(5), u["output_tokens"])
	require.Equal(t, float64(3), u["cache_read_input_tokens"], "cached_tokens → cache_read_input_tokens")
}

func TestConvertResponseMessToResp(t *testing.T) {
	body := []byte(`{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-3-5-sonnet",
		"content": [{"type": "text", "text": "hello"}, {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "x"}}],
		"stop_reason": "tool_use", "stop_sequence": null,
		"usage": {"input_tokens": 10, "output_tokens": 5, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 3}
	}`)
	out, err := ConvertResponse(body, domain.ProtocolConvertRespToMess)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "response", m["object"])
	require.Equal(t, "msg_1", m["id"])
	require.Equal(t, "completed", m["status"])
	output := arrOf(t, m, "output")
	require.Len(t, output, 2)
	mi := output[0].(map[string]any)
	require.Equal(t, "message", mi["type"])
	require.Equal(t, "hello", arrOf(t, mi, "content")[0].(map[string]any)["text"])
	fc := output[1].(map[string]any)
	require.Equal(t, "function_call", fc["type"])
	require.Equal(t, `{"city":"x"}`, fc["arguments"], "input 对象 → arguments JSON 字符串")
	u := m["usage"].(map[string]any)
	require.Equal(t, float64(10), u["input_tokens"])
	require.Equal(t, float64(15), u["total_tokens"])
	require.Equal(t, float64(3), u["input_tokens_details"].(map[string]any)["cached_tokens"])
}

func TestConvertResponseMessToChat(t *testing.T) {
	body := []byte(`{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-3-5-sonnet",
		"content": [{"type": "text", "text": "hello"}, {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "x"}}],
		"stop_reason": "tool_use", "stop_sequence": null,
		"usage": {"input_tokens": 10, "output_tokens": 5, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 3}
	}`)
	out, err := ConvertResponse(body, domain.ProtocolConvertChatToMess)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "chat.completion", m["object"])
	require.Equal(t, "msg_1", m["id"])
	choices := arrOf(t, m, "choices")
	ch := choices[0].(map[string]any)
	require.Equal(t, "tool_calls", ch["finish_reason"])
	msg := ch["message"].(map[string]any)
	require.Equal(t, "hello", msg["content"])
	tc := arrOf(t, msg, "tool_calls")[0].(map[string]any)
	require.Equal(t, `{"city":"x"}`, tc["function"].(map[string]any)["arguments"])
	u := m["usage"].(map[string]any)
	require.Equal(t, float64(10), u["prompt_tokens"])
	require.Equal(t, float64(15), u["total_tokens"])
	require.Equal(t, float64(3), u["prompt_tokens_details"].(map[string]any)["cached_tokens"])
}

// --- 流式事件映射 ---

// mapAll 依次映射一组事件（name/data 交替对），逐帧做完整 SSE 帧校验
// （评审 M-1 后升级：帧尾空行、行 field: 前缀、data 载荷 JSON 合法或
// [DONE]——钉住帧格式，防子串断言漏网），返回全部产出帧的文本拼接。
func mapAll(t *testing.T, dir domain.ProtocolConvert, events ...string) string {
	t.Helper()
	m := NewStreamMapper(dir)
	var out []string
	for i := 0; i+1 < len(events); i += 2 {
		frame, drop := m.Map(events[i], []byte(events[i+1]))
		if drop {
			out = append(out, "[drop]")
			continue
		}
		validateFrames(t, frame)
		out = append(out, string(frame))
	}
	return strings.Join(out, "|")
}

// validateFrames 校验映射产出字节为完整合法 SSE 帧序列：以空行终止；每行
// `data: `/`event: ` 前缀；data 载荷为合法 JSON（[DONE] 终止帧例外）。
// 一次 Map 可能返回多帧（如 completed → chunk + [DONE]），按空行拆分逐帧校验。
func validateFrames(t *testing.T, frame []byte) {
	t.Helper()
	if len(frame) < 2 || frame[len(frame)-1] != '\n' || frame[len(frame)-2] != '\n' {
		t.Fatalf("帧必须以空行终止: %q", frame)
	}
	for _, f := range bytes.Split(frame, []byte("\n\n")) {
		if len(f) == 0 {
			continue
		}
		lines := strings.Split(string(f), "\n")
		var data []string
		for _, ln := range lines {
			if ln == "" {
				continue
			}
			if !strings.HasPrefix(ln, "data: ") && !strings.HasPrefix(ln, "event: ") {
				t.Fatalf("帧行缺少 field: 前缀: %q", f)
			}
			if strings.HasPrefix(ln, "data: ") {
				data = append(data, ln[len("data: "):])
			}
		}
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("帧 data 载荷非法 JSON: %q", f)
		}
	}
}

func TestMapRespToChatStream(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertChatToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"in_progress","model":"gpt-4o","output":[],"usage":null}}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hel"}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"lo"}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}`,
		"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"city\": \"x\"}"}`,
		"response.output_text.done", `{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hello"}`,
		"response.completed", `{"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"completed"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
		"data-ignore", `x`, // 不应出现（占位，mapAll 以 name 为事件名）
	)
	require.NotContains(t, out, "output_text.done", "非映射事件丢弃")
	require.Contains(t, out, `"delta":{"content":"","role":"assistant"}`, "角色前导 chunk")
	require.Contains(t, out, `"delta":{"content":"hel"}`, "文本 delta chunk")
	require.Contains(t, out, `"delta":{"content":"lo"}`)
	require.Contains(t, out, `"tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"call_1","index":1,"type":"function"}]`, "tool_calls 前导 id = call_id（M-1）")
	require.Contains(t, out, `"tool_calls":[{"function":{"arguments":"{\"city\": \"x\"}"},"index":1}]`, "arguments delta")
	require.Contains(t, out, `"finish_reason":"tool_calls"`, "含 function_call → tool_calls")
	require.Contains(t, out, `"usage":{"completion_tokens":5,"prompt_tokens":3,"total_tokens":8}`, "收尾 chunk 内联 usage")
	require.Contains(t, out, "data: [DONE]", "收尾 [DONE]")
}

func TestMapRespToChatFailed(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertChatToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_1","object":"response","status":"in_progress","model":"m","output":[]}}`,
		"response.failed", `{"type":"response.failed","response":{"id":"rsp_1","object":"response","status":"failed","error":{"code":"server_error","message":"boom"}}}`,
	)
	require.Contains(t, out, `data: {"error":{"message":"boom"}}`, "failed → chat 流式错误帧")
	require.NotContains(t, out, "[DONE]")
}

func TestMapRespToMessStream(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"in_progress","model":"gpt-4o","output":[],"usage":null}}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}`,
		"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"city\": \"x\"}"}`,
		"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"delta":""}`,
		"response.completed", `{"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"completed"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
	)
	require.Contains(t, out, `event: message_start`, "message_start 首帧")
	require.Contains(t, out, `"input_tokens":0`, "usage.input_tokens 完成前不可知 → 0")
	require.Contains(t, out, `"content_block":{"text":"","type":"text"}`, "文本块惰性 start")
	require.Contains(t, out, `"delta":{"text":"hi","type":"text_delta"}`, "文本 delta")
	require.Contains(t, out, `"content_block":{"id":"call_1","input":{},"name":"get_weather","type":"tool_use"}`, "tool_use 块 id = call_id（M-1）")
	require.Contains(t, out, `"delta":{"partial_json":"{\"city\": \"x\"}","type":"input_json_delta"}`, "json delta")
	require.Contains(t, out, `event: content_block_stop`+"\n"+`data: {"index":1,"type":"content_block_stop"}`, "tool_use 块 stop")
	require.Contains(t, out, `"stop_reason":"tool_use"`, "stop_reason 映射")
	require.Contains(t, out, `"usage":{"output_tokens":5}`, "message_delta 用量")
	require.Contains(t, out, `event: message_stop`, "message_stop 收尾")
}

func TestMapMessToRespStream(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertRespToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":3}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\": \"x\"}"}}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	require.Contains(t, out, `event: response.created`+"\n"+`data: {"response":{"created_at":0,"id":"msg_1","model":"claude-3-5-sonnet","object":"response","output":[],"parallel_tool_calls":true,"status":"in_progress","usage":null},"type":"response.created"}`, "response.created 首帧")
	require.Contains(t, out, `"item_id":"msg_msg_1_0"`, "合成 item id（message 块）")
	require.Contains(t, out, `"content_index":0,"delta":"hel","item_id":"msg_msg_1_0","output_index":0,"type":"response.output_text.delta"`)
	require.Contains(t, out, `"call_id":"toolu_1","id":"fc_toolu_1","name":"get_weather","status":"in_progress","type":"function_call"`, "function_call 项")
	require.Contains(t, out, `"delta":"{\"city\": \"x\"}","item_id":"fc_toolu_1","output_index":1,"type":"response.function_call_arguments.delta"`)
	// response.completed：累积输出 + 用量
	require.Contains(t, out, `event: response.completed`)
	require.Contains(t, out, `"arguments":"{\"city\": \"x\"}"`, "arguments 累积")
	require.Contains(t, out, `"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":20,"total_tokens":30`, "用量累积")
	require.Contains(t, out, `"cached_tokens":3`, "cache_read 映射")
	require.Contains(t, out, `"status":"completed"`, "终态项")
}

func TestMapMessToChatStream(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertChatToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":3}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\": \"x\"}"}}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	require.Contains(t, out, `"delta":{"content":"","role":"assistant"}`, "角色前导")
	require.Contains(t, out, `"delta":{"content":"hi"}`)
	require.Contains(t, out, `"tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"toolu_1","index":1,"type":"function"}]`)
	require.Contains(t, out, `"tool_calls":[{"function":{"arguments":"{\"city\": \"x\"}"},"index":1}]`)
	require.Contains(t, out, `"finish_reason":"tool_calls"`, "tool_use → tool_calls")
	require.Contains(t, out, `"usage":{"completion_tokens":20,"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":3},"total_tokens":30}`, "用量累积（input 来自 message_start）")
	require.Contains(t, out, `"prompt_tokens_details":{"cached_tokens":3}`)
	require.Contains(t, out, "data: [DONE]")
	require.NotContains(t, out, `"message_stop"`, "message_stop 丢弃（收尾已在 message_delta）")
}

// --- P3：缺 event: 名（data-only）帧不丢 ---

// TestMapDataOnlyFramesInferred 缺名帧带 type 字段 → 按 data.type 推断事件名，
// 与具名帧同分派（fakeupstream /v1/responses 形态：只发 data: 行，无 event: 行）。
func TestMapDataOnlyFramesInferred(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertChatToResp,
		"", `{"type":"response.created","response":{"id":"rsp_1","object":"response","status":"in_progress","model":"m","output":[]}}`,
		"", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}`,
		"", `{"type":"response.completed","response":{"id":"rsp_1","object":"response","status":"completed","model":"m","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
	)
	require.Contains(t, out, `"delta":{"content":"","role":"assistant"}`, "created 推断 → 角色前导 chunk")
	require.Contains(t, out, `"delta":{"content":"hi"}`, "文本 delta 推断 → content chunk")
	require.Contains(t, out, `"usage":{"completion_tokens":5,"prompt_tokens":3,"total_tokens":8}`, "completed 推断 → 收尾 chunk 内联用量")
	require.Contains(t, out, "data: [DONE]", "completed 推断 → [DONE] 收尾")
}

// TestMapDataOnlyFramesInferredMessToResp 缺名帧推断对 anthropic 模板方向同样生效
// （anthropic 帧的 type 字段亦与事件名同值）。
func TestMapDataOnlyFramesInferredMessToResp(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertRespToMess,
		"", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
		"", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"", `{"type":"message_stop"}`,
	)
	require.Contains(t, out, `event: response.created`, "message_start 推断 → response.created")
	require.Contains(t, out, `"delta":"hi"`, "text_delta 推断 → output_text.delta")
	require.Contains(t, out, `event: response.completed`, "message_stop 推断 → response.completed")
}

// TestMapDataOnlyFramesInferredFallback 缺名帧 type 非首键（帧首 `{"type":`
// 锚定不匹配）→ 回退全量解码推断（共享 sserelay.InferEventName 的回退路径在
// protoconv 侧同样生效——转换流用例零回归）。
func TestMapDataOnlyFramesInferredFallback(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertChatToResp,
		"", `{"response":{"id":"rsp_1","object":"response","status":"in_progress","model":"m","output":[]},"type":"response.created"}`,
		"", `{"type":"response.completed","response":{"id":"rsp_1","object":"response","status":"completed","model":"m","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
	)
	require.Contains(t, out, `"delta":{"content":"","role":"assistant"}`, "非首键 type 回退推断 → 角色前导 chunk")
	require.Contains(t, out, `"usage":{"completion_tokens":5,"prompt_tokens":3,"total_tokens":8}`, "completed 推断 → 收尾 chunk 内联用量")
	require.Contains(t, out, "data: [DONE]", "completed 推断 → [DONE] 收尾")
}

// TestMapDataOnlyFramesPassthrough 缺名帧无法推断（非 JSON / 无 type 字段）
// → 原样透传 data 帧保留字节（不静默丢弃，P3）。
func TestMapDataOnlyFramesPassthrough(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertChatToResp)

	frame, drop := m.Map("", []byte("[DONE]"))
	require.False(t, drop, "无法推断的缺名帧不得丢弃")
	require.Equal(t, "data: [DONE]\n\n", string(frame), "data 帧原样透传")

	frame, drop = m.Map("", []byte(`{"unknown":1}`))
	require.False(t, drop, "JSON 但无 type 字段 → 透传")
	require.Equal(t, "data: {\"unknown\":1}\n\n", string(frame))

	// 多行 payload 逐行重建 data: 前缀（SSE 规范连续 data: 行以 \n 连接）
	frame, drop = m.Map("", []byte("a\nb"))
	require.False(t, drop)
	require.Equal(t, "data: a\ndata: b\n\n", string(frame))

	// 空帧（连续空行）：无字节可透传，保持丢弃
	_, drop = m.Map("", nil)
	require.True(t, drop)
}

// TestMapNamedFramesBehaviorUnchanged 具名帧行为不变：已知事件正常映射，
// 具名但未映射事件（in_progress 等）仍按映射表丢弃。
func TestMapNamedFramesBehaviorUnchanged(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertChatToResp)

	_, drop := m.Map("response.in_progress", []byte(`{"type":"response.in_progress"}`))
	require.True(t, drop, "具名但未映射事件仍丢弃（行为不变）")

	frame, drop := m.Map("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"x"}`))
	require.False(t, drop)
	require.Contains(t, string(frame), `"delta":{"content":"x"}`, "具名事件映射不变")
}

// --- M-1：工具调用匹配键（call_id 优先）多轮链路 ---

// respWithFC 构造含 function_call{id, call_id} 的 resp 响应 JSON（id 与 call_id
// 不同值——真实上游即如此，匹配键必须是 call_id）。
func respWithFC(id, callID string) []byte {
	return []byte(`{
		"id": "rsp_1", "object": "response", "created_at": 1750000000, "status": "completed", "model": "gpt-4o",
		"output": [
			{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
			 "content": [{"type": "output_text", "text": "ok", "annotations": []}]},
			{"id": "` + id + `", "type": "function_call", "call_id": "` + callID + `", "name": "get_weather", "arguments": "{}", "status": "completed"}
		],
		"usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}
	}`)
}

// TestToolCallChainRespToChatRoundTrip（M-1）：上游 function_call{id 与 call_id
// 不同} → 客户端侧工具 ID = call_id → 客户端回传 tool 消息 → function_call_
// output.call_id 与上游一致。两轮工具调用断言链路不随轮次断裂。
func TestToolCallChainRespToChatRoundTrip(t *testing.T) {
	for _, round := range []struct{ id, callID string }{
		{"fc_1", "call_1"},
		{"fc_2", "call_2"}, // 第二轮：再次确认匹配键稳定
	} {
		// 上游 resp 响应 → chat：tool_call id = call_id（非 item id）
		out, err := ConvertResponse(respWithFC(round.id, round.callID), domain.ProtocolConvertChatToResp)
		require.NoError(t, err)
		m := obj(t, out)
		msg := arrOf(t, m, "choices")[0].(map[string]any)["message"].(map[string]any)
		tc := arrOf(t, msg, "tool_calls")[0].(map[string]any)
		require.Equal(t, round.callID, tc["id"], "客户端工具调用 ID = call_id（匹配键）")
		require.NotEqual(t, round.id, tc["id"], "不得泄漏 item id（fc_ 格式）")

		// 客户端回传（chat tool 消息 tool_call_id = 客户端收到的 ID）→ chat→resp：
		// function_call_output.call_id 与上游 call_id 一致
		req := []byte(`{"model":"gpt-4o","messages":[
			{"role":"assistant","content":"ok","tool_calls":[{"id":"` + round.callID + `","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"` + round.callID + `","content":"{\"temp\":20}"}
		]}`)
		out2, err := ConvertRequest(req, domain.ProtocolConvertChatToResp)
		require.NoError(t, err)
		input := arrOf(t, obj(t, out2), "input")
		require.Equal(t, "message", input[0].(map[string]any)["type"], "assistant 文本消息项")
		fc := input[1].(map[string]any)
		require.Equal(t, "function_call", fc["type"])
		require.Equal(t, round.callID, fc["call_id"], "function_call 项 call_id 与上游一致")
		fco := input[2].(map[string]any)
		require.Equal(t, "function_call_output", fco["type"])
		require.Equal(t, round.callID, fco["call_id"], "function_call_output.call_id 与上游一致（链路闭合）")
	}
}

// TestToolCallChainStreamingCallID（M-1 流式）：resp output_item.added（流式）
// → chat tool_calls 前导 id = call_id；→ anthropic tool_use 块 id = call_id。
func TestToolCallChainStreamingCallID(t *testing.T) {
	item := `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}`

	// resp → chat 流式
	out := mapAll(t, domain.ProtocolConvertChatToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_1","object":"response","status":"in_progress","model":"m","output":[]}}`,
		"response.output_item.added", item,
	)
	require.Contains(t, out, `"id":"call_1","index":1,"type":"function"`, "chat tool_calls id = call_id")
	require.NotContains(t, out, `"id":"fc_1"`, "不泄漏 item id")

	// resp → mess 流式
	out2 := mapAll(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_1","object":"response","status":"in_progress","model":"m","output":[]}}`,
		"response.output_item.added", item,
	)
	require.Contains(t, out2, `"content_block":{"id":"call_1","input":{},"name":"get_weather","type":"tool_use"}`, "tool_use 块 id = call_id")
}

// TestToolCallChainRespToMessRoundTrip（M-1 resp→mess）：上游 function_call →
// anthropic tool_use.id = call_id（非流式 + 请求输入方向），客户端 tool_result
// 回传 → function_call_output.call_id 与上游一致。
func TestToolCallChainRespToMessRoundTrip(t *testing.T) {
	// 非流式响应：tool_use id = call_id
	out, err := ConvertResponse(respWithFC("fc_1", "call_1"), domain.ProtocolConvertMessToResp)
	require.NoError(t, err)
	content := arrOf(t, obj(t, out), "content")
	tu := content[1].(map[string]any)
	require.Equal(t, "tool_use", tu["type"])
	require.Equal(t, "call_1", tu["id"], "tool_use.id = call_id（tool_result.tool_use_id 匹配键）")
	require.NotEqual(t, "fc_1", tu["id"])

	// 请求输入方向：resp input function_call{id≠call_id} → tool_use.id = call_id
	req := []byte(`{"model":"gpt-4o","input":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"{\"temp\":20}"}
	]}`)
	out2, err := ConvertRequest(req, domain.ProtocolConvertRespToMess)
	require.NoError(t, err)
	msgs := arrOf(t, obj(t, out2), "messages")
	assistant := msgs[0].(map[string]any)
	tu2 := arrOf(t, assistant, "content")[1].(map[string]any)
	require.Equal(t, "call_1", tu2["id"], "请求方向 tool_use.id = call_id（多轮链闭合）")
	tr := arrOf(t, msgs[1].(map[string]any), "content")[0].(map[string]any)
	require.Equal(t, "call_1", tr["tool_use_id"], "tool_result.tool_use_id = call_id，与 tool_use.id 命中")

	// 反向：tool_use{id="toolu_1"} → mess→resp function_call.call_id = "toolu_1"（合成匹配键）
	out3, err := ConvertResponse([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"x"}}],
		"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`),
		domain.ProtocolConvertRespToMess)
	require.NoError(t, err)
	fc := arrOf(t, obj(t, out3), "output")[0].(map[string]any)
	require.Equal(t, "toolu_1", fc["call_id"], "mess→resp function_call.call_id = tool_use id（匹配键保真）")
}

// TestConvertRequestChatToMessBothMaxTokens（M-2）：max_completion_tokens 与
// max_tokens 同时提供时 max_completion_tokens 优先（与 chatToResp 同语义）。
func TestConvertRequestChatToMessBothMaxTokens(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":200,"max_completion_tokens":300}`),
		domain.ProtocolConvertChatToMess)
	require.NoError(t, err)
	require.Equal(t, float64(300), obj(t, out)["max_tokens"], "max_completion_tokens 优先于 max_tokens（M-2）")
}

func TestConvertRequestUnsupportedDirection(t *testing.T) {
	_, err := ConvertRequest([]byte(`{}`), domain.ProtocolConvertOff)
	require.ErrorContains(t, err, "unsupported direction")
}

func TestEncodeFrame(t *testing.T) {
	f := EncodeFrame("ev", map[string]any{"a": 1})
	require.Equal(t, "event: ev\ndata: {\"a\":1}\n\n", string(f))
	f = EncodeFrame("", "x")
	require.Equal(t, "data: \"x\"\n\n", string(f))
}

// --- 评审回归（2026-08-11 protoconv-opt-review）：完整帧/纯工具/多部件/数组 content ---

// TestMapRespToChatCompleteFrames（评审 M-1 回归）：完整帧字节断言——每帧带
// `data: ` 前缀与空行终止；completed 的 [DONE] 独立成帧（不粘连）。
func TestMapRespToChatCompleteFrames(t *testing.T) {
	m := NewStreamMapper(domain.ProtocolConvertChatToResp)

	frame, drop := m.Map("response.created", []byte(`{"type":"response.created","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"in_progress","model":"gpt-4o","output":[],"usage":null}}`))
	require.False(t, drop)
	require.Equal(t, "data: {\"choices\":[{\"delta\":{\"content\":\"\",\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}],\"created\":1750000000,\"id\":\"rsp_1\",\"model\":\"gpt-4o\",\"object\":\"chat.completion.chunk\"}\n\n", string(frame), "chunk 帧 = data: 前缀 + 载荷 + 空行终止")

	frame, drop = m.Map("response.output_text.delta", []byte(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hel"}`))
	require.False(t, drop)
	require.Equal(t, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"},\"finish_reason\":null,\"index\":0}],\"created\":1750000000,\"id\":\"rsp_1\",\"model\":\"gpt-4o\",\"object\":\"chat.completion.chunk\"}\n\n", string(frame))

	frame, drop = m.Map("response.completed", []byte(`{"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"gpt-4o","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"completed"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
	require.False(t, drop)
	require.Equal(t, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\",\"index\":0}],\"created\":1750000000,\"id\":\"rsp_1\",\"model\":\"gpt-4o\",\"object\":\"chat.completion.chunk\",\"usage\":{\"completion_tokens\":5,\"prompt_tokens\":3,\"total_tokens\":8}}\n\ndata: [DONE]\n\n", string(frame), "completed = chunk 帧 + [DONE] 独立帧（不粘连）")

	// failed → data-only 错误帧（新 mapper：completed 后 done 守卫已激活）
	m2 := NewStreamMapper(domain.ProtocolConvertChatToResp)
	frame, drop = m2.Map("response.failed", []byte(`{"type":"response.failed","response":{"id":"rsp_1","object":"response","status":"failed","error":{"code":"server_error","message":"boom"}}}`))
	require.False(t, drop)
	require.Equal(t, "data: {\"error\":{\"message\":\"boom\"}}\n\n", string(frame))
}

// TestConvertResponseRespToChatPureTool（评审 M-2 回归）：纯工具响应（无文本
// 部件）→ content 恒为合法空字符串。
func TestConvertResponseRespToChatPureTool(t *testing.T) {
	out, err := ConvertResponse([]byte(`{"id":"rsp_1","object":"response","status":"completed","model":"m","output":[{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}","status":"completed"}]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out) // 完整 JSON 合法
	msg := arrOf(t, m, "choices")[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "", msg["content"], "无文本部件 → content 空字符串")
	tcs := arrOf(t, msg, "tool_calls")
	require.Len(t, tcs, 1)
	require.Equal(t, "call_1", tcs[0].(map[string]any)["id"], "tool_call id = call_id（M-1）")
}

// TestConvertResponseRespToChatMultiPartText（评审 M-2 回归）：多部件文本 →
// 拼接单字符串（含转义引号与 \uXXXX 多字节部件边界）。
func TestConvertResponseRespToChatMultiPartText(t *testing.T) {
	out, err := ConvertResponse([]byte(`{"id":"rsp_1","object":"response","status":"completed","model":"m","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hel"},{"type":"output_text","text":"lo \"world\" 世界"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	msg := arrOf(t, m, "choices")[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "hello \"world\" 世界", msg["content"], "多部件文本拼接")
}

// TestConvertRequestChatToRespArrayContent（评审 M-3 回归）：system/tool 数组
// content → 文本 \n 拼接且整体合法 JSON（含转义部件）。
func TestConvertRequestChatToRespArrayContent(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": [{"type": "text", "text": "rule A"}, {"type": "text", "text": "rule \"B\""}]},
			{"role": "user", "content": "hi"},
			{"role": "tool", "tool_call_id": "call_1", "content": [{"type": "text", "text": "{\"temp\": 20}"}]}
		]
	}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	input := arrOf(t, m, "input")
	require.Len(t, input, 3)
	dev := input[0].(map[string]any)
	require.Equal(t, "developer", dev["role"])
	dc := arrOf(t, dev, "content")[0].(map[string]any)
	require.Equal(t, "rule A\nrule \"B\"", dc["text"], "数组 content 文本以 \n 拼接")
	fco := input[2].(map[string]any)
	require.Equal(t, "{\"temp\": 20}", fco["output"], "tool 数组 content 文本")
}

// TestConvertRequestChatToRespArrayContentEmptyFirst（评审 I-2 回归）：text 块
// 数组首部件为空字符串 → 前导 \n 分隔符仍保留（joinStrings("\n") 语义；
// 旧实现按 joined 长度判空丢分隔符）。
func TestConvertRequestChatToRespArrayContentEmptyFirst(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":""},{"type":"text","text":"rule"}]}]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	input := arrOf(t, m, "input")
	require.Len(t, input, 1)
	dc := arrOf(t, input[0].(map[string]any), "content")[0].(map[string]any)
	require.Equal(t, "\nrule", dc["text"], "空首部件后仍保留 \n 分隔符")
}

// TestConvertRequestChatToRespNullBody（评审 I-2 对齐）：null 体 → 空对象输出
// （与 map 版 decodeObj(null) → nil map → {} 一致）。
func TestConvertRequestChatToRespNullBody(t *testing.T) {
	out, err := ConvertRequest([]byte("null"), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	require.Equal(t, "{}", string(out), "null 体 → 空对象")
}

// TestConvertResponseNullBody（评审 I-2 对齐）：null 体 → 零值字段合法输出。
func TestConvertResponseNullBody(t *testing.T) {
	out, err := ConvertResponse([]byte("null"), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "", m["id"], "null 体 → 零值字段")
	require.Equal(t, float64(0), m["created"])
}

// TestConvertRequestChatToRespToolCallDoubleID（评审 I-3 对齐）：请求方向
// tool_call 同时含 id+call_id → 取 id（与 map 版一致；call_id 优先仅响应方向）。
func TestConvertRequestChatToRespToolCallDoubleID(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":"ok","tool_calls":[{"id":"call_id_1","call_id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	out, err := ConvertRequest(body, domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	input := arrOf(t, obj(t, out), "input")
	require.Len(t, input, 2)
	fc := input[1].(map[string]any)
	require.Equal(t, "call_id_1", fc["id"], "请求方向取 id")
	require.Equal(t, "call_id_1", fc["call_id"])
}

// TestConvertRequestChatToRespEscapedKey（评审 I-1 重做回归）：真实 \uXXXX
// 转义键（raw 恒比字面量长，长度前置检查会短路转义分支——此处用真实转义
// 字符构造，非明文假用例）。
func TestConvertRequestChatToRespEscapedKey(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","\u006d\u0065ssages":[{"role":"user","content":"hi"}]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	input := arrOf(t, m, "input")
	require.Len(t, input, 1, "转义键 messages 仍命中")
	require.Equal(t, "user", input[0].(map[string]any)["role"])
}

// TestConvertRequestChatToRespEscapedValue（评审 I-1 重做回归）：role 值含真实
// \uXXXX 转义 → 仍按 system 处理（产生 developer 消息项）。
func TestConvertRequestChatToRespEscapedValue(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"model":"m","messages":[{"role":"syst\u0065m","content":"rules"}]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	input := arrOf(t, m, "input")
	require.Len(t, input, 1, "转义 role 值仍按 system 处理")
	require.Equal(t, "developer", input[0].(map[string]any)["role"])
}

// TestMapRespToChatEscapedType（评审 I-1 重做回归，流式方向）：item.type 值含
// 真实转义 → output_item.added 仍按 function_call 产生 tool_calls 前导 chunk。
func TestMapRespToChatEscapedType(t *testing.T) {
	out := mapAll(t, domain.ProtocolConvertChatToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_1","object":"response","status":"in_progress","model":"m","output":[]}}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_c\u0061ll","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}`,
	)
	require.Contains(t, out, `"tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"call_1","index":1,"type":"function"}]`, "转义 type 值仍按 function_call 处理")
}
