// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// issue 46 门禁 —— G：端到端（spec:
// .omo/plans/spec-protoconv-reasoning.md §6.4 第 33–37 条）。
//
// 这里只用对外入口 ConvertRequest / ConvertResponse / StreamMapper 串多轮，
// 断言整条链路而不是单点映射：上游 thinking 块 → 客户端 reasoning item →
// 原样回放 → 上游 thinking 块的签名逐字节不变。方向命名按 spec（以**客户端**
// 协议命名）。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// e2eUpstreamThinkingRound 轮 1 的上游（Anthropic）响应：thinking + tool_use。
const e2eUpstreamThinkingRound = `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[` +
	`{"type":"thinking","thinking":"我需要先列目录","signature":"sig_round1"},` +
	`{"type":"tool_use","id":"toolu_1","name":"exec_command","input":{"cmd":"ls"}}],` +
	`"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":20}}`

// e2eUserInputItem 客户端 resp 请求的 user 项（回放历史时排在最前）。
const e2eUserInputItem = `{"type":"message","role":"user","content":[{"type":"input_text","text":"run ls"}]}`

// walkJSON 递归遍历 JSON 值里的每个对象（#37 的结构扫描：不用子串匹配）。
func walkJSON(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, c := range x {
			walkJSON(c, fn)
		}
	case []any:
		for _, c := range x {
			walkJSON(c, fn)
		}
	}
}

// replayRespThinking 把客户端 resp 的 output 项原样放进下一轮 input 并转换，
// 返回上游 messages 里 assistant 轮的 content 块（#34 / #35 共用）。
func replayRespThinking(t *testing.T, items []any) []any {
	t.Helper()
	input := make([]any, 0, len(items)+3)
	var user map[string]any
	require.NoError(t, json.Unmarshal([]byte(e2eUserInputItem), &user))
	input = append(input, user)
	input = append(input, items...)
	input = append(input, map[string]any{
		"type": "function_call_output", "call_id": "toolu_1", "output": "a.txt",
	})
	inputJSON, err := json.Marshal(input)
	require.NoError(t, err)

	reqBody := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":` +
		string(inputJSON) + `}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, reqBody)
	msgs := arrOf(t, out, "messages")
	require.Len(t, msgs, 3, "user / assistant / tool_result")
	return blocksOf(t, out, "messages", 1)
}

// assertReplayedThinking assistant 轮必须是 [thinking, tool_use]，且 thinking 在
// 最前、签名与上游逐字节相等（#34 / #35 的同一口径）。
func assertReplayedThinking(t *testing.T, blocks []any, wantSig, wantText string) {
	t.Helper()
	require.Len(t, blocks, 2, "assistant 轮 = thinking + tool_use")
	th, ok := blocks[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "thinking", th["type"], "thinking 必须在 tool_use 之前")
	require.Equal(t, wantText, th["thinking"], "thinking 原文逐字节相等")
	require.Equal(t, wantSig, th["signature"], "签名逐字节相等")
	tool, ok := blocks[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool_use", tool["type"])
}

// --- §6.4 #33：轮 1（resp_to_mess 非流）---

func TestReasoningE2ERound1ResponseToMess(t *testing.T) {
	out := responseOf(t, domain.ProtocolConvertRespToMess, e2eUpstreamThinkingRound)
	output := arrOf(t, out, "output")
	require.Len(t, output, 2, "[reasoning, function_call]")

	rs, ok := output[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "reasoning", rs["type"], "reasoning 项排在最前")
	require.Equal(t, "rs_msg_1_0", rs["id"])
	require.Equal(t, []any{map[string]any{"type": "summary_text", "text": "我需要先列目录"}},
		arrOf(t, rs, "summary"), "summary 承载 thinking 原文")

	enc, ok := rs["encrypted_content"].(string)
	require.True(t, ok, "reasoning item 必须带 encrypted_content")
	blk, ok := openThinking(enc)
	require.True(t, ok, "encrypted_content 是本包信封")
	require.Equal(t, thinkingBlock{Type: "thinking", Thinking: "我需要先列目录", Signature: "sig_round1"}, blk,
		"信封内的 thinking / signature 与上游逐字节相等")

	fc, ok := output[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function_call", fc["type"])
	require.Equal(t, "toolu_1", fc["call_id"])
}

// --- §6.4 #34：轮 2（output 原样进 input → 上游 thinking 在 tool_use 之前）---

func TestReasoningE2ERound2ReplayRequest(t *testing.T) {
	out := responseOf(t, domain.ProtocolConvertRespToMess, e2eUpstreamThinkingRound)
	blocks := replayRespThinking(t, arrOf(t, out, "output"))
	assertReplayedThinking(t, blocks, "sig_round1", "我需要先列目录")
}

// --- §6.4 #35：轮 3（流式）——信封只能取自该流式轮自身的 output_item.done ---

func TestReasoningE2ERound3StreamEnvelope(t *testing.T) {
	const (
		streamText = "我先看看目录结构"
		streamSig  = "sig_stream_round3"
	)
	streamEvents := []string{
		"message_start", `{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"我先看看"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"目录结构"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + streamSig + `"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"exec_command","input":{}}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":1}`,
		"message_stop", `{"type":"message_stop"}`,
	}
	frames := runStream(t, domain.ProtocolConvertRespToMess, streamEvents...)

	// 信封只能从该流式轮自身的 output_item.done 取；该帧缺失即判失败。
	done := framesOfType(frames, "response.output_item.done")
	require.Len(t, done, 1, "信封的唯一权威通道是本轮的 output_item.done")
	reasoningItem, ok := done[0].data["item"].(map[string]any)
	require.True(t, ok, "done 必须带 item")
	require.Equal(t, "reasoning", reasoningItem["type"])
	enc, ok := reasoningItem["encrypted_content"].(string)
	require.True(t, ok, "done 的 item 必须带 encrypted_content")
	require.NotEmpty(t, enc)
	blk, ok := openThinking(enc)
	require.True(t, ok)
	require.Equal(t, streamSig, blk.Signature, "信封内签名 = 上游 signature_delta")
	require.Equal(t, streamText, blk.Thinking, "信封内原文 = 累积的 thinking_delta")

	// 「缺失即判失败」的前提：上游不发 content_block_stop 时根本没有 done 帧，
	// 客户端也就无从取得信封（证明不能靠 completed 兜底）。
	noStop := make([]string, 0, len(streamEvents))
	for i := 0; i+1 < len(streamEvents); i += 2 {
		if streamEvents[i] == "content_block_stop" &&
			strings.Contains(streamEvents[i+1], `"index":0`) {
			continue
		}
		noStop = append(noStop, streamEvents[i], streamEvents[i+1])
	}
	noDoneFrames := runStream(t, domain.ProtocolConvertRespToMess, noStop...)
	require.Empty(t, framesOfType(noDoneFrames, "response.output_item.done"),
		"没有 content_block_stop 就没有 done —— 这正是「不得退回 completed」的原因")

	// 用该 done 的 reasoning item + 该流的 function_call item 回放（#34 同形）
	fcItem := map[string]any{
		"type": "function_call", "id": "fc_toolu_1", "call_id": "toolu_1",
		"name": "exec_command", "arguments": `{"cmd":"ls"}`, "status": "completed",
	}
	blocks := replayRespThinking(t, []any{reasoningItem, fcItem})
	assertReplayedThinking(t, blocks, streamSig, streamText)
}

// --- §6.4 #36：mess_to_resp —— 臂往返后上游收到原样 reasoning item ---

func TestReasoningE2ERound4ArmReplay(t *testing.T) {
	upstreamResp := `{"id":"rsp_1","object":"response","status":"completed","model":"gpt","output":[` +
		`{"type":"reasoning","id":"rs_9","summary":[{"type":"summary_text","text":"先想一想"}],` +
		`"encrypted_content":"opaque-abc"},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}],` +
		`"usage":{"input_tokens":1,"output_tokens":2}}`
	round1 := responseOf(t, domain.ProtocolConvertMessToResp, upstreamResp)
	content := arrOf(t, round1, "content")
	require.Len(t, content, 2, "[thinking, tool_use]")

	th, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "thinking", th["type"], "thinking 块最先")
	require.Equal(t, "先想一想", th["thinking"], "文本取 summary[].text")
	arm, ok := openReasoningRef(th["signature"].(string))
	require.True(t, ok, "signature 是 reasoning_ref 臂")
	require.Equal(t, reasoningRef{Type: reasoningRefType, EncryptedContent: "opaque-abc", ID: "rs_9"}, arm)

	// 客户端把整轮原样回放（thinking 块 + tool_use + tool_result）
	assistantContent, err := json.Marshal([]any{
		map[string]any{"type": "thinking", "thinking": th["thinking"], "signature": th["signature"]},
		map[string]any{"type": "tool_use", "id": "call_1", "name": "exec_command", "input": map[string]any{"cmd": "ls"}},
	})
	require.NoError(t, err)
	reqBody := `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"messages":[{"role":"user","content":"run ls"},` +
		`{"role":"assistant","content":` + string(assistantContent) + `},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"a.txt"}]}]}`
	out := requestOf(t, domain.ProtocolConvertMessToResp, reqBody)
	input := arrOf(t, out, "input")
	require.GreaterOrEqual(t, len(input), 2)

	// 上游收到**原样**的 reasoning item（键序无关，但键集合与值都必须一致）
	require.Equal(t, map[string]any{
		"type": "reasoning", "id": "rs_9", "encrypted_content": "opaque-abc",
	}, input[1], "臂回放后与原 reasoning item 逐字节相等（无 summary、无多余键）")

	// 该轮余下的 tool_use 仍按原顺序跟在后面
	var sawToolUse bool
	for _, item := range input[2:] {
		im, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if im["type"] == "function_call" {
			sawToolUse = true
			require.Equal(t, "call_1", im["call_id"])
		}
	}
	require.True(t, sawToolUse, "tool_use 仍在回放轮里")
}

// --- §6.4 #37：反例（上游 thinking 无签名 → 零个缺信封的 reasoning item）---

func TestReasoningE2ENoUnsignedReasoningItem(t *testing.T) {
	upstream := `{"id":"msg_9","type":"message","role":"assistant","model":"claude","content":[` +
		`{"type":"thinking","thinking":"没有签名的思考"},` +
		`{"type":"thinking","thinking":"另一个也没签名","signature":""},` +
		`{"type":"text","text":"答"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":2}}`
	raw, err := ConvertResponse([]byte(upstream), domain.ProtocolConvertRespToMess)
	require.NoError(t, err)

	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc))

	// 结构扫描：树里任何 type=="reasoning" 的对象都必须带非空 encrypted_content
	var reasoning int
	var texts []string
	walkJSON(doc, func(m map[string]any) {
		if t, ok := m["text"].(string); ok {
			texts = append(texts, t)
		}
		if m["type"] != "reasoning" {
			return
		}
		reasoning++
		enc, ok := m["encrypted_content"].(string)
		require.True(t, ok, "reasoning item 必须带字符串 encrypted_content: %v", m)
		require.NotEmpty(t, enc, "encrypted_content 不得为空")
	})
	require.Zero(t, reasoning, "上游 thinking 无签名 → 一个 reasoning item 都不产出")
	require.NotContains(t, texts, "没有签名的思考", "连 summary 都不发")
	require.NotContains(t, texts, "另一个也没签名", "连 summary 都不发")

	// 流式同理：无 signature_delta 时 done 与 completed.output 都不得出现 item
	frames := runStream(t, domain.ProtocolConvertRespToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_10","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"流式里也没签名"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	require.Empty(t, framesOfType(frames, "response.output_item.done"), "R1 不成立 → 不发 done")
	var streamReasoning int
	for _, fr := range framesOfType(frames, "response.completed") {
		walkJSON(fr.data, func(m map[string]any) {
			if m["type"] != "reasoning" {
				return
			}
			streamReasoning++
			enc, ok := m["encrypted_content"].(string)
			require.True(t, ok)
			require.NotEmpty(t, enc)
		})
	}
	require.Zero(t, streamReasoning, "流式 completed.output[] 里也不得出现 reasoning item")
}
