// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// (f) 不变量（spec §3.3/§3.6）。M1 只跑**负例**：断言检查器能识别历史字节里的
// 非法组合（这些字节是硬编码的报文，不由 converter 产生）；M2 起把不变量接到
// 全部黄金用例的输出上。
//
// 三条 §3.3 不变量：
//  1. 不存在孤儿 tool_result（tool_use_id 必有对应 tool_use）；违反 → 调用方 drop。
//  2. **允许**孤儿 function_call（reasoning_test.go 的形态必须绿）。
//  3. assistant 项非空；call_id 不重复。
//
// 另两条是本 spec 的存在理由（RT1/J2），与 1 同为「组合性非法」：
//  4. resp 目标的 assistant 项不得出现 input_text（§1.1 的 400 报文）。
//  5. mess 目标的 assistant 消息不得出现 image 块。

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// decodeAny 解码任意 JSON（测试期，分配不受约束；坏 JSON 直接 panic——
// 语料是硬编码的合法报文）。helper 不接 *testing.T，便于在断言里复用。
func decodeAny(body []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		panic(err)
	}
	return m
}

// respAssistantInputTexts 违反 4：resp 请求的 assistant message 项里出现
// input_text 部件（上游只接受 output_text / refusal —— RT1）。
func respAssistantInputTexts(body []byte) []string {
	m := decodeAny(body)
	input, _ := m["input"].([]any)
	var bad []string
	for i, it := range input {
		item, ok := it.(map[string]any)
		if !ok || item["type"] != "message" || item["role"] != "assistant" {
			continue
		}
		content, _ := item["content"].([]any)
		for j, p := range content {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if part["type"] == "input_text" {
				bad = append(bad, "input["+strconv.Itoa(i)+"].content["+strconv.Itoa(j)+"]")
			}
		}
	}
	return bad
}

// messAssistantImages 违反 5：mess 目标的 assistant 消息里出现 image 块（J2）。
func messAssistantImages(body []byte) []string {
	m := decodeAny(body)
	msgs, _ := m["messages"].([]any)
	var bad []string
	for i, mv := range msgs {
		msg, ok := mv.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		content, _ := msg["content"].([]any)
		for j, b := range content {
			blk, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "image", "document":
				bad = append(bad, "messages["+strconv.Itoa(i)+"].content["+strconv.Itoa(j)+"]")
			}
		}
	}
	return bad
}

// messOrphanToolResults 违反 1：tool_result 的 tool_use_id 没有对应 tool_use。
func messOrphanToolResults(body []byte) []string {
	m := decodeAny(body)
	msgs, _ := m["messages"].([]any)
	used := map[string]bool{}
	for _, mv := range msgs {
		msg, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		content, _ := msg["content"].([]any)
		for _, b := range content {
			if blk, ok := b.(map[string]any); ok && blk["type"] == "tool_use" {
				if id, ok := blk["id"].(string); ok {
					used[id] = true
				}
			}
		}
	}
	var bad []string
	for i, mv := range msgs {
		msg, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		content, _ := msg["content"].([]any)
		for j, b := range content {
			blk, ok := b.(map[string]any)
			if !ok || blk["type"] != "tool_result" {
				continue
			}
			id, _ := blk["tool_use_id"].(string)
			if !used[id] {
				bad = append(bad, "messages["+strconv.Itoa(i)+"].content["+strconv.Itoa(j)+"]")
			}
		}
	}
	return bad
}

// messDuplicateCallIDs 违反 3b：同一请求里 tool_use 的 id 重复。
func messDuplicateCallIDs(body []byte) []string {
	m := decodeAny(body)
	msgs, _ := m["messages"].([]any)
	seen := map[string]bool{}
	var dup []string
	for _, mv := range msgs {
		msg, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		content, _ := msg["content"].([]any)
		for _, b := range content {
			blk, ok := b.(map[string]any)
			if !ok || blk["type"] != "tool_use" {
				continue
			}
			id, _ := blk["id"].(string)
			if seen[id] {
				dup = append(dup, id)
			}
			seen[id] = true
		}
	}
	return dup
}

// respEmptyAssistantItems 违反 3a：assistant message 项没有任何内容部件。
func respEmptyAssistantItems(body []byte) []string {
	m := decodeAny(body)
	input, _ := m["input"].([]any)
	var bad []string
	for i, it := range input {
		item, ok := it.(map[string]any)
		if !ok || item["type"] != "message" || item["role"] != "assistant" {
			continue
		}
		content, _ := item["content"].([]any)
		if len(content) == 0 {
			bad = append(bad, "input["+strconv.Itoa(i)+"]")
		}
	}
	return bad
}

// --- M1 负例：检查器必须识别历史字节 ---

// TestInvariantAssistantInputText 违反 4 的负例：§1.1 上游 400 的报文形态
// （assistant 历史里写 input_text）。
func TestInvariantAssistantInputText(t *testing.T) {
	bad := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"assistant","content":[{"type":"input_text","text":"a"}]}
	]}`)
	got := respAssistantInputTexts(bad)
	require.Len(t, got, 1, "必须识别 assistant 项里的 input_text")
	require.Equal(t, "input[1].content[0]", got[0])

	good := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]}
	]}`)
	require.Empty(t, respAssistantInputTexts(good))
}

// TestInvariantAssistantNoMedia 违反 5 的负例：assistant 消息里的 image 块。
func TestInvariantAssistantNoMedia(t *testing.T) {
	bad := []byte(`{"model":"m","messages":[{"role":"assistant","content":[
		{"type":"text","text":"t"},
		{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}}
	]}]}`)
	got := messAssistantImages(bad)
	require.Len(t, got, 1, "必须识别 assistant 消息里的 image 块")
	require.Equal(t, "messages[0].content[1]", got[0])

	good := []byte(`{"model":"m","messages":[
		{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://e.com/i.png"}}]}
	]}`)
	require.Empty(t, messAssistantImages(good), "user 消息的 image 块合法")
}

// TestInvariantNoOrphanToolResult 违反 1 的负例：孤儿 tool_result（M2 起该
// 行为改为 drop，故 M1 只断言检查器识别历史字节）。
func TestInvariantNoOrphanToolResult(t *testing.T) {
	orphan := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tu1","content":"x"}
	]}]}`)
	require.Len(t, messOrphanToolResults(orphan), 1, "必须识别孤儿 tool_result")

	paired := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"x"}]}
	]}`)
	require.Empty(t, messOrphanToolResults(paired))
}

// TestInvariantAssistantItemInvariants 违反 3 的负例：assistant 项为空、
// call_id 重复。
func TestInvariantAssistantItemInvariants(t *testing.T) {
	empty := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"assistant","content":[]}
	]}`)
	require.Len(t, respEmptyAssistantItems(empty), 1, "必须识别空的 assistant 项")

	dup := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[
			{"type":"tool_use","id":"tu1","name":"f","input":{}},
			{"type":"tool_use","id":"tu1","name":"g","input":{}}
		]}
	]}`)
	require.Equal(t, []string{"tu1"}, messDuplicateCallIDs(dup))
}

// TestInvariantAllowsOrphanFunctionCall 不变量 2：孤儿 function_call 合法
// （reasoning_test.go:317/345/370/401 的形态）。
func TestInvariantAllowsOrphanFunctionCall(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}
	]}`)
	require.Empty(t, messOrphanToolResults(body), "孤儿 function_call 不属任何不变量违规")
	require.Empty(t, respAssistantInputTexts(body))
	require.Empty(t, respEmptyAssistantItems(body))
}

// TestInvariantOnConverterOutput_fixed 已修正的两条（4/5）在 converter 输出上
// 成立——这是 M1 三处修正的直接反面断言。
func TestInvariantOnConverterOutputFixed(t *testing.T) {
	out, err := ConvertRequest([]byte(`{"messages":[
		{"role":"assistant","content":"hi"},
		{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]}
	]}`), domain.ProtocolConvertChatToResp)
	require.NoError(t, err)
	require.Empty(t, respAssistantInputTexts(out), "assistant 历史不得写 input_text（RT1）")

	out, err = ConvertRequest([]byte(`{"messages":[
		{"role":"assistant","content":[{"type":"image_url","image_url":"https://e.com/i.png"}]}
	]}`), domain.ProtocolConvertChatToMess)
	require.NoError(t, err)
	require.Empty(t, messAssistantImages(out), "assistant 消息不得出现 image 块（J2）")
}
