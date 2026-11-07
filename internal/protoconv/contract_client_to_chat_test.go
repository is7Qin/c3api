// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// 契约真源门禁（spec §8 a3 + m）：客户端 Mess/Resp → 上游 Chat 的四条新方向
// （两个请求面 mess_to_chat_req / resp_to_chat_req，两个响应面
// chat_to_mess_resp / chat_to_resp_resp）的"字段名 / 结构 / 字面量"独立真源。
//
// 独立性：期望值全部手写在 testdata/spec/contract_client_to_chat.tsv 里，取自
// spec §4/§5 的映射表 —— **不是**从实现反向采集的（否则门禁只是复述实现）。
// 断言一律按 dst_path **取值后**判形态与值，**不用字面 strings.Contains**：
//   - 同名键上 Contains 恒真（`model` 透传写错也照样"含 model"）；
//   - 嵌套路径上 Contains 恒红（`tools.0.function.parameters.type` 这种路径
//     永远不会作为整段字节出现在产物里）。
//
// 列（8 列，路径一律 gjson 语法）：
//
//	dir      内部内容模型方向名（= dirName，与 members.tsv/anchors.tsv 同词表）
//	scope    输入夹具名（= testdata/golden 的用例名，见 contractFixtures）
//	src_path 源侧取值路径（expect 为 equals:… 时与它比较；cond 用它做前置）
//	dst_path 产物侧取值路径（断言对象）
//	form     产物形态：string/number/array/object/bool/null/absent
//	cond     前置：src（源必须存在该路径）/ src_null（源存在且为 null）/ -（无前置）
//	expect   期望：字面量 | equals:<src_path> | len:<n> | raw:<JSON 文本> | absent
//	rule_id  规则名（必须在本文件里以 assertRule("<rule_id>", …) 的**实参**出现）
//
// 每条规则都要能红（改哪一行会红写在行注释里）：例如
// ct_mess_max_tokens_key_absent 在 `max_completion_tokens` 被写回 `max_tokens`
// 时红；ct_resp_text_format_nested_name 在 text.format 被扁平直译时红；
// ct_mess_tools_parameters_object 在 input_schema 原样透传时红。

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

const (
	// contractTSVPath 契约真源表。
	contractTSVPath = "testdata/spec/contract_client_to_chat.tsv"
	// contractSpecFile anchors.tsv 里指向契约表的 spec_file 值（spec §8(m)）。
	contractSpecFile = "contract_client_to_chat"
)

// contractRow 契约表一行。
type contractRow struct {
	dir     string
	scope   string
	srcPath string
	dstPath string
	form    string
	cond    string
	expect  string
	ruleID  string
	line    int // 1-based 物理行号（anchors.tsv 的 line 列指它）
}

// contractCase 一条规则运行时拿到的东西：表行 + 源 + 产物。
type contractCase struct {
	row contractRow
	in  gjson.Result
	out gjson.Result
}

// contractFixture 按 scope 选的输入夹具。resp=false → ConvertRequest（输入是
// 客户端请求体）；resp=true → ConvertResponse（输入是模板协议 = chat 的响应体）。
type contractFixture struct {
	conv domain.ProtocolConvert
	resp bool
	body string
}

// contractRows 读契约表（无表头：文件逐行即数据，与 members.tsv/anchors.tsv 同款）。
func contractRows(t *testing.T) []contractRow {
	t.Helper()
	raw, err := os.ReadFile(contractTSVPath)
	require.NoError(t, err)
	var out []contractRow
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		require.Len(t, f, 8,
			"契约表第 %d 行必须 8 列：dir/scope/src_path/dst_path/form/cond/expect/rule_id（%q）", i+1, line)
		out = append(out, contractRow{
			dir: f[0], scope: f[1], srcPath: f[2], dstPath: f[3],
			form: f[4], cond: f[5], expect: f[6], ruleID: f[7],
			line: i + 1,
		})
	}
	require.NotEmpty(t, out, "契约表不能是空表（空表 = 门禁空转）")
	return out
}

// contractFixtures 输入夹具。请求体逐字复制 testdata/golden 的输入（spec §8(c3)
// 要求"最小合法体复用 (a) 的 golden 输入体"），响应体是 chat 上游的非流式响应。
func contractFixtures() map[string]contractFixture {
	return map[string]contractFixture{
		// --- 请求面：Mess 客户端 → chat 上游 ---
		"mess_to_chat_req_text": {
			conv: domain.ProtocolConvertMessToChat,
			body: `{"model":"m","max_tokens":64,"system":"be nice","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"hi "},{"type":"text","text":"there"}]},{"role":"user","content":[{"type":"text","text":"again"}]}]}`,
		},
		"mess_to_chat_req_tools": {
			conv: domain.ProtocolConvertMessToChat,
			body: `{"model":"m","messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]},{"type":"text","text":"next"}]}],"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}],"tool_choice":{"type":"auto"},"stop_sequences":["STOP"]}`,
		},
		"mess_to_chat_req_image_system": {
			conv: domain.ProtocolConvertMessToChat,
			body: `{"model":"m","system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}},{"type":"text","text":"what"}]}],"stop_sequences":["a","b","c","d","e"],"temperature":0.7,"top_p":0.9,"metadata":{"user_id":"u1"},"stream":true,"stream_options":{"include_obfuscation":false}}`,
		},
		"mess_to_chat_req_multiturn": {
			conv: domain.ProtocolConvertMessToChat,
			body: `{"model":"m","max_tokens":256,"system":"sys","messages":[{"role":"user","content":"u1"},{"role":"assistant","content":[{"type":"text","text":"t1"},{"type":"tool_use","id":"a","name":"f","input":{"x":1}},{"type":"tool_use","id":"b","name":"g","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ra"},{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"rb"}]},{"role":"assistant","content":[{"type":"text","text":"t2"},{"type":"tool_use","id":"c","name":"f","input":{"x":2}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"rc"}]},{"type":"text","text":"u2"}]}],"tools":[{"name":"f","description":"df","input_schema":{"type":"object","properties":{"x":{"type":"integer"}}}},{"name":"g","description":"dg","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"stop_sequences":["END"],"metadata":{"user_id":"u9"}}`,
		},
		// --- 请求面：Responses 客户端 → chat 上游 ---
		"resp_to_chat_req_text": {
			conv: domain.ProtocolConvertRespToChat,
			body: `{"model":"m","instructions":"be nice","input":"hello","max_output_tokens":64}`,
		},
		"resp_to_chat_req_tools": {
			conv: domain.ProtocolConvertRespToChat,
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"web_search_call","id":"ws1","status":"completed"}],"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"integer"}}},"strict":true},{"type":"web_search_preview"}],"tool_choice":{"type":"function","name":"f"},"parallel_tool_calls":false}`,
		},
		"resp_to_chat_req_image_system": {
			conv: domain.ProtocolConvertRespToChat,
			body: `{"model":"m","instructions":"sys","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"what"},{"type":"input_image","image_url":"https://e.com/i.png"},{"type":"input_file","file_id":"f_1"}]}],"text":{"format":{"type":"json_schema","name":"S","schema":{"type":"object"},"strict":true}},"reasoning":{"effort":"xhigh"},"temperature":0.7,"top_p":0.9,"user":"u1","stream":true}`,
		},
		"resp_to_chat_req_multiturn": {
			conv: domain.ProtocolConvertRespToChat,
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"u1"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"type":"function_call","call_id":"a","name":"f","arguments":"{\"x\":1}"},{"type":"function_call","call_id":"b","name":"g","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"ra"},{"type":"function_call_output","call_id":"b","output":[{"type":"input_text","text":"rb"}]},{"type":"function_call_output","call_id":"ghost","output":"lost"},{"type":"message","role":"user","content":[{"type":"input_text","text":"u2"}]}]}`,
		},
		// --- 响应面：chat 上游 → Mess 客户端 ---
		"chat_to_mess_resp_text": {
			conv: domain.ProtocolConvertMessToChat, resp: true,
			body: `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`,
		},
		"chat_to_mess_resp_tools": {
			conv: domain.ProtocolConvertMessToChat, resp: true,
			body: `{"id":"c2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"calling","reasoning_content":"think","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},{"id":"c2","type":"function","function":{"name":"g","arguments":"{oops"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":7}}}`,
		},
		"chat_to_mess_resp_length": {
			conv: domain.ProtocolConvertMessToChat, resp: true,
			body: `{"id":"c3","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`,
		},
		// --- 响应面：chat 上游 → Responses 客户端 ---
		"chat_to_resp_resp_text": {
			conv: domain.ProtocolConvertRespToChat, resp: true,
			body: `{"id":"c1","object":"chat.completion","created":99,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`,
		},
		"chat_to_resp_resp_tools": {
			conv: domain.ProtocolConvertRespToChat, resp: true,
			body: `{"id":"c2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"calling","reasoning_content":"think","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":7}}}`,
		},
		"chat_to_resp_resp_incomplete": {
			conv: domain.ProtocolConvertRespToChat, resp: true,
			body: `{"id":"c3","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"trunc"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`,
		},
	}
}

// checkContractRow 按表行判产物：① cond 前置（防空转）② form 形态 ③ expect 值。
func checkContractRow(t *testing.T, r contractRow, in, out gjson.Result) {
	t.Helper()
	// ① 前置：源里必须真的有这个键，否则这一行在"验证一个没发生的事"
	switch r.cond {
	case "-":
	case "src":
		require.True(t, in.Get(r.srcPath).Exists(),
			"行 %d（%s）：夹具里没有源路径 %s ⇒ 该 rule 空转", r.line, r.ruleID, r.srcPath)
	case "src_null":
		v := in.Get(r.srcPath)
		require.True(t, v.Exists() && v.Type == gjson.Null,
			"行 %d（%s）：源路径 %s 必须存在且为 null（实际 %q）", r.line, r.ruleID, r.srcPath, v.Raw)
	default:
		t.Fatalf("行 %d：未知 cond %q", r.line, r.cond)
	}

	got := out.Get(r.dstPath)
	// ② 形态
	switch r.form {
	case "absent":
		require.False(t, got.Exists(), "行 %d（%s）：%s 不应出现在产物里（实际 %s）",
			r.line, r.ruleID, r.dstPath, got.Raw)
		require.Equal(t, "absent", r.expect, "行 %d：form=absent 的 expect 只能是 absent", r.line)
		return
	case "null":
		require.True(t, got.Exists(), "行 %d（%s）：%s 必须写出（null 也要有键）", r.line, r.ruleID, r.dstPath)
		require.Equal(t, gjson.Null, got.Type, "行 %d（%s）：%s 必须是 JSON null", r.line, r.ruleID, r.dstPath)
	case "string":
		require.Equal(t, gjson.String, got.Type, "行 %d（%s）：%s 必须是字符串（实际 %s）",
			r.line, r.ruleID, r.dstPath, got.Raw)
	case "number":
		require.Equal(t, gjson.Number, got.Type, "行 %d（%s）：%s 必须是数字（实际 %s）",
			r.line, r.ruleID, r.dstPath, got.Raw)
	case "bool":
		require.Contains(t, []gjson.Type{gjson.True, gjson.False}, got.Type,
			"行 %d（%s）：%s 必须是布尔（实际 %s）", r.line, r.ruleID, r.dstPath, got.Raw)
	case "array":
		require.True(t, got.IsArray(), "行 %d（%s）：%s 必须是数组（实际 %s）",
			r.line, r.ruleID, r.dstPath, got.Raw)
	case "object":
		require.True(t, got.IsObject(), "行 %d（%s）：%s 必须是对象（实际 %s）",
			r.line, r.ruleID, r.dstPath, got.Raw)
	default:
		t.Fatalf("行 %d：未知 form %q", r.line, r.form)
	}

	// ③ 期望值
	switch {
	case strings.HasPrefix(r.expect, "equals:"):
		p := strings.TrimPrefix(r.expect, "equals:")
		want := in.Get(p)
		require.True(t, want.Exists(), "行 %d（%s）：源侧 %s 不存在，equals 无从比较", r.line, r.ruleID, p)
		require.Equal(t, want.Raw, got.Raw, "行 %d（%s）：%s 必须等于源 %s（源 %s / 产物 %s）",
			r.line, r.ruleID, r.dstPath, p, want.Raw, got.Raw)
	case strings.HasPrefix(r.expect, "len:"):
		n, err := strconv.Atoi(strings.TrimPrefix(r.expect, "len:"))
		require.NoError(t, err, "行 %d：len: 期望值非法", r.line)
		require.Len(t, got.Array(), n, "行 %d（%s）：%s 的元素个数", r.line, r.ruleID, r.dstPath)
	case strings.HasPrefix(r.expect, "raw:"):
		require.Equal(t, strings.TrimPrefix(r.expect, "raw:"), got.Raw,
			"行 %d（%s）：%s 的原始 JSON 文本不符", r.line, r.ruleID, r.dstPath)
	default:
		switch r.form {
		case "string":
			require.Equal(t, r.expect, got.Str, "行 %d（%s）：%s 的值", r.line, r.ruleID, r.dstPath)
		case "number":
			want, err := strconv.ParseFloat(r.expect, 64)
			require.NoError(t, err, "行 %d：数字字面量非法 %q", r.line, r.expect)
			require.Equal(t, want, got.Float(), "行 %d（%s）：%s 的值", r.line, r.ruleID, r.dstPath)
		case "bool":
			require.Equal(t, r.expect == "true", got.Bool(),
				"行 %d（%s）：%s 的值", r.line, r.ruleID, r.dstPath)
		case "null":
			require.Equal(t, "null", r.expect, "行 %d：form=null 的 expect 只能是 null", r.line)
		default:
			t.Fatalf("行 %d：form=%s 的 expect 必须用 equals:/len:/raw: 前缀（字面量 %q 无法判等价）",
				r.line, r.form, r.expect)
		}
	}
}

// ---- 规则体 ----

// ruleCheck 一条规则的附加断言（TSV 的 form/expect 已经判过"某个路径等于某个
// 值"；这里判**结构不变量**：源侧的键不得漏进产物、多项之间必须自洽等）。
type ruleCheck func(t *testing.T, c contractCase)

// dstPerForm 兜底规则体：按 form 列复核产物路径的存在性（absent ↔ 必须不存在，
// 其余 ↔ 必须存在）。每条规则的具体值由 TSV 的 form/expect 列判；这里保证即使
// 把某行的 expect 改成恒真，仍有实打实的存在性断言在跑。
func dstPerForm(t *testing.T, c contractCase) {
	t.Helper()
	v := c.out.Get(c.row.dstPath)
	if c.row.form == "absent" {
		require.False(t, v.Exists(), "行 %d（%s）：%s 不应出现（实际 %s）",
			c.row.line, c.row.ruleID, c.row.dstPath, v.Raw)
		return
	}
	require.True(t, v.Exists(), "行 %d（%s）：产物缺少 %s", c.row.line, c.row.ruleID, c.row.dstPath)
}

// chatToolsNestedOnly chat 的 tools[] 是**嵌套**形态 {function:{…},type:"function"}
// （spec §4.1/F16）；源侧的 input_schema 键不得漏进产物（漏了就是"原样透传"）。
func chatToolsNestedOnly(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	tools := c.out.Get("tools")
	require.True(t, tools.IsArray(), "行 %d：tools 必须是数组", c.row.line)
	tools.ForEach(func(_, tv gjson.Result) bool {
		require.True(t, tv.IsObject(), "行 %d：tools[] 每项必须是对象", c.row.line)
		require.False(t, tv.Get("input_schema").Exists(),
			"行 %d：tools[] 里出现了源侧 input_schema 键（应重排进 function.parameters）", c.row.line)
		require.True(t, tv.Get("function").IsObject(),
			"行 %d：tools[] 每项必须有 function 对象", c.row.line)
		require.Equal(t, "function", tv.Get("type").Str,
			"行 %d：tools[] 的 type 必须是 function", c.row.line)
		return true
	})
}

// responseFormatNestedOnly chat 的 response_format 只有嵌套形态（json_schema 一层），
// 不得把 Responses 的扁平 name/schema 直接搬过去（spec §4.2/F19）。
func responseFormatNestedOnly(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	rf := c.out.Get("response_format")
	require.True(t, rf.IsObject(), "行 %d：response_format 必须是对象", c.row.line)
	require.False(t, rf.Get("name").Exists(),
		"行 %d：response_format 不得出现扁平 name（应搬进 json_schema）", c.row.line)
	require.False(t, rf.Get("schema").Exists(),
		"行 %d：response_format 不得出现扁平 schema（应搬进 json_schema）", c.row.line)
	require.True(t, rf.Get("json_schema").IsObject(),
		"行 %d：response_format.json_schema 必须是对象", c.row.line)
}

// toolChoiceNestedOnly chat 的 tool_choice 对象只有嵌套形态
// {function:{name},type:"function"}（spec §4.2/F19）。
func toolChoiceNestedOnly(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	tc := c.out.Get("tool_choice")
	require.True(t, tc.IsObject(), "行 %d：tool_choice 必须是对象", c.row.line)
	require.False(t, tc.Get("name").Exists(),
		"行 %d：tool_choice 不得出现扁平 name（应搬进 function）", c.row.line)
	require.True(t, tc.Get("function.name").Exists(),
		"行 %d：tool_choice.function.name 必须存在", c.row.line)
	require.Equal(t, "function", tc.Get("type").Str, "行 %d：tool_choice.type 必须是 function", c.row.line)
}

// messUsageNetIdentity Messages 的 usage：四项恒写（含 0），且
// input = gross - cached - write（spec §5.4 的唯一换算点 respInputNet）。
func messUsageNetIdentity(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	u := c.out.Get("usage")
	for _, k := range []string{
		"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens",
	} {
		require.True(t, u.Get(k).Exists(), "行 %d：usage.%s 必须写（含 0）", c.row.line, k)
	}
	gross := c.in.Get("usage.prompt_tokens").Int()
	net := u.Get("input_tokens").Int()
	require.Equal(t,
		gross-u.Get("cache_read_input_tokens").Int()-u.Get("cache_creation_input_tokens").Int(), net,
		"行 %d：input_tokens 必须是净额 gross-cached-write", c.row.line)
}

// respUsageNetIdentity Responses 的 usage：input_tokens_details 两键恒写（含 0，
// 官方必填语义 F14），且 input = gross - cached - write。
func respUsageNetIdentity(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	u := c.out.Get("usage")
	d := u.Get("input_tokens_details")
	require.True(t, d.Get("cached_tokens").Exists(),
		"行 %d：input_tokens_details.cached_tokens 必须写（含 0）", c.row.line)
	require.True(t, d.Get("cache_write_tokens").Exists(),
		"行 %d：input_tokens_details.cache_write_tokens 必须写（含 0）", c.row.line)
	gross := c.in.Get("usage.prompt_tokens").Int()
	require.Equal(t,
		gross-d.Get("cached_tokens").Int()-d.Get("cache_write_tokens").Int(), u.Get("input_tokens").Int(),
		"行 %d：input_tokens 必须是净额 gross-cached-write", c.row.line)
}

// respUsageTotalIdentity Responses 的 total_tokens = net input + output。
func respUsageTotalIdentity(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	u := c.out.Get("usage")
	require.Equal(t, u.Get("input_tokens").Int()+u.Get("output_tokens").Int(), u.Get("total_tokens").Int(),
		"行 %d：total_tokens 必须等于 input + output", c.row.line)
}

// messToolChainOrder chat 的硬约束：role:"tool" 必须按 assistant 声明顺序紧邻输出
// （spec §4.3）。这里把整条链的 tool_call_id 序列钉成字面量。
func messToolChainOrder(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	var ids []string
	c.out.Get("messages").ForEach(func(_, m gjson.Result) bool {
		if m.Get("role").Str == "tool" {
			ids = append(ids, m.Get("tool_call_id").Str)
		}
		return true
	})
	require.Equal(t, []string{"a", "b", "c"}, ids,
		"行 %d：tool 消息必须按 assistant 的声明顺序 a→b→c 逐条紧邻输出", c.row.line)
}

// noGhostToolReference 未声明过的 call_id 的结果必须丢弃（不得合成对应调用，
// spec §4.3 第 1 条）。
func noGhostToolReference(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	c.out.Get("messages").ForEach(func(_, m gjson.Result) bool {
		require.NotEqual(t, "ghost", m.Get("tool_call_id").Str,
			"行 %d：孤儿结果 ghost 必须被丢弃（不得凭空合成 tool 消息）", c.row.line)
		return true
	})
}

// outputItemsTyped Responses 的 output 必须是**数组**，且每项带 string type
// （开放联合；不是 null、不是对象）。
func outputItemsTyped(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	v := c.out.Get("output")
	require.True(t, v.IsArray(), "行 %d：output 必须是数组（null 不是数组）", c.row.line)
	require.NotEmpty(t, v.Array(), "行 %d：output 不得为空数组", c.row.line)
	v.ForEach(func(_, it gjson.Result) bool {
		require.True(t, it.IsObject(), "行 %d：output[] 每项必须是对象", c.row.line)
		require.Equal(t, gjson.String, it.Get("type").Type,
			"行 %d：output[] 每项必须带 string type", c.row.line)
		return true
	})
}

// noSignatureAnywhere chat 源没有签名通道（F12）：thinking 块不得带 signature，
// 也不得编造 —— 这是有意的取舍，不是遗漏。
func noSignatureAnywhere(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	blocks := c.out.Get("content")
	require.True(t, blocks.IsArray(), "行 %d：content 必须是数组", c.row.line)
	blocks.ForEach(func(_, b gjson.Result) bool {
		require.False(t, b.Get("signature").Exists(),
			"行 %d：content[] 里不得出现 signature（chat 源无签名通道，禁止发明）", c.row.line)
		return true
	})
}

// argumentsAllStrings tool_calls[].function.arguments 必须是**字符串**（装着 JSON），
// 不是对象 —— 写成对象上游解出来是 string 而非 object（spec §4.1/§4.2）。
func argumentsAllStrings(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	seen := 0
	// chat 形态：messages[].tool_calls[].function.arguments
	c.out.Get("messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			require.Equal(t, gjson.String, tc.Get("function.arguments").Type,
				"行 %d：tool_calls[].function.arguments 必须是字符串", c.row.line)
			seen++
			return true
		})
		return true
	})
	// Responses 形态：output[].arguments —— 两条转换方向的产物形态不同，同一条
	// 规则必须两种都认，否则在另一方向上恒为"空转"（假绿）。
	c.out.Get("output").ForEach(func(_, it gjson.Result) bool {
		if it.Get("type").Str != "function_call" {
			return true
		}
		require.Equal(t, gjson.String, it.Get("arguments").Type,
			"行 %d：output[].arguments 必须是字符串", c.row.line)
		seen++
		return true
	})
	require.NotZero(t, seen,
		"行 %d：夹具里没有工具调用（messages[].tool_calls / output[]），该 rule 空转", c.row.line)
}

// streamOptionsAlwaysUsage 开流式必须**强制** include_usage=true（spec §4.1/F3：
// 否则上游不发 usage 帧，客户端与服务端都看到 0 token）。
func streamOptionsAlwaysUsage(t *testing.T, c contractCase) {
	dstPerForm(t, c)
	require.Equal(t, gjson.True, c.out.Get("stream_options.include_usage").Type,
		"行 %d：stream_options.include_usage 必须被强制为 true", c.row.line)
}

// contractRules 规则表：**每个 rule_id 必须以 assertRule("<rule_id>", …) 的实参
// 出现**（§8(a3) 的硬要求：写成注释等于没写）。TestContractRuleCoverage 用
// strings.Contains(src, `assertRule("<id>"`) 逐条核验，缺一条即红。
func contractRules() map[string]ruleCheck {
	m := make(map[string]ruleCheck, 121)
	assertRule := func(id string, fn ruleCheck) {
		if fn == nil {
			panic("protoconv: assertRule(" + id + ") 的规则体是 nil（死断言）")
		}
		if _, dup := m[id]; dup {
			panic("protoconv: 契约规则重复注册：" + id)
		}
		m[id] = fn
	}
	// scope: mess_to_chat_req_text
	assertRule("ct_mess_model_passthrough", dstPerForm)
	assertRule("ct_mess_max_tokens_renamed", dstPerForm)
	assertRule("ct_mess_max_tokens_key_absent", dstPerForm)
	assertRule("ct_mess_system_is_first_message", dstPerForm)
	assertRule("ct_mess_system_content_folded", dstPerForm)
	assertRule("ct_mess_messages_array_len", dstPerForm)
	assertRule("ct_mess_asst_text_joined", dstPerForm)
	assertRule("ct_mess_user_block_text_type", dstPerForm)
	// scope: mess_to_chat_req_tools
	assertRule("ct_mess_tools_parameters_object", chatToolsNestedOnly)
	assertRule("ct_mess_tools_name_nested", dstPerForm)
	assertRule("ct_mess_tools_input_schema_absent", dstPerForm)
	assertRule("ct_mess_tools_type_function", dstPerForm)
	assertRule("ct_mess_tool_choice_auto", dstPerForm)
	assertRule("ct_mess_stop_single_string", dstPerForm)
	assertRule("ct_mess_tool_call_id_paired", dstPerForm)
	assertRule("ct_mess_tool_call_id_from_tool_use", dstPerForm)
	assertRule("ct_mess_tool_result_text_kept", dstPerForm)
	assertRule("ct_mess_tool_result_image_moved", dstPerForm)
	assertRule("ct_mess_image_base64_data_url", dstPerForm)
	assertRule("ct_mess_arguments_is_string", argumentsAllStrings)
	// scope: mess_to_chat_req_image_system
	assertRule("ct_mess_system_blocks_folded_by_newline", dstPerForm)
	assertRule("ct_mess_thinking_budget_to_effort", dstPerForm)
	assertRule("ct_mess_stop_truncated_to_4", dstPerForm)
	assertRule("ct_mess_stream_options_forced", streamOptionsAlwaysUsage)
	assertRule("ct_mess_stream_options_preserved", dstPerForm)
	assertRule("ct_mess_metadata_user_id", dstPerForm)
	assertRule("ct_mess_temperature_dropped_with_thinking", dstPerForm)
	assertRule("ct_mess_top_p_dropped_with_thinking", dstPerForm)
	assertRule("ct_mess_image_url_passthrough", dstPerForm)
	// scope: mess_to_chat_req_multiturn
	assertRule("ct_mess_multiturn_tool_id_first", dstPerForm)
	assertRule("ct_mess_multiturn_tool_id_second", dstPerForm)
	assertRule("ct_mess_tool_result_is_error_prefix", dstPerForm)
	assertRule("ct_mess_multiturn_tool_id_third", dstPerForm)
	assertRule("ct_mess_multiturn_tool_calls_ordered", dstPerForm)
	assertRule("ct_mess_multiturn_message_count", messToolChainOrder)
	// scope: resp_to_chat_req_text
	assertRule("ct_resp_instructions_first_system_role", dstPerForm)
	assertRule("ct_resp_instructions_first_system_content", dstPerForm)
	assertRule("ct_resp_input_string_user_content", dstPerForm)
	assertRule("ct_resp_model_passthrough", dstPerForm)
	assertRule("ct_resp_max_output_tokens_renamed", dstPerForm)
	assertRule("ct_resp_max_output_tokens_key_absent", dstPerForm)
	// scope: resp_to_chat_req_tools
	assertRule("ct_resp_tool_choice_nested_type", toolChoiceNestedOnly)
	assertRule("ct_resp_tool_choice_nested_name", toolChoiceNestedOnly)
	assertRule("ct_resp_tool_choice_flat_key_absent", dstPerForm)
	assertRule("ct_resp_tools_parameters_object", chatToolsNestedOnly)
	assertRule("ct_resp_tools_strict_nested", dstPerForm)
	assertRule("ct_resp_tools_non_function_filtered", dstPerForm)
	assertRule("ct_resp_function_call_id", dstPerForm)
	assertRule("ct_resp_function_call_arguments_string", argumentsAllStrings)
	assertRule("ct_resp_function_call_output_role_tool", dstPerForm)
	assertRule("ct_resp_function_call_output_paired", dstPerForm)
	assertRule("ct_resp_function_call_output_content", dstPerForm)
	assertRule("ct_resp_parallel_tool_calls_passthrough", dstPerForm)
	assertRule("ct_resp_input_text_block_type", dstPerForm)
	// scope: resp_to_chat_req_image_system
	assertRule("ct_resp_text_format_nested_type", responseFormatNestedOnly)
	assertRule("ct_resp_text_format_nested_name", responseFormatNestedOnly)
	assertRule("ct_resp_text_format_nested_schema", responseFormatNestedOnly)
	assertRule("ct_resp_text_format_nested_strict", responseFormatNestedOnly)
	assertRule("ct_resp_text_format_flat_key_absent", dstPerForm)
	assertRule("ct_resp_reasoning_effort_passthrough", dstPerForm)
	assertRule("ct_resp_stream_options_forced", streamOptionsAlwaysUsage)
	assertRule("ct_resp_input_image_url", dstPerForm)
	assertRule("ct_resp_stream_passthrough", dstPerForm)
	assertRule("ct_resp_user_passthrough", dstPerForm)
	assertRule("ct_resp_temperature_passthrough_with_effort", dstPerForm)
	assertRule("ct_resp_input_file_dropped", dstPerForm)
	// scope: resp_to_chat_req_multiturn
	assertRule("ct_resp_multiturn_calls_merged", dstPerForm)
	assertRule("ct_resp_multiturn_tool_id_first", dstPerForm)
	assertRule("ct_resp_multiturn_tool_id_second", dstPerForm)
	assertRule("ct_resp_multiturn_orphan_dropped", noGhostToolReference)
	assertRule("ct_resp_reasoning_content", dstPerForm)
	// scope: chat_to_mess_resp_text
	assertRule("ct_mess_resp_type_message", dstPerForm)
	assertRule("ct_mess_resp_role_assistant", dstPerForm)
	assertRule("ct_mess_resp_content_array", dstPerForm)
	assertRule("ct_mess_resp_text_block_type", dstPerForm)
	assertRule("ct_mess_resp_text_block_text", dstPerForm)
	assertRule("ct_mess_resp_stop_to_end_turn", dstPerForm)
	assertRule("ct_mess_resp_stop_sequence_null", dstPerForm)
	assertRule("ct_mess_resp_usage_input_net", messUsageNetIdentity)
	assertRule("ct_mess_resp_usage_cached", dstPerForm)
	assertRule("ct_mess_resp_usage_write", dstPerForm)
	assertRule("ct_mess_resp_usage_output", dstPerForm)
	assertRule("ct_mess_resp_usage_no_total", dstPerForm)
	// scope: chat_to_mess_resp_tools
	assertRule("ct_mess_resp_thinking_first_block", dstPerForm)
	assertRule("ct_mess_resp_thinking_text", dstPerForm)
	assertRule("ct_mess_resp_thinking_no_signature", noSignatureAnywhere)
	assertRule("ct_mess_resp_tool_use_type", dstPerForm)
	assertRule("ct_mess_resp_tool_use_id", dstPerForm)
	assertRule("ct_mess_resp_tool_use_name", dstPerForm)
	assertRule("ct_mess_resp_tool_use_input_object", dstPerForm)
	assertRule("ct_mess_resp_bad_arguments_fallback", dstPerForm)
	assertRule("ct_mess_resp_stop_tool_calls_to_tool_use", dstPerForm)
	// scope: chat_to_mess_resp_length
	assertRule("ct_mess_resp_stop_length_to_max_tokens", dstPerForm)
	assertRule("ct_mess_resp_empty_text_block_written", dstPerForm)
	assertRule("ct_mess_resp_empty_text_is_empty_string", dstPerForm)
	// scope: chat_to_resp_resp_text
	assertRule("ct_resp_resp_object_response", dstPerForm)
	assertRule("ct_resp_resp_status_completed", dstPerForm)
	assertRule("ct_resp_resp_output_is_array", outputItemsTyped)
	assertRule("ct_resp_resp_message_item_type", dstPerForm)
	assertRule("ct_resp_resp_message_item_role", dstPerForm)
	assertRule("ct_resp_resp_output_text_type", dstPerForm)
	assertRule("ct_resp_resp_output_text", dstPerForm)
	assertRule("ct_resp_resp_annotations_array", dstPerForm)
	assertRule("ct_resp_resp_created_at", dstPerForm)
	assertRule("ct_resp_resp_usage_input_net", respUsageNetIdentity)
	assertRule("ct_resp_resp_usage_cached", dstPerForm)
	assertRule("ct_resp_resp_usage_write", dstPerForm)
	assertRule("ct_resp_resp_usage_total", respUsageTotalIdentity)
	assertRule("ct_resp_resp_usage_output", dstPerForm)
	// scope: chat_to_resp_resp_tools
	assertRule("ct_resp_resp_reasoning_item_first", dstPerForm)
	assertRule("ct_resp_resp_reasoning_summary_type", dstPerForm)
	assertRule("ct_resp_resp_reasoning_no_encrypted_content", dstPerForm)
	assertRule("ct_resp_resp_message_item_second", dstPerForm)
	assertRule("ct_resp_resp_function_call_item_third", dstPerForm)
	assertRule("ct_resp_resp_arguments_string", argumentsAllStrings)
	assertRule("ct_resp_resp_function_call_call_id", dstPerForm)
	assertRule("ct_resp_resp_function_call_name", dstPerForm)
	assertRule("ct_resp_resp_reasoning_tokens_moved", dstPerForm)
	// scope: chat_to_resp_resp_incomplete
	assertRule("ct_resp_resp_status_incomplete", dstPerForm)
	assertRule("ct_resp_resp_incomplete_reason", dstPerForm)
	return m
}

// contractTestSource 读本文件源码（用于"rule_id 必须作为 assertRule 实参出现"的
// 字面核验 —— 只看注册表会让"注册了但没写实参"的作弊路径隐身）。
func contractTestSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("contract_client_to_chat_test.go")
	require.NoError(t, err)
	return string(raw)
}

// TestContractClientToChat §8(a3)：逐行跑契约表 —— 取 dst_path 的值、判形态、
// 判值。**每一行都必须被某个 scope 的夹具真的跑到**（夹具缺失即红），并且该行
// 的 rule_id 必须在源码里以 assertRule 实参出现。
func TestContractClientToChat(t *testing.T) {
	rows := contractRows(t)
	fixtures := contractFixtures()
	rules := contractRules()
	src := contractTestSource(t)

	for _, r := range rows {
		t.Run(r.ruleID, func(t *testing.T) {
			fx, ok := fixtures[r.scope]
			require.True(t, ok, "行 %d（%s）：scope %q 没有输入夹具（该行会空转）", r.line, r.ruleID, r.scope)
			require.True(t, strings.HasPrefix(r.scope, r.dir+"_"),
				"行 %d：scope %q 必须属于 dir %q", r.line, r.scope, r.dir)
			fn, ok := rules[r.ruleID]
			require.True(t, ok,
				"行 %d：rule_id %q 在测试代码里没有 assertRule 注册（只写注释等于没写）", r.line, r.ruleID)
			require.Contains(t, src, "assertRule(\""+r.ruleID+"\"",
				"行 %d：rule_id %q 必须以 assertRule(\"…\", …) 的实参形式出现", r.line, r.ruleID)

			in := gjson.Parse(fx.body)
			require.True(t, in.Exists(), "行 %d：夹具 %q 的输入不是合法 JSON", r.line, r.scope)

			var (
				raw []byte
				err error
			)
			if fx.resp {
				raw, err = ConvertResponse([]byte(fx.body), fx.conv)
			} else {
				raw, err = ConvertRequest([]byte(fx.body), fx.conv)
			}
			require.NoError(t, err, "行 %d（%s）：转换失败", r.line, r.ruleID)
			require.True(t, json.Valid(raw), "行 %d（%s）：产物不是合法 JSON：%s", r.line, r.ruleID, raw)

			out := gjson.ParseBytes(raw)
			checkContractRow(t, r, in, out)
			fn(t, contractCase{row: r, in: in, out: out})
		})
	}
}

// TestContractRuleCoverage §8(a3) 的"无实参即红"：规则表 ↔ TSV 行**双向**全覆盖。
// 注册了却没有行 = 死断言；有行却没注册 = 该行没被验证。
func TestContractRuleCoverage(t *testing.T) {
	rows := contractRows(t)
	rules := contractRules()
	src := contractTestSource(t)

	seen := make(map[string]int, len(rules))
	for _, r := range rows {
		seen[r.ruleID]++
	}
	for id := range rules {
		require.NotZero(t, seen[id], "assertRule(%q) 在契约表里没有对应行（死断言）", id)
		require.Contains(t, src, "assertRule(\""+id+"\"", "assertRule(%q) 必须以实参形式出现", id)
	}
	require.Len(t, rules, len(seen),
		"规则表与契约表的 rule_id 集合必须一一对应（表 %d 行 / 注册 %d 条）", len(rows), len(rules))
	require.GreaterOrEqual(t, len(rows), 40, "契约表覆盖不足：spec §4/§5 的字面量规则远多于 40 条")
}

// TestContractAnchorsVerified §8(m)：contract_client_to_chat 的 anchor 逐字核验。
//
// anchors.tsv 的 line 列此时**不是**规范文档行号，而是契约表自己的行号：
// ① 取该行；② excerpt 必须是该行 expect 列的子串（也就必然是该行原文的子串）；
// ③ 该行 rule_id 必须以 assertRule 实参出现在本文件；④ anchor 的 (dir, member)
// 必须与该行的 (dir, rule_id) 一致（防止 anchor 指错行）。
// 这解决了"契约表本身没有可比对散文"的死结。
func TestContractAnchorsVerified(t *testing.T) {
	rows := contractRows(t)
	rules := contractRules()
	src := contractTestSource(t)

	dirSet := make(map[string]struct{}, dirCount)
	for d := direction(0); d < dirCount; d++ {
		dirSet[dirName[d]] = struct{}{}
	}

	anchored := 0
	for _, f := range tsvRows(t, "testdata/spec/anchors.tsv") {
		require.Len(t, f, 5, "anchors.tsv 每行五列：dir/member/spec_file/line/excerpt")
		if f[2] != contractSpecFile {
			continue
		}
		anchored++
		line, err := strconv.Atoi(f[3])
		require.NoError(t, err, "anchor %s/%s 的 line 必须是契约表行号", f[0], f[1])
		row, found := contractRowByLine(rows, line)
		require.True(t, found, "anchor %s/%s 指向契约表第 %d 行，但该行不存在（编造 anchor）", f[0], f[1], line)

		// ① excerpt 必须是该行 expect 列的子串（§8(m) 原文）
		require.Contains(t, row.expect, f[4],
			"anchor %s/%s：excerpt 必须是第 %d 行 expect 列的子串（expect %q / excerpt %q）",
			f[0], f[1], line, row.expect, f[4])
		require.LessOrEqual(t, len(f[4]), 120, "anchor 片段必须 ≤120B：%s", f[1])

		// ② anchor 的 (dir, member) 必须与该行一致
		require.Equal(t, row.dir, f[0], "anchor %s/%s 的 dir 与第 %d 行不符", f[0], f[1], line)
		require.Equal(t, row.ruleID, f[1], "anchor %s/%s 的 member 与第 %d 行的 rule_id 不符", f[0], f[1], line)

		// ③ 该 rule 必须以 assertRule 实参出现（同 (a3)）
		require.Contains(t, src, "assertRule(\""+row.ruleID+"\"",
			"第 %d 行的 rule_id %q 必须以 assertRule 实参出现", line, row.ruleID)
		_, ok := rules[row.ruleID]
		require.True(t, ok, "第 %d 行的 rule_id %q 未注册", line, row.ruleID)
		require.Contains(t, dirSet, f[0], "anchor 的 dir 必须是 dirName 之一：%q", f[0])
	}
	require.NotZero(t, anchored, "anchors.tsv 里必须至少有一条 spec_file=%s 的 anchor", contractSpecFile)
}

// contractRowByLine 按物理行号取表行。
func contractRowByLine(rows []contractRow, line int) (contractRow, bool) {
	for _, r := range rows {
		if r.line == line {
			return r, true
		}
	}
	return contractRow{}, false
}
