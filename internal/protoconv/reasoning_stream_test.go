// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// issue 46 门禁 —— M3：流式四个方向（spec:
// .omo/plans/spec-protoconv-reasoning.md §6.3 第 23–32 条，含 30b）。
//
// 断言口径同 spec §6：**每条断言都基于「帧序列 + 每帧解析后的结构」**，不用子串
// 匹配。三件事贯穿全文件：
//   - 信封一律解出来与上游值逐字节比对；
//   - 先由 content_block_start 建 index→kind 映射，再校验每帧 delta 的归属；
//   - 同一 content index 只允许一次 content_block_stop。
//
// 方向命名按 spec（以**客户端**协议命名）：resp_to_mess = 客户端 resp / 上游
// mess（本包 domain.ProtocolConvertRespToMess）；mess_to_resp = 客户端 mess /
// 上游 resp（domain.ProtocolConvertMessToResp）。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// sseFrame 一条已解析的 SSE 帧（event 名 + data 载荷；[DONE] 的 data 为 nil）。
type sseFrame struct {
	event string
	data  map[string]any
	raw   string
}

// runStream 依次喂入 name/data 事件对，返回全部产出帧（含多帧返回）。每个产出
// 帧都过 validateFrames（§6.3 #32：所有新增帧必须是合法 SSE 帧）。
func runStream(t *testing.T, dir domain.ProtocolConvert, events ...string) []sseFrame {
	t.Helper()
	m := NewStreamMapper(dir)
	var out []sseFrame
	for i := 0; i+1 < len(events); i += 2 {
		frame, drop := m.Map(events[i], []byte(events[i+1]))
		if drop {
			continue
		}
		validateFrames(t, frame)
		out = append(out, parseSSEFrames(t, frame)...)
	}
	return out
}

// parseSSEFrames 帧字节 → 逐帧结构（data 必须是 JSON 对象，[DONE] 例外）。
func parseSSEFrames(t *testing.T, frame []byte) []sseFrame {
	t.Helper()
	var out []sseFrame
	for f := range strings.SplitSeq(string(frame), "\n\n") {
		if f == "" {
			continue
		}
		var fr sseFrame
		var data []string
		for _, ln := range strings.Split(f, "\n") {
			if name, ok := strings.CutPrefix(ln, "event: "); ok {
				fr.event = name
				continue
			}
			if payload, ok := strings.CutPrefix(ln, "data: "); ok {
				data = append(data, payload)
			}
		}
		fr.raw = strings.Join(data, "\n")
		if fr.raw != "[DONE]" {
			var m map[string]any
			require.NoError(t, json.Unmarshal([]byte(fr.raw), &m), "data 载荷应为 JSON 对象: %q", fr.raw)
			fr.data = m
		}
		out = append(out, fr)
	}
	return out
}

// frameType 帧的 data.type（无 type → ""）。
func frameType(fr sseFrame) string {
	if fr.data == nil {
		return ""
	}
	s, _ := fr.data["type"].(string)
	return s
}

// framesOfType 按 data.type 过滤帧（保持帧序）。
func framesOfType(frames []sseFrame, typ string) []sseFrame {
	var out []sseFrame
	for _, fr := range frames {
		if frameType(fr) == typ {
			out = append(out, fr)
		}
	}
	return out
}

// frameIndexOf 帧的 data.index（Anthropic 帧必带）。
func frameIndexOf(t *testing.T, fr sseFrame) float64 {
	t.Helper()
	v, ok := fr.data["index"].(float64)
	require.True(t, ok, "帧缺少 index: %s", fr.raw)
	return v
}

// blockKindMap 由 content_block_start 建 index → content_block.type（§6.3 #28）。
func blockKindMap(t *testing.T, frames []sseFrame) map[float64]string {
	t.Helper()
	kinds := make(map[float64]string)
	for _, fr := range framesOfType(frames, "content_block_start") {
		block, ok := fr.data["content_block"].(map[string]any)
		require.True(t, ok, "content_block_start 缺 content_block")
		kind, _ := block["type"].(string)
		kinds[frameIndexOf(t, fr)] = kind
	}
	return kinds
}

// deltaPayload 取 content_block_delta 的 delta 对象。
func deltaPayload(t *testing.T, fr sseFrame) map[string]any {
	t.Helper()
	d, ok := fr.data["delta"].(map[string]any)
	require.True(t, ok, "content_block_delta 缺 delta: %s", fr.raw)
	return d
}

// reasoningIDs completed.response.output[] 里 reasoning item 的 id 序列。
func reasoningIDs(t *testing.T, completed sseFrame) []string {
	t.Helper()
	resp, ok := completed.data["response"].(map[string]any)
	require.True(t, ok, "response.completed 缺 response")
	output, ok := resp["output"].([]any)
	require.True(t, ok, "response.completed 缺 output")
	var ids []string
	for _, item := range output {
		im, ok := item.(map[string]any)
		if !ok || im["type"] != "reasoning" {
			continue
		}
		id, ok := im["id"].(string)
		require.True(t, ok, "reasoning item 缺 id")
		ids = append(ids, id)
	}
	return ids
}

// --- §6.3 #23：resp_to_mess —— thinking 块 → added / delta / stop 时的 done ---

func TestReasoningStreamRespToMessThinking(t *testing.T) {
	const enc = "gAAAA-upstream-opaque-blob"
	frames := runStream(t, domain.ProtocolConvertRespToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"让我"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"看看"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_1"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":1}`,
		"message_stop", `{"type":"message_stop"}`,
	)

	// added（in-progress 展示项，summary 空）
	added := framesOfType(frames, "response.output_item.added")
	require.Len(t, added, 2, "thinking → added；text → added")
	item, ok := added[0].data["item"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "reasoning", item["type"])
	require.Equal(t, "rs_msg_1_0", item["id"], "id 与 done 同源")
	require.Empty(t, item["summary"].([]any), "added 不带 summary")

	// summary delta
	deltas := framesOfType(frames, "response.reasoning_summary_text.delta")
	require.Len(t, deltas, 2)
	var texts []string
	for _, fr := range deltas {
		require.Equal(t, "rs_msg_1_0", fr.data["item_id"], "delta 与 added 同 id")
		require.EqualValues(t, 0, fr.data["summary_index"])
		texts = append(texts, fr.data["delta"].(string))
	}
	require.Equal(t, []string{"让我", "看看"}, texts, "thinking_delta 逐帧转 summary delta")

	// done（由 content_block_stop 触发）带信封
	done := framesOfType(frames, "response.output_item.done")
	require.Len(t, done, 1, "只有 thinking 块发 done")
	require.EqualValues(t, 0, done[0].data["output_index"], "done 落在 thinking 的 output_index")
	doneItem, ok := done[0].data["item"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "rs_msg_1_0", doneItem["id"])
	require.Equal(t, "completed", doneItem["status"])
	require.Equal(t, []any{map[string]any{"type": "summary_text", "text": "让我看看"}},
		doneItem["summary"], "summary 为累积文本")
	blk, ok := openThinking(doneItem["encrypted_content"].(string))
	require.True(t, ok, "done 的 encrypted_content 是本包信封")
	require.Equal(t, "sig_1", blk.Signature, "信封解码后与上游 signature_delta 逐字节相等")
	require.Equal(t, "让我看看", blk.Thinking)

	// 流内不得出现 signature_delta（签名只走 done 的信封）
	for _, fr := range frames {
		require.NotEqual(t, "signature_delta", frameType(fr), "signature_delta 无 resp 对应事件")
		if delta, ok := fr.data["delta"].(map[string]any); ok {
			require.NotEqual(t, "signature_delta", delta["type"], "delta 不得是 signature_delta")
		}
	}

	// completed.output[] 也带同一个 reasoning item
	completed := framesOfType(frames, "response.completed")
	require.Len(t, completed, 1)
	require.Equal(t, []string{"rs_msg_1_0"}, reasoningIDs(t, completed[0]))
}

// --- §6.3 #24：resp_to_mess —— redacted 块 → added + done ---

func TestReasoningStreamRespToMessRedacted(t *testing.T) {
	frames := runStream(t, domain.ProtocolConvertRespToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"OPAQUE"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	require.Len(t, framesOfType(frames, "response.output_item.added"), 1, "redacted 只发一次 added")
	done := framesOfType(frames, "response.output_item.done")
	require.Len(t, done, 1, "redacted 立即发 done")
	item := done[0].data["item"].(map[string]any)
	require.Equal(t, "reasoning", item["type"])
	require.Empty(t, item["summary"].([]any), "redacted 无摘要")
	blk, ok := openThinking(item["encrypted_content"].(string))
	require.True(t, ok)
	require.Equal(t, thinkingBlock{Type: "redacted_thinking", Data: "OPAQUE"}, blk, "data 逐字节相等")

	completed := framesOfType(frames, "response.completed")
	require.Equal(t, []string{item["id"].(string)}, reasoningIDs(t, completed[0]))
}

// --- §6.3 #25：R1 流式（无签名不发 done；completed.output 零缺信封项）---

func TestReasoningStreamRespToMessR1(t *testing.T) {
	// 块 0：有 delta 但无 signature_delta → R1 不成立
	// 块 1：有签名但上游没发 content_block_stop → 兜底进 completed.output[]
	frames := runStream(t, domain.ProtocolConvertRespToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"没签名的思考"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":0}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"有签名的思考"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig_ok"}}`,
		"message_stop", `{"type":"message_stop"}`,
	)

	// 块 0 走了 content_block_stop 但无签名 → R1 不成立 → 不发 done；
	// 块 1 有签名但上游没发 content_block_stop → 由 completed.output[] 兜底（也不发 done）。
	done := framesOfType(frames, "response.output_item.done")
	require.Empty(t, done, "两个块都不发 done（块 0 不可往返；块 1 走 completed 兜底）")

	completed := framesOfType(frames, "response.completed")
	require.Len(t, completed, 1)
	ids := reasoningIDs(t, completed[0])
	require.Equal(t, []string{"rs_msg_3_1"}, ids, "只有 R1 成立的块进 completed.output[]")
	require.NotContains(t, completed[0].raw, "没签名的思考", "连 summary 都不发")

	// completed.output[] 里零个缺 / null encrypted_content 的 reasoning item，也不得
	// 出现 null 项（R1 不成立时调用方必须整项丢掉，而不是留个空壳）。
	resp := completed[0].data["response"].(map[string]any)
	for _, item := range resp["output"].([]any) {
		require.NotNil(t, item, "completed.output[] 不得出现 null 项")
		im, ok := item.(map[string]any)
		if !ok || im["type"] != "reasoning" {
			continue
		}
		enc, ok := im["encrypted_content"].(string)
		require.True(t, ok, "reasoning item 必须带字符串 encrypted_content")
		require.NotEmpty(t, enc)
	}

	// id 不重复
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		require.False(t, seen[id], "completed.output[] 里 id 不得重复: %s", id)
		seen[id] = true
	}
}

// --- §6.3 #26：mess_to_resp —— done → signature_delta（臂）严格早于 stop ---

func TestReasoningStreamMessToRespDone(t *testing.T) {
	const enc = "gAAAA-upstream-opaque"
	arm := sealReasoningRef(reasoningRef{EncryptedContent: enc, ID: "rs_1"})
	require.NotEmpty(t, arm)

	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_1","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"想一下"}`,
		"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"想一下"}],"encrypted_content":"`+enc+`"}}`,
	)

	starts := framesOfType(frames, "content_block_start")
	require.Len(t, starts, 1)
	block := starts[0].data["content_block"].(map[string]any)
	require.Equal(t, "thinking", block["type"])
	require.Equal(t, "", block["thinking"], "thinking 文本走 delta")
	require.Equal(t, "", block["signature"], "签名走 signature_delta")

	deltas := framesOfType(frames, "content_block_delta")
	require.Len(t, deltas, 2, "thinking_delta + signature_delta")
	require.Equal(t, "thinking_delta", deltaPayload(t, deltas[0])["type"])
	require.Equal(t, "想一下", deltaPayload(t, deltas[0])["thinking"])
	require.Equal(t, "signature_delta", deltaPayload(t, deltas[1])["type"])
	require.Equal(t, arm, deltaPayload(t, deltas[1])["signature"], "signature_delta 逐字符串等于 reasoning_ref 臂")

	// signature_delta 与 stop 同 index，且严格早于 stop（帧序）
	sigIdx := frameIndexOf(t, deltas[1])
	stop := framesOfType(frames, "content_block_stop")
	require.Len(t, stop, 1)
	require.Equal(t, sigIdx, frameIndexOf(t, stop[0]), "signature_delta 与 stop 同 index")
	sigAt, stopAt := -1, -1
	for i, fr := range frames {
		if frameType(fr) == "content_block_delta" && deltaPayload(t, fr)["type"] == "signature_delta" {
			sigAt = i
		}
		if frameType(fr) == "content_block_stop" {
			stopAt = i
		}
	}
	require.Greater(t, sigAt, -1)
	require.Greater(t, stopAt, sigAt, "签名必须早于块 stop")

	// 臂可解回原 encrypted_content 与 id（请求侧据此还原 reasoning item）
	back, ok := openReasoningRef(deltaPayload(t, deltas[1])["signature"].(string))
	require.True(t, ok)
	require.Equal(t, reasoningRef{Type: reasoningRefType, EncryptedContent: enc, ID: "rs_1"}, back)
}

// --- §6.3 #27：mess_to_resp 兜底（缺 done → completed 补 stop，不补签名）---

func TestReasoningStreamMessToRespCompletedFallback(t *testing.T) {
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_2","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_2","output_index":0,"summary_index":0,"delta":"嗯"}`,
		"response.completed", `{"type":"response.completed","response":{"id":"rsp_2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)
	stop := framesOfType(frames, "content_block_stop")
	require.Len(t, stop, 1, "completed 兜底补 stop（同一 index 只一次）")
	require.Equal(t, "content_block_stop", frameType(stop[0]))

	// 兜底不补 signature_delta
	for _, fr := range framesOfType(frames, "content_block_delta") {
		require.NotEqual(t, "signature_delta", deltaPayload(t, fr)["type"])
	}

	// stop 早于 message_delta（客户端先收块结束再收 stop_reason）
	stopAt, deltaAt := -1, -1
	for i, fr := range frames {
		if frameType(fr) == "content_block_stop" {
			stopAt = i
		}
		if frameType(fr) == "message_delta" {
			deltaAt = i
		}
	}
	require.Greater(t, stopAt, -1)
	require.Greater(t, deltaAt, stopAt, "content_block_stop 必须先于 message_delta")
}

// --- §6.3 #28：结构断言（index → kind 映射与 delta 归属）---

func TestReasoningStreamMessToRespIndexKind(t *testing.T) {
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_3","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"想"}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"答"}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"f","arguments":""}}`,
		"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{}"}`,
		"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"blob"}}`,
		"response.output_text.done", `{"type":"response.output_text.done","item_id":"msg_1","output_index":1,"text":"答"}`,
		"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":2}`,
	)

	kinds := blockKindMap(t, frames)
	require.Equal(t, map[float64]string{0: "thinking", 1: "text", 2: "tool_use"}, kinds,
		"同一 output_index 上不同 kind 必须拿到不同 content index")

	allowed := map[string]map[string]bool{
		"thinking": {"thinking_delta": true, "signature_delta": true},
		"text":     {"text_delta": true},
		"tool_use": {"input_json_delta": true},
	}
	stopped := make(map[float64]bool)
	for _, fr := range frames {
		switch frameType(fr) {
		case "content_block_delta":
			idx := frameIndexOf(t, fr)
			kind, ok := kinds[idx]
			require.True(t, ok, "delta 落在未 start 的 index %v", idx)
			dt, _ := deltaPayload(t, fr)["type"].(string)
			require.True(t, allowed[kind][dt], "index %v 是 %s 块，收到 %s", idx, kind, dt)
		case "content_block_stop":
			idx := frameIndexOf(t, fr)
			_, started := kinds[idx]
			require.True(t, started, "stop 的 index %v 必须先 start", idx)
			require.False(t, stopped[idx], "同一 index 只能 stop 一次: %v", idx)
			stopped[idx] = true
		}
	}
	require.Len(t, stopped, 3, "三个块各收尾一次")
}

// --- §6.3 #29：上游省略 output_index（身份靠 item id，kind 决定 index）---

func TestReasoningStreamMessToRespOmittedOutputIndex(t *testing.T) {
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_4","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_item.added", `{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","summary_index":0,"delta":"想"}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","delta":"答"}`,
		"response.output_item.done", `{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"blob"}}`,
		"response.output_text.done", `{"type":"response.output_text.done","item_id":"msg_1","text":"答"}`,
	)

	kinds := blockKindMap(t, frames)
	require.Equal(t, map[float64]string{0: "thinking", 1: "text"}, kinds,
		"reasoning 与 message 都不带 output_index → 仍拿不同 index")
	var thinkIdx, textIdx float64
	for idx, kind := range kinds {
		if kind == "thinking" {
			thinkIdx = idx
		} else {
			textIdx = idx
		}
	}
	require.NotEqual(t, thinkIdx, textIdx)

	for _, fr := range framesOfType(frames, "content_block_delta") {
		dt, _ := deltaPayload(t, fr)["type"].(string)
		if dt == "text_delta" {
			require.Equal(t, textIdx, frameIndexOf(t, fr), "text_delta 不得落进 thinking 块")
		} else {
			require.Equal(t, thinkIdx, frameIndexOf(t, fr))
		}
	}
}

// --- §4.5 第 5 条 / §6.3 #28：上游只发 done（无 added、无 delta）---

func TestReasoningStreamMessToRespDoneWithoutAdded(t *testing.T) {
	const enc = "gAAAA-done-only"
	arm := sealReasoningRef(reasoningRef{EncryptedContent: enc, ID: "rs_x"})
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_x","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_x","type":"reasoning","summary":[{"type":"summary_text","text":"只有 done"}],"encrypted_content":"`+enc+`"}}`,
	)

	// 帧序必须是 start → signature_delta → stop（缺 start 客户端组不出合法块）
	var order []string
	for _, fr := range frames {
		switch frameType(fr) {
		case "content_block_start":
			order = append(order, "start")
		case "content_block_delta":
			order = append(order, deltaPayload(t, fr)["type"].(string))
		case "content_block_stop":
			order = append(order, "stop")
		}
	}
	require.Equal(t, []string{"start", "signature_delta", "stop"}, order,
		"没有 added/delta 时必须惰性补 start，且先于 signature_delta 与 stop")

	// start 是 thinking 块（与 delta 路径同口径的空块头）
	start := framesOfType(frames, "content_block_start")[0]
	block, ok := start.data["content_block"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "thinking", block["type"])
	require.Equal(t, "", block["thinking"])
	require.Equal(t, "", block["signature"])

	// 三帧同 index，签名是 reasoning_ref 臂且可解回原 encrypted_content
	startIdx := frameIndexOf(t, start)
	sig := framesOfType(frames, "content_block_delta")[0]
	stop := framesOfType(frames, "content_block_stop")[0]
	require.Equal(t, startIdx, frameIndexOf(t, sig))
	require.Equal(t, startIdx, frameIndexOf(t, stop))
	require.Equal(t, arm, deltaPayload(t, sig)["signature"])
	back, ok := openReasoningRef(deltaPayload(t, sig)["signature"].(string))
	require.True(t, ok)
	require.Equal(t, reasoningRef{Type: reasoningRefType, EncryptedContent: enc, ID: "rs_x"}, back)
}

// --- 身份键撞车回归：reasoning 与 message 同 id 时不得污染正文块 ---

func TestReasoningStreamMessToRespDoneIDCollision(t *testing.T) {
	const enc = "gAAAA-collided"
	// 非规范上游：message item 与 reasoning item 共用同一个 id "shared"。
	// id 形态的身份键不含 kind，若 done 分支不做「该 index 是不是 thinking 块」
	// 的归属判据，就会把 signature_delta 塞进 text 块并提前 stop —— 之后的正文
	// delta 全被丢弃，客户端拿到半截正文。
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_c","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"shared","type":"message","role":"assistant","content":[]}}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"shared","output_index":0,"delta":"前半"}`,
		// 撞车的 reasoning added（thinking 门关闭 → 整块丢弃，且不得写负索引占位）
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"shared","type":"reasoning","summary":[]}}`,
		// 撞车的 reasoning 文本 delta 也不得落进正文块
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"shared","output_index":0,"summary_index":0,"delta":"错位的思考"}`,
		"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"shared","type":"reasoning","summary":[],"encrypted_content":"`+enc+`"}}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"shared","output_index":0,"delta":"后半"}`,
		"response.output_text.done", `{"type":"response.output_text.done","item_id":"shared","output_index":0,"text":"前半后半"}`,
	)

	// ④ 所有帧的 index 必须非负（内部占位常量不得外泄）
	for _, fr := range frames {
		if idx, ok := fr.data["index"]; ok {
			require.GreaterOrEqual(t, idx.(float64), float64(0), "帧 index 不得为负: %s", fr.raw)
		}
	}

	// ① 正文块不得收到 signature_delta / thinking_delta
	for _, fr := range framesOfType(frames, "content_block_delta") {
		dt := deltaPayload(t, fr)["type"]
		require.NotEqual(t, "signature_delta", dt,
			"id 撞车时 reasoning done 必须整帧丢弃，不得把签名塞进正文块")
		require.NotEqual(t, "thinking_delta", dt,
			"id 撞车时 reasoning 文本 delta 也不得落进正文块")
	}

	// ② 正文块不得被提前 stop：done 之后的 delta 仍要出帧
	var texts []string
	for _, fr := range framesOfType(frames, "content_block_delta") {
		if deltaPayload(t, fr)["type"] == "text_delta" {
			texts = append(texts, deltaPayload(t, fr)["text"].(string))
		}
	}
	require.Equal(t, []string{"前半", "后半"}, texts, "撞车不得截断正文")

	// ③ 总正文与上游一致，且只有一个 stop（来自 output_text.done）
	require.Equal(t, "前半后半", strings.Join(texts, ""))
	stops := framesOfType(frames, "content_block_stop")
	require.Len(t, stops, 1, "只有正文块 stop 一次")
	require.EqualValues(t, 0, frameIndexOf(t, stops[0]))
	require.Equal(t, map[float64]string{0: "text"}, blockKindMap(t, frames),
		"不得凭空 start 出 thinking 块")
}

// --- 反向撞车：thinking 块先占 index，正文 item 复用同一 id ---

func TestReasoningStreamMessToRespReverseIDCollision(t *testing.T) {
	const enc = "gAAAA-reverse"
	arm := sealReasoningRef(reasoningRef{EncryptedContent: enc, ID: "shared"})
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_r","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		// thinking 先拿到 index 0（id=shared）
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"shared","type":"reasoning","summary":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"shared","output_index":0,"summary_index":0,"delta":"想"}`,
		// 正文 item 复用同一个 id：不得落进 thinking 块，必须另立 index
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"shared","type":"message","role":"assistant","content":[]}}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"shared","output_index":0,"delta":"正文"}`,
		"response.output_text.done", `{"type":"response.output_text.done","item_id":"shared","output_index":0,"text":"正文"}`,
		// thinking 的收尾（信封不得因正文 done 丢失）
		"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"shared","type":"reasoning","summary":[],"encrypted_content":"`+enc+`"}}`,
	)

	kinds := blockKindMap(t, frames)
	require.Len(t, kinds, 2, "thinking 与正文必须各占一个 index")
	var thinkIdx, textIdx float64 = -1, -1
	for idx, kind := range kinds {
		switch kind {
		case "thinking":
			thinkIdx = idx
		case "text":
			textIdx = idx
		}
	}
	require.GreaterOrEqual(t, thinkIdx, float64(0))
	require.GreaterOrEqual(t, textIdx, float64(0))
	require.NotEqual(t, thinkIdx, textIdx, "正文不得复用 thinking 的 index")

	// ② 各自 delta 只落在自己块内
	for _, fr := range framesOfType(frames, "content_block_delta") {
		dt, _ := deltaPayload(t, fr)["type"].(string)
		idx := frameIndexOf(t, fr)
		switch dt {
		case "thinking_delta", "signature_delta":
			require.Equal(t, thinkIdx, idx, "%s 必须落在 thinking 块", dt)
		case "text_delta":
			require.Equal(t, textIdx, idx, "%s 必须落在正文块", dt)
		}
	}

	// ③ 两个块各自 start / stop 恰好一次
	started := make(map[float64]int)
	for _, fr := range framesOfType(frames, "content_block_start") {
		started[frameIndexOf(t, fr)]++
	}
	require.Equal(t, map[float64]int{thinkIdx: 1, textIdx: 1}, started)
	stopped := make(map[float64]int)
	for _, fr := range framesOfType(frames, "content_block_stop") {
		stopped[frameIndexOf(t, fr)]++
	}
	require.Equal(t, map[float64]int{thinkIdx: 1, textIdx: 1}, stopped, "每块只 stop 一次")

	// ④ thinking 的信封仍然发出（正文 done 不得吞掉它）
	var sigSeen bool
	for _, fr := range framesOfType(frames, "content_block_delta") {
		if deltaPayload(t, fr)["type"] != "signature_delta" {
			continue
		}
		sigSeen = true
		require.Equal(t, arm, deltaPayload(t, fr)["signature"])
	}
	require.True(t, sigSeen, "thinking 块的信封必须仍在 thinking 块上发出")
}

// --- §6.3 #30：thinking 门（先 text 后 reasoning → 整块丢弃）---

func TestReasoningStreamMessToRespThinkingGate(t *testing.T) {
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_5","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"先说话"}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"rs_late","type":"reasoning","summary":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_late","output_index":1,"summary_index":0,"delta":"迟到的思考"}`,
		"response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"id":"rs_late","type":"reasoning","summary":[],"encrypted_content":"blob"}}`,
	)

	kinds := blockKindMap(t, frames)
	require.Equal(t, map[float64]string{0: "text"}, kinds, "迟到的 thinking 块不得 start")
	require.Empty(t, framesOfType(frames, "response.output_item.done"), "不得发 done")
	require.Empty(t, framesOfType(frames, "response.reasoning_summary_text.delta"), "不得发 summary delta")
	for _, fr := range frames {
		require.NotContains(t, fr.raw, "迟到的思考", "门后的 thinking 文本不得外泄")
	}
}

// --- §6.3 #30b：迟到 done（signature_delta 仍落原 index 且早于其 stop）---

func TestReasoningStreamMessToRespLateDone(t *testing.T) {
	const enc = "gAAAA-late"
	arm := sealReasoningRef(reasoningRef{EncryptedContent: enc, ID: "rs_1"})
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_6","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"想"}`,
		// thinking 还没 done，text 先开始（允许短暂并行：新块取更高 index）
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"答"}`,
		// 迟到的 thinking done
		"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"`+enc+`"}}`,
	)

	kinds := blockKindMap(t, frames)
	require.Equal(t, map[float64]string{0: "thinking", 1: "text"}, kinds)

	// signature_delta 落在原（thinking）index，且严格早于该 index 的 stop
	sigAt, stopAt := -1, -1
	var sigIdx float64 = -1
	for i, fr := range frames {
		switch frameType(fr) {
		case "content_block_delta":
			if deltaPayload(t, fr)["type"] == "signature_delta" {
				sigAt = i
				sigIdx = frameIndexOf(t, fr)
				require.Equal(t, arm, deltaPayload(t, fr)["signature"])
			}
		case "content_block_stop":
			if idx := frameIndexOf(t, fr); idx == 0 {
				stopAt = i
			}
		}
	}
	require.Greater(t, sigAt, -1, "迟到 done 仍要发签名")
	require.EqualValues(t, 0, sigIdx, "签名必须落在 thinking 块的原 index")
	require.Greater(t, stopAt, sigAt, "签名严格早于该 index 的 stop")
}

// --- §6.3 #31：chat 两个方向的思考文本 ---

func TestReasoningStreamChatDirections(t *testing.T) {
	// chat_to_resp：resp 的思考 delta → chat 的 delta.reasoning_content
	frames := runStream(t, domain.ProtocolConvertChatToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_7","object":"response","created_at":1,"status":"in_progress","model":"gpt","output":[],"usage":null}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"想"}`,
		"response.reasoning_text.delta", `{"type":"response.reasoning_text.delta","item_id":"rs_1","output_index":0,"content_index":0,"delta":"一下"}`,
	)
	var texts []string
	for _, fr := range frames {
		choices, ok := fr.data["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		chunk := choices[0].(map[string]any)
		delta, ok := chunk["delta"].(map[string]any)
		if !ok {
			continue
		}
		if rc, ok := delta["reasoning_content"].(string); ok {
			texts = append(texts, rc)
		}
	}
	require.Equal(t, []string{"想", "一下"}, texts, "chat_to_resp：两类 reasoning delta → reasoning_content")

	// chat_to_mess：thinking_delta → reasoning_content；signature_delta 丢弃
	frames = runStream(t, domain.ProtocolConvertChatToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
	)
	require.Empty(t, framesOfType(frames, "content_block_delta"), "signature_delta 不产帧")
	var chatTexts []string
	for _, fr := range frames {
		choices, ok := fr.data["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		delta, ok := choices[0].(map[string]any)["delta"].(map[string]any)
		if !ok {
			continue
		}
		if rc, ok := delta["reasoning_content"].(string); ok {
			chatTexts = append(chatTexts, rc)
		}
	}
	require.Equal(t, []string{"想"}, chatTexts, "chat_to_mess：thinking_delta → reasoning_content")
}

// --- §4.5 第 2 条回归：上游**错配** output_index（身份必须靠 item id）---

func TestReasoningStreamMessToRespMismatchedOutputIndex(t *testing.T) {
	frames := runStream(t, domain.ProtocolConvertMessToResp,
		"response.created", `{"type":"response.created","response":{"id":"rsp_8","object":"response","status":"in_progress","model":"gpt","output":[]}}`,
		"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"想"}`,
		// message 项在 output_index 2，但后续 delta 报的是 0 / 1（错配）
		"response.output_item.added", `{"type":"response.output_item.added","output_index":2,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		"response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"答"}`,
		"response.output_text.done", `{"type":"response.output_text.done","item_id":"msg_1","output_index":1,"text":"答"}`,
		"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"blob"}}`,
	)

	kinds := blockKindMap(t, frames)
	require.Equal(t, map[float64]string{0: "thinking", 2: "text"}, kinds,
		"text 块必须留在 added 分配的 index 2（不能跟着错配的 output_index 跑）")

	var textIdx, thinkIdx float64 = -1, -1
	for idx, kind := range kinds {
		if kind == "text" {
			textIdx = idx
		} else {
			thinkIdx = idx
		}
	}
	for _, fr := range framesOfType(frames, "content_block_delta") {
		if deltaPayload(t, fr)["type"] == "text_delta" {
			require.Equal(t, textIdx, frameIndexOf(t, fr), "text_delta 必须落在文本块 index")
		}
	}
	stopped := make(map[float64]bool)
	for _, fr := range framesOfType(frames, "content_block_stop") {
		idx := frameIndexOf(t, fr)
		_, started := kinds[idx]
		require.True(t, started, "stop 的 index %v 必须先 start", idx)
		require.False(t, stopped[idx], "同一 index 只能 stop 一次: %v", idx)
		stopped[idx] = true
	}
	require.Equal(t, map[float64]bool{thinkIdx: true, textIdx: true}, stopped, "两块各收尾一次")
}

// --- thinking 门在 mess_to_resp 方向同样生效（上游 mess 侧迟到 thinking）---

func TestReasoningStreamRespToMessThinkingGate(t *testing.T) {
	frames := runStream(t, domain.ProtocolConvertRespToMess,
		"message_start", `{"type":"message_start","message":{"id":"msg_4","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"先说话"}}`,
		"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"迟到的思考"}}`,
		"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig_late"}}`,
		"content_block_stop", `{"type":"content_block_stop","index":1}`,
		"message_stop", `{"type":"message_stop"}`,
	)
	// 只有 text 块的 added（message 项）；门后的 thinking 块不得产任何 item 事件
	added := framesOfType(frames, "response.output_item.added")
	require.Len(t, added, 1, "只有 text 块的 added")
	require.Equal(t, "message", added[0].data["item"].(map[string]any)["type"], "不是 reasoning item")
	require.Empty(t, framesOfType(frames, "response.output_item.done"), "门后的 thinking 块不得发 done")
	require.Empty(t, framesOfType(frames, "response.reasoning_summary_text.delta"), "门后不得发思考 delta")
	for _, fr := range frames {
		require.NotContains(t, fr.raw, "迟到的思考")
		require.NotContains(t, fr.raw, "sig_late")
	}
}
