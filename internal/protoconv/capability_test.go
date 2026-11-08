// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestCapabilityTableComplete 能力表必须逐一登记 domain 的每个方向：漏登记
// 会退化成零值（三项 false）→ 配置层 400 + 流式 400，表现为"这个方向不可用"
// 而没有任何编译期或启动期报错，属于最难定位的一类缺口。
func TestCapabilityTableComplete(t *testing.T) {
	all := domain.AllProtocolConverts()
	require.Len(t, dirCaps, len(all), "能力表条目数必须等于方向全集")
	for _, pc := range all {
		_, ok := dirCaps[pc]
		require.True(t, ok, "方向 %q 未登记能力表（会退化成三项 false）", pc)
	}
}

// TestCapabilityExistingDirectionsGA 既有 4 方向早已上线，三项恒 true——
// 任何一次改动把它们置 false 都会让存量组配置立刻 400。
func TestCapabilityExistingDirectionsGA(t *testing.T) {
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertChatToResp,
		domain.ProtocolConvertMessToResp,
		domain.ProtocolConvertRespToMess,
		domain.ProtocolConvertChatToMess,
	} {
		c := Capability(pc)
		require.True(t, c.Request, "%q 请求面能力丢失", pc)
		require.True(t, c.Response, "%q 响应面能力丢失", pc)
		require.True(t, c.Stream, "%q 流式能力丢失", pc)
		require.True(t, StreamSupported(pc), "%q StreamSupported 不一致", pc)
	}
}

// TestCapabilityNewDirections 两个新方向三项全真（M2 请求/响应面 + M3 流式）。
// 这张表是"接线进度"的显式账本：任何一项回到 false 都必须同步改这里（并说明
// 为什么退回去），不是恒真断言。
func TestCapabilityNewDirections(t *testing.T) {
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertMessToChat,
		domain.ProtocolConvertRespToChat,
	} {
		c := Capability(pc)
		require.Equal(t, DirectionCaps{Request: true, Response: true, Stream: true}, c,
			"%q 的能力三元组与接线进度不符", pc)
		require.True(t, StreamSupported(pc), "%q StreamSupported 与能力表不一致", pc)
		// 三项全真 ⇒ 已 GA：配置层不再拒绝（service 的闸门读同一张表）。
		require.True(t, c.Request && c.Response && c.Stream, "%q 三项应全真", pc)
	}
}

// TestUnsupportedDirectionSentinel 未接线方向必须返回 **可 errors.Is 的 sentinel**
// （调用方靠它区分"未支持方向"与"请求体非法"；字符串匹配会在改文案时静默失效）。
//
// 注意本测试只覆盖 sentinel 的**排他性**（未登记的方向值 → 必须是 sentinel；
// 已登记方向 → 绝不能是 sentinel）。"能力位 == 真的接线"的**双向等价**由
// TestCapabilityWiringEquivalence 用最小合法体逐方向断言（§8 c3）。
func TestUnsupportedDirectionSentinel(t *testing.T) {
	// 未登记的方向值必须返回 sentinel（两个新方向已全接线，故改用非法方向值：
	// sentinel 的意义是"配置/部署与实现不一致"，不是"某个方向名"）。
	for _, f := range []func([]byte, domain.ProtocolConvert) ([]byte, error){ConvertRequest, ConvertResponse} {
		out, err := f([]byte(`{"id":"x"}`), domain.ProtocolConvert("no_such_direction"))
		require.ErrorIs(t, err, ErrUnsupportedDirection)
		// spec §2.3：未支持方向只允许**一处** `return nil, ErrUnsupportedDirection`。
		require.Nil(t, out, "未支持方向不得返回产物（调用方按 err 判空之外还可能在错误路径复用 out）")
	}
	// 已接线方向绝不能返回该 sentinel（否则会把正常请求误判成"方向不支持"）。
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertChatToResp,
		domain.ProtocolConvertMessToResp,
		domain.ProtocolConvertRespToMess,
		domain.ProtocolConvertChatToMess,
		domain.ProtocolConvertMessToChat,
		domain.ProtocolConvertRespToChat,
	} {
		_, err := ConvertRequest([]byte(`{"model":"m","messages":[]}`), pc)
		require.False(t, errors.Is(err, ErrUnsupportedDirection), "已接线方向 %q 不应返回 ErrUnsupportedDirection", pc)
	}
}

// capBody 单个方向**两面**的最小合法体（§8 c3 的输入）。
//
//   - req  = 客户端请求体 → ConvertRequest 的输入（chat / mess / resp 三种客户端协议之一）
//   - resp = 模板协议（上游）响应体 → ConvertResponse 的输入（chat / mess / resp 之一，
//     取决于该方向的**模板协议**，不是客户端协议）
//
// src 字段记出处，失败信息里直接告诉评审"这条输入来自哪个 golden/契约用例"。
type capBody struct {
	req, resp       string
	reqSrc, respSrc string
}

// capabilityRequestBodies 6 个方向 × 两面的最小合法体。
//
// 夹具纪律（§8 c3 原文："取该方向的最小合法体，复用 (a) 的 golden 输入体"）：
// **不许现编**。两个新方向直接用 contract_client_to_chat_test.go 的 contractFixtures()
// 的同一份输入体（与 (a3)/(m) 共用，改一处两边一起动）；其余 4 个既有方向逐字取
// goldenCases() 里对应用例的输入体（只按可读性重排换行/缩进 —— 折点都落在结构位置，
// JSON 语义不变），用例名写在 src 字段里。若将来 golden 的**输入**变了，这里只需
// 同步覆盖广度，不影响门禁语义（本门禁不比对产物字节，只判 err/JSON 合法性）。
func capabilityRequestBodies(t *testing.T) map[domain.ProtocolConvert]capBody {
	t.Helper()
	fixtures := contractFixtures()
	fixture := func(scope string) string {
		f, ok := fixtures[scope]
		require.True(t, ok,
			"contractFixtures 缺少夹具 %q（(c3) 必须复用 (a) 的 golden 输入体，不许现编；夹具被改名/删除时这里红）", scope)
		return f.body
	}
	return map[domain.ProtocolConvert]capBody{
		// --- 4 个既有方向（golden 输入体逐字） ---
		domain.ProtocolConvertChatToResp: {
			reqSrc: "golden:chat_to_resp_typical",
			req: `{"model":"m","messages":[
				{"role":"system","content":"sys"},
				{"role":"developer","content":[{"type":"text","text":"dev A"},{"type":"text","text":"dev B"}]},
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"calling","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
				{"role":"tool","tool_call_id":"c1","content":"ok"},
				{"role":"function","name":"g","content":[{"type":"text","text":"legacy"}]}
			],"max_tokens":10,"user":"u1","store":true}`,
			respSrc: "golden:resp_to_chat_response",
			resp: `{"id":"r1","model":"m","created_at":17,"status":"completed","output":[
				{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"think"}]},
				{"type":"message","role":"assistant","content":[
					{"type":"output_text","text":"hello"},
					{"type":"refusal","refusal":"no"},
					{"type":"output_text","text":" world"}
				]},
				{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
				{"type":"unknown_item"}
			],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7,"input_tokens_details":{"cached_tokens":1,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}`,
		},
		domain.ProtocolConvertChatToMess: {
			reqSrc: "golden:chat_to_mess_asst_media",
			req: `{"messages":[
				{"role":"assistant","content":"ok"},
				{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]},
				{"role":"assistant","content":[{"type":"text","text":"kept"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}
			]}`,
			respSrc: "golden:mess_to_chat_response",
			resp: `{"id":"msg_1","model":"m","stop_reason":"tool_use","content":[
				{"type":"text","text":"hello"},
				{"type":"thinking","thinking":"think"},
				{"type":"redacted_thinking","data":"dead"},
				{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}},
				{"type":"document","source":{"type":"url","url":"https://e.com/d.pdf"}},
				{"type":"unknown_block"}
			],"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}`,
		},
		domain.ProtocolConvertMessToResp: {
			reqSrc: "golden:mess_to_resp_typical",
			req: `{"model":"m","system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"max_tokens":9,
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
				],"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"high"}}`,
			respSrc: "golden:resp_to_mess_response",
			resp: `{"id":"r1","model":"m","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[
				{"type":"reasoning","encrypted_content":"enc","summary":[{"type":"summary_text","text":"think"}]},
				{"type":"message","role":"assistant","content":[
					{"type":"output_text","text":"hello"},
					{"type":"refusal","refusal":"no"}
				]},
				{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},
				{"type":"unknown_item"}
			],"usage":{"input_tokens":9,"output_tokens":4,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}`,
		},
		domain.ProtocolConvertRespToMess: {
			reqSrc:  "golden:resp_to_mess_string_input",
			req:     `{"input":"plain text","model":"m"}`,
			respSrc: "golden:mess_to_resp_response",
			resp: `{"id":"msg_1","model":"m","stop_reason":"max_tokens","content":[
				{"type":"thinking","thinking":"think","signature":"sig"},
				{"type":"redacted_thinking","data":"dead"},
				{"type":"text","text":"hello"},
				{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}},
				{"type":"unknown_block"}
			],"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}`,
		},
		// --- 2 个新方向（契约表夹具，与 (a3) 同一份输入体） ---
		domain.ProtocolConvertMessToChat: {
			reqSrc:  "contract:mess_to_chat_req_text",
			req:     fixture("mess_to_chat_req_text"),
			respSrc: "contract:chat_to_mess_resp_text",
			resp:    fixture("chat_to_mess_resp_text"),
		},
		domain.ProtocolConvertRespToChat: {
			reqSrc:  "contract:resp_to_chat_req_text",
			req:     fixture("resp_to_chat_req_text"),
			respSrc: "contract:chat_to_resp_resp_text",
			resp:    fixture("chat_to_resp_resp_text"),
		},
	}
}

// capBadBody 明确不是 JSON 的输入：用它证明"普通错误"（输入非法）不会被伪装成
// ErrUnsupportedDirection —— 调用方对后者的反应是"去查配置/部署"，对前者是"回 400"。
const capBadBody = "not-json-at-all"

// capTruncate 失败信息的产物预览（避免把整段上游体打进日志）。
func capTruncate(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// TestCapabilityWiringEquivalence §8(c3)：能力表必须与**真实接线**双向一致 —— 只判
// "接线与否"，不判"最终是否 GA"（后者是 (c3f) 的跨里程碑断言）。
//
// 对每个方向、两个面（request / response）逐条断言：
//
//	① require.Equal(want, err == nil)：能力位 true 必须真成功，false 必须真报错；
//	② errors.Is(err, ErrUnsupportedDirection) **只在** want==false 时为真
//	   （同时要求两者严格互补：普通错误不许伪装成 sentinel）；
//	③ want==true 时产物必须非空且 json.Valid(out)（不是空壳、不是半截字节）；
//	④ want==true 时**非法输入**必须报普通错误（不得返回 sentinel）。
//
// **改什么会红**：dirCaps 与 Convert* 的 switch 任一侧改到不一致（或两面挪错、sentinel
// 换成字符串匹配、成功路径返回空/半截 JSON）即红 —— 负例已用 -overlay 在树外副本实测；
// 新增 domain 方向却不补夹具，卡在 require.True(ok)。
func TestCapabilityWiringEquivalence(t *testing.T) {
	bodies := capabilityRequestBodies(t)
	faces := []struct {
		name    string
		capOf   func(DirectionCaps) bool
		convert func([]byte, domain.ProtocolConvert) ([]byte, error)
		pick    func(capBody) (body, src string)
	}{
		{
			name:    "request",
			capOf:   func(c DirectionCaps) bool { return c.Request },
			convert: ConvertRequest,
			pick:    func(b capBody) (string, string) { return b.req, b.reqSrc },
		},
		{
			name:    "response",
			capOf:   func(c DirectionCaps) bool { return c.Response },
			convert: ConvertResponse,
			pick:    func(b capBody) (string, string) { return b.resp, b.respSrc },
		},
	}
	for _, face := range faces {
		for _, pc := range domain.AllProtocolConverts() {
			b, ok := bodies[pc]
			require.True(t, ok,
				"方向 %q 没有最小合法体：新增方向必须在本表补夹具，否则 (c3) 对该方向空转", pc)
			body, src := face.pick(b)
			require.NotEmpty(t, body, "方向 %q 的 %s 面夹具为空（来源 %s）", pc, face.name, src)

			want := face.capOf(Capability(pc))
			out, err := face.convert([]byte(body), pc)

			// ① 双向一致：true ⇒ 必须真成功；false ⇒ 必须真报错。
			require.Equal(t, want, err == nil,
				"方向 %q 的 %s 面：能力位=%v 但转换结果 err=%v（输入来源 %s）—— 能力表与接线必须双向一致",
				pc, face.name, want, err, src)
			// ② sentinel 只在"未接线"时为真，且与能力位严格互补。
			require.False(t, want && errors.Is(err, ErrUnsupportedDirection),
				"方向 %q 的 %s 面：能力位=true（已对外宣称支持）却返回 ErrUnsupportedDirection —— Convert* 的 switch 少了一个 case",
				pc, face.name)
			require.Equal(t, !want, errors.Is(err, ErrUnsupportedDirection),
				"方向 %q 的 %s 面：能力位=%v，但 errors.Is(err, ErrUnsupportedDirection)=%v —— 普通错误不得伪装成 sentinel",
				pc, face.name, want, errors.Is(err, ErrUnsupportedDirection))

			if !want {
				// 未接线只允许一处 `return nil, ErrUnsupportedDirection`（spec §2.3）。
				require.Nil(t, out, "方向 %q 的 %s 面未接线却返回了产物", pc, face.name)
				continue
			}

			// ③ 已接线：产物必须是完整合法 JSON。
			require.NotEmpty(t, out, "方向 %q 的 %s 面能力位=true 但产物为空（err=nil 的空产物 = 静默丢请求）", pc, face.name)
			require.True(t, json.Valid(out), "方向 %q 的 %s 面产物不是合法 JSON：%s", pc, face.name, capTruncate(out))

			// ④ 已接线：非法输入必须走普通错误（不会被误判成"方向不支持"）。
			_, errBad := face.convert([]byte(capBadBody), pc)
			require.False(t, errors.Is(errBad, ErrUnsupportedDirection),
				"方向 %q 的 %s 面：非法输入返回了 ErrUnsupportedDirection（能力位=true 的方向不该出现这个 sentinel）",
				pc, face.name)
		}
	}
}
