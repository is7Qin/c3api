// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// (c) 逐行黄金 + (f) 不变量接线 + spec §5.1 的验收断言。
//
// (c)：内容模型的**每一行**至少一条用例，drop/omit/empty/TD 的用例断言其形态
// （不是「没崩」而是「目标侧确实没有该物」）。覆盖度由 membersOf(dir) 与用例表
// 双向比对强制——新增成员不写用例即红。

import (
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// rowCase 一行内容模型的黄金用例。
type rowCase struct {
	member string
	body   func(t *testing.T) string
	want   []string // 目标侧必须出现（断言策略接线成功）
	absent []string // 目标侧必须不出现（断言 drop/omit/降级的形态）
}

func lit(s string) func(*testing.T) string { return func(*testing.T) string { return s } }

// rowCases 方向 → 逐行用例。
func rowCases(t *testing.T) map[direction][]rowCase {
	t.Helper()
	// reasoning 信封：只有本包信封（encrypted_content）才可回放（§2.3/§2.5）
	env := sealThinking(thinkingBlock{Type: "thinking", Thinking: "think", Signature: "sig"})
	require.NotEmpty(t, env)
	envRedacted := sealThinking(thinkingBlock{Type: "redacted_thinking", Data: "dead"})
	require.NotEmpty(t, envRedacted)
	// 请求侧的 thinking 块用「reasoning_ref 臂」签名才能回放成 reasoning item
	// （spec §2.5：只有本包信封可往返，Anthropic 原生签名不外发）
	sig := sealReasoningRef(reasoningRef{EncryptedContent: env, ID: "rs_1"})
	require.NotEmpty(t, sig)

	return map[direction][]rowCase{
		dirChatToResp: {
			{"c_sysdev_text", lit(`{"messages":[{"role":"system","content":"SYS1"}]}`),
				[]string{`"role":"developer"`, `SYS1`, `"type":"input_text"`}, nil},
			{"c_user_text", lit(`{"messages":[{"role":"user","content":"U1"}]}`),
				[]string{`{"text":"U1","type":"input_text"}`}, nil},
			{"c_user_img_url", lit(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]}]}`),
				[]string{`"image_url":"https://e.com/i.png"`, `"type":"input_image"`}, nil},
			{"c_user_img_data", lit(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`),
				[]string{`"image_url":"data:image/png;base64,QUJD"`}, nil},
			{"c_user_img_bare64", lit(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"QUJD"}]}]}`),
				[]string{`"image_url":"QUJD"`}, nil},
			{"c_user_audio", lit(`{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}}]}]}`),
				nil, []string{`"input_audio"`, `"AA"`}},
			{"c_user_file_data", lit(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"QUJD","filename":"a.txt"}}]}]}`),
				[]string{`"file_data":"QUJD"`, `"filename":"a.txt"`, `"type":"input_file"`}, nil},
			{"c_user_file_id", lit(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f1"}}]}]}`),
				nil, []string{`f1`, `"input_file"`}},
			{"c_asst_text", lit(`{"messages":[{"role":"assistant","content":"A1"}]}`),
				[]string{`{"annotations":[],"text":"A1","type":"output_text"}`}, []string{`"input_text"`}},
			{"c_asst_refusal", lit(`{"messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"NO"}]}]}`),
				[]string{`{"refusal":"NO","type":"refusal"}`}, nil},
			{"c_asst_other_parts", lit(`{"messages":[{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]}]}`),
				nil, []string{`"input_image"`, `i.png`}},
			{"c_asst_tool_function", lit(`{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]}]}`),
				[]string{`"type":"function_call"`, `"call_id":"c1"`}, nil},
			{"c_asst_tool_custom", lit(`{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"CZ1","input":"i"}}]}]}`),
				nil, []string{`CZ1`}},
			{"c_tool_msg", lit(`{"messages":[{"role":"tool","tool_call_id":"c1","content":"OK1"}]}`),
				[]string{`"call_id":"c1"`, `"output":"OK1"`, `"type":"function_call_output"`}, nil},
			{"c_function_msg", lit(`{"messages":[{"role":"function","name":"g","content":"OK2"}]}`),
				[]string{`"call_id":"g"`, `"output":"OK2"`}, nil},
			{"c_tail", lit(`{"messages":[{"role":"user","content":[{"type":"unknown_part","text":"ZZZ"}]}]}`),
				nil, []string{`ZZZ`}},
		},
		dirChatToMess: {
			{"cm_sysdev_text", lit(`{"messages":[{"role":"system","content":"SYS1"}]}`),
				[]string{`"system":"SYS1"`}, []string{`"role":"system"`}},
			{"cm_user_text", lit(`{"messages":[{"role":"user","content":"U1"}]}`),
				[]string{`{"content":"U1","role":"user"}`}, nil},
			{"cm_user_img_url", lit(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]}]}`),
				[]string{`{"source":{"type":"url","url":"https://e.com/i.png"},"type":"image"}`}, nil},
			{"cm_user_img_data", lit(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`),
				[]string{`{"source":{"data":"QUJD","media_type":"image/png","type":"base64"},"type":"image"}`}, nil},
			{"cm_user_img_bare64", lit(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"QUJD"}]}]}`),
				nil, []string{`QUJD`}},
			{"cm_user_audio", lit(`{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}}]}]}`),
				nil, []string{`"AA"`}},
			{"cm_user_file_data", lit(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"QUJD"}}]}]}`),
				nil, []string{`QUJD`}},
			{"cm_user_file_id", lit(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f1"}}]}]}`),
				nil, []string{`f1`}},
			{"cm_asst_text", lit(`{"messages":[{"role":"assistant","content":"A1"}]}`),
				[]string{`{"text":"A1","type":"text"}`}, nil},
			{"cm_asst_refusal", lit(`{"messages":[{"role":"assistant","content":[{"type":"refusal","refusal":"NO"}]}]}`),
				[]string{`{"text":"NO","type":"text"}`}, []string{`"type":"refusal"`}},
			{"cm_asst_other_parts", lit(`{"messages":[{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]}]}`),
				nil, []string{`"type":"image"`, `i.png`}},
			{"cm_asst_tool_function", lit(`{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]}]}`),
				[]string{`"type":"tool_use"`, `"id":"c1"`}, nil},
			{"cm_asst_tool_custom", lit(`{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"CZ1","input":"i"}}]}]}`),
				nil, []string{`CZ1`}},
			{"cm_tool_msg", lit(`{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"OK1"}]}`),
				[]string{`{"content":"OK1","tool_use_id":"c1","type":"tool_result"}`}, nil},
			{"cm_function_msg", lit(`{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"g","function":{"name":"f","arguments":"{}"}}]},{"role":"function","name":"g","content":"OK2"}]}`),
				[]string{`{"content":"OK2","tool_use_id":"g","type":"tool_result"}`}, nil},
			{"cm_tail", lit(`{"messages":[{"role":"user","content":[{"type":"unknown_part","text":"ZZZ"}]}]}`),
				nil, []string{`ZZZ`}},
		},
		dirMessToResp: {
			{"m_user_text", lit(`{"messages":[{"role":"user","content":"U1"}]}`),
				[]string{`{"text":"U1","type":"input_text"}`}, nil},
			{"m_asst_text", lit(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"A1"}]}]}`),
				[]string{`{"text":"A1","type":"output_text"}`}, nil},
			{"m_user_img_base64", lit(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`),
				[]string{`"image_url":"data:image/png;base64,QUJD"`, `"type":"input_image"`}, nil},
			{"m_user_img_url", lit(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}}]}]}`),
				[]string{`"image_url":"https://e.com/i.png"`}, nil},
			{"m_user_img_file_id", lit(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"f1"}}]}]}`),
				nil, []string{`f1`, `"input_image"`}},
			{"m_asst_img_base64", lit(`{"messages":[{"role":"assistant","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`),
				nil, []string{`QUJD`}},
			{"m_asst_img_url", lit(`{"messages":[{"role":"assistant","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}}]}]}`),
				nil, []string{`i.png`}},
			{"m_asst_img_file_id", lit(`{"messages":[{"role":"assistant","content":[{"type":"image","source":{"type":"file","file_id":"f1"}}]}]}`),
				nil, []string{`f1`}},
			{"m_user_doc_b64textcontent", lit(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}}]}]}`),
				nil, []string{`application/pdf`, `"input_file"`}},
			{"m_user_doc_file_id", lit(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"f1"}}]}]}`),
				nil, []string{`f1`}},
			{"m_user_doc_url", lit(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://e.com/d.pdf"}}]}]}`),
				[]string{`"file_url":"https://e.com/d.pdf"`, `"type":"input_file"`}, nil},
			{"m_user_search_result", lit(`{"messages":[{"role":"user","content":[{"type":"search_result","source":"s","title":"SEARCHTITLE","content":[{"type":"text","text":"x"}]}]}]}`),
				nil, []string{`SEARCHTITLE`}},
			{"m_asst_search_result", lit(`{"messages":[{"role":"assistant","content":[{"type":"search_result","source":"s","title":"SEARCHTITLE","content":[]}]}]}`),
				nil, []string{`SEARCHTITLE`}},
			{"m_asst_tool_use", lit(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}}]}]}`),
				[]string{`"type":"function_call"`, `"call_id":"tu1"`}, nil},
			{"m_tool_result_string", lit(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"OK"}]}]}`),
				[]string{`"call_id":"tu1"`, `"output":"OK"`}, nil},
			{"m_tool_result_array_text", lit(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"ARR"}]}]}]}`),
				[]string{`"output":"ARR"`}, nil},
			{"m_tool_result_array_notext", lit(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"image","source":{"type":"url","url":"u"}}]},]}]}`),
				[]string{`{"call_id":"tu1","output":"","type":"function_call_output"}`}, nil},
			{"m_tool_result_no_content", lit(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1"}]}]}`),
				[]string{`{"call_id":"tu1","output":"","type":"function_call_output"}`}, nil},
			{"m_tool_result_nested_media", lit(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}},{"type":"text","text":""}]}]}]}`),
				[]string{`"output":""`}, nil},
			{"m_thinking", func(*testing.T) string {
				return `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"think","signature":"` + sig + `"}]}]}`
			}, []string{`"type":"reasoning"`, `"encrypted_content":`}, nil},
			{"m_redacted_thinking", func(*testing.T) string {
				return `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"` + sig + `"}]}]}`
			}, []string{`"type":"reasoning"`, `"encrypted_content":`}, nil},
			{"m_tail", lit(`{"messages":[{"role":"user","content":[{"type":"unknown_block","text":"ZZZ"}]}]}`),
				nil, []string{`ZZZ`}},
			{"m_top_system", lit(`{"system":"SYS1","messages":[{"role":"user","content":"u"}]}`),
				[]string{`"instructions":"SYS1"`}, nil},
		},
		dirRespToMess: {
			{"r_msg_user_input_text", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"U1"}]}]}`),
				[]string{`{"text":"U1","type":"text"}`}, nil},
			{"r_msg_user_output_text", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"output_text","text":"U2"}]}]}`),
				[]string{`{"text":"U2","type":"text"}`}, nil},
			{"r_msg_asst_input_text", lit(`{"input":[{"type":"message","role":"assistant","content":[{"type":"input_text","text":"A1"}]}]}`),
				[]string{`{"text":"A1","type":"text"}`, `"role":"assistant"`}, nil},
			{"r_msg_user_img_url", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://e.com/i.png"}]}]}`),
				[]string{`{"source":{"type":"url","url":"https://e.com/i.png"},"type":"image"}`}, nil},
			{"r_msg_user_img_data_url", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]}]}`),
				[]string{`{"source":{"data":"QUJD","media_type":"image/png","type":"base64"},"type":"image"}`}, nil},
			{"r_msg_user_img_other", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"ftp://e.com/i.png"}]}]}`),
				nil, []string{`ftp://`}},
			{"r_msg_user_img_file_id", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"f1"}]}]}`),
				nil, []string{`f1`}},
			{"r_msg_user_file_url", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_url":"https://e.com/d.pdf"}]}]}`),
				[]string{`{"source":{"type":"url","url":"https://e.com/d.pdf"},"type":"document"}`}, nil},
			{"r_msg_user_file_id", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"f2"}]}]}`),
				nil, []string{`f2`}},
			{"r_msg_user_file_data", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_data":"QUJD"}]}]}`),
				nil, []string{`QUJD`}},
			{"r_msg_user_input_audio", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}}]}]}`),
				nil, []string{`"AA"`}},
			{"r_msg_sysdev_text", lit(`{"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"SYS1"}]}]}`),
				[]string{`"system":"SYS1"`}, []string{`"role":"system"`}},
			{"r_msg_asst_output_text", lit(`{"input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A2"}]}]}`),
				[]string{`{"text":"A2","type":"text"}`}, nil},
			{"r_msg_asst_refusal", lit(`{"input":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"NO"}]}]}`),
				[]string{`{"text":"NO","type":"text"}`}, []string{`"type":"refusal"`}},
			{"r_msg_asst_media", lit(`{"input":[{"type":"message","role":"assistant","content":[{"type":"input_image","image_url":"https://e.com/i.png"}]}]}`),
				nil, []string{`"image"`, `"document"`}},
			{"r_function_call", lit(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`),
				[]string{`"type":"tool_use"`, `"id":"c1"`}, nil},
			{"r_reasoning", func(*testing.T) string {
				return `{"reasoning":{"effort":"high"},"input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + env + `"}]}`
			}, []string{`"type":"thinking"`}, nil},
			{"r_item_reference", lit(`{"input":[{"type":"item_reference","id":"REF1"}]}`),
				nil, []string{`REF1`}},
			{"r_tail", lit(`{"input":[{"type":"unknown_item","id":"ZZZ"}]}`),
				nil, []string{`ZZZ`}},
			{"r_fco_string", lit(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"OK"}]}`),
				[]string{`{"content":"OK","tool_use_id":"c1","type":"tool_result"}`}, nil},
			{"r_fco_array_text", lit(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"ARR"}]}]}`),
				[]string{`"content":"ARR"`}, nil},
			{"r_fco_array_notext", lit(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"https://e.com/i.png"}]}]}`),
				[]string{`{"content":"","tool_use_id":"c1","type":"tool_result"}`}, nil},
		},
		dirRespToChat: {
			{"x_message_output_text", lit(`{"id":"r1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"HELLO"}]}]}`),
				[]string{`"content":"HELLO"`}, nil},
			{"x_message_refusal", lit(`{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"NO"}]}]}`),
				[]string{`"content":"NO"`, `"finish_reason":"content_filter"`}, nil},
			{"x_function_call", lit(`{"id":"r1","output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`),
				[]string{`"tool_calls"`, `"finish_reason":"tool_calls"`}, nil},
			{"x_reasoning", lit(`{"id":"r1","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]}]}`),
				[]string{`"reasoning_content":"think"`}, nil},
			{"x_message_media", lit(`{"id":"r1","output":[{"type":"message","role":"assistant","content":[{"type":"input_image","image_url":"https://e.com/i.png"}]}]}`),
				nil, []string{`i.png`}},
			{"x_tail", lit(`{"id":"r1","output":[{"type":"unknown_item","content":[{"type":"text","text":"ZZZ"}]}]}`),
				nil, []string{`ZZZ`}},
		},
		dirRespToMessResp: {
			{"y_output_text", lit(`{"id":"r1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"HELLO"}]}]}`),
				[]string{`{"text":"HELLO","type":"text"}`}, nil},
			{"y_refusal", lit(`{"id":"r1","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"NO"}]}]}`),
				[]string{`{"text":"NO","type":"text"}`}, []string{`"type":"refusal"`}},
			{"y_function_call", lit(`{"id":"r1","output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`),
				[]string{`{"id":"c1"`, `"type":"tool_use"`}, nil},
			{"y_reasoning", func(*testing.T) string {
				return `{"id":"r1","output":[{"type":"reasoning","encrypted_content":"enc","summary":[{"type":"summary_text","text":"think"}]}]}`
			}, []string{`"type":"thinking"`}, nil},
			{"y_tail", lit(`{"id":"r1","output":[{"type":"unknown_item"}]}`),
				nil, []string{`"unknown_item"`}},
		},
		dirMessToChatReq: {
			{"mc_sys_text", lit(`{"model":"m","system":"SYS","messages":[]}`),
				[]string{`"role":"system"`, `SYS`}, nil},
			{"mc_sys_block", lit(`{"system":[{"type":"text","text":"S1"},{"type":"image","source":{"type":"url","url":"https://e.com/s.png"}},{"type":"text","text":"S2"}],"messages":[]}`),
				[]string{`"role":"system"`, `S1`, `S2`}, []string{`s.png`}},
			{"mc_user_text", lit(`{"messages":[{"role":"user","content":[{"type":"text","text":"U1"}]}]}`),
				[]string{`"text":"U1"`, `"role":"user"`}, nil},
			{"mc_user_img_b64", lit(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`),
				[]string{`data:image/png;base64,QUJD`}, nil},
			{"mc_user_img_url", lit(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}},{"type":"image","source":{"type":"file","file_id":"f_1"}}]}]}`),
				[]string{`https://e.com/i.png`}, []string{`f_1`}},
			{"mc_user_doc", lit(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"DOCTXT"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"UERG"}},{"type":"search_result","content":"SR1"}]}]}`),
				[]string{`DOCTXT`}, []string{`UERG`, `SR1`}},
			{"mc_user_tr", lit(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"R1"}]}]}`),
				[]string{`"role":"tool"`, `"tool_call_id":"tu1"`, `R1`}, nil},
			{"mc_user_tr_err", lit(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","is_error":true,"content":[{"type":"text","text":"BOOM"}]}]}]}`),
				[]string{`"content":"Error: BOOM"`, `"tool_call_id":"tu1"`}, nil},
			{"mc_user_tr_img", lit(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}]}`),
				[]string{`"content":"[image]"`, `data:image/png;base64,QUJD`}, nil},
			{"mc_asst_text", lit(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"HELLO"}]}]}`),
				[]string{`"content":"HELLO"`}, nil},
			{"mc_asst_tooluse", lit(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}}]}]}`),
				[]string{`"tool_calls"`, `"name":"f"`, `"id":"tu1"`}, nil},
			{"mc_asst_thinking", lit(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"think","signature":"SIG"}]}]}`),
				[]string{`"reasoning_content":"think"`}, []string{`SIG`}},
			{"mc_asst_redacted", lit(`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"RED"},{"type":"unknown_block","text":"ZZZ"}]}]}`),
				nil, []string{`RED`, `ZZZ`}},
			{"mc_tools", lit(`{"messages":[],"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}]}`),
				[]string{`"tools":[{"function":{`, `"parameters":{"type":"object"`, `"type":"function"`}, []string{`"input_schema"`}},
			{"mc_tool_strict", lit(`{"messages":[],"tools":[{"name":"f","description":"d","strict":true,"input_schema":{"type":"object"}}]}`),
				[]string{`"strict":true`}, nil},
			{"mc_tool_choice_str", lit(`{"messages":[],"tool_choice":"any"}`),
				[]string{`"tool_choice":"required"`}, nil},
			{"mc_tool_choice_tool", lit(`{"messages":[],"tool_choice":{"type":"tool","name":"f"}}`),
				[]string{`"tool_choice":{"function":{"name":"f"},"type":"function"}`}, nil},
			{"mc_tool_choice_parallel", lit(`{"messages":[],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`),
				[]string{`"parallel_tool_calls":false`}, nil},
			{"mc_max_tokens", lit(`{"max_tokens":128,"messages":[]}`),
				[]string{`"max_completion_tokens":128`}, []string{`"max_tokens"`}},
			{"mc_stop", lit(`{"messages":[],"stop_sequences":["A","B"]}`),
				[]string{`"stop":["A","B"]`}, []string{`"stop_sequences"`}},
			{"mc_meta_user", lit(`{"messages":[],"metadata":{"user_id":"u1"}}`),
				[]string{`"user":"u1"`}, []string{`"metadata"`}},
		},
		dirRespToChatReq: {
			{"rc_instructions", lit(`{"model":"m","instructions":"SYS","input":"hi"}`),
				[]string{`"content":"SYS"`, `"role":"system"`}, nil},
			{"rc_input_str", lit(`{"model":"m","input":"hello"}`),
				[]string{`{"content":"hello","role":"user"}`}, nil},
			{"rc_msg_text", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"U1"}]}]}`),
				[]string{`"text":"U1"`, `"role":"user"`}, nil},
			{"rc_msg_img", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://e.com/i.png"}]}]}`),
				[]string{`"image_url":{"url":"https://e.com/i.png"}`}, nil},
			{"rc_msg_file", lit(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"f_1"}]}]}`),
				nil, []string{`f_1`}},
			{"rc_fc", lit(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{"a":1}"}]}`),
				[]string{`"tool_calls"`, `"name":"f"`, `"id":"c1"`, `arguments`}, nil},
			{"rc_fco", lit(`{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"OK"}]}`),
				[]string{`"role":"tool"`, `"tool_call_id":"c1"`, `"content":"OK"`}, nil},
			{"rc_reasoning", lit(`{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]}]}`),
				[]string{`"reasoning_content":"think"`}, nil},
			{"rc_encrypted", lit(`{"input":[{"type":"reasoning","encrypted_content":"ENC","summary":[{"type":"summary_text","text":"t"}]}]}`),
				[]string{`"reasoning_content":"t"`}, []string{`ENC`}},
			{"rc_max_out", lit(`{"max_output_tokens":77,"input":"hi"}`),
				[]string{`"max_completion_tokens":77`}, []string{`"max_output_tokens"`}},
			{"rc_tools", lit(`{"input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`),
				[]string{`"tools":[{"function":{`, `"type":"function"`}, []string{`"parameters":{},`}},
			{"rc_tools_strict", lit(`{"input":"hi","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}}]}`),
				[]string{`"strict":true`}, nil},
			{"rc_tool_choice", lit(`{"input":"hi","tool_choice":{"type":"function","name":"f"}}`),
				[]string{`"tool_choice":{"function":{"name":"f"},"type":"function"}`}, nil},
			{"rc_text_format", lit(`{"input":"hi","text":{"format":{"type":"json_schema","name":"S","schema":{"type":"object"},"strict":true}}}`),
				[]string{`"response_format":{"json_schema":{"name":"S","schema":{"type":"object"},"strict":true},"type":"json_schema"}`}, nil},
			{"rc_effort", lit(`{"input":"hi","reasoning":{"effort":"high"}}`),
				[]string{`"reasoning_effort":"high"`}, nil},
			// 有状态字段的策略是 polReject（整请求 400），不是"丢弃"：
			// TestRowCoverage 对 polReject 成员改判"必须报 ErrStatefulFields"。
			{"rc_prev_id", lit(`{"input":"hi","previous_response_id":"resp_1"}`), nil, nil},
		},
		dirMessToChatResp: {
			{"z_text", lit(`{"id":"m1","content":[{"type":"text","text":"HELLO"}]}`),
				[]string{`"content":"HELLO"`}, nil},
			{"z_thinking", lit(`{"id":"m1","content":[{"type":"thinking","thinking":"think"}]}`),
				[]string{`"reasoning_content":"think"`}, nil},
			{"z_tool_use", lit(`{"id":"m1","content":[{"type":"tool_use","id":"tu1","name":"f","input":{}}]}`),
				[]string{`"tool_calls"`, `"id":"tu1"`}, nil},
			{"z_media", lit(`{"id":"m1","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}}]}`),
				nil, []string{`i.png`}},
			{"z_tail", lit(`{"id":"m1","content":[{"type":"unknown_block","text":"ZZZ"}]}`),
				nil, []string{`ZZZ`}},
		},
	}
}

// rowOfMember 取某成员的策略行（找不到返回零值行，pol 为 polUnknown）。
func rowOfMember(d direction, member string) row {
	for _, rw := range rowsOf(d) {
		if shapeName[rw.sh] == member {
			return rw
		}
	}
	return row{}
}

// TestRowCoverage (c)：每个成员至少一条用例，且每条用例的形态断言成立。
func TestRowCoverage(t *testing.T) {
	cases := rowCases(t)
	require.Len(t, cases, int(dirCount), "七个方向都要有用例")
	for d := direction(0); d < dirCount; d++ {
		cs := cases[d]
		covered := map[string]bool{}
		for _, c := range cs {
			body := c.body(t)
			var out []byte
			var err error
			switch d {
			case dirRespToChat, dirRespToMessResp, dirMessToChatResp:
				out, err = ConvertResponse([]byte(body), dirToConvert(d))
			default:
				out, err = ConvertRequest([]byte(body), dirToConvert(d))
			}
			if rowOfMember(d, c.member).pol == polReject {
				// polReject 的语义是"整请求拒绝"，不是"丢弃该字段"：断言它确实
				// 报可识别的 sentinel，而不是静默产出少了东西的报文。
				require.ErrorIs(t, err, ErrStatefulFields, "%s/%s：有状态字段必须整请求拒绝", dirName[d], c.member)
				require.False(t, covered[c.member], "成员 %s 有重复用例", c.member)
				covered[c.member] = true
				continue
			}
			require.NoError(t, err, "%s/%s", dirName[d], c.member)
			s := string(out)
			for _, w := range c.want {
				require.Contains(t, s, w, "%s：期望目标侧出现 %q，实际 %s", c.member, w, s)
			}
			for _, a := range c.absent {
				require.NotContains(t, s, a, "%s：期望目标侧不出现 %q，实际 %s", c.member, a, s)
			}
			require.False(t, covered[c.member], "成员 %s 有重复用例", c.member)
			covered[c.member] = true
		}
		var want []string
		for _, m := range membersOf(d) {
			want = append(want, m.name)
		}
		var got []string
		for name := range covered {
			got = append(got, name)
		}
		require.ElementsMatch(t, want, got, "方向 %s 的逐行用例与成员清单必须一一对应", dirName[d])
	}
}

// dirToConvert 内容模型方向 → 转换器方向（响应面方向映射回其客户端协议）。
func dirToConvert(d direction) domain.ProtocolConvert {
	switch d {
	case dirChatToResp:
		return domain.ProtocolConvertChatToResp
	case dirChatToMess:
		return domain.ProtocolConvertChatToMess
	case dirMessToResp:
		return domain.ProtocolConvertMessToResp
	case dirRespToMess:
		return domain.ProtocolConvertRespToMess
	case dirRespToChat:
		return domain.ProtocolConvertChatToResp
	case dirRespToMessResp:
		return domain.ProtocolConvertMessToResp
	case dirMessToChatResp:
		return domain.ProtocolConvertChatToMess
	case dirMessToChatReq:
		return domain.ProtocolConvertMessToChat
	case dirRespToChatReq:
		return domain.ProtocolConvertRespToChat
	}
	return ""
}

// TestInvariantsOnAllGoldenOutputs (f) 接线：不变量作用到**全部**黄金用例的
// converter 输出上（M1 只跑负例，M2 起接线）。
func TestInvariantsOnAllGoldenOutputs(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			if c.stream {
				return // SSE 帧是增量载荷，(f) 只作用于完整报文
			}
			out := []byte(c.run(t))
			switch dirTarget(c.vocab) {
			case tgtResp:
				require.Empty(t, respAssistantInputTexts(out), "assistant 项不得出现 input_text（RT1）")
				require.Empty(t, respEmptyAssistantItems(out), "assistant 项不得为空")
			case tgtMess:
				require.Empty(t, messAssistantImages(out), "assistant 消息不得出现 image/document 块（J2）")
				require.Empty(t, messOrphanToolResults(out), "不得有孤儿 tool_result（不合成 call）")
				require.Empty(t, messDuplicateCallIDs(out), "call_id 不得重复")
			}
		})
	}
	// 逐行用例的输出同样要过不变量
	for d, cs := range rowCases(t) {
		for _, c := range cs {
			if rowOfMember(d, c.member).pol == polReject {
				continue // polReject 没有产物（整请求 400），不变量无从谈起
			}
			out, err := ConvertRequest([]byte(c.body(t)), dirToConvert(d))
			if err != nil {
				out, err = ConvertResponse([]byte(c.body(t)), dirToConvert(d))
			}
			require.NoError(t, err, "%s/%s", dirName[d], c.member)
			switch dirTarget(d) {
			case tgtResp:
				require.Empty(t, respAssistantInputTexts(out), "%s：assistant 项不得出现 input_text", c.member)
			case tgtMess:
				require.Empty(t, messAssistantImages(out), "%s：assistant 消息不得出现媒体块", c.member)
				require.Empty(t, messOrphanToolResults(out), "%s：不得有孤儿 tool_result", c.member)
			}
		}
	}
}

// --- spec §5.1 验收 ---

// TestImageByteEqualityAcrossPaths §5.1#7：同一张图片经 chat（data URL）与
// mess（base64 + media_type）两条路径 → input_image 部件逐字节相同。
func TestImageByteEqualityAcrossPaths(t *testing.T) {
	chat, err := ConvertRequest([]byte(`{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	mess, err := ConvertRequest([]byte(`{"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`), domain.ProtocolConvertMessToResp)
	require.NoError(t, err)

	part := `{"image_url":"data:image/png;base64,QUJD","type":"input_image"}`
	require.Contains(t, string(chat), part, "chat 的 data URL 路径")
	require.Contains(t, string(mess), part, "mess 的 base64 路径必须产出同一份字节")
}

// TestRespToChatPureRefusal §5.1#4：resp→chat 非流仅 refusal → content 含其
// 文本、finish_reason == content_filter。
func TestRespToChatPureRefusal(t *testing.T) {
	out, err := ConvertResponse([]byte(`{"id":"r1","status":"completed","output":[
		{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot"}]}
	]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "content_filter", arrOf(t, m, "choices")[0].(map[string]any)["finish_reason"])
	msg := arrOf(t, m, "choices")[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "I cannot", msg["content"])
}

// TestRefusalStreaming §5.1#9：response.refusal.delta → chat delta.content /
// mess content_block_delta{text_delta}，且帧内部件类型过词表。
func TestRefusalStreaming(t *testing.T) {
	chatFrames := string(mapAll(t, domain.ProtocolConvertChatToResp,
		"response.refusal.delta", `{"type":"response.refusal.delta","item_id":"i1","output_index":0,"delta":"no"}`,
	))
	require.Contains(t, chatFrames, `{"content":"no"}`)

	// resp 的 refusal 增量 → Messages 的 content_block_delta{text_delta}
	messFrames := string(mapAll(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"r1","model":"m","output":[]}}`,
		"response.refusal.delta", `{"type":"response.refusal.delta","item_id":"i1","output_index":0,"delta":"no"}`,
	))
	require.Contains(t, messFrames, `"type":"text_delta"`)
	require.Contains(t, messFrames, `"text":"no"`)
}

// TestStopReasonSymmetry §5.1#10：A4 三向对称（max_tokens / refusal 各一条）。
func TestStopReasonSymmetry(t *testing.T) {
	// max_tokens（mess → resp 非流）
	out, err := ConvertResponse([]byte(`{"id":"m1","stop_reason":"max_tokens","content":[{"type":"text","text":"x"}]}`), domain.ProtocolConvertRespToMess)
	require.NoError(t, err)
	m := obj(t, out)
	require.Equal(t, "incomplete", m["status"])
	require.Equal(t, "max_output_tokens", m["incomplete_details"].(map[string]any)["reason"])

	// refusal（mess → resp 非流）
	out, err = ConvertResponse([]byte(`{"id":"m1","stop_reason":"refusal","content":[{"type":"text","text":"x"}]}`), domain.ProtocolConvertRespToMess)
	require.NoError(t, err)
	m = obj(t, out)
	require.Equal(t, "incomplete", m["status"])
	require.Equal(t, "content_filter", m["incomplete_details"].(map[string]any)["reason"])

	// refusal（resp → mess 非流）：含 refusal 且无 function_call
	out, err = ConvertResponse([]byte(`{"id":"r1","status":"completed","output":[
		{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}`), domain.ProtocolConvertMessToResp)
	require.NoError(t, err)
	require.Equal(t, "refusal", obj(t, out)["stop_reason"])

	// max_tokens（resp → mess 非流）
	out, err = ConvertResponse([]byte(`{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`), domain.ProtocolConvertMessToResp)
	require.NoError(t, err)
	require.Equal(t, "max_tokens", obj(t, out)["stop_reason"])

	// 流式：mess stop_reason=max_tokens → resp status=incomplete
	frames := string(mapAll(t, domain.ProtocolConvertRespToMess,
		"message_start", `{"type":"message_start","message":{"id":"m1","model":"m","usage":{"input_tokens":1}}}`,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":1}}`,
		"message_stop", `{"type":"message_stop"}`,
	))
	require.Contains(t, frames, `"status":"incomplete"`)
	require.Contains(t, frames, `"reason":"max_output_tokens"`)

	// 流式：resp refusal → chat finish_reason=content_filter
	frames = string(mapAll(t, domain.ProtocolConvertChatToResp,
		"response.created", `{"type":"response.created","response":{"id":"r1","model":"m","created_at":1,"output":[]}}`,
		"response.completed", `{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[
			{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]}}`,
	))
	require.Contains(t, frames, `"finish_reason":"content_filter"`)
}

// TestNoFileIDCrossesProtocols §5.1#6 的否定面：任何 file_id 形态都不得出现在
// 目标侧（J10）。
func TestNoFileIDCrossesProtocols(t *testing.T) {
	cases := []struct {
		dir  domain.ProtocolConvert
		body string
	}{
		{domain.ProtocolConvertMessToResp, `{"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"file","file_id":"FID"}},
			{"type":"document","source":{"type":"file","file_id":"FID"}}]}]}`},
		{domain.ProtocolConvertRespToMess, `{"input":[{"type":"message","role":"user","content":[
			{"type":"input_image","file_id":"FID"},{"type":"input_file","file_id":"FID"}]}]}`},
		{domain.ProtocolConvertChatToResp, `{"messages":[{"role":"user","content":[
			{"type":"file","file":{"file_id":"FID"}}]}]}`},
		{domain.ProtocolConvertChatToMess, `{"messages":[{"role":"user","content":[
			{"type":"file","file":{"file_id":"FID"}}]}]}`},
	}
	for _, c := range cases {
		out, err := ConvertRequest([]byte(c.body), c.dir)
		require.NoError(t, err)
		require.False(t, strings.Contains(string(out), "FID"), "%s：file_id 不得跨协议（J10）", c.dir)
	}
}
