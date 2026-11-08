// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// chatToRespConvert 走公开入口（client=Responses、upstream=chat 的响应方向）。
func chatToRespConvert(t *testing.T, body string) []byte {
	t.Helper()
	out, err := ConvertResponse([]byte(body), domain.ProtocolConvertRespToChat)
	require.NoError(t, err)
	return out
}

// TestChatToRespEmptyOutputIsEmptyArray 钉住 I6：chat 响应里没有文本、没有
// reasoning、没有 tool_calls 时，Responses 的 `output` 必须**恰好是字面量 `[]`**。
//
// 反例（修复前）：`output` 用的是 nil `[]any`，json.Marshal 后是 `null`。spec
// 规定 Responses 的 `output` 是数组，官方 SDK 与客户端按数组反序列化，`null`
// 直接是协议错。
func TestChatToRespEmptyOutputIsEmptyArray(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no_choices_key", `{"id":"c1","model":"m"}`},
		{"empty_choices", `{"id":"c1","model":"m","choices":[]}`},
		{"null_choices", `{"id":"c1","model":"m","choices":null}`},
		{"choice_not_object", `{"id":"c1","model":"m","choices":["x"]}`},
		{"empty_message", `{"id":"c1","model":"m","choices":[{"index":0,"message":{}}]}`},
		{"no_message_key", `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"stop"}]}`},
		{"null_message", `{"id":"c1","model":"m","choices":[{"index":0,"message":null}]}`},
		{"empty_content", `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""}}]}`},
		{"null_content", `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null}}]}`},
		{"empty_tool_calls", `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[]}}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := chatToRespConvert(t, c.body)
			out := gjson.GetBytes(got, "output")
			require.True(t, out.Exists(), "output 必须存在：%s", got)
			require.True(t, out.IsArray(), "output 必须是数组，实际原始字节：%q（整条：%s）", out.Raw, got)
			require.Equal(t, "[]", out.Raw, "空响应的 output 必须恰好是 []，不可为 null")
			require.NotContains(t, string(got), `"output":null`)
		})
	}
}

// TestChatToRespFunctionCallArgumentsVerbatim 钉住「arguments 原样字符串」：
// Responses 的 function_call.arguments 直接透传 chat 源的
// `tool_calls[].function.arguments` 字符串，**包括空串**；缺失（或无字符串可
// 透传）→ 空字符串 `""`，绝不编造成 `{}`（spec §5.2 + §7「不编造 arguments」）。
func TestChatToRespFunctionCallArgumentsVerbatim(t *testing.T) {
	wrap := func(args string) string {
		return `{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f"` + args + `}}]}}]}`
	}
	cases := []struct{ name, body, want string }{
		// 正常 JSON 字符串：原样，不重新序列化、不压缩空白。
		{"verbatim", wrap(`, "arguments":"{\"a\":1}"`), `{"a":1}`},
		{"verbatim_with_spaces", wrap(`, "arguments":" { \"a\" : 1 } "`), ` { "a" : 1 } `},
		{"verbatim_non_object_json", wrap(`, "arguments":"[1,2]"`), `[1,2]`},
		{"verbatim_malformed", wrap(`, "arguments":"{not json"`), `{not json`},
		// 空串：原样透传空串（修复前被改写成 `{}`）。
		{"empty_string", wrap(`, "arguments":""`), ``},
		// 缺失：空串（修复前被写成 `{}`）。
		{"missing", wrap(``), ``},
		{"null", wrap(`, "arguments":null`), ``},
		{"not_string", wrap(`, "arguments":123`), ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := chatToRespConvert(t, c.body)
			out := gjson.GetBytes(got, "output")
			require.True(t, out.IsArray(), "output 必须是数组：%s", got)
			require.Len(t, out.Array(), 1, "只应有一个 function_call 项：%s", got)
			item := out.Array()[0]
			require.Equal(t, "function_call", item.Get("type").Str)
			require.Equal(t, "call_1", item.Get("call_id").Str)
			require.Equal(t, "f", item.Get("name").Str)
			// 键**必须在场**且值等于字面量 want（空串写 ""，不是缺键、不是 {}）。
			args := item.Get("arguments")
			require.True(t, args.Exists(), "arguments 键必须在场：%s", got)
			require.Equal(t, c.want, args.Str, "arguments 必须原样透传：%s", got)
			if c.want == "" {
				require.NotEqual(t, "{}", args.Str, "缺失/空 arguments 不得被改写成 {}")
			}
		})
	}
}

// TestChatToRespOutputArrayShapeNonEmpty 对照组：非空响应里 output 同样是数组，
// 防止「恒为 []」的修法误伤正常路径（顺序 reasoning → message → function_call）。
func TestChatToRespOutputArrayShapeNonEmpty(t *testing.T) {
	got := chatToRespConvert(t, `{"id":"c2","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"calling","reasoning_content":"think","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	out := gjson.GetBytes(got, "output")
	require.True(t, out.IsArray(), "output 必须是数组：%s", got)
	require.Len(t, out.Array(), 3)
	require.Equal(t, []string{"reasoning", "message", "function_call"}, []string{
		out.Array()[0].Get("type").Str,
		out.Array()[1].Get("type").Str,
		out.Array()[2].Get("type").Str,
	})
	require.Equal(t, `{"a":1}`, out.Array()[2].Get("arguments").Str)
}
