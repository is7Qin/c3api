// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// 逐字节黄金：内容模型重构（行表分派）不得改变除 spec §4 列明的缺陷修正之外的
// 任何输出字节。语料覆盖四个请求方向、四个响应方向与四个流式方向，每个用例的
// 期望字节存在 testdata/golden/<name>.txt（由重构前的实现产出，见实现报告）。
//
// 更新方式：新增用例先 `GOLDEN_UPDATE=1 go test ./internal/protoconv -run TestGoldenOutputs`
// 只写未登记的 name，review 产物后 `REVIEW_UPDATE=1 go test -run TestGoldenReviewed`
// 登记（spec §8(a2)）；已在册的用例禁止重采。

const goldenDir = "testdata/golden"

type goldenCase struct {
	name string
	run  func(t *testing.T) string // 返回被钉住的输出字节（流式用例把帧拼起来）
	// fixed 该用例覆盖 spec §4 的三处缺陷修正之一：期望字节在 M1 之后重采，
	// 不参与「与重构前逐字节一致」的断言。
	fixed bool
	// vocab 该用例的目标协议所属方向（(d′) 按它的目标协议取词表）。
	vocab direction
	// stream 该用例是 SSE 帧序列（帧级词表校验）。
	stream bool
}

func goldenRequest(dir domain.ProtocolConvert, body string) func(t *testing.T) string {
	return func(t *testing.T) string {
		out, err := ConvertRequest([]byte(body), dir)
		require.NoError(t, err)
		return string(out)
	}
}

func goldenResponse(dir domain.ProtocolConvert, body string) func(t *testing.T) string {
	return func(t *testing.T) string {
		out, err := ConvertResponse([]byte(body), dir)
		require.NoError(t, err)
		return string(out)
	}
}

// goldenStream 把一串事件映射成帧，帧之间用 "\n---\n" 分隔（帧自身含空行，
// 故用显式分隔符避免歧义）。
func goldenStream(dir domain.ProtocolConvert, events []benchEv) func(t *testing.T) string {
	return func(t *testing.T) string {
		m := NewStreamMapper(dir)
		var out []byte
		for i, ev := range events {
			frame, drop := m.Map(ev.name, ev.data)
			if drop {
				continue
			}
			if i > 0 && len(out) > 0 {
				out = append(out, "\n---\n"...)
			}
			out = append(out, frame...)
		}
		return string(out)
	}
}

// goldenStreamEOF 与 goldenStream 相同，但**不喂 `[DONE]`**：模拟上游正常 EOF
// （sserelay.Config.Terminal 被调用一次），用来钉住"无 [DONE] 也必须收尾"。
func goldenStreamEOF(dir domain.ProtocolConvert, events []benchEv) func(t *testing.T) string {
	return func(t *testing.T) string {
		m := NewStreamMapper(dir)
		var out []byte
		for _, ev := range events {
			frame, drop := m.Map(ev.name, ev.data)
			if drop {
				continue
			}
			if len(out) > 0 {
				out = append(out, "\n---\n"...)
			}
			out = append(out, frame...)
		}
		if fin := m.Finish(); len(fin) > 0 {
			if len(out) > 0 {
				out = append(out, "\n---\n"...)
			}
			out = append(out, fin...)
		}
		return string(out)
	}
}

// goldenCases 语料。每条用例的键是文件名的前缀，内容固定不变（改动会改期望
// 字节，必须同时改这里与 testdata）。
func goldenCases() []goldenCase {
	return []goldenCase{
		{
			// spec (f)②：真进 T 且带 description（锁写序与 hint 位置）。
			// 分支带 additionalProperties:false ⇒ 4c 丢弃分支约束 ⇒ deg=simplify ⇒ 追加提示。
			name:  "resp_to_mess_tools_dialect",
			vocab: dirRespToMess,
			fixed: true,
			run:   goldenRequest(domain.ProtocolConvertRespToMess, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}]}}]}`),
		},
		{
			// spec (f)①：P_in 成立 ⇒ 原字节（锁 I2，零改动）。
			name:  "resp_to_mess_tools_idle",
			vocab: dirRespToMess,
			fixed: true,
			run:   goldenRequest(domain.ProtocolConvertRespToMess, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}]}`),
		},
		{
			// (e)④：`toolsMessFromChat` 的具名金格（chat 源是 {function:{…}} 包装）。
			// 真进 T + `parameters` 缺失的无参工具（写 `input_schema` 但不加 hint）。
			name:  "chat_to_mess_tools_dialect",
			vocab: dirChatToMess,
			fixed: true,
			run:   goldenRequest(domain.ProtocolConvertChatToMess, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","oneOf":[{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}]}}},{"type":"function","function":{"name":"noparam","description":"e"}},{"type":"web_search"}]}`),
		},
		{
			name:  "chat_to_resp_typical",
			vocab: dirChatToResp,
			fixed: true, // §4 修正 1：assistant 文本 → output_text + annotations
			run: goldenRequest(domain.ProtocolConvertChatToResp, `{"model":"m","messages":[
				{"role":"system","content":"sys"},
				{"role":"developer","content":[{"type":"text","text":"dev A"},{"type":"text","text":"dev B"}]},
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"calling","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
				{"role":"tool","tool_call_id":"c1","content":"ok"},
				{"role":"function","name":"g","content":[{"type":"text","text":"legacy"}]}
			],"max_tokens":10,"user":"u1","store":true}`),
		},
		{
			name:  "chat_to_resp_user_parts",
			vocab: dirChatToResp,
			run: goldenRequest(domain.ProtocolConvertChatToResp, `{"messages":[{"role":"user","content":[
				{"type":"text","text":"p1"},
				{"type":"image_url","image_url":{"url":"https://e.com/i.png","detail":"high"}},
				{"type":"image_url","image_url":"data:image/png;base64,QUJD"},
				{"type":"image_url","image_url":"QUJDRA=="},
				{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}},
				{"type":"file","file":{"file_id":"f1"}},
				{"type":"file","file":{"file_data":"QUJD","filename":"a.txt"}},
				{"type":"unknown_part"}
			]}]}`),
		},
		{
			name:  "chat_to_resp_asst_parts",
			vocab: dirChatToResp,
			fixed: true, // §4 修正 1+2：assistant 文本 → output_text；assistant 媒体 → drop
			run: goldenRequest(domain.ProtocolConvertChatToResp, `{"messages":[{"role":"assistant","content":[
				{"type":"text","text":"answer"},
				{"type":"image_url","image_url":"https://e.com/i.png"},
				{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}},
				{"type":"file","file":{"file_data":"QUJD"}},
				{"type":"refusal","refusal":"no"}
			]},{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]}]}`),
		},
		{
			name:  "chat_to_mess_typical",
			vocab: dirChatToMess,
			run: goldenRequest(domain.ProtocolConvertChatToMess, `{"model":"m","max_completion_tokens":7,"messages":[
				{"role":"system","content":"s1"},
				{"role":"developer","content":[{"type":"text","text":"s2"}]},
				{"role":"user","content":"hi"},
				{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":"https://e.com/i.png"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"},{"type":"image_url","image_url":"QUJD"},{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}},{"type":"file","file":{"file_id":"f"}}]},
				{"role":"assistant","content":"thinking","tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"res"}]},
				{"role":"function","name":"g","content":"legacy"}
			]}`),
		},
		{
			name:  "chat_to_mess_asst_media",
			vocab: dirChatToMess,
			fixed: true, // §4 修正 3：assistant 消息里不得出现 image 块
			run: goldenRequest(domain.ProtocolConvertChatToMess, `{"messages":[
				{"role":"assistant","content":"ok"},
				{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]},
				{"role":"assistant","content":[{"type":"text","text":"kept"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}
			]}`),
		},
		{
			name:  "mess_to_resp_typical",
			vocab: dirMessToResp,
			run: goldenRequest(domain.ProtocolConvertMessToResp, `{"model":"m","system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"max_tokens":9,
				"messages":[
					{"role":"user","content":"hi"},
					{"role":"assistant","content":[
						{"type":"text","text":"t1"},
						{"type":"tool_use","id":"tu1","name":"f","input":{"x":1}}
					]},
					{"role":"user","content":[
						{"type":"tool_result","tool_use_id":"tu1","content":"str"},
						{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"arr"}]},
						{"type":"tool_result","tool_use_id":"tu3","content":[{"type":"text","text":""}]},
						{"type":"tool_result","tool_use_id":"tu4","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}}]},
						{"type":"tool_result","tool_use_id":"tu5"},
						{"type":"text","text":"after"}
					]}
				],"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"high"}}`),
		},
		{
			name:  "mess_to_resp_user_parts",
			vocab: dirMessToResp,
			run: goldenRequest(domain.ProtocolConvertMessToResp, `{"messages":[{"role":"user","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}},
				{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}},
				{"type":"image","source":{"type":"file","file_id":"f1"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA"}},
				{"type":"document","source":{"type":"url","url":"https://e.com/d.pdf"}},
				{"type":"document","source":{"type":"file","file_id":"f2"}},
				{"type":"search_result","source":"s","title":"t","content":[{"type":"text","text":"x"}]}
			]},{"role":"assistant","content":[
				{"type":"thinking","thinking":"th","signature":"sig"},
				{"type":"redacted_thinking","data":"deadbeef"},
				{"type":"search_result","source":"s","title":"t","content":[]},
				{"type":"text","text":"out"}
			]}]}`),
		},
		{
			name:  "resp_to_mess_typical",
			vocab: dirRespToMess,
			run: goldenRequest(domain.ProtocolConvertRespToMess, `{"model":"m","instructions":"ins","max_output_tokens":11,
				"input":[
					{"type":"message","role":"system","content":[{"type":"input_text","text":"sys"}]},
					{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},
					{"type":"message","role":"user","content":[{"type":"input_text","text":"u1"},{"type":"output_text","text":"u2"}]},
					{"type":"message","role":"assistant","content":[{"type":"input_text","text":"a1"},{"type":"output_text","text":"a2"}]},
					{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},
					{"type":"function_call_output","call_id":"c1","output":"done"},
					{"type":"function_call_output","call_id":"c2","output":[{"type":"input_text","text":"arr"},{"type":"text","text":"arr2"}]},
					{"type":"function_call_output","call_id":"c3","output":[{"type":"input_image","image_url":"https://e.com/i.png"}]},
					{"type":"item_reference","id":"x"},
					{"type":"unknown_item"}
				]}`),
		},
		{
			// §4 修正之外的第四处：resp→mess 的 assistant 图片 → drop。
			// (d′) 门禁与 J2/§5.1#3 都要求它，M1 无法在保留该行为的同时全绿。
			name:  "resp_to_mess_media",
			fixed: true,
			vocab: dirRespToMess,
			run: goldenRequest(domain.ProtocolConvertRespToMess, `{"input":[
					{"type":"message","role":"user","content":[
						{"type":"input_image","image_url":"https://e.com/i.png"},
						{"type":"input_image","image_url":"data:image/png;base64,QUJD"},
						{"type":"input_image","image_url":"ftp://e.com/i.png"},
						{"type":"input_image","file_id":"f1"},
						{"type":"input_file","file_url":"https://e.com/d.pdf"},
						{"type":"input_file","file_id":"f2"},
						{"type":"input_file","file_data":"QUJD","filename":"a.txt"},
						{"type":"input_audio","input_audio":{"data":"AA","format":"wav"}}
					]},
					{"type":"message","role":"assistant","content":[
						{"type":"input_image","image_url":"https://e.com/i.png"},
						{"type":"refusal","refusal":"no"}
					]}
				]}`),
		},
		{
			name:  "resp_to_mess_string_input",
			vocab: dirRespToMess,
			run:   goldenRequest(domain.ProtocolConvertRespToMess, `{"input":"plain text","model":"m"}`),
		},
		{
			name:  "resp_to_chat_response",
			vocab: dirRespToChat,
			run: goldenResponse(domain.ProtocolConvertChatToResp, `{"id":"r1","model":"m","created_at":17,"status":"completed","output":[
					{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"think"}]},
					{"type":"message","role":"assistant","content":[
						{"type":"output_text","text":"hello"},
						{"type":"refusal","refusal":"no"},
						{"type":"output_text","text":" world"}
					]},
					{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
					{"type":"unknown_item"}
				],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7,"input_tokens_details":{"cached_tokens":1,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}`),
		},
		{
			name:  "resp_to_mess_response",
			vocab: dirRespToMessResp,
			run: goldenResponse(domain.ProtocolConvertMessToResp, `{"id":"r1","model":"m","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[
					{"type":"reasoning","encrypted_content":"enc","summary":[{"type":"summary_text","text":"think"}]},
					{"type":"message","role":"assistant","content":[
						{"type":"output_text","text":"hello"},
						{"type":"refusal","refusal":"no"}
					]},
					{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},
					{"type":"unknown_item"}
				],"usage":{"input_tokens":9,"output_tokens":4,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}`),
		},
		{
			name:  "mess_to_chat_response",
			vocab: dirMessToChatResp,
			run: goldenResponse(domain.ProtocolConvertChatToMess, `{"id":"msg_1","model":"m","stop_reason":"tool_use","content":[
					{"type":"text","text":"hello"},
					{"type":"thinking","thinking":"think"},
					{"type":"redacted_thinking","data":"dead"},
					{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}},
					{"type":"document","source":{"type":"url","url":"https://e.com/d.pdf"}},
					{"type":"unknown_block"}
				],"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}`),
		},
		{
			name:  "mess_to_resp_response",
			vocab: dirMessToResp,
			run: goldenResponse(domain.ProtocolConvertRespToMess, `{"id":"msg_1","model":"m","stop_reason":"max_tokens","content":[
					{"type":"thinking","thinking":"think","signature":"sig"},
					{"type":"redacted_thinking","data":"dead"},
					{"type":"text","text":"hello"},
					{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}},
					{"type":"unknown_block"}
				],"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}`),
		},
		{
			// M2a：mess → chat 请求面，纯文本路径（顶层 system 折叠 + 多轮）。
			name:  "mess_to_chat_req_text",
			vocab: dirMessToChatReq,
			run: goldenRequest(domain.ProtocolConvertMessToChat, `{"model":"m","max_tokens":64,"system":"be nice","messages":[
				{"role":"user","content":"hello"},
				{"role":"assistant","content":[{"type":"text","text":"hi "},{"type":"text","text":"there"}]},
				{"role":"user","content":[{"type":"text","text":"again"}]}
			]}`),
		},
		{
			// M2a：工具路径。tool_use → tool_calls、tool_result → 就地 role:"tool"
			// 消息、tool_result 内的图片挪到紧随其后的 user 消息、tools 走嵌套写法。
			name:  "mess_to_chat_req_tools",
			vocab: dirMessToChatReq,
			run: goldenRequest(domain.ProtocolConvertMessToChat, `{"model":"m","messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]},{"type":"text","text":"next"}]}
			],"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}],"tool_choice":{"type":"auto"},"stop_sequences":["STOP"]}`),
		},
		{
			// M2a：图片 + system 多块折叠 + 思考预算 + stop 截断 + 流式
			// stream_options 合并（强制 include_usage）；effort 非空时丢弃
			// temperature/top_p。
			name:  "mess_to_chat_req_image_system",
			vocab: dirMessToChatReq,
			run: goldenRequest(domain.ProtocolConvertMessToChat, `{"model":"m","system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],"thinking":{"type":"enabled","budget_tokens":2048},"messages":[
				{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}},{"type":"text","text":"what"}]}
			],"stop_sequences":["a","b","c","d","e"],"temperature":0.7,"top_p":0.9,"metadata":{"user_id":"u1"},"stream":true,"stream_options":{"include_obfuscation":false}}`),
		},
		{
			// M2a：多轮 tool_calls↔tool_result 配对（spec §8(a) 的"配对"用例）。
			// 一条 assistant 带 2 个 tool_use ⇒ 紧随 2 条 role:"tool"；第二条
			// tool_result 带 is_error ⇒ "Error: " 前缀；末轮 tool_result 后还有
			// 文本 ⇒ 合成同一条 user 消息。
			name:  "mess_to_chat_req_multiturn",
			vocab: dirMessToChatReq,
			run: goldenRequest(domain.ProtocolConvertMessToChat, `{"model":"m","max_tokens":256,"system":"sys","messages":[
				{"role":"user","content":"u1"},
				{"role":"assistant","content":[{"type":"text","text":"t1"},{"type":"tool_use","id":"a","name":"f","input":{"x":1}},{"type":"tool_use","id":"b","name":"g","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ra"},{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"rb"}]},
				{"role":"assistant","content":[{"type":"text","text":"t2"},{"type":"tool_use","id":"c","name":"f","input":{"x":2}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"rc"}]},{"type":"text","text":"u2"}]}
			],"tools":[{"name":"f","description":"df","input_schema":{"type":"object","properties":{"x":{"type":"integer"}}}},{"name":"g","description":"dg","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"stop_sequences":["END"],"metadata":{"user_id":"u9"}}`),
		},
		{
			// M2b：resp → chat 请求面，纯文本路径（instructions → system 消息）。
			name:  "resp_to_chat_req_text",
			vocab: dirRespToChatReq,
			run:   goldenRequest(domain.ProtocolConvertRespToChat, `{"model":"m","instructions":"be nice","input":"hello","max_output_tokens":64}`),
		},
		{
			// M2b：工具路径。内置工具（web_search_preview）被读取侧过滤，
			// function_call 合并进同一条 assistant，function_call_output → role:"tool"，
			// tool_choice 从扁平重排成嵌套。
			name:  "resp_to_chat_req_tools",
			vocab: dirRespToChatReq,
			run: goldenRequest(domain.ProtocolConvertRespToChat, `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
				{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},
				{"type":"function_call_output","call_id":"c1","output":"ok"},
				{"type":"web_search_call","id":"ws1","status":"completed"}
			],"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"integer"}}},"strict":true},{"type":"web_search_preview"}],"tool_choice":{"type":"function","name":"f"},"parallel_tool_calls":false}`),
		},
		{
			// M2b：图片 + system/developer 消息 + 文件丢弃 + text.format 结构重排 +
			// reasoning.effort 直传（effort 非空 ⇒ 丢弃 temperature/top_p）。
			name:  "resp_to_chat_req_image_system",
			vocab: dirRespToChatReq,
			run: goldenRequest(domain.ProtocolConvertRespToChat, `{"model":"m","instructions":"sys","input":[
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},
				{"type":"message","role":"user","content":[
					{"type":"input_text","text":"what"},
					{"type":"input_image","image_url":"https://e.com/i.png"},
					{"type":"input_file","file_id":"f_1"}
				]}
			],"text":{"format":{"type":"json_schema","name":"S","schema":{"type":"object"},"strict":true}},"reasoning":{"effort":"xhigh"},"temperature":0.7,"top_p":0.9,"user":"u1","stream":true}`),
		},
		{
			// M2b：多轮配对（spec §8(a) 的配对用例）。reasoning 与两个连续
			// function_call 落进**同一条** assistant（chat 的 tool_calls 必须紧邻
			// 其结果），两条结果按声明顺序紧邻输出，之后的 user 文本另起一条。
			name:  "resp_to_chat_req_multiturn",
			vocab: dirRespToChatReq,
			run: goldenRequest(domain.ProtocolConvertRespToChat, `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"u1"}]},
				{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
				{"type":"function_call","call_id":"a","name":"f","arguments":"{\"x\":1}"},
				{"type":"function_call","call_id":"b","name":"g","arguments":"{}"},
				{"type":"function_call_output","call_id":"a","output":"ra"},
				{"type":"function_call_output","call_id":"b","output":[{"type":"input_text","text":"rb"}]},
				{"type":"function_call_output","call_id":"ghost","output":"lost"},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"u2"}]}
			]}`),
		},
		{
			// M2c：chat → mess 响应面，纯文本（usage 走 §5.4 净额口径）。
			name:  "chat_to_mess_resp_text",
			vocab: dirChatToMessResp,
			run:   goldenResponse(domain.ProtocolConvertMessToChat, `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`),
		},
		{
			// M2c：工具 + 思考 + 用量明细。thinking 块不写 signature（chat 无签名
			// 通道），tool_use 的 input 由 arguments 解析而来。
			name:  "chat_to_mess_resp_tools",
			vocab: dirChatToMessResp,
			run:   goldenResponse(domain.ProtocolConvertMessToChat, `{"id":"c2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"calling","reasoning_content":"think","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},{"id":"c2","type":"function","function":{"name":"g","arguments":"{oops"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":7}}}`),
		},
		{
			// M2c：length → max_tokens；空 content → **仍写一个空 text 块**
			// （Messages 客户端对 content:[] 会报协议错）。
			name:  "chat_to_mess_resp_length",
			vocab: dirChatToMessResp,
			run:   goldenResponse(domain.ProtocolConvertMessToChat, `{"id":"c3","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`),
		},
		{
			// M2c：chat → resp 响应面，纯文本（output 项 id 确定性合成）。
			name:  "chat_to_resp_resp_text",
			vocab: dirChatToRespResp,
			run:   goldenResponse(domain.ProtocolConvertRespToChat, `{"id":"c1","object":"chat.completion","created":99,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`),
		},
		{
			// M2c：工具 + 思考 + 用量明细。output 顺序固定 reasoning → message →
			// function_call；input_tokens_details 的**两个键恒写**（官方必填）。
			name:  "chat_to_resp_resp_tools",
			vocab: dirChatToRespResp,
			run:   goldenResponse(domain.ProtocolConvertRespToChat, `{"id":"c2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"calling","reasoning_content":"think","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":7}}}`),
		},
		{
			// M2c：length → status:"incomplete" + incomplete_details.reason。
			name:  "chat_to_resp_resp_incomplete",
			vocab: dirChatToRespResp,
			run:   goldenResponse(domain.ProtocolConvertRespToChat, `{"id":"c3","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"trunc"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`),
		},
		{
			name:   "stream_resp_to_chat",
			vocab:  dirRespToChat,
			stream: true,
			run: goldenStream(domain.ProtocolConvertChatToResp, []benchEv{
				{"response.created", []byte(`{"type":"response.created","response":{"id":"r1","model":"m","created_at":5,"status":"in_progress","output":[],"usage":null}}`)},
				{"response.output_text.delta", []byte(`{"type":"response.output_text.delta","item_id":"i1","output_index":0,"delta":"he"}`)},
				{"response.reasoning_summary_text.delta", []byte(`{"type":"response.reasoning_summary_text.delta","delta":"th"}`)},
				{"response.output_item.added", []byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"f","arguments":""}}`)},
				{"response.function_call_arguments.delta", []byte(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`)},
				{"response.completed", []byte(`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[{"type":"function_call","call_id":"c1"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)},
			}),
		},
		{
			name:   "stream_resp_to_mess",
			vocab:  dirMessToResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertRespToMess, []benchEv{
				{"message_start", []byte(`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":3}}}`)},
				{"content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)},
				{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"he"}}`)},
				{"content_block_stop", []byte(`{"type":"content_block_stop","index":0}`)},
				{"content_block_start", []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"f"}}`)},
				{"content_block_delta", []byte(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)},
				{"content_block_stop", []byte(`{"type":"content_block_stop","index":1}`)},
				{"message_delta", []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`)},
				{"message_stop", []byte(`{"type":"message_stop"}`)},
			}),
		},
		{
			name:   "stream_mess_to_resp",
			vocab:  dirRespToMessResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertMessToResp, []benchEv{
				{"response.created", []byte(`{"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress","output":[]}}`)},
				{"response.output_item.added", []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}}`)},
				{"response.output_text.delta", []byte(`{"type":"response.output_text.delta","item_id":"i1","output_index":0,"delta":"he"}`)},
				{"response.output_item.done", []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","content":[{"type":"output_text","text":"he"}]}}`)},
				{"response.completed", []byte(`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)},
			}),
		},
		{
			name:   "stream_mess_to_chat",
			vocab:  dirMessToChatResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertChatToMess, []benchEv{
				{"message_start", []byte(`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":3}}}`)},
				{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"he"}}`)},
				{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"th"}}`)},
				{"content_block_start", []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"f"}}`)},
				{"content_block_delta", []byte(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)},
				{"message_delta", []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)},
				{"message_stop", []byte(`{"type":"message_stop"}`)},
			}),
		},
		{
			// M3：chat 上游纯文本流 → mess 帧（message_start 补发 + 文本块 +
			// finish_reason 只记录 + usage 末帧 + [DONE] 收尾）。
			name:   "chat_to_mess_stream_text",
			vocab:  dirChatToMessResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertMessToChat, []benchEv{
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`)},
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`)},
				{"", []byte(`[DONE]`)},
			}),
		},
		{
			// M3：思考 + 分片工具调用（arguments 分两片）+ tool_calls 停因。
			name:   "chat_to_mess_stream_tools",
			vocab:  dirChatToMessResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertMessToChat, []benchEv{
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"th"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`)},
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)},
				{"", []byte(`[DONE]`)},
			}),
		},
		{
			// M3：**上游不发 [DONE]** 的正常 EOF（走 Finish 收尾，恰好一次）。
			name:   "chat_to_mess_stream_eof",
			vocab:  dirChatToMessResp,
			stream: true,
			run: goldenStreamEOF(domain.ProtocolConvertMessToChat, []benchEv{
				{"", []byte(`{"id":"c3","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c3","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":null}`)},
			}),
		},
		{
			// M3：chat 上游纯文本流 → resp 帧（response.created + output_item +
			// delta + done + completed）。
			name:   "chat_to_resp_stream_text",
			vocab:  dirChatToRespResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertRespToChat, []benchEv{
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`)},
				{"", []byte(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"completion_tokens_details":{"reasoning_tokens":7}}}`)},
				{"", []byte(`[DONE]`)},
			}),
		},
		{
			// M3：思考 + 工具调用（resp 目标是 reasoning / function_call 两个项）。
			name:   "chat_to_resp_stream_tools",
			vocab:  dirChatToRespResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertRespToChat, []benchEv{
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c2","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`)},
				{"", []byte(`[DONE]`)},
			}),
		},
		{
			// M3：length → status:"incomplete" + incomplete_details。
			name:   "chat_to_resp_stream_length",
			vocab:  dirChatToRespResp,
			stream: true,
			run: goldenStream(domain.ProtocolConvertRespToChat, []benchEv{
				{"", []byte(`{"id":"c3","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"trunc"},"finish_reason":null}],"usage":null}`)},
				{"", []byte(`{"id":"c3","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":null}`)},
				{"", []byte(`[DONE]`)},
			}),
		},
	}
}

// TestGoldenCorpusSize spec §8(g)：语料条数写死（不用变量派生），新增/删除用例
// 必须同时改这里。当前 35 = 既有 21 + 两个请求面各 4（3 + 多轮配对）+ 两个响应面
// 各 3 + 两个 chat 源流式方向各 3（M3）。
func TestGoldenCorpusSize(t *testing.T) {
	require.Len(t, goldenCases(), 41)
}

// TestGoldenOutputs 逐字节钉住四个请求/响应方向与四个流式方向的输出。
func TestGoldenOutputs(t *testing.T) {
	update := os.Getenv("GOLDEN_UPDATE") == "1"
	reviewed := readReviewed(t)
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			got := c.run(t)
			path := filepath.Join(goldenDir, c.name+".txt")
			if update {
				// spec §8(a2)：在册用例禁止重采。期望字节一旦被 review 过，就不能
				// 用一条命令把"实现改坏了"变成"期望改对了"。
				if _, ok := reviewed[c.name]; ok {
					t.Fatalf("golden %q 已在 %s 登记，禁止用 GOLDEN_UPDATE=1 重采；"+
						"确属规格变更时先删登记行再重采", c.name, reviewedPath)
				}
				require.NoError(t, os.MkdirAll(goldenDir, 0o755))
				require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			note := "输出字节必须与重构前逐字节一致"
			if c.fixed {
				note = "该用例覆盖 spec §4 的缺陷修正，差异必须与修正条目逐条对照"
			}
			require.Equal(t, string(want), got, "%s（%s）", note, c.name)
		})
	}
}

// reviewedPath 已 review 过的 golden 登记表（name + 文件字节的 sha256）。
const reviewedPath = "testdata/golden/reviewed.tsv"

// readReviewed 读登记表（name → sha256）。文件缺失视为空表。
func readReviewed(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(reviewedPath)
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	require.NoError(t, err)
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		require.Len(t, f, 2, "reviewed.tsv 每行两列：name/sha256")
		out[f[0]] = f[1]
	}
	return out
}

// TestGoldenReviewed spec §8(a2)：golden 防重采。
//
// 定位要说清：它是**提高作弊的可见成本**，不是独立真值（真值见 (a3) 契约表与
// (f) 端到端）。所以它的断言只有三条：① 每个用例都登记过；② 磁盘上的字节与
// 登记值一致；③ 登记表里没有指向已不存在用例的死行。
//
// 新增用例的正常流程：`GOLDEN_UPDATE=1 go test -run TestGoldenOutputs`（只写
// 未登记的 name）→ 人工 review 产物 → `REVIEW_UPDATE=1 go test -run TestGoldenReviewed`
// 把当前字节登记进表。
func TestGoldenReviewed(t *testing.T) {
	if os.Getenv("REVIEW_UPDATE") == "1" {
		var b strings.Builder
		for _, c := range goldenCases() {
			sum, err := fileSHA256(filepath.Join(goldenDir, c.name+".txt"))
			require.NoError(t, err, "用例 %q 还没有 golden 文件", c.name)
			b.WriteString(c.name + "\t" + sum + "\n")
		}
		require.NoError(t, os.WriteFile(reviewedPath, []byte(b.String()), 0o644))
		return
	}
	reg := readReviewed(t)
	live := map[string]bool{}
	for _, c := range goldenCases() {
		live[c.name] = true
		want, ok := reg[c.name]
		require.True(t, ok, "golden %q 未在 %s 登记（新增用例须走 GOLDEN_UPDATE + REVIEW_UPDATE）", c.name, reviewedPath)
		sum, err := fileSHA256(filepath.Join(goldenDir, c.name+".txt"))
		require.NoError(t, err)
		require.Equal(t, want, sum, "golden %q 的字节与登记值不一致：期望字节被改动过，必须由 review 显式确认", c.name)
	}
	for name := range reg {
		require.True(t, live[name], "reviewed.tsv 登记了已不存在的用例 %q（删用例要同步删登记行）", name)
	}
}
