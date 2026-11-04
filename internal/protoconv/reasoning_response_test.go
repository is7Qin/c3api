// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// issue 46 门禁 —— M2：非流响应四方向（spec:
// .omo/plans/spec-protoconv-reasoning.md §6.2 第 16–22 条）。
//
// 断言口径同 spec §6：信封一律解出来与原始值逐字节比对；R1 的反例直接扫
// 响应体，不看「是否 Contains 某个 key」。流式三个面属 M3，本文件不覆盖。

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// responseOf 跑一次响应方向转换并解析成 map（断言结构用）。
func responseOf(t *testing.T, dir domain.ProtocolConvert, body string) map[string]any {
	t.Helper()
	out, err := ConvertResponse([]byte(body), dir)
	require.NoError(t, err)
	return obj(t, out)
}

// messResponseBody anthropic message 对象外壳（content 由调用方给）。
func messResponseBody(id, content, stopReason string) string {
	return `{"id":"` + id + `","type":"message","role":"assistant","model":"claude",` +
		`"content":` + content + `,"stop_reason":"` + stopReason + `",` +
		`"usage":{"input_tokens":10,"output_tokens":20}}`
}

// respResponseBody resp 响应对象外壳（output 由调用方给）。
func respResponseBody(id, output string) string {
	return `{"id":"` + id + `","object":"response","created_at":1,"status":"completed","model":"gpt",` +
		`"output":` + output + `,"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
}

// --- §6.2 #16：thinking 块 → 前置 reasoning item（信封逐字节往返）---

func TestReasoningRespToMessResponseThinking(t *testing.T) {
	content := `[{"type":"thinking","thinking":"我来看看","signature":"sig_xyz"},` +
		`{"type":"text","text":"done"},` +
		`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"x"}}]`
	resp := responseOf(t, domain.ProtocolConvertRespToMess, messResponseBody("msg_1", content, "tool_use"))
	output := arrOf(t, resp, "output")
	require.Len(t, output, 3, "reasoning + message + function_call")

	rs := output[0].(map[string]any)
	require.Equal(t, "reasoning", rs["type"], "reasoning 项必须最先")
	require.Equal(t, "rs_msg_1_0", rs["id"], "id = rs_<msgid>_<n>")
	require.Equal(t, []any{map[string]any{"type": "summary_text", "text": "我来看看"}},
		arrOf(t, rs, "summary"), "summary 与 thinking 原文相等")

	blk, ok := openThinking(rs["encrypted_content"].(string))
	require.True(t, ok, "加密内容是本包信封")
	require.Equal(t, thinkingBlock{Type: "thinking", Thinking: "我来看看", Signature: "sig_xyz"}, blk,
		"信封解码后 signature / thinking 逐字节相等")

	msg := output[1].(map[string]any)
	require.Equal(t, "message", msg["type"])
	require.Equal(t, []any{map[string]any{"type": "output_text", "text": "done", "annotations": []any{}}},
		arrOf(t, msg, "content"))
	require.Equal(t, "function_call", output[2].(map[string]any)["type"])
}

// --- §6.2 #17：R1 —— 有文本但无签名 → 连 summary 都不发 ---

func TestReasoningRespToMessResponseR1DropsUnsealed(t *testing.T) {
	content := `[{"type":"thinking","thinking":"只有文本没有签名","signature":""},` +
		`{"type":"text","text":"答"}]`
	resp := responseOf(t, domain.ProtocolConvertRespToMess, messResponseBody("msg_2", content, "end_turn"))
	output := arrOf(t, resp, "output")
	require.Len(t, output, 1, "只剩 message 项")
	require.Equal(t, "message", output[0].(map[string]any)["type"])

	raw := string(mustMarshal(t, resp))
	require.NotContains(t, raw, "encrypted_content", "不得出现 encrypted_content")
	require.NotContains(t, raw, "只有文本没有签名", "连 summary 都不发")
	require.NotContains(t, raw, `"reasoning"`, "没有任何 reasoning 项")
}

// --- §6.2 #18：R1 上限（原文 256 KiB+1 / redacted data 为空）---

func TestReasoningRespToMessResponseR1Limits(t *testing.T) {
	huge := strings.Repeat("x", maxThinkingBytes+1)

	content := `[{"type":"thinking","thinking":"` + huge + `","signature":"sig_big"},` +
		`{"type":"text","text":"答"}]`
	resp := responseOf(t, domain.ProtocolConvertRespToMess, messResponseBody("msg_3", content, "end_turn"))
	require.Len(t, arrOf(t, resp, "output"), 1, "原文超 256 KiB → 不产出 reasoning 项")
	require.NotContains(t, string(mustMarshal(t, resp)), "encrypted_content")

	emptyRedacted := `[{"type":"redacted_thinking","data":""},{"type":"text","text":"答"}]`
	resp = responseOf(t, domain.ProtocolConvertRespToMess, messResponseBody("msg_4", emptyRedacted, "end_turn"))
	require.Len(t, arrOf(t, resp, "output"), 1, "redacted 的 data 为空 → 不产出 reasoning 项")

	// 未签名的 thinking 也不产出（R1 的第一条：无签名即不可往返）
	unsigned := `[{"type":"thinking","thinking":"想"},{"type":"text","text":"答"}]`
	resp = responseOf(t, domain.ProtocolConvertRespToMess, messResponseBody("msg_5", unsigned, "end_turn"))
	require.Len(t, arrOf(t, resp, "output"), 1)
}

// --- §6.2 #19：redacted 块 → summary:[] + 信封 ---

func TestReasoningRespToMessResponseRedacted(t *testing.T) {
	content := `[{"type":"redacted_thinking","data":"OPAQUE"},{"type":"text","text":"ok"}]`
	resp := responseOf(t, domain.ProtocolConvertRespToMess, messResponseBody("msg_6", content, "end_turn"))
	output := arrOf(t, resp, "output")
	require.Len(t, output, 2)

	rs := output[0].(map[string]any)
	require.Equal(t, "reasoning", rs["type"])
	require.Empty(t, arrOf(t, rs, "summary"), "redacted 无摘要")
	require.Equal(t, []string{"encrypted_content", "id", "summary", "type"}, keySetOf(rs),
		"reasoning 项只带这四个键（无 thinking / signature）")

	blk, ok := openThinking(rs["encrypted_content"].(string))
	require.True(t, ok)
	require.Equal(t, thinkingBlock{Type: "redacted_thinking", Data: "OPAQUE"}, blk, "data 逐字节相等")
}

// --- §6.2 #20：mess_to_resp —— reasoning item → thinking 块（signature = 臂）---

func TestReasoningMessToRespResponseThinking(t *testing.T) {
	output := `[{"type":"reasoning","id":"rs_up","summary":[{"type":"summary_text","text":"想一下"}],` +
		`"encrypted_content":"opaque-blob"},` +
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`
	out := responseOf(t, domain.ProtocolConvertMessToResp, respResponseBody("rsp_1", output))
	content := arrOf(t, out, "content")
	require.Len(t, content, 2)

	first := content[0].(map[string]any)
	require.Equal(t, "thinking", first["type"], "thinking 块最先")
	require.Equal(t, "想一下", first["thinking"], "文本取 summary[].text")
	arm, ok := openReasoningRef(first["signature"].(string))
	require.True(t, ok, "signature 是 reasoning_ref 臂（不是 encrypted_content 原文）")
	require.Equal(t, reasoningRef{Type: reasoningRefType, EncryptedContent: "opaque-blob", ID: "rs_up"}, arm,
		"臂内的 encrypted_content 与 id 逐字节相等")
	require.Equal(t, "text", content[1].(map[string]any)["type"])

	// content[].reasoning_text 兜底（无 summary）
	output = `[{"type":"reasoning","id":"rs_2","summary":[],` +
		`"content":[{"type":"reasoning_text","text":"原始思考"}]}]`
	out = responseOf(t, domain.ProtocolConvertMessToResp, respResponseBody("rsp_2", output))
	first = arrOf(t, out, "content")[0].(map[string]any)
	require.Equal(t, "原始思考", first["thinking"], "summary 空 → content[].reasoning_text 兜底")

	// encrypted_content 空/缺失 → signature:""（约束 B：字段必须在）
	for _, encField := range []string{`"encrypted_content":"",`, ``} {
		output = `[{"type":"reasoning","id":"rs_3","summary":[{"type":"summary_text","text":"只有文本"}],` +
			encField + `"type":"reasoning"}]`
		out = responseOf(t, domain.ProtocolConvertMessToResp, respResponseBody("rsp_3", output))
		first = arrOf(t, out, "content")[0].(map[string]any)
		sig, ok := first["signature"]
		require.True(t, ok, "signature 字段必须在（%s）", encField)
		require.Equal(t, "", sig, "无加密内容 → 空签名")
	}

	// 文本与加密内容都没有 → 该块无可承载内容，丢
	output = `[{"type":"reasoning","id":"rs_4","summary":[]}]`
	out = responseOf(t, domain.ProtocolConvertMessToResp, respResponseBody("rsp_4", output))
	require.Empty(t, arrOf(t, out, "content"), "无文本无臂 → 不产块")
}

// --- §6.2 #21：chat_to_mess —— thinking → reasoning_content ---

func TestReasoningChatToMessResponseContent(t *testing.T) {
	content := `[{"type":"thinking","thinking":"先想"},` +
		`{"type":"thinking","thinking":"再想"},` +
		`{"type":"redacted_thinking","data":"OPAQUE"},` +
		`{"type":"text","text":"答"}]`
	out := responseOf(t, domain.ProtocolConvertChatToMess, messResponseBody("msg_7", content, "end_turn"))
	msg, ok := arrOf(t, out, "choices")[0].(map[string]any)["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "先想再想", msg["reasoning_content"], "多块按序 \"\" 拼接")
	require.Equal(t, "答", msg["content"])
	require.Equal(t, "assistant", msg["role"])

	// 无 thinking 块 → 不出现 reasoning_content 键
	plain := responseOf(t, domain.ProtocolConvertChatToMess,
		messResponseBody("msg_8", `[{"type":"text","text":"答"}]`, "end_turn"))
	msg, ok = arrOf(t, plain, "choices")[0].(map[string]any)["message"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, msg, "reasoning_content", "无 thinking 块时不多出字段")
}

// --- §6.2 #22：chat_to_resp —— reasoning item → reasoning_content + 用量 ---

func TestReasoningChatToRespResponseContent(t *testing.T) {
	body := `{"id":"rsp_5","object":"response","created_at":7,"status":"completed","model":"gpt","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"想一想"}]},` +
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"答案"}]}],` +
		`"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14,` +
		`"output_tokens_details":{"reasoning_tokens":4}}}`
	out := responseOf(t, domain.ProtocolConvertChatToResp, body)
	msg, ok := arrOf(t, out, "choices")[0].(map[string]any)["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "想一想", msg["reasoning_content"])
	require.Equal(t, "答案", msg["content"])

	usage, ok := out["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"reasoning_tokens": float64(4)}, usage["completion_tokens_details"],
		"output_tokens_details.reasoning_tokens → completion_tokens_details.reasoning_tokens")

	// 无 reasoning_tokens（或 0）→ 不发明零值键
	for _, usage := range []string{
		`"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}`,
		`"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14,` +
			`"output_tokens_details":{"reasoning_tokens":0}}`,
	} {
		body := `{"id":"rsp_6","status":"completed","output":[` +
			`{"type":"message","id":"msg_2","role":"assistant","content":[{"type":"output_text","text":"答案"}]}],` +
			usage + `}`
		out := responseOf(t, domain.ProtocolConvertChatToResp, body)
		usageOut, ok := out["usage"].(map[string]any)
		require.True(t, ok)
		require.NotContains(t, usageOut, "completion_tokens_details", usage)
	}

	// content[].reasoning_text 兜底（无 summary）
	body = `{"id":"rsp_7","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_2","summary":[],"content":[{"type":"reasoning_text","text":"原始思考"}]},` +
		`{"type":"message","id":"msg_3","role":"assistant","content":[{"type":"output_text","text":"答"}]}]}`
	out = responseOf(t, domain.ProtocolConvertChatToResp, body)
	msg, ok = arrOf(t, out, "choices")[0].(map[string]any)["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "原始思考", msg["reasoning_content"], "reasoning_text 兜底")
}

// --- 跨面闭环（非流部分；spec §2.5 / §6.4 #33–#36 的响应 ↔ 请求往返）---

// TestReasoningMessToRespArmRoundTripThroughResponse 响应侧封臂 → 请求侧还原：
// 两端必须是同一个 encrypted_content 与 id（M1 里只能单侧断言的另一半）。
func TestReasoningMessToRespArmRoundTripThroughResponse(t *testing.T) {
	const opaque = "gAAAA-B-upstream-opaque"
	output := `[{"type":"reasoning","id":"rs_up","summary":[{"type":"summary_text","text":"想一下"}],` +
		`"encrypted_content":"` + opaque + `"},` +
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`
	resp := responseOf(t, domain.ProtocolConvertMessToResp, respResponseBody("rsp_1", output))
	thinkBlock := arrOf(t, resp, "content")[0]
	require.NotEmpty(t, thinkBlock.(map[string]any)["signature"], "响应侧把 encrypted_content 封成了臂")

	// 把响应侧产出的 thinking 块原样放回下一轮请求
	blocksJSON, err := json.Marshal([]any{thinkBlock, map[string]any{"type": "text", "text": "done"}})
	require.NoError(t, err)
	reqBody := `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":` + string(blocksJSON) + `},` +
		`{"role":"user","content":"接着来"}]}`
	input := arrOf(t, requestOf(t, domain.ProtocolConvertMessToResp, reqBody), "input")
	require.Len(t, input, 4)
	require.Equal(t, map[string]any{
		"type": "reasoning", "id": "rs_up", "encrypted_content": opaque,
	}, input[1], "臂回放后与原 reasoning item 逐字节相等（含 id，无 summary）")
}

// TestReasoningRespToMessEnvelopeRoundTripThroughResponse 上游 thinking 块 → 客户端
// reasoning 信封 → 回放请求后 thinking 块（签名/原文）逐字节相等。
func TestReasoningRespToMessEnvelopeRoundTripThroughResponse(t *testing.T) {
	content := `[{"type":"thinking","thinking":"先查目录","signature":"sig_round"},` +
		`{"type":"tool_use","id":"toolu_1","name":"ls","input":{}}]`
	resp := responseOf(t, domain.ProtocolConvertRespToMess, messResponseBody("msg_9", content, "tool_use"))
	output := arrOf(t, resp, "output")
	require.Len(t, output, 2)

	// 客户端把 output 原样回放（codex 的历史回放形态），排在本轮工具调用之前
	inputJSON, err := json.Marshal([]any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "run ls"},
		}},
		output[0],
		output[1],
		map[string]any{"type": "function_call_output", "call_id": "toolu_1", "output": "a.txt"},
	})
	require.NoError(t, err)
	reqBody := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":` + string(inputJSON) + `}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, reqBody)

	blocks := blocksOf(t, out, "messages", 1)
	require.Len(t, blocks, 2, "assistant 轮 = thinking + tool_use")
	blk := blocks[0].(map[string]any)
	require.Equal(t, "thinking", blk["type"], "thinking 在 tool_use 之前")
	require.Equal(t, "先查目录", blk["thinking"])
	require.Equal(t, "sig_round", blk["signature"], "签名逐字节往返")
}

// keySetOf 取 map 的键集合（升序；断言「只带这些键」用）。
func keySetOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
