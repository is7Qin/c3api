// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// chat 的 `function.arguments` 是**装着 JSON 文本的字符串**（OpenAI 的
// `ChatCompletionMessageToolCall.Function.Arguments string`）。写成对象是最容易
// 犯也最容易漏的类型错误：上游会直接 400，而"看着像 JSON"的 golden 又看不出来。
// 本文件同时钉住 spec §13 E1（arguments 保留源表示）与 oracle 终审 C14
// （缺省/null input 必须产出字符串 `"{}"`，不是对象 `{}`）。

// chatToolArguments 取产物里第 idx 个 tool_call 的 arguments 原始值。
func chatToolArguments(t *testing.T, body []byte, idx int) gjson.Result {
	t.Helper()
	path := "messages.0.tool_calls." + strconv.Itoa(idx) + ".function.arguments"
	v := gjson.GetBytes(body, path)
	require.True(t, v.Exists(), "取不到 %s", path)
	return v
}

func TestMessChatToolArgumentsIsString(t *testing.T) {
	for _, tc := range []struct {
		name    string
		toolUse string
		wantStr string
		// wantRaw 产物里 `"arguments":` 后面**逐字节**该是什么。缺省/null
		// 必须是字符串字面量 `"{}"`；有 input 时是源 JSON 再包一层字符串。
		wantRaw string
	}{
		{"input_missing", `{"type":"tool_use","id":"a","name":"f"}`, `{}`, `"{}"`},
		{"input_null", `{"type":"tool_use","id":"a","name":"f","input":null}`, `{}`, `"{}"`},
		{"input_object_kept_verbatim", `{"type":"tool_use","id":"a","name":"f","input":{"a":1,"b":[2]}}`,
			`{"a":1,"b":[2]}`, `"{\"a\":1,\"b\":[2]}"`},
		// JSON 允许顶层是数组：照样原样搬进字符串（不因"不像对象"就换成 {}）。
		{"input_array_kept_verbatim", `{"type":"tool_use","id":"a","name":"f","input":[1,2]}`, `[1,2]`, `"[1,2]"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","messages":[{"role":"assistant","content":[` + tc.toolUse + `]}]}`)
			got, err := ConvertRequest(body, domain.ProtocolConvertMessToChat)
			require.NoError(t, err)

			args := chatToolArguments(t, got, 0)
			require.Equal(t, gjson.String, args.Type,
				"arguments 必须是 JSON 字符串，实际是 %v（原文 %s）", args.Type, args.Raw)
			require.Equal(t, tc.wantStr, args.Str, "arguments 解出来的文本")
			require.True(t, gjson.Valid(args.Str), "arguments 里的文本必须是合法 JSON：%s", args.Str)
			// 直接钉住产物的**原始字节**：arguments 的 JSON 类型是 string，
			// 报文里必须是 `"arguments":"…"` 而不是 `"arguments":…`。
			// （上游 json.loads 之后前者是 str、后者是 dict/array；后者直接 400。）
			require.Contains(t, string(got), `"arguments":`+tc.wantRaw,
				"arguments 的原始字节不符，实际报文：%s", got)
			require.NotContains(t, string(got), `"arguments":{`,
				"arguments 不得是对象（这会变成 dict 而不是 str）")
			require.NotContains(t, string(got), `"arguments":[`,
				"arguments 不得是数组（这会变成 list 而不是 str）")
		})
	}
}

func TestRespChatToolArgumentsNotDoubleEncoded(t *testing.T) {
	// resp 源里 arguments 已经是"装着 JSON 的字符串"⇒ 原样搬，不能再包一层。
	body := []byte(`{"model":"m","input":[` +
		`{"type":"function_call","call_id":"a","name":"f","arguments":"{\"a\":1}"}]}`)
	got, err := ConvertRequest(body, domain.ProtocolConvertRespToChat)
	require.NoError(t, err)

	args := chatToolArguments(t, got, 0)
	require.Equal(t, gjson.String, args.Type, "arguments 必须是字符串")
	require.Equal(t, `{"a":1}`, args.Str, "不得二次编码")
	require.NotEqual(t, `"{\"a\":1}"`, args.Str, "二次编码会让上游收到一个字符串而不是对象")
}
