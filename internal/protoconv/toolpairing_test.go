// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// 本文件钉住 spec §4.3 的多轮工具配对不变量（oracle 终审 C13）：
//
//  1. `tool_result` / `function_call_output` 必须能回指**紧邻的上一条 assistant**
//     声明的 call id，否则丢弃并计 `tool_result_orphan`（不合成对应 call，J11）；
//  2. 同一 assistant 的多个调用，其结果**按声明顺序**输出（源里 b,a → 产物 a,b），
//     且 tool 消息必须紧邻其 assistant、中间不得插 user 文本；
//  3. 同一 id 的重复结果只认第一条（第二条无从表达）。
//
// 这些用例直接断言 chat 产物的 `messages[].tool_call_id` 序列与丢弃计数，
// 不依赖 golden，故不受"golden 由实现自己产出"的影响。

// chatToolCallIDs 按出现顺序取产物里所有 role:"tool" 消息的 tool_call_id。
func chatToolCallIDs(t *testing.T, body []byte) []string {
	t.Helper()
	ids := []string{}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").Str == "tool" {
			ids = append(ids, m.Get("tool_call_id").Str)
		}
		return true
	})
	return ids
}

// chatToolContents 同上但取 content（用于验证结果没有串位）。
func chatToolContents(t *testing.T, body []byte) []string {
	t.Helper()
	out := []string{}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").Str == "tool" {
			out = append(out, m.Get("content").Str)
		}
		return true
	})
	return out
}

func TestMessChatToolResultPairing(t *testing.T) {
	asstAB := `{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"a","name":"f","input":{"x":1}},` +
		`{"type":"tool_use","id":"b","name":"g","input":{}}]}`

	for _, tc := range []struct {
		name     string
		messages string
		wantIDs  []string
		wantStr  []string
		drops    int64
	}{
		{
			// 没有前置 assistant 声明 ⇒ 孤儿，绝不能写成 role:"tool"。
			name:     "orphan_without_any_assistant",
			messages: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"v"}]}]`,
			wantIDs:  []string{},
			wantStr:  []string{},
			drops:    1,
		},
		{
			// 源顺序 b,a → 产物必须重排成声明顺序 a,b，且内容不串位。
			name: "reversed_source_order_is_reordered",
			messages: `[` + asstAB + `,{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"b","content":"rb"},` +
				`{"type":"tool_result","tool_use_id":"a","content":"ra"}]}]`,
			wantIDs: []string{"a", "b"},
			wantStr: []string{"ra", "rb"},
			drops:   0,
		},
		{
			// 非相邻（中间隔了一条 user 文本）⇒ 窗口已关，第二轮的孤儿必须丢。
			name: "not_adjacent_to_declaring_assistant",
			messages: `[` + asstAB + `,` +
				`{"role":"user","content":[{"type":"text","text":"hi"}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ra"}]}]`,
			wantIDs: []string{},
			wantStr: []string{},
			drops:   1,
		},
		{
			// 同一 id 两条结果：只认第一条。
			name: "duplicate_result_keeps_first",
			messages: `[` + asstAB + `,{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"a","content":"first"},` +
				`{"type":"tool_result","tool_use_id":"a","content":"second"}]}]`,
			wantIDs: []string{"a"},
			wantStr: []string{"first"},
			drops:   1,
		},
		{
			// 只有 b 有结果：a 不合成内容（J11），b 仍按声明顺序落位。
			name: "missing_one_result_does_not_invent",
			messages: `[` + asstAB + `,{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"b","content":"rb"}]}]`,
			wantIDs: []string{"b"},
			wantStr: []string{"rb"},
			drops:   0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetDropCounts()
			t.Cleanup(ResetDropCounts)
			body := []byte(`{"model":"m","messages":` + tc.messages + `}`)
			got, err := ConvertRequest(body, domain.ProtocolConvertMessToChat)
			require.NoError(t, err)
			require.Equal(t, tc.wantIDs, chatToolCallIDs(t, got), "tool 消息的 id 序列")
			require.Equal(t, tc.wantStr, chatToolContents(t, got), "tool 消息的 content 序列")
			require.Equal(t, tc.drops, DropCounts()["mess_to_chat|req|tool_result_orphan"],
				"孤儿/重复结果的丢弃计数")
		})
	}
}

// 结果先写完、user 文本排在它后面（§4.3 规则 2「中间不得插 user 文本」）。
func TestMessChatToolMessagesPrecedeUserText(t *testing.T) {
	ResetDropCounts()
	t.Cleanup(ResetDropCounts)
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"text","text":"and then?"},` +
		`{"type":"tool_result","tool_use_id":"a","content":"ra"}]}]}`)
	got, err := ConvertRequest(body, domain.ProtocolConvertMessToChat)
	require.NoError(t, err)

	roles := []string{}
	gjson.GetBytes(got, "messages").ForEach(func(_, m gjson.Result) bool {
		roles = append(roles, m.Get("role").Str)
		return true
	})
	require.Equal(t, []string{"assistant", "tool", "user"}, roles,
		"tool 消息必须紧跟 assistant，user 文本排在最后")
	require.Equal(t, "and then?", gjson.GetBytes(got, "messages.2.content.0.text").Str)
}

func TestRespChatToolOutputPairing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
		drops int64
	}{
		{
			// 逆序结果按声明顺序重排。
			name: "reversed_source_order_is_reordered",
			input: `[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},` +
				`{"type":"function_call","call_id":"b","name":"g","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"b","output":"rb"},` +
				`{"type":"function_call_output","call_id":"a","output":"ra"}]`,
			want:  []string{"a", "b"},
			drops: 0,
		},
		{
			// 中间隔了一条 user 消息 ⇒ 配对窗口关闭。
			name: "not_adjacent_to_declaring_assistant",
			input: `[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
				`{"type":"function_call_output","call_id":"a","output":"ra"}]`,
			want:  []string{},
			drops: 1,
		},
		{
			name:  "orphan_without_any_call",
			input: `[{"type":"function_call_output","call_id":"x","output":"v"}]`,
			want:  []string{},
			drops: 1,
		},
		{
			name: "duplicate_output_keeps_first",
			input: `[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},` +
				`{"type":"function_call_output","call_id":"a","output":"first"},` +
				`{"type":"function_call_output","call_id":"a","output":"second"}]`,
			want:  []string{"a"},
			drops: 1,
		},
		{
			// 被丢弃的未知 item 不产出任何 chat 消息 ⇒ 不该关闭配对窗口（spec §13 E7）。
			name: "dropped_unknown_item_keeps_window_open",
			input: `[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},` +
				`{"type":"web_search_call","id":"ws_1"},` +
				`{"type":"function_call_output","call_id":"a","output":"ra"}]`,
			want:  []string{"a"},
			drops: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetDropCounts()
			t.Cleanup(ResetDropCounts)
			body := []byte(`{"model":"m","input":` + tc.input + `}`)
			got, err := ConvertRequest(body, domain.ProtocolConvertRespToChat)
			require.NoError(t, err)
			require.Equal(t, tc.want, chatToolCallIDs(t, got), "tool 消息的 id 序列")
			require.Equal(t, tc.drops, DropCounts()["resp_to_chat|req|tool_result_orphan"],
				"孤儿/重复结果的丢弃计数")
		})
	}
}
