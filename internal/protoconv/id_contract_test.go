// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// id_contract_test.go 把「Responses 项的 id 契约」钉成可执行检查，而不是散落
// 在各方向的手写期望里：
//
//   - 请求：官方 schema 的 FunctionToolCall required 只有 type/call_id/name/
//     arguments —— item id 可选，出现时须 `fc_` 开头。转换器因此一律**省略** id，
//     只写配对键 call_id（chat 的 tool_calls[].id / Messages 的 tool_use.id）。
//   - 响应：输出类型的 id 是 **Optional**（官方 `ResponseFunctionToolCall` 就是
//     `id: Optional[str]`；`FunctionCallItemResource` 属于 agent 资源模型、带
//     turn_id，不能当输出 schema 引用）。转换器仍合成 id 的理由只有两条：流内
//     身份要可引用（added/delta/done/completed 共用同一 id），且唯一被观察到的
//     上游规则是「出现即须 `fc_` 前缀」。流式与非流式同源（`fc_` + call_id，
//     call id 迟到时用响应 id + 内容 index）。
//   - 流内身份：同一 output_index 的 added / delta / done / completed 必须共用
//     一个 item id（客户端按 item_id 归并分片）。
//
// 这些断言只看**结构**，不依赖具体方向的手写字节期望；golden 负责逐字节回归。

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// responsesFunctionCalls 返回 JSON 里指定路径的 function_call 项集合。
func responsesFunctionCalls(t *testing.T, raw []byte, path string) []gjson.Result {
	t.Helper()
	var out []gjson.Result
	gjson.GetBytes(raw, path).ForEach(func(_, item gjson.Result) bool {
		if item.IsObject() && item.Get("type").Str == "function_call" {
			out = append(out, item)
		}
		return true
	})
	return out
}

// TestRequestFunctionCallItemsOmitItemID chat/mess 两条请求方向都必须：
// 不写 function_call.id（可选字段，写上须 fc_ 前缀，而源 id 是 call id），
// 但 call_id/name/arguments 三个必填字段齐全。
func TestRequestFunctionCallItemsOmitItemID(t *testing.T) {
	cases := []struct {
		name     string
		dir      domain.ProtocolConvert
		body     string
		wantCall string
	}{
		{
			name: "chat_to_resp", dir: domain.ProtocolConvertChatToResp, wantCall: "call_1",
			body: `{"model":"m","messages":[
				{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`,
		},
		{
			name: "mess_to_resp", dir: domain.ProtocolConvertMessToResp, wantCall: "toolu_1",
			body: `{"model":"m","max_tokens":1,"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ConvertRequest([]byte(tc.body), tc.dir)
			require.NoError(t, err)

			calls := responsesFunctionCalls(t, out, "input")
			require.Len(t, calls, 1, "必须产出一个 function_call")
			fc := calls[0]
			require.False(t, fc.Get("id").Exists(),
				"请求方向不得写 function_call.id（源 id 是 call id，前缀必然不是 fc_）")
			require.Equal(t, tc.wantCall, fc.Get("call_id").Str, "配对键必须原样保留")
			require.Equal(t, "f", fc.Get("name").Str)
			require.True(t, fc.Get("arguments").Exists(), "arguments 必填")

			// function_call_output：id 由 API 生成（输入可省），我们恒不写；
			// call_id 必填。
			var outs int
			gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").Str != "function_call_output" {
					return true
				}
				outs++
				require.False(t, item.Get("id").Exists(), "function_call_output 不写 id")
				require.Equal(t, tc.wantCall, item.Get("call_id").Str)
				require.True(t, item.Get("output").Exists(), "output 必填")
				return true
			})
			require.Equal(t, 1, outs)
		})
	}
}

// TestResponseFunctionCallItemIDContract 两条响应方向（非流式 + 流式）：
// function_call.id 必须是 `fc_` + call_id，且同一次响应里流式与非流式给出同一个
// id、流内所有事件引用同一个 item id。
func TestResponseFunctionCallItemIDContract(t *testing.T) {
	t.Run("chat_upstream", func(t *testing.T) {
		const chatReply = `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
		out, err := ConvertResponse([]byte(chatReply), domain.ProtocolConvertRespToChat)
		require.NoError(t, err)
		calls := responsesFunctionCalls(t, out, "output")
		require.Len(t, calls, 1)
		require.Equal(t, "fc_call_1", calls[0].Get("id").Str, "非流式 item id = fc_ + call_id")
		require.Equal(t, "call_1", calls[0].Get("call_id").Str)

		// 流式：同一 call id 的 added/delta/done/completed 必须是同一个 id。
		frames := chatStreamRun(t, domain.ProtocolConvertRespToChat, true,
			`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		)
		ids := map[string]bool{}
		for _, f := range frames {
			switch f.event {
			case "response.output_item.added", "response.output_item.done":
				ids[f.data.Get("item.id").Str] = true
			case "response.function_call_arguments.delta":
				ids[f.data.Get("item_id").Str] = true
			case "response.completed":
				gjson.Get(f.data.Raw, "response.output").ForEach(func(_, item gjson.Result) bool {
					if item.Get("type").Str == "function_call" {
						ids[item.Get("id").Str] = true
					}
					return true
				})
			}
		}
		require.Equal(t, map[string]bool{"fc_call_1": true}, ids,
			"流内所有 function_call 引用必须收敛到同一个 fc_ id（与非流式一致）")
	})

	t.Run("mess_upstream", func(t *testing.T) {
		const messReply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
		out, err := ConvertResponse([]byte(messReply), domain.ProtocolConvertRespToMess)
		require.NoError(t, err)
		calls := responsesFunctionCalls(t, out, "output")
		require.Len(t, calls, 1)
		require.Equal(t, "fc_toolu_1", calls[0].Get("id").Str)
		require.Equal(t, "toolu_1", calls[0].Get("call_id").Str)

		m := NewStreamMapper(domain.ProtocolConvertRespToMess)
		var raw []byte
		for _, ev := range [][2]string{
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			frame, drop := m.Map(ev[0], []byte(ev[1]))
			if !drop {
				raw = append(raw, frame...)
			}
		}
		raw = append(raw, m.Finish()...)

		ids := map[string]bool{}
		for _, f := range parseSSEFrames(t, raw) {
			switch f.event {
			case "response.output_item.added", "response.output_item.done":
				if item, ok := f.data["item"].(map[string]any); ok && item["type"] == "function_call" {
					ids[strAny(item["id"])] = true
				}
			case "response.function_call_arguments.delta":
				ids[strAny(f.data["item_id"])] = true
			case "response.completed":
				if resp, ok := f.data["response"].(map[string]any); ok {
					if output, ok := resp["output"].([]any); ok {
						for _, it := range output {
							if item, ok := it.(map[string]any); ok && item["type"] == "function_call" {
								ids[strAny(item["id"])] = true
							}
						}
					}
				}
			}
		}
		require.Equal(t, map[string]bool{"fc_toolu_1": true}, ids,
			"mess 上游的流式与非流式必须给出同一个 fc_ id")
	})
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}
