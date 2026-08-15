// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// 提取单测用真实上游 JSON 构造（评审 I-1：不得用结构体 marshal 自证——
// RawJSON 路径必须经过 SDK UnmarshalJSON 才能得到原始字节）。

// —— chat 流式 usage 帧（顶层 usage.*；cached_tokens 嵌套于
// prompt_tokens_details，与 SDK CompletionUsage 结构体一致——评审 I-1） ——

func TestChatStreamUsage(t *testing.T) {
	frame := []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":5},"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2}}}`)
	u, ok := chatStreamUsage(frame)
	require.True(t, ok, "usage 存在 → ok")
	require.Equal(t, int64(10), u.it)
	require.Equal(t, int64(20), u.ot)
	require.Equal(t, int64(30), u.tt)
	require.Equal(t, int64(5), u.cr, "prompt_tokens_details.cached_tokens 直读")
	require.Equal(t, int64(6), u.cc, "ephemeral 5m+1h 聚合")

	// 空对象 usage 仍存在（字段缺失 → 0，不阻塞采集）
	u, ok = chatStreamUsage([]byte(`{"usage":{}}`))
	require.True(t, ok)
	require.Zero(t, u.cr)
	require.Zero(t, u.cc)
	// 缺失 / 显式 null → ok=false（调用方不更新——null 帧不得清零）
	_, ok = chatStreamUsage([]byte(`{"id":"x"}`))
	require.False(t, ok)
	_, ok = chatStreamUsage([]byte(`{"usage":null}`))
	require.False(t, ok, "显式 null → 不存在")
	u, ok = chatStreamUsage([]byte(`{"usage":{"prompt_tokens_details":{"cached_tokens":null}}}`))
	require.True(t, ok)
	require.Zero(t, u.cr, "显式 null 与缺失等价")
	require.Zero(t, u.cc)
}

// —— chat 非流式（SDK UnmarshalJSON → PromptTokensDetails 直读 + RawJSON gjson） ——

func TestChatUsageFromResponse(t *testing.T) {
	raw := `{"id":"x","object":"chat.completion","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":5},"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2}}}`
	var resp openai.ChatCompletion
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	require.True(t, resp.JSON.Usage.Valid())
	pt, ct, tt, cr, cc := chatUsageFromResponse(resp.Usage)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(5), cr, "SDK PromptTokensDetails.CachedTokens 直读")
	require.Equal(t, int64(6), cc, "RawJSON 保留上游原始字节 → ephemeral 聚合")

	// 无 cache 字段 → 0
	plain := `{"id":"x","object":"chat.completion","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	var p openai.ChatCompletion
	require.NoError(t, json.Unmarshal([]byte(plain), &p))
	_, _, _, cr, cc = chatUsageFromResponse(p.Usage)
	require.Zero(t, cr)
	require.Zero(t, cc)
}

// —— Anthropic 流式（message.usage.* 前缀） ——

func TestAnthropicStreamUsage(t *testing.T) {
	start := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}}`)
	u, ok := anthropicStartUsage(start)
	require.True(t, ok)
	require.Equal(t, int64(10), u.it)
	require.Equal(t, int64(7), u.cr, "message.usage.cache_read_input_tokens")
	require.Equal(t, int64(3), u.cc, "message.usage.cache_creation_input_tokens")
	require.Zero(t, u.ot, "ot 无对应字段恒 0（调用点 tt 下游自算）")

	delta := []byte(`{"type":"message_delta","usage":{"output_tokens":20}}`)
	require.Equal(t, int64(20), anthropicDeltaOutput(delta))

	// 缺 cache 字段 → 0
	u, ok = anthropicStartUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`))
	require.True(t, ok)
	require.Zero(t, u.cr)
	require.Zero(t, u.cc)
	// 无 message.usage / 显式 null → ok=false
	_, ok = anthropicStartUsage([]byte(`{"type":"message_start"}`))
	require.False(t, ok)
	_, ok = anthropicStartUsage([]byte(`{"type":"message_start","message":{"usage":null}}`))
	require.False(t, ok, "显式 null → 不存在")
}

// —— Anthropic 非流式（SDK 结构体直读） ——

func TestAnthropicUsageFromResponse(t *testing.T) {
	raw := `{"id":"x","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}`
	var resp anthropic.Message
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	require.True(t, resp.JSON.Usage.Valid())
	pt, ct, tt, cr, cc := anthropicUsageFromResponse(resp.Usage)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(7), cr)
	require.Equal(t, int64(3), cc)
}

// —— Responses 流式（response.usage.* 前缀） ——

func TestResponsesStreamUsage(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"r","model":"m","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}},"output":[]}}`)
	u, ok := responsesCompletedUsage(completed)
	require.True(t, ok)
	require.Equal(t, int64(10), u.it)
	require.Equal(t, int64(20), u.ot)
	require.Equal(t, int64(30), u.tt)
	require.Equal(t, int64(5), u.cr, "response.usage.input_tokens_details.cached_tokens")
	require.Zero(t, u.cc, "Responses 无 cache_creation 对象，恒 0 预期（M4）")

	// 无 response / 无 usage / 显式 null → ok=false
	_, ok = responsesCompletedUsage([]byte(`{"type":"response.completed"}`))
	require.False(t, ok)
	_, ok = responsesCompletedUsage([]byte(`{"type":"response.completed","response":{"id":"r"}}`))
	require.False(t, ok, "completed 帧但 usage 缺失（error 终态形状）")
	_, ok = responsesCompletedUsage([]byte(`{"type":"response.completed","response":{"usage":null}}`))
	require.False(t, ok, "显式 null → 不存在")
}

// —— codex resp 顶层 usage（P1-1——T6：SDK 路径 usage 形状为顶层；fixture 对齐
// codex-sdk responses_test.go respUsage 形状 + cache 明细） ——

func TestResponsesTopLevelUsage(t *testing.T) {
	// 流式 completed 帧形态（顶层 usage——response 对象内无 usage）
	completed := []byte(`{"type":"response.completed","response":{"id":"r","object":"response","status":"completed"},"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}}`)
	u, ok := responsesTopLevelUsage(completed)
	require.True(t, ok)
	require.Equal(t, int64(10), u.it)
	require.Equal(t, int64(20), u.ot)
	require.Equal(t, int64(30), u.tt)
	require.Equal(t, int64(2), u.cr, "顶层 usage.input_tokens_details.cached_tokens")
	require.Equal(t, int64(4), u.cc, "顶层 cache_creation ephemeral 5m+1h 聚合")

	// 合成体形态（无 type 字段——usage 同样顶层）
	composite := []byte(`{"id":"resp_001","object":"response","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2}}}`)
	u, ok = responsesTopLevelUsage(composite)
	require.True(t, ok)
	require.Equal(t, int64(10), u.it)
	require.Equal(t, int64(20), u.ot)
	require.Equal(t, int64(30), u.tt)
	require.Equal(t, int64(2), u.cr)
	require.Zero(t, u.cc, "无 cache_creation → 0")

	// 缺失 → ok=false；空对象仍存在；显式 null 字段 → 0（不阻塞采集）
	_, ok = responsesTopLevelUsage([]byte(`{"id":"x"}`))
	require.False(t, ok, "usage 缺失 → ok=false")
	u, ok = responsesTopLevelUsage([]byte(`{"usage":{}}`))
	require.True(t, ok)
	require.Zero(t, u.cr)
	require.Zero(t, u.cc)
	u, ok = responsesTopLevelUsage([]byte(`{"usage":{"input_tokens_details":{"cached_tokens":null}}}`))
	require.True(t, ok)
	require.Zero(t, u.cr, "显式 null 与缺失等价")
}

func TestSniffResponsesCompletedTop(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"r","object":"response","status":"completed"},"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}}`)
	u, ok := sniffResponsesCompletedTop(completed)
	require.True(t, ok, "completed 帧命中")
	require.Equal(t, int64(10), u.it)
	require.Equal(t, int64(20), u.ot)
	require.Equal(t, int64(30), u.tt)
	require.Equal(t, int64(2), u.cr)
	require.Equal(t, int64(4), u.cc)

	// 精确判定：正文含 "type":"response.completed" 子串的**非 completed 帧**
	//（消息文本）不命中——WS 路径 bytes.Contains 预筛会误命中（P1-1 冻结防线）
	messageFrame := []byte(`{"type":"message","content":[{"type":"output_text","text":"say {\"type\":\"response.completed\"} please"}]}`)
	_, ok = sniffResponsesCompletedTop(messageFrame)
	require.False(t, ok, "正文含子串的非 completed 帧不得命中（type 精确判定）")

	// 非 JSON 行 / 未知类型 → 不命中
	_, ok = sniffResponsesCompletedTop([]byte(`this is not json`))
	require.False(t, ok)
	_, ok = sniffResponsesCompletedTop([]byte(`{"type":"output_item.done","item":{"id":"m"}}`))
	require.False(t, ok)

	// completed 但 usage 缺失 → 命中 + 全 0（缺失 = 0，不阻塞采集）
	u, ok = sniffResponsesCompletedTop([]byte(`{"type":"response.completed","response":{"id":"r"}}`))
	require.True(t, ok)
	require.Zero(t, u.it)
	require.Zero(t, u.cc)
}

// —— A-1 双实现对照（改造前 gjson 版本保留为测试内对照——评审 I-1 语义等价
// 验证） ——

// 对照实现 = 改造前生产代码原样（gjson 多遍扫描）；ok = usage 存在性判定
// （与原调用方 Type == gjson.JSON 前置检查同构：缺失/显式 null → Type Null →
// false）。对照仅存在于测试（生产热路径已全量迁移 scanKeyValue 族）。

func chatStreamUsageRef(data []byte) (usageTuple, bool) {
	t := usageTuple{
		it: gjson.GetBytes(data, "usage.prompt_tokens").Int(),
		ot: gjson.GetBytes(data, "usage.completion_tokens").Int(),
		tt: gjson.GetBytes(data, "usage.total_tokens").Int(),
		cr: gjson.GetBytes(data, "usage.prompt_tokens_details.cached_tokens").Int(),
		cc: gjson.GetBytes(data, "usage.cache_creation.ephemeral_5m_input_tokens").Int() +
			gjson.GetBytes(data, "usage.cache_creation.ephemeral_1h_input_tokens").Int(),
	}
	return t, gjson.GetBytes(data, "usage").Type == gjson.JSON
}

func anthropicStartUsageRef(data []byte) (usageTuple, bool) {
	t := usageTuple{
		it: gjson.GetBytes(data, "message.usage.input_tokens").Int(),
		cr: gjson.GetBytes(data, "message.usage.cache_read_input_tokens").Int(),
		cc: gjson.GetBytes(data, "message.usage.cache_creation_input_tokens").Int(),
	}
	return t, gjson.GetBytes(data, "message.usage").Type == gjson.JSON
}

func anthropicDeltaOutputRef(data []byte) int64 {
	return gjson.GetBytes(data, "usage.output_tokens").Int()
}

func responsesCompletedUsageRef(data []byte) (usageTuple, bool) {
	t := usageTuple{
		it: gjson.GetBytes(data, "response.usage.input_tokens").Int(),
		ot: gjson.GetBytes(data, "response.usage.output_tokens").Int(),
		tt: gjson.GetBytes(data, "response.usage.total_tokens").Int(),
		cr: gjson.GetBytes(data, "response.usage.input_tokens_details.cached_tokens").Int(),
		cc: gjson.GetBytes(data, "response.usage.cache_creation.ephemeral_5m_input_tokens").Int() +
			gjson.GetBytes(data, "response.usage.cache_creation.ephemeral_1h_input_tokens").Int(),
	}
	return t, gjson.GetBytes(data, "response.usage").Type == gjson.JSON
}

func responsesTopLevelUsageRef(data []byte) (usageTuple, bool) {
	t := usageTuple{
		it: gjson.GetBytes(data, "usage.input_tokens").Int(),
		ot: gjson.GetBytes(data, "usage.output_tokens").Int(),
		tt: gjson.GetBytes(data, "usage.total_tokens").Int(),
		cr: gjson.GetBytes(data, "usage.input_tokens_details.cached_tokens").Int(),
		cc: gjson.GetBytes(data, "usage.cache_creation.ephemeral_5m_input_tokens").Int() +
			gjson.GetBytes(data, "usage.cache_creation.ephemeral_1h_input_tokens").Int(),
	}
	return t, gjson.GetBytes(data, "usage").Type == gjson.JSON
}

func sniffResponsesCompletedTopRef(data []byte) (usageTuple, bool) {
	if gjson.GetBytes(data, "type").String() != "response.completed" {
		return usageTuple{}, false
	}
	t, _ := responsesTopLevelUsageRef(data)
	return t, true
}

// TestUsageExtractEquivalence A-1 语义等价双实现对照：真实上游形态用例 + 病态
// 用例（显式 null / 缺失字段 / 字符串数字 / 嵌套同名键 / 键名前缀干扰 /
// cache_creation 单桶缺失）全跑新实现与 gjson 对照（真实 JSON 构造，非结构体
// marshal 自证——评审 I-1），输出元组与 ok 全等。已知差异方向（float / 指数 /
// 超 int64 / 字符串 \uXXXX 数字 / bool 字面——"保守 0"）不入本表，单独断言
// （TestScanIntValuePathologicalDivergence）。
func TestUsageExtractEquivalence(t *testing.T) {
	chatFrames := []string{
		`{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":5},"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2}}}`,
		`{"id":"x","choices":[],"usage":{}}`, // 空对象仍存在
		`{"id":"x","choices":[]}`,            // usage 缺失
		`{"id":"x","choices":[],"usage":null}`, // 显式 null
		`{"id":"x","choices":[],"usage":[]}`, // usage 为数组（gjson JSON Type 含数组）
		`{"id":"x","choices":[],"usage":{"prompt_tokens":null,"completion_tokens":null,"total_tokens":null,"prompt_tokens_details":{"cached_tokens":null}}}`,
		`{"id":"x","choices":[],"usage":{"prompt_tokens":"10","completion_tokens":"20","total_tokens":"30"}}`, // 字符串数字
		`{"id":"x","choices":[],"usage":{"prompt_tokens":-5,"completion_tokens":0,"total_tokens":-5}}`,       // 负数
		`{"id":"x","choices":[],"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":1,"total_tokens":9223372036854775807}}`, // int64 边界
		`{"id":"x","choices":[],"usage":{"prompt_tokens_extra":5,"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`, // 键名前缀干扰
		`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"prompt_tokens_details":{"prompt_tokens":99,"cached_tokens":5},"completion_tokens":2,"total_tokens":9}}`, // 嵌套同名键（子区间内不误定位）
		`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"cache_creation":{"ephemeral_1h_input_tokens":2}}}`, // cache_creation 单桶缺失
		`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"cache_creation":{}}}`, // cache_creation 空对象
	}
	for _, f := range chatFrames {
		got, ok := chatStreamUsage([]byte(f))
		want, wantOK := chatStreamUsageRef([]byte(f))
		require.Equalf(t, want, got, "chat 帧: %s", f)
		require.Equalf(t, wantOK, ok, "chat ok 帧: %s", f)
	}

	anthropicFrames := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}}`,
		`{"type":"message_start","message":{}}`,
		`{"type":"message_start"}`,
		`{"type":"message_start","message":{"usage":null}}`,
		`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`{"type":"message_start","message":{"usage":{"cache_read_input_tokens":"7","cache_creation_input_tokens":3}}}`,
	}
	for _, f := range anthropicFrames {
		got, ok := anthropicStartUsage([]byte(f))
		want, wantOK := anthropicStartUsageRef([]byte(f))
		require.Equalf(t, want, got, "anthropic start 帧: %s", f)
		require.Equalf(t, wantOK, ok, "anthropic start ok 帧: %s", f)
	}
	deltaFrames := []string{
		`{"type":"message_delta","usage":{"output_tokens":20}}`,
		`{"type":"message_delta","usage":{}}`,
		`{"type":"message_delta"}`,
		`{"type":"message_delta","usage":null}`,
	}
	for _, f := range deltaFrames {
		require.Equalf(t, anthropicDeltaOutputRef([]byte(f)), anthropicDeltaOutput([]byte(f)), "anthropic delta 帧: %s", f)
	}

	responsesFrames := []string{
		`{"type":"response.completed","response":{"id":"r","model":"m","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}},"output":[]}}`,
		`{"type":"response.completed","response":{"id":"r","usage":{}}}`,
		`{"type":"response.completed","response":{"id":"r"}}`, // error 终态形状（无 usage）
		`{"type":"response.completed","response":{"id":"r","usage":null}}`,
		`{"type":"response.completed"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5},"cache_creation":{"ephemeral_5m_input_tokens":1}}}}`,
	}
	for _, f := range responsesFrames {
		got, ok := responsesCompletedUsage([]byte(f))
		want, wantOK := responsesCompletedUsageRef([]byte(f))
		require.Equalf(t, want, got, "responses completed 帧: %s", f)
		require.Equalf(t, wantOK, ok, "responses completed ok 帧: %s", f)
	}

	topFrames := []string{
		`{"type":"response.completed","response":{"id":"r","object":"response","status":"completed"},"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}}`,
		`{"id":"resp_001","object":"response","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2}}}`,
		`{"type":"response.completed","response":{"id":"r"}}`,
		`{"type":"response.completed","response":{"id":"r"},"usage":null}`,
		`{"id":"x"}`,
		`{"type":"message","content":[{"type":"output_text","text":"say {\"type\":\"response.completed\"} please"}],"usage":{"input_tokens":10}}`, // 正文含 type 子串的非 completed 帧（sniff 不误命中 → ok=false）
		`this is not json`,
	}
	for _, f := range topFrames {
		got, ok := responsesTopLevelUsage([]byte(f))
		want, wantOK := responsesTopLevelUsageRef([]byte(f))
		require.Equalf(t, want, got, "顶层 usage 帧: %s", f)
		require.Equalf(t, wantOK, ok, "顶层 usage ok 帧: %s", f)

		// sniff 同帧对照（type 精确判定 + 顶层 usage——内联扫描与二次扫描全等）
		sgot, sok := sniffResponsesCompletedTop([]byte(f))
		swant, swantOK := sniffResponsesCompletedTopRef([]byte(f))
		require.Equalf(t, swant, sgot, "sniff 帧: %s", f)
		require.Equalf(t, swantOK, sok, "sniff ok 帧: %s", f)
	}
}

// TestScanIntValuePathologicalDivergence A-1 病态输入差异（差异方向已注释标注
// 于 scanIntValue——本实现一律"保守 0"，不做双实现相等断言）：
//   - float 字面：gjson safeInt 截断取整（12.5 → 12）
//   - 指数 1e3：gjson parseInt 取前导数字（1）
//   - 超 int64：gjson parseInt 无溢出检查回绕
//   - 字符串数字含 \uXXXX 转义：gjson 值 unescape 后解析（"123" → 123）
//   - bool 字面：gjson true → 1 / false → 0
//
// 相等面边界同时钉住（与 gjson 一致：负数 / int64 极值 / 纯字符串数字）。
func TestScanIntValuePathologicalDivergence(t *testing.T) {
	require.Zero(t, scanIntValue([]byte(`12.5`)), "float：gjson 截断取 12")
	require.Zero(t, scanIntValue([]byte(`1e3`)), "指数：gjson 取前导数字 1")
	require.Zero(t, scanIntValue([]byte(`18446744073709551616`)), "超 int64：gjson 回绕")
	require.Zero(t, scanIntValue([]byte("\"1\\u0032\\u0033\"")), "字符串 \\uXXXX 数字：gjson 解码后解析 123")
	require.Zero(t, scanIntValue([]byte(`true`)), "bool：gjson true → 1")
	require.Zero(t, scanIntValue([]byte(`null`)), "null → 0（两实现同）")
	require.Zero(t, scanIntValue(nil), "空区间 → 0")

	require.Equal(t, int64(math.MaxInt64), scanIntValue([]byte(`9223372036854775807`)), "int64 上界")
	require.Equal(t, int64(math.MinInt64), scanIntValue([]byte(`-9223372036854775808`)), "int64 下界")
	require.Equal(t, int64(123), scanIntValue([]byte(`"123"`)), "字符串数字")
	require.Equal(t, int64(-5), scanIntValue([]byte(`-5`)), "负数")
}

// TestUsageExtractZeroAlloc A-1 零分配断言（对齐 respImageCount 先例——gjson
// GetBytes 物化 Raw 字符串分配；scanKeyValue 族纯切片零分配）：A 项全部函数
// 命中/未命中路径均钉 AllocsPerRun == 0。
func TestUsageExtractZeroAlloc(t *testing.T) {
	chat := []byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":5},"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":2}}}`)
	anthropic := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":7,"cache_creation_input_tokens":3}}}`)
	delta := []byte(`{"type":"message_delta","usage":{"output_tokens":20}}`)
	completed := []byte(`{"type":"response.completed","response":{"id":"r","model":"m","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}},"output":[]}}`)
	top := []byte(`{"type":"response.completed","response":{"id":"r"},"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":3}}}`)
	miss := []byte(`{"id":"x"}`)

	require.Zero(t, testing.AllocsPerRun(100, func() { chatStreamUsage(chat) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { chatStreamUsage(miss) }), "usage 缺失路径同样零分配")
	require.Zero(t, testing.AllocsPerRun(100, func() { anthropicStartUsage(anthropic) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { anthropicDeltaOutput(delta) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { responsesCompletedUsage(completed) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { responsesTopLevelUsage(top) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { sniffResponsesCompletedTop(top) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { sniffResponsesCompletedTop([]byte(`{"type":"message"}`)) }), "type 未命中路径同样零分配")
	// scanIntValue 双分支（数字字面 / 字符串数字——string([]byte) 走编译器
	// 免分配优化路径，实测 0）
	require.Zero(t, testing.AllocsPerRun(100, func() { scanIntValue([]byte(`12345`)) }))
	require.Zero(t, testing.AllocsPerRun(100, func() { scanIntValue([]byte(`"12345"`)) }))
}

// —— Responses 非流式（直读 + RawJSON） ——

func TestResponsesUsageFromResponse(t *testing.T) {
	raw := `{"id":"r","object":"response","model":"m","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}},"output":[]}`
	var resp responses.Response
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	require.True(t, resp.JSON.Usage.Valid())
	pt, ct, tt, cr, cc := responsesUsageFromResponse(resp.Usage)
	require.Equal(t, int64(10), pt)
	require.Equal(t, int64(20), ct)
	require.Equal(t, int64(30), tt)
	require.Equal(t, int64(5), cr, "SDK InputTokensDetails.CachedTokens 直读")
	require.Zero(t, cc, "恒 0 预期（M4）")
}

// —— buildLog 接线（评审 I-2）：cr/cc → UsageLog.CacheRead/CreationTokens ——

func TestBuildLogWiresCacheTokens(t *testing.T) {
	l := (&Proxy{}).buildLog("req1", 1, 2, "m", "m", domain.FormatOpenAIChat, 200, domain.ErrNone,
		usageTuple{it: 10, ot: 20, tt: 30, cr: 4, cc: 6}, time.Now())
	require.Equal(t, int64(4), l.CacheReadTokens)
	require.Equal(t, int64(6), l.CacheCreationTokens)

	nilU := (&Proxy{}).buildLog("req2", 1, 2, "m", "m", domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{}, time.Now())
	require.Zero(t, nilU.CacheReadTokens, "零值元组 → 0（不 panic）")
	require.Zero(t, nilU.CacheCreationTokens)
}

// —— mappedFor 判定（评审 I-1）：映射/无映射/used 空 ——

func TestMappedFor(t *testing.T) {
	require.Equal(t, "gpt-4o-upstream", mappedFor("gpt-4o", "gpt-4o-upstream"), "有映射 → 映射后模型")
	require.Equal(t, "", mappedFor("gpt-4o", "gpt-4o"), "无映射（used == 请求模型）→ 空")
	require.Equal(t, "", mappedFor("gpt-4o", ""), "used 空（Select 失败未使用任何账号）→ 空")
	require.Equal(t, "", mappedFor("", ""), "请求模型缺失（401）→ 空")
}

// —— buildLog 模型语义（评审 I-1）：Model=客户端请求模型、MappedModel=映射后模型 ——

func TestBuildLogModelSemantics(t *testing.T) {
	mapped := (&Proxy{}).buildLog("r1", 1, 2, "gpt-4o", "gpt-4o-upstream", domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{}, time.Now())
	require.Equal(t, "gpt-4o", mapped.Model, "Model = 客户端请求模型")
	require.Equal(t, "gpt-4o-upstream", mapped.MappedModel, "MappedModel = 映射后实际模型")

	plain := (&Proxy{}).buildLog("r2", 1, 2, "gpt-4o", "gpt-4o", domain.FormatOpenAIChat, 200, domain.ErrNone, usageTuple{}, time.Now())
	require.Equal(t, "gpt-4o", plain.Model)
	require.Equal(t, "", plain.MappedModel, "无映射 → MappedModel 空")
}
