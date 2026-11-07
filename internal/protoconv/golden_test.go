// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// 逐字节黄金：内容模型重构（行表分派）不得改变除 spec §4 列明的缺陷修正之外的
// 任何输出字节。语料覆盖四个请求方向、四个响应方向与四个流式方向，每个用例的
// 期望字节存在 testdata/golden/<name>.txt（由重构前的实现产出，见实现报告）。
//
// 更新方式：GOLDEN_UPDATE=1 go test ./internal/protoconv -run TestGoldenOutputs

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
	}
}

// TestGoldenOutputs 逐字节钉住四个请求/响应方向与四个流式方向的输出。
func TestGoldenOutputs(t *testing.T) {
	update := os.Getenv("GOLDEN_UPDATE") == "1"
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			got := c.run(t)
			path := filepath.Join(goldenDir, c.name+".txt")
			if update {
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
