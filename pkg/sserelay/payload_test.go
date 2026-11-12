// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- 判据矩阵（spec-inband-error-truth r10 §3-1 / §5.2 的 T-* 组） ---

// plainChunk 普通业务 chunk（两针都不命中 ⇒ 零解析零分配）。
const plainChunk = `{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"},"index":0}]}`

func TestAsPayloadErrorNull(t *testing.T) { // T-null
	// "error": null 与缺字段都不是失败：针③ 要求 error 后紧跟 '{'。
	cases := []struct {
		name string
		ev   Event
	}{
		{"显式 null", Event{Data: []byte(`{"error":null}`)}},
		{"缺字段", Event{Data: []byte(plainChunk)}},
		{"error 为字符串", Event{Data: []byte(`{"error":"boom"}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if pe := AsPayloadError(c.ev); pe != nil {
				t.Fatalf("AsPayloadError(%q) = %#v，期望 nil", c.ev.Data, pe)
			}
		})
	}
}

func TestAsPayloadErrorNested(t *testing.T) { // T-nested
	// 嵌套 error 是**内容**（item.error），不是帧级失败：顶层确认是结构体解析的
	// 副产品——`item.error` 天然不命中。
	for _, data := range []string{
		`{"type":"response.output_item.done","item":{"type":"mcp_call","error":{"message":"tool boom"}}}`,
		`{"type":"response.output_item.done","item":{"type":"web_search_call","error":"rate limited"}}`,
	} {
		if pe := AsPayloadError(Event{Data: []byte(data)}); pe != nil {
			t.Fatalf("嵌套 error 不得判失败：%s ⇒ %#v", data, pe)
		}
	}
}

func TestAsPayloadErrorEmpty(t *testing.T) { // T-empty
	// 空对象不命中；只有 message（无 type）**必须**命中（OR 不是 AND——Azure 风格）。
	if pe := AsPayloadError(Event{Data: []byte(`{"error":{}}`)}); pe != nil {
		t.Fatalf(`{"error":{}} 期望 nil，得到 %#v`, pe)
	}
	pe := AsPayloadError(Event{Data: []byte(`{"error":{"message":"x"}}`)})
	if pe == nil {
		t.Fatal(`{"error":{"message":"x"}} 必须命中（内层 OR：无 type 也是失败）`)
	}
	if pe.Type != "" || pe.Message != "x" {
		t.Fatalf("取值错误：%#v", pe)
	}
}

func TestAsPayloadErrorDupKeys(t *testing.T) { // T-dup
	// 重复键按 encoding/json 后值语义：预筛可能由先出现的键触发，最终由后值裁决。
	if pe := AsPayloadError(Event{Data: []byte(`{"type":"error","type":"message"}`)}); pe != nil {
		t.Fatalf(`后值 message ⇒ nil，得到 %#v`, pe)
	}
	pe := AsPayloadError(Event{Data: []byte(`{"type":"message","type":"error"}`)})
	if pe == nil {
		t.Fatal(`后值 error ⇒ 必须命中`)
	}
}

func TestAsPayloadErrorEscapeIsDocumentary(t *testing.T) { // T-escape（文档性断言）
	// 正文里的转义形态 `\"error\":{` 不命中：针要求 error 前是裸引号。此断言无法
	// 被 §5.3 的任何变异打红——不计入覆盖账（spec §5.2 明示）。
	data := `{"type":"response.output_text.delta","delta":"curl -d \"{\\\"error\\\":{\\\"message\\\":\\\"x\\\"}}\""}`
	if pe := AsPayloadError(Event{Data: []byte(data)}); pe != nil {
		t.Fatalf("转义形态不得命中：%#v", pe)
	}
}

func TestAsPayloadErrorTopLevelType(t *testing.T) { // 顶层 type == "error"
	pe := AsPayloadError(Event{Data: []byte(`{"type":"error","code":"invalid_request_error","message":"x"}`)})
	if pe == nil {
		t.Fatal("顶层 type==error 必须命中")
	}
	if pe.Message != "x" {
		t.Fatalf("Message 取顶层 message：%#v", pe)
	}
	// 内层优先：内外都有时取 error.type/error.message。
	pe = AsPayloadError(Event{Data: []byte(`{"type":"error","message":"outer","error":{"type":"server_error","message":"inner"}}`)})
	if pe == nil || pe.Type != "server_error" || pe.Message != "inner" {
		t.Fatalf("内层优先：%#v", pe)
	}
}

func TestAsPayloadErrorAzureMessageOnly(t *testing.T) { // Azure 风格（无 type）
	pe := AsPayloadError(Event{Data: []byte(`{"error":{"message":"quota exceeded","code":"insufficient_quota"}}`)})
	if pe == nil || pe.Message != "quota exceeded" {
		t.Fatalf("Azure message-only 必须命中且取 message：%#v", pe)
	}
	if pe.Type != "" {
		t.Fatalf("不取 code ⇒ Type 必须为空：%#v", pe)
	}
}

func TestAsPayloadErrorEventField(t *testing.T) { // 针①：event: error（也要解析）
	// data 里既无 "type":"error" 也无 error 对象 ⇒ ②③ 全不命中，只有①命中。
	pe := AsPayloadError(Event{Event: []byte("error"), Data: []byte(`{"message":"failed"}`)})
	if pe == nil || pe.Message != "failed" {
		t.Fatalf("① 命中且必须解析出 message：%#v", pe)
	}
	// 折叠：大小写不敏感。
	if pe := AsPayloadError(Event{Event: []byte("ERROR"), Data: []byte(`{"message":"x"}`)}); pe == nil {
		t.Fatal("event 名折叠比较：ERROR 同样命中")
	}
	// ① 的空 payload 形态：命中但无字段 ⇒ Error() 用自述文案。
	pe = AsPayloadError(Event{Event: []byte("error")})
	if pe == nil || pe.Message != "" {
		t.Fatalf("event-only 也必须命中：%#v", pe)
	}
}

func TestAsPayloadErrorSkipsParseWithoutNeedle(t *testing.T) { // 解析失败不早退的反面
	// 单字段类型不匹配时 encoding/json 跳过该字段继续填充其余字段 ⇒ 仍可判 hit。
	pe := AsPayloadError(Event{Data: []byte(`{"type":123,"error":{"message":"boom"}}`)})
	if pe == nil || pe.Message != "boom" {
		t.Fatalf("类型不匹配必须不早退：%#v", pe)
	}
}

func TestPayloadErrorErrorAndRawJSON(t *testing.T) {
	pe := &PayloadError{Type: "server_error", Message: "Our servers are currently overloaded."}
	if got := pe.Error(); got != "Our servers are currently overloaded." {
		t.Fatalf("Error() 原样返回 Message，得到 %q", got)
	}
	pe2 := &PayloadError{}
	if got := pe2.Error(); got != "upstream error payload" {
		t.Fatalf("空 Message ⇒ 自述文案，得到 %q", got)
	}
	raw := pe.RawJSON()
	var probe struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("RawJSON 必须是合法 JSON：%q (%v)", raw, err)
	}
	if probe.Error.Type != "server_error" || probe.Error.Message != pe.Message {
		t.Fatalf("RawJSON 内容错误：%q", raw)
	}
	// 只含 type/message：上游 code/param 等字段不进合成体（spec §6-2）。
	if strings.Contains(raw, "code") || strings.Contains(raw, "param") {
		t.Fatalf("RawJSON 只合成 type/message：%q", raw)
	}
	// 空字段省略 ⇒ 空错误是 `{"error":{}}`（合法 JSON）。
	if got := (&PayloadError{}).RawJSON(); got != `{"error":{}}` {
		t.Fatalf("空 PayloadError RawJSON = %q", got)
	}
}

// --- 性能门禁（spec §3-5 / brief §5.4） ---

// TestAsPayloadErrorMissAllocs 未命中路径的硬门禁：两针 bytes.Contains、零解析、
// 零分配。字节串在循环外预构造（AllocsPerRun 只计被测函数内的分配）。
func TestAsPayloadErrorMissAllocs(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
	}{
		{"普通 chunk", Event{Data: []byte(plainChunk)}},
		{"error:null chunk", Event{Data: []byte(`{"id":"c1","error":null,"choices":[]}`)}},
		{"缺 error 字段的 completed", Event{Data: []byte(`{"type":"response.completed","response":{"id":"r"}}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := c.ev
			if got := testing.AllocsPerRun(200, func() {
				if pe := AsPayloadError(ev); pe != nil {
					t.Fatalf("期望 nil，得到 %#v", pe)
				}
			}); got != 0 {
				t.Fatalf("%s：AllocsPerRun = %v，门禁要求 0", c.name, got)
			}
		})
	}
}

// largeErrorPayload ≥64 KiB 的 error 对象（大帧账目：probe.Error 按 error 对象大小
// 复制一次，受 MaxLineBytes 约束）。
func largeErrorPayload() []byte {
	return []byte(`{"error":{"type":"server_error","message":"` + strings.Repeat("x", 64<<10) + `"}}`)
}

// TestAsPayloadErrorHitAllocs 命中路径的 allocs 目数（实测值写进报告；spec §3-5
// 只锁"命中才解析"与"不整帧复制"，不锁具体 alloc 数）。
func TestAsPayloadErrorHitAllocs(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
	}{
		{"T-nested（针③ 命中但判 nil）", Event{Data: []byte(`{"type":"response.output_item.done","item":{"type":"mcp_call","error":{"message":"tool boom"}}}`)}},
		{"T-empty（空对象判 nil）", Event{Data: []byte(`{"error":{}}`)}},
		{"顶层 type:error", Event{Data: []byte(`{"type":"error","message":"x"}`)}},
		{"Azure message-only", Event{Data: []byte(`{"error":{"message":"quota exceeded","code":"insufficient_quota"}}`)}},
		{"大 error 对象（≥64 KiB）", Event{Data: largeErrorPayload()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := c.ev
			allocs := testing.AllocsPerRun(100, func() { _ = AsPayloadError(ev) })
			t.Logf("%s: AllocsPerRun = %v（字节数 %d）", c.name, allocs, len(ev.Data))
		})
	}
}

// --- benchmarks（B/op 由 -benchmem 回填；命中路径与未命中路径分列） ---

func benchAsPayloadError(b *testing.B, data string) {
	b.Helper()
	ev := Event{Data: []byte(data)}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		_ = AsPayloadError(ev)
	}
}

func BenchmarkAsPayloadErrorPlain(b *testing.B) { benchAsPayloadError(b, plainChunk) }

func BenchmarkAsPayloadErrorNull(b *testing.B) {
	benchAsPayloadError(b, `{"id":"c1","error":null,"choices":[]}`)
}

func BenchmarkAsPayloadErrorNested(b *testing.B) {
	benchAsPayloadError(b, `{"type":"response.output_item.done","item":{"type":"mcp_call","error":{"message":"tool boom"}}}`)
}

func BenchmarkAsPayloadErrorEmpty(b *testing.B) { benchAsPayloadError(b, `{"error":{}}`) }

func BenchmarkAsPayloadErrorTopLevelType(b *testing.B) {
	benchAsPayloadError(b, `{"type":"error","message":"x"}`)
}

func BenchmarkAsPayloadErrorAzureMessageOnly(b *testing.B) {
	benchAsPayloadError(b, `{"error":{"message":"quota exceeded","code":"insufficient_quota"}}`)
}

func BenchmarkAsPayloadErrorLarge(b *testing.B) {
	benchAsPayloadError(b, string(largeErrorPayload()))
}
