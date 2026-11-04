// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

// issue 46 门禁 —— M1：请求侧四方向（spec:
// .omo/plans/spec-protoconv-reasoning.md §6.1 第 1–15 条）。
//
// 断言口径同 spec §6：一切「信封可解」的断言都把信封解出来与原始值逐字节比对，
// 不靠子串匹配。非流响应 / 流式两个面属 M2 / M3，本文件不覆盖。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// userMsg 一个最小的 user 轮（各方向请求体共用）：既是轮边界，也保证
// messages / input 非空。
const (
	respUserInput = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	chatUserMsg   = `{"role":"user","content":"hi"}`
	messUserMsg   = `{"role":"user","content":"hi"}`
)

// sealOf 测试用信封：Anthropic thinking 块 → encrypted_content 字符串。
func sealOf(typ, thinking, signature, data string) string {
	return sealThinking(thinkingBlock{Type: typ, Thinking: thinking, Signature: signature, Data: data})
}

// armOf 测试用第二类信封臂：上游不透明 encrypted_content → thinking 块 signature。
func armOf(encryptedContent, id string) string {
	return sealReasoningRef(reasoningRef{EncryptedContent: encryptedContent, ID: id})
}

// requestOf 跑一次请求方向转换并解析成 map（断言结构用）。
func requestOf(t *testing.T, dir domain.ProtocolConvert, body string) map[string]any {
	t.Helper()
	out, err := ConvertRequest([]byte(body), dir)
	require.NoError(t, err)
	return obj(t, out)
}

// blocksOf 取顶层数组 key 里第 i 项的 content 块序列。
func blocksOf(t *testing.T, m map[string]any, key string, i int) []any {
	t.Helper()
	msgs := arrOf(t, m, key)
	require.Greater(t, len(msgs), i, "%s 至少 %d 条", key, i+1)
	msg, ok := msgs[i].(map[string]any)
	require.True(t, ok, "%s[%d] 非对象", key, i)
	c, ok := msg["content"].([]any)
	require.True(t, ok, "%s[%d].content 非数组", key, i)
	return c
}

// thinkingOf 取转换结果里的 thinking 字段（无 → nil）。
func thinkingOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	v, ok := m["thinking"]
	if !ok {
		return nil
	}
	th, ok := v.(map[string]any)
	require.True(t, ok, "thinking 非对象")
	return th
}

// thinkingBlockCount 统计 messages 里 thinking / redacted_thinking 块的数量。
func thinkingBlockCount(t *testing.T, m map[string]any) int {
	t.Helper()
	n := 0
	for _, mv := range arrOf(t, m, "messages") {
		msg, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, blk := range content {
			bm, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			if bm["type"] == "thinking" || bm["type"] == "redacted_thinking" {
				n++
			}
		}
	}
	return n
}

// --- §6.1 #1：effort → budget 表 ---

func TestReasoningRequestEffortToBudget(t *testing.T) {
	cases := []struct {
		effort string
		budget float64
	}{
		{"low", 2048},
		{"medium", 8192},
		{"high", 16384},
		{"xhigh", 16384},
		{"max", 16384},
	}
	for _, c := range cases {
		body := `{"model":"m","max_output_tokens":64000,"reasoning":{"effort":"` + c.effort +
			`"},"input":[` + respUserInput + `]}`
		out := requestOf(t, domain.ProtocolConvertRespToMess, body)
		th := thinkingOf(t, out)
		require.NotNil(t, th, "effort=%s 应开启 thinking", c.effort)
		require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": c.budget}, th, c.effort)
		// 恒满足 1024 ≤ budget < max_tokens（mess.txt:3831）
		require.Equal(t, float64(64000), out["max_tokens"], "绝不抬高客户端上限")
	}
}

// --- §6.1 #2：none / minimal / 未知 / 缺失 → 无 thinking ---

func TestReasoningRequestNoThinking(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "turbo"} {
		body := `{"model":"m","max_output_tokens":64000,"reasoning":{"effort":"` + effort +
			`"},"input":[` + respUserInput + `]}`
		require.NotContains(t, requestOf(t, domain.ProtocolConvertRespToMess, body), "thinking",
			"effort=%s 枚举外/不开启", effort)
	}

	// reasoning 缺失 / 缺 effort / 非对象
	for _, reasoning := range []string{``, `"reasoning":{"summary":"auto"},`, `"reasoning":null,`} {
		body := `{"model":"m","max_output_tokens":64000,` + reasoning + `"input":[` + respUserInput + `]}`
		require.NotContains(t, requestOf(t, domain.ProtocolConvertRespToMess, body), "thinking", reasoning)
	}

	// chat 侧：枚举外 / 缺失
	for _, effort := range []string{`"reasoning_effort":"turbo",`, ``} {
		body := `{"model":"m","max_tokens":8000,` + effort + `"messages":[` + chatUserMsg + `]}`
		require.NotContains(t, requestOf(t, domain.ProtocolConvertChatToMess, body), "thinking", effort)
	}
}

// --- §6.1 #3：no_raise（只压不抬；容量不足如实不开启）---

func TestReasoningRequestNoRaise(t *testing.T) {
	cases := []struct {
		name    string
		maxOut  string // 字段片段（含尾逗号）
		wantMax float64
		budget  float64 // 0 = 不开启
	}{
		{"50000", `"max_output_tokens":50000,`, 50000, 16384},
		{"32768", `"max_output_tokens":32768,`, 32768, 16384},
		{"16384", `"max_output_tokens":16384,`, 16384, 15360},
		{"4096", `"max_output_tokens":4096,`, 4096, 3072},
		{"2048", `"max_output_tokens":2048,`, 2048, 1024},
		{"2047-无容量", `"max_output_tokens":2047,`, 2047, 0},
		{"缺失", ``, 4096, 3072},
	}
	for _, c := range cases {
		body := `{"model":"m",` + c.maxOut + `"reasoning":{"effort":"high"},"input":[` + respUserInput + `]}`
		out := requestOf(t, domain.ProtocolConvertRespToMess, body)
		require.Equal(t, c.wantMax, out["max_tokens"], "%s: 写出值（绝不抬高）", c.name)
		th := thinkingOf(t, out)
		if c.budget == 0 {
			require.Nil(t, th, "%s: budget 不足 1024 → 不开启", c.name)
			continue
		}
		require.NotNil(t, th, "%s: 应开启", c.name)
		require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": c.budget}, th, c.name)
		require.Less(t, c.budget, c.wantMax, "%s: budget 必须小于写出的 max_tokens", c.name)
	}
}

// --- §6.1 #4：全字段非法 → 写出 4096 且 budget 3072（同一个变量）---

func TestReasoningRequestNormalizeWhenAllFieldsInvalid(t *testing.T) {
	invalids := []string{`"4096"`, `"1_000"`, `"04096"`, `"+4096"`, `4096.9`, `1e20`, `null`, `0`, `-5`}
	for _, bad := range invalids {
		body := `{"model":"m","reasoning_effort":"medium",` +
			`"max_completion_tokens":` + bad + `,"max_tokens":` + bad + `,` +
			`"messages":[` + chatUserMsg + `]}`
		out := requestOf(t, domain.ProtocolConvertChatToMess, body)
		require.Equal(t, float64(4096), out["max_tokens"], "chat: %s → 写出 4096", bad)
		require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": float64(3072)},
			thinkingOf(t, out), "chat: %s → budget 按 4096 算", bad)
	}

	// resp 方向只有一个上限字段：非法 → 同样归一为 4096
	for _, bad := range invalids {
		body := `{"model":"m","max_output_tokens":` + bad + `,"reasoning":{"effort":"medium"},"input":[` + respUserInput + `]}`
		out := requestOf(t, domain.ProtocolConvertRespToMess, body)
		require.Equal(t, float64(4096), out["max_tokens"], "resp: %s → 写出 4096", bad)
		require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": float64(3072)},
			thinkingOf(t, out), "resp: %s → budget 按 4096 算", bad)
	}
}

// --- §6.1 #5：逐字段判定（非法/缺失 → 继续下一字段，不直接落 4096）---

func TestReasoningRequestMaxTokensFieldPriority(t *testing.T) {
	// max_completion_tokens:null 视为缺失 → 用 max_tokens:8000（写出与 budget 同源）
	body := `{"model":"m","reasoning_effort":"high","max_completion_tokens":null,"max_tokens":8000,` +
		`"messages":[` + chatUserMsg + `]}`
	out := requestOf(t, domain.ProtocolConvertChatToMess, body)
	require.Equal(t, float64(8000), out["max_tokens"], "null 视为缺失 → 继续看 max_tokens")
	require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": float64(6976)},
		thinkingOf(t, out), "budget 与写出值同源（8000-1024）")

	// 优先级：max_completion_tokens 合法则用它
	body = `{"model":"m","reasoning_effort":"high","max_completion_tokens":12000,"max_tokens":8000,` +
		`"messages":[` + chatUserMsg + `]}`
	out = requestOf(t, domain.ProtocolConvertChatToMess, body)
	require.Equal(t, float64(12000), out["max_tokens"], "max_completion_tokens 优先")
	require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": float64(10976)},
		thinkingOf(t, out), "budget 按 12000 算（min(16384, 10976)）")
}

// --- §6.1 #6：§3.2 伴随改写（丢采样参数 / 强制 tool_choice 优先）---

func TestReasoningRequestCompanionRewrites(t *testing.T) {
	// thinking 开启 → 丢 temperature / top_p
	body := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},` +
		`"temperature":0.3,"top_p":0.9,"input":[` + respUserInput + `]}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, body)
	require.NotNil(t, thinkingOf(t, out))
	require.NotContains(t, out, "temperature", "thinking 开启 → 丢 temperature")
	require.NotContains(t, out, "top_p", "thinking 开启 → 丢 top_p")

	// 未开启 → 采样参数照常透传
	body = `{"model":"m","max_output_tokens":32000,"temperature":0.3,"top_p":0.9,"input":[` + respUserInput + `]}`
	out = requestOf(t, domain.ProtocolConvertRespToMess, body)
	require.Equal(t, float64(0.3), out["temperature"])
	require.Equal(t, float64(0.9), out["top_p"])

	// 强制 tool_choice（any / tool）→ 不开启 thinking，且强制意图原样保留
	for _, c := range []struct {
		toolChoice string
		want       map[string]any
	}{
		{`"required"`, map[string]any{"type": "any"}},
		{`{"type":"function","name":"f"}`, map[string]any{"name": "f", "type": "tool"}},
	} {
		body := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},` +
			`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],` +
			`"tool_choice":` + c.toolChoice + `,"input":[` + respUserInput + `]}`
		out := requestOf(t, domain.ProtocolConvertRespToMess, body)
		require.Nil(t, thinkingOf(t, out), "tool_choice=%s 强制 → 不开启 thinking", c.toolChoice)
		require.Equal(t, c.want, out["tool_choice"], "……但强制意图必须保留")
	}

	// chat 方向同样受 §3.2 约束
	body = `{"model":"m","reasoning_effort":"high","max_tokens":32000,"temperature":0.4,` +
		`"messages":[` + chatUserMsg + `]}`
	out = requestOf(t, domain.ProtocolConvertChatToMess, body)
	require.NotNil(t, thinkingOf(t, out))
	require.NotContains(t, out, "temperature", "chat→mess: thinking 开启 → 丢 temperature")
}

// --- §3.2（扩展口径：所有目标方向）：resp 目标开启 reasoning 时同样丢 sampling ---

func TestReasoningRespTargetDropsSampling(t *testing.T) {
	// mess_to_resp：thinking 开启 → 丢 temperature / top_p
	body := `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"temperature":0.3,"top_p":0.9,"messages":[` + messUserMsg + `]}`
	out := requestOf(t, domain.ProtocolConvertMessToResp, body)
	require.Contains(t, out, "reasoning", "reasoning 仍要开启")
	require.NotContains(t, out, "temperature", "reasoning 开启 → 丢 temperature")
	require.NotContains(t, out, "top_p", "reasoning 开启 → 丢 top_p")

	// 未开启 reasoning → 原样透传
	body = `{"model":"m","max_tokens":32000,"temperature":0.3,"top_p":0.9,"messages":[` + messUserMsg + `]}`
	out = requestOf(t, domain.ProtocolConvertMessToResp, body)
	require.Equal(t, float64(0.3), out["temperature"])
	require.Equal(t, float64(0.9), out["top_p"])

	// chat_to_resp：reasoning_effort 开启 → 丢 temperature / top_p
	body = `{"model":"m","messages":[` + chatUserMsg + `],"reasoning_effort":"high",` +
		`"temperature":0.4,"top_p":0.5}`
	out = requestOf(t, domain.ProtocolConvertChatToResp, body)
	require.Contains(t, out, "reasoning", "reasoning 仍要开启")
	require.NotContains(t, out, "temperature", "reasoning 开启 → 丢 temperature")
	require.NotContains(t, out, "top_p", "reasoning 开启 → 丢 top_p")

	// 未开启 → 透传
	body = `{"model":"m","messages":[` + chatUserMsg + `],"temperature":0.4,"top_p":0.5}`
	out = requestOf(t, domain.ProtocolConvertChatToResp, body)
	require.Equal(t, float64(0.4), out["temperature"])
	require.Equal(t, float64(0.5), out["top_p"])

	// resp 目标**不**因 reasoning 对 tool_choice 降级（OpenAI 侧支持强制工具调用）
	body = `{"model":"m","messages":[` + chatUserMsg + `],"reasoning_effort":"high",` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],` +
		`"tool_choice":"required"}`
	out = requestOf(t, domain.ProtocolConvertChatToResp, body)
	require.Contains(t, out, "reasoning", "强制 tool_choice 不得关掉 reasoning")
	require.Equal(t, "required", out["tool_choice"], "强制意图原样保留")
}

// --- §6.1 #7：信封回放（原顺序进 assistant 轮，逐字节相等）---

func TestReasoningRequestReplayEnvelopeOrder(t *testing.T) {
	thinkEnc := sealOf("thinking", "先查一下目录", "sig_1", "")
	redactedEnc := sealOf("redacted_thinking", "", "", "OPAQUE-DATA")
	body := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":[` +
		respUserInput + `,` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + thinkEnc + `"},` +
		`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"` + redactedEnc + `"},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, body)

	require.Len(t, arrOf(t, out, "messages"), 2, "user + 合并后的 assistant 轮")
	blocks := blocksOf(t, out, "messages", 1)
	require.Len(t, blocks, 3, "thinking + redacted_thinking + tool_use")

	first := blocks[0].(map[string]any)
	require.Equal(t, "thinking", first["type"], "thinking 按 input 原顺序")
	require.Equal(t, "先查一下目录", first["thinking"])
	require.Equal(t, "sig_1", first["signature"])

	second := blocks[1].(map[string]any)
	require.Equal(t, "redacted_thinking", second["type"], "redacted 紧随其后（原顺序）")
	require.Equal(t, "OPAQUE-DATA", second["data"])

	require.Equal(t, "tool_use", blocks[2].(map[string]any)["type"])
}

// --- §6.1 #8：全有或全无（正：任一项不可回放 → 一个块都不写）---

func TestReasoningRequestAllOrNothingPositive(t *testing.T) {
	good := sealOf("thinking", "可回放", "sig_ok", "")
	body := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":[` +
		respUserInput + `,` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + good + `"},` +
		`{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"无信封"}],"encrypted_content":null},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, body)
	require.Zero(t, thinkingBlockCount(t, out), "一项不可回放 → 全部 thinking 块都不写")
	require.NotContains(t, string(mustMarshal(t, out)), `"signature"`, "不存在伪造的无签名块")

	// 相邻 tool_use 不受影响（不因降级丢工具轮）
	blocks := blocksOf(t, out, "messages", 1)
	require.Len(t, blocks, 1)
	require.Equal(t, "tool_use", blocks[0].(map[string]any)["type"])
}

// --- §6.1 #9：全有或全无（反：工具循环不得被误判）---

func TestReasoningRequestAllOrNothingNegative(t *testing.T) {
	encs := []string{
		sealOf("thinking", "轮一", "sig_1", ""),
		sealOf("thinking", "轮二", "sig_2", ""),
		sealOf("thinking", "轮三", "sig_3", ""),
	}
	// 三工具轮：[user, r, fc, fc_out, r, fc, fc_out, r, fc]
	body := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":[` +
		respUserInput + `,` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + encs[0] + `"},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"o1"},` +
		`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"` + encs[1] + `"},` +
		`{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c2","output":"o2"},` +
		`{"type":"reasoning","id":"rs_3","summary":[],"encrypted_content":"` + encs[2] + `"},` +
		`{"type":"function_call","call_id":"c3","name":"f","arguments":"{}"}]}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, body)

	require.Equal(t, 3, thinkingBlockCount(t, out), "每个 r 都在本轮 fc 之前 → 全部回放")
	msgs := arrOf(t, out, "messages")
	require.Len(t, msgs, 6, "user / assistant ×3 / tool_result ×2 …")
	for _, i := range []int{1, 3, 5} {
		blocks := blocksOf(t, out, "messages", i)
		require.Len(t, blocks, 2, "assistant 轮 = thinking + tool_use")
		blkMap := blocks[0].(map[string]any)
		require.Equal(t, "thinking", blkMap["type"], "thinking 必须在该轮最前")
		require.Equal(t, "tool_use", blocks[1].(map[string]any)["type"])
		want, ok := openThinking(encs[i/2])
		require.True(t, ok)
		require.Equal(t, want.Signature, blkMap["signature"], "签名与信封内的原始签名逐字节相等")
		require.Equal(t, want.Thinking, blkMap["thinking"], "原文与信封内的原文逐字节相等")
	}
}

// --- §6.1 #10：顺序不可修复（同轮 reasoning 在 function_call 之后）---

func TestReasoningRequestOrderUnfixable(t *testing.T) {
	okEnc := sealOf("thinking", "先于工具", "sig_1", "")
	lateEnc := sealOf("thinking", "晚于工具", "sig_2", "")
	body := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":[` +
		respUserInput + `,` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + okEnc + `"},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"` + lateEnc + `"},` +
		`{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"}]}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, body)
	require.Zero(t, thinkingBlockCount(t, out), "同轮 reasoning 出现在 tool_use 之后 → 全部丢弃")
}

// --- §6.1 #11：redacted 信封 → {"type":"redacted_thinking","data":…} ---

func TestReasoningRequestReplayRedacted(t *testing.T) {
	enc := sealOf("redacted_thinking", "", "", "ENCRYPTED-DATA")
	body := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":[` +
		respUserInput + `,` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + enc + `"}]}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, body)
	blocks := blocksOf(t, out, "messages", 1)
	require.Len(t, blocks, 1)
	blk := blocks[0].(map[string]any)
	require.Equal(t, map[string]any{"type": "redacted_thinking", "data": "ENCRYPTED-DATA"}, blk,
		"只带不透明 data（无 thinking / signature 键）")
}

// --- §2.3 轮边界（含任意 *_output 结尾项）---

func TestReasoningScanRespThinkingTurnBoundary(t *testing.T) {
	enc := sealOf("thinking", "想", "sig_1", "")
	okTurn := func(boundary string) string {
		return `[` + respUserInput + `,` +
			`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + enc + `"},` +
			`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
			boundary + `,` +
			`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"` + enc + `"},` +
			`{"type":"function_call","call_id":"c2","name":"f","arguments":"{}"}]`
	}
	// 任意 *_output 结尾的项都切轮：每个 r 都是本轮轮首 → 可回放
	for _, boundary := range []string{
		`{"type":"function_call_output","call_id":"c1","output":"o"}`,
		`{"type":"custom_tool_call_output","call_id":"c1","output":"o"}`,
		`{"type":"local_shell_call_output","call_id":"c1","output":"o"}`,
		`{"type":"computer_call_output","call_id":"c1","output":"o"}`,
	} {
		require.True(t, scanRespThinking(gjson.Parse(okTurn(boundary))), "轮边界 %s", boundary)
	}

	// 同一轮里 reasoning 在 function_call 之后 → 不可回放
	late := `[` + respUserInput + `,` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + enc + `"},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"` + enc + `"}]`
	require.False(t, scanRespThinking(gjson.Parse(late)))

	// 无信封 / 非本包信封 → 不可回放
	for _, encField := range []string{`null`, `""`, `"gAAAA-openai-blob"`} {
		body := `[` + respUserInput + `,{"type":"reasoning","id":"rs_1","encrypted_content":` + encField + `}]`
		require.False(t, scanRespThinking(gjson.Parse(body)), "encrypted_content=%s", encField)
	}

	// input 不是数组（纯字符串输入）→ 没有 reasoning item 需要裁决
	require.True(t, scanRespThinking(gjson.Parse(`"hi"`)))
	require.True(t, scanRespThinking(gjson.Parse(`[]`)))
}

// TestReasoningScanRespThinkingIgnoresUnknownItems 只有 assistant message 与
// function_call 算正文：内建工具 / 引用类 item（web_search_call /
// item_reference / computer_call / mcp_*）不得让同轮 reasoning 被判「顺序不可
// 修复」（否则整请求降级成不带思考）。
func TestReasoningScanRespThinkingIgnoresUnknownItems(t *testing.T) {
	enc := sealOf("thinking", "想", "sig_1", "")
	unknowns := []string{
		`{"type":"web_search_call","id":"ws_1","status":"completed"}`,
		`{"type":"item_reference","id":"ref_1"}`,
		`{"type":"computer_call","call_id":"cc_1","action":{}}`,
		`{"type":"mcp_call","id":"mcp_1","name":"tool"}`,
		`{"type":"mcp_list_tools","id":"mcp_2"}`,
	}
	for _, mid := range unknowns {
		// [user, r, 未知项, r] → 两个 reasoning 都算轮首，全部可回放
		body := `[` + respUserInput + `,` +
			`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + enc + `"},` +
			mid + `,` +
			`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"` + enc + `"}]`
		require.True(t, scanRespThinking(gjson.Parse(body)), "%s 不算正文", mid)

		// [user, 未知项, r] → 同样回放
		body = `[` + respUserInput + `,` + mid +
			`,{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + enc + `"}]`
		require.True(t, scanRespThinking(gjson.Parse(body)), "%s 也不切轮", mid)
	}

	// 反面对照：assistant message 仍算正文 → 其后的 reasoning 不可回放
	body := `[` + respUserInput + `,` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"答"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + enc + `"}]`
	require.False(t, scanRespThinking(gjson.Parse(body)), "assistant 正文之后的 reasoning 不可回放")

	// 请求级：未知项夹在两个 reasoning 之间时，两块都要真的回放成 thinking 块
	encA := sealOf("thinking", "第一段", "sig_a", "")
	encB := sealOf("thinking", "第二段", "sig_b", "")
	reqBody := `{"model":"m","max_output_tokens":32000,"reasoning":{"effort":"high"},"input":[` +
		respUserInput + `,` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"` + encA + `"},` +
		`{"type":"web_search_call","id":"ws_1"},` +
		`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"` + encB + `"}]}`
	out := requestOf(t, domain.ProtocolConvertRespToMess, reqBody)
	require.Equal(t, 2, thinkingBlockCount(t, out), "不得因未知 item 降级")
	// 两块都按原顺序写出（未知 item 会让 assistant 轮另起一段，Anthropic 会合并
	// 连续同角色消息，故只钉文本顺序）
	var texts []string
	for _, mv := range arrOf(t, out, "messages") {
		msg, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, blk := range content {
			bm, ok := blk.(map[string]any)
			if !ok || bm["type"] != "thinking" {
				continue
			}
			texts = append(texts, bm["thinking"].(string))
		}
	}
	require.Equal(t, []string{"第一段", "第二段"}, texts)
}

// --- §6.1 #12：mess→resp 的 thinking / output_config.effort ---

func TestReasoningMessToRespThinkingParam(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil = 不发 reasoning
	}{
		{"enabled-16384", `"thinking":{"type":"enabled","budget_tokens":16384},`,
			map[string]any{"effort": "high", "summary": "auto"}},
		{"enabled-8192", `"thinking":{"type":"enabled","budget_tokens":8192},`,
			map[string]any{"effort": "medium", "summary": "auto"}},
		{"enabled-2048", `"thinking":{"type":"enabled","budget_tokens":2048},`,
			map[string]any{"effort": "low", "summary": "auto"}},
		{"enabled-1025", `"thinking":{"type":"enabled","budget_tokens":1025},`,
			map[string]any{"effort": "low", "summary": "auto"}},
		{"budget-0", `"thinking":{"type":"enabled","budget_tokens":0},`,
			map[string]any{"summary": "auto"}},
		{"budget-负", `"thinking":{"type":"enabled","budget_tokens":-1},`,
			map[string]any{"summary": "auto"}},
		{"adaptive-无budget", `"thinking":{"type":"adaptive"},`,
			map[string]any{"summary": "auto"}},
		{"disabled", `"thinking":{"type":"disabled"},`, nil},
		{"between_tools", `"thinking":{"type":"between_tools"},`, nil},
		{"null", `"thinking":null,`, nil},
		{"缺失", ``, nil},
	}
	for _, c := range cases {
		body := `{"model":"m","max_tokens":32000,` + c.body + `"messages":[` + messUserMsg + `]}`
		out := requestOf(t, domain.ProtocolConvertMessToResp, body)
		if c.want == nil {
			require.NotContains(t, out, "reasoning", "%s: 不发 reasoning", c.name)
			continue
		}
		require.Equal(t, c.want, out["reasoning"], c.name)
	}

	// output_config.effort 覆盖 budget 反查值
	body := `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":2048},` +
		`"output_config":{"effort":"xhigh"},"messages":[` + messUserMsg + `]}`
	require.Equal(t, map[string]any{"effort": "xhigh", "summary": "auto"},
		requestOf(t, domain.ProtocolConvertMessToResp, body)["reasoning"])

	// output_config.effort 枚举外 → 保留反查值
	body = `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":8192},` +
		`"output_config":{"effort":"turbo"},"messages":[` + messUserMsg + `]}`
	require.Equal(t, map[string]any{"effort": "medium", "summary": "auto"},
		requestOf(t, domain.ProtocolConvertMessToResp, body)["reasoning"])
}

// --- §6.1 #13：include 合并去重 + reasoning_ref 臂往返 ---

func TestReasoningMessToRespIncludeMerged(t *testing.T) {
	// 开启 thinking：保留客户端原有元素 + 追加，且无重复
	body := `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"include":["message.output_text.logprobs","reasoning.encrypted_content"],` +
		`"messages":[` + messUserMsg + `]}`
	out := requestOf(t, domain.ProtocolConvertMessToResp, body)
	require.Equal(t, []any{"message.output_text.logprobs", "reasoning.encrypted_content"},
		arrOf(t, out, "include"), "客户端原有元素在前，加密思考只出现一次")

	// 客户端未带 include → 只写加密思考
	body = `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"messages":[` + messUserMsg + `]}`
	require.Equal(t, []any{"reasoning.encrypted_content"},
		arrOf(t, requestOf(t, domain.ProtocolConvertMessToResp, body), "include"))

	// 客户端把 include 写成单个字符串（合法但少见）→ 按单元素并入，不丢原值
	body = `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"include":"message.output_text.logprobs","messages":[` + messUserMsg + `]}`
	require.Equal(t, []any{"message.output_text.logprobs", "reasoning.encrypted_content"},
		arrOf(t, requestOf(t, domain.ProtocolConvertMessToResp, body), "include"),
		"字符串形态也要保留原值")

	// 既非字符串也非数组 → 只发自己的元素（不丢弃整条 include）
	body = `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"include":{"k":1},"messages":[` + messUserMsg + `]}`
	require.Equal(t, []any{"reasoning.encrypted_content"},
		arrOf(t, requestOf(t, domain.ProtocolConvertMessToResp, body), "include"))

	// 未开启 thinking → 不凭空追加（也无加密思考可往返）
	body = `{"model":"m","max_tokens":32000,"messages":[` + messUserMsg + `]}`
	require.NotContains(t, requestOf(t, domain.ProtocolConvertMessToResp, body), "include")
}

func TestReasoningMessToRespArmRoundTrip(t *testing.T) {
	const opaque = "gAAAAAB-upstream-opaque-blob"
	arm := armOf(opaque, "rs_upstream_1")
	require.NotEmpty(t, arm)

	body := `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
		`"messages":[` + messUserMsg + `,` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"想","signature":"` + arm + `"},` +
		`{"type":"text","text":"答"}]},` +
		`{"role":"user","content":"continue"}]}`
	input := arrOf(t, requestOf(t, domain.ProtocolConvertMessToResp, body), "input")
	require.Len(t, input, 4, "user / reasoning / assistant / user")

	rs := input[1].(map[string]any)
	require.Equal(t, "reasoning", rs["type"], "reasoning item 排在该轮内容之前")
	require.Equal(t, opaque, rs["encrypted_content"], "encrypted_content 逐字节往返")
	require.Equal(t, "rs_upstream_1", rs["id"], "id 一并还原")
	require.NotContains(t, rs, "summary", "臂里没有 summary，不得凭空补")

	msg := input[2].(map[string]any)
	require.Equal(t, "assistant", msg["role"])
	content := arrOf(t, msg, "content")
	require.Len(t, content, 1, "thinking 块不外泄成 Anthropic 块")
	require.Equal(t, "output_text", content[0].(map[string]any)["type"])

	// id 为空 → 不写 id 键
	noID := armOf(opaque, "")
	body = `{"model":"m","max_tokens":32000,"messages":[` + messUserMsg + `,` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"想","signature":"` + noID + `"}]}]}`
	input = arrOf(t, requestOf(t, domain.ProtocolConvertMessToResp, body), "input")
	require.Len(t, input, 2)
	require.Equal(t, map[string]any{"type": "reasoning", "encrypted_content": opaque}, input[1],
		"id 为空则不写该键")
}

func TestReasoningMessToRespDropsNonArmSignatures(t *testing.T) {
	// 真 Anthropic 签名 / 空串 / 其他 type 的旧信封 → 丢（约束 A）
	thinkingEnv := sealOf("thinking", "旧信封", "sig_1", "")
	cases := []string{`"sig_real_anthropic"`, `""`, thinkingEnv}
	for _, sig := range cases {
		body := `{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16384},` +
			`"messages":[` + messUserMsg + `,` +
			`{"role":"assistant","content":[{"type":"thinking","thinking":"想","signature":` + sig + `},` +
			`{"type":"text","text":"答"}]}]}`
		input := arrOf(t, requestOf(t, domain.ProtocolConvertMessToResp, body), "input")
		require.Len(t, input, 2, "signature=%s → 不产 reasoning item", sig)
		require.Equal(t, "assistant", input[1].(map[string]any)["role"])
	}

	// 空的 encrypted_content 不得封臂（否则响应侧产出、请求侧丢弃）
	require.Empty(t, sealReasoningRef(reasoningRef{EncryptedContent: ""}),
		"encrypted_content 为空 → 不封臂（退化为 signature:\"\"）")
}

// --- §6.1 #14：chat→mess（effort→budget + 工具历史不开启）---

func TestReasoningChatToMessEffort(t *testing.T) {
	body := `{"model":"m","reasoning_effort":"low","max_tokens":8000,"messages":[` + chatUserMsg + `]}`
	out := requestOf(t, domain.ProtocolConvertChatToMess, body)
	require.Equal(t, map[string]any{"type": "enabled", "budget_tokens": float64(2048)},
		thinkingOf(t, out), "effort low + max_tokens 8000 → budget 2048")
	require.Equal(t, float64(8000), out["max_tokens"], "写出值与 budget 同源，绝不抬高")

	// 历史含 assistant tool_calls → 不开启（chat 无签名通道，回放会 422）
	withTools := `{"model":"m","reasoning_effort":"low","max_tokens":8000,"messages":[` + chatUserMsg + `,` +
		`{"role":"assistant","content":"调用工具","tool_calls":[{"id":"call_1","type":"function",` +
		`"function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`
	require.NotContains(t, requestOf(t, domain.ProtocolConvertChatToMess, withTools), "thinking",
		"历史含 tool_calls → 不开启 thinking")

	// 历史无 tool_calls（含普通 assistant 轮）→ 开启
	noTools := `{"model":"m","reasoning_effort":"low","max_tokens":8000,"messages":[` + chatUserMsg + `,` +
		`{"role":"assistant","content":"好"},` + chatUserMsg + `]}`
	require.NotNil(t, thinkingOf(t, requestOf(t, domain.ProtocolConvertChatToMess, noTools)),
		"历史无 tool_calls → 开启 thinking")

	// chat 的 reasoning_content 无签名通道 → 不进 Anthropic 历史
	withRC := `{"model":"m","reasoning_effort":"low","max_tokens":8000,"messages":[` + chatUserMsg + `,` +
		`{"role":"assistant","content":"好","reasoning_content":"思考"}]}`
	out = requestOf(t, domain.ProtocolConvertChatToMess, withRC)
	asst := blocksOf(t, out, "messages", 1)
	require.Len(t, asst, 1, "assistant 只有 text 块")
	require.Equal(t, "text", asst[0].(map[string]any)["type"])
}

// --- §6.1 #15：chat→resp（summary:"auto" + 枚举外丢弃）---

func TestReasoningChatToRespSummaryAuto(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := `{"model":"m","messages":[` + chatUserMsg + `],"reasoning_effort":"` + effort + `"}`
		require.Equal(t, map[string]any{"effort": effort, "summary": "auto"},
			requestOf(t, domain.ProtocolConvertChatToResp, body)["reasoning"], effort)
	}
	// 枚举外（含非字符串）丢弃
	for _, effort := range []string{"turbo", "bogus", ""} {
		body := `{"model":"m","messages":[` + chatUserMsg + `],"reasoning_effort":"` + effort + `"}`
		require.NotContains(t, requestOf(t, domain.ProtocolConvertChatToResp, body), "reasoning",
			"effort=%s 枚举外丢弃", effort)
	}
}

// --- reasoning.go 的纯函数契约（M1 的其余部分）---

func TestReasoningEnvelopePrimitives(t *testing.T) {
	// thinking 块往返：字段逐字节相等
	enc := sealOf("thinking", "原文 \"quoted\"", "sig_1", "")
	require.True(t, strings.HasPrefix(enc, sealPrefix), "信封带 mark 前缀")
	b, ok := openThinking(enc)
	require.True(t, ok)
	require.Equal(t, thinkingBlock{Type: "thinking", Thinking: "原文 \"quoted\"", Signature: "sig_1"}, b)

	// redacted 块往返
	rb, ok := openThinking(sealOf("redacted_thinking", "", "", "OPAQUE"))
	require.True(t, ok)
	require.Equal(t, thinkingBlock{Type: "redacted_thinking", Data: "OPAQUE"}, rb)

	// 不可回放：无签名 / 无 data / 未知 type
	require.False(t, canReplay(thinkingBlock{Type: "thinking", Thinking: "x"}))
	require.Empty(t, sealThinking(thinkingBlock{Type: "thinking", Thinking: "x"}))
	require.False(t, canReplay(thinkingBlock{Type: "redacted_thinking"}))
	require.False(t, canReplay(thinkingBlock{Type: "text", Thinking: "x", Signature: "s"}))

	// 别家 blob / 坏 base64 / 空串 → 不认
	for _, bad := range []string{"", "gAAAA-openai", sealPrefix + "!!!", sealPrefix + "e30"} {
		_, ok := openThinking(bad)
		require.False(t, ok, "%q 不可解", bad)
	}

	// 原文 ≤256KiB 且信封 ≤512KiB（同一谓词，产出/回放同源）
	tooBig := strings.Repeat("x", maxThinkingBytes+1)
	require.False(t, canReplay(thinkingBlock{Type: "thinking", Thinking: tooBig, Signature: "s"}),
		"原文超 256KiB → 不可往返")
	require.True(t, canReplay(thinkingBlock{Type: "thinking", Thinking: "x", Signature: "s"}))
}

func TestReasoningRefArmPrimitives(t *testing.T) {
	arm := armOf("opaque-blob", "rs_1")
	r, ok := openReasoningRef(arm)
	require.True(t, ok)
	require.Equal(t, reasoningRef{Type: reasoningRefType, EncryptedContent: "opaque-blob", ID: "rs_1"}, r)

	// thinking 型信封不能被当成臂（否则 Anthropic 签名会当 encrypted_content 发上游）
	_, ok = openReasoningRef(sealOf("thinking", "想", "sig_1", ""))
	require.False(t, ok, "判别按解码后的 type")

	// 空 encrypted_content / 别家 blob / 坏 base64 → 不认
	for _, bad := range []string{"", "gAAAA-openai-blob", sealPrefix + "!!!"} {
		_, ok := openReasoningRef(bad)
		require.False(t, ok, "%q 不是 reasoning_ref 臂", bad)
	}
}

func TestReasoningValues(t *testing.T) {
	// effort ↔ budget 反查
	require.Equal(t, "low", effortForBudget(1))
	require.Equal(t, "low", effortForBudget(2048))
	require.Equal(t, "medium", effortForBudget(2049))
	require.Equal(t, "medium", effortForBudget(8192))
	require.Equal(t, "high", effortForBudget(8193))
	require.Equal(t, "high", effortForBudget(16384))
	require.Equal(t, "", effortForBudget(0))
	require.Equal(t, "", effortForBudget(-1))

	// 表值
	require.EqualValues(t, 2048, budgetForEffort("low"))
	require.EqualValues(t, 8192, budgetForEffort("medium"))
	require.EqualValues(t, 16384, budgetForEffort("high"))
	require.EqualValues(t, 0, budgetForEffort("none"))
	require.EqualValues(t, 0, budgetForEffort(""))

	// no_raise：budget = min(表值, max-1024)，< 1024 不开启
	b, ok := thinkingBudgetFor("high", 16384)
	require.EqualValues(t, 15360, b)
	require.True(t, ok)
	b, ok = thinkingBudgetFor("high", 2048)
	require.EqualValues(t, 1024, b)
	require.True(t, ok)
	_, ok = thinkingBudgetFor("high", 2047)
	require.False(t, ok)
	_, ok = thinkingBudgetFor("high", 0)
	require.False(t, ok)

	// thinkingValue：不开启 → 空串，开启 → Anthropic 字段字面量
	require.Empty(t, thinkingValue("none", 32000))
	require.Equal(t, `{"type":"enabled","budget_tokens":16384}`, thinkingValue("high", 64000))

	// 合法上限：严格正整数，拒绝小数 / 指数 / 非数字
	for _, bad := range []string{`"4096"`, `4096.9`, `1e20`, `null`, `0`, `-5`, `true`} {
		_, ok := validMaxTokens(gjson.Parse(bad))
		require.False(t, ok, "%s 非法", bad)
	}
	n, ok := validMaxTokens(gjson.Parse("8000"))
	require.True(t, ok)
	require.EqualValues(t, 8000, n)
	_, ok = validMaxTokens(gjson.Parse("1000000001"))
	require.False(t, ok, "超过 1e9")

	// 非标准数字字面量：Raw 必须是纯十进制字面量（-?[0-9]+）；1_000 / 04096 /
	// +4096 都不是合法 JSON 数字字面量，一律非法。
	_, ok = validMaxTokens(gjson.Parse("1_000"))
	require.False(t, ok, "1_000 视为非法（→ 调用方按缺失处理，写出 4096）")
	for _, raw := range []string{"04096", "+4096", "-0"} {
		_, ok = validMaxTokens(gjson.Parse(raw))
		require.False(t, ok, "%s 视为非法", raw)
	}

	// 逐字段判定：非法继续下一字段；全非法 → 4096
	n, ok = maxTokensField(gjson.Parse("null"), gjson.Parse("8000"))
	require.True(t, ok)
	require.EqualValues(t, 8000, n)
	n, ok = maxTokensField(gjson.Parse("null"), gjson.Parse("4096.9"))
	require.False(t, ok)
	require.EqualValues(t, defaultMaxTokens, n)

	// effort 枚举：none/minimal 合法但不开启 thinking
	require.Equal(t, "none", chatEffort(gjson.Parse(`"none"`)))
	require.Equal(t, "", chatEffort(gjson.Parse(`"turbo"`)))
	require.Equal(t, "high", reasoningEffortFromBody(gjson.Parse(`{"effort":"high"}`)))
	require.Equal(t, "", reasoningEffortFromBody(gjson.Parse(`{"effort":"turbo"}`)))
	require.Equal(t, "", reasoningEffortFromBody(gjson.Parse(`null`)))
}

// TestReasoningBudgetAllocationFree 请求侧 effort→budget 必须 0 分配
// （spec §5 性能约定：信封解码只在 c3a1: 命中时发生，数值换算纯算术）。
func TestReasoningBudgetAllocationFree(t *testing.T) {
	require.Zero(t, testing.AllocsPerRun(100, func() { budgetForEffort("high") }))
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = thinkingBudgetFor("medium", 32000) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { _, _ = validMaxTokens(gjson.Parse("32000")) }))
}

// mustMarshal 把断言用的 map 还原成字节（「不应出现某个键」的全局扫描用）。
func mustMarshal(t *testing.T, m map[string]any) []byte {
	t.Helper()
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return out
}
