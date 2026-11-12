// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestAsPayloadErrorMultiAnchor(t *testing.T) { // T-multi
	// 同一帧里"`error` 键但值非对象"可能排在合格键之前 ⇒ 首个锚点不合格**必须继续扫**
	// 下一个出现位置（r16 只判首个出现位置的实现会漏判）。
	cases := []struct {
		name string
		data string
	}{
		{"error:null 后跟 type:error", `{"error":null,"type":"error"}`},
		{"error 字符串后跟 error 对象", `{"error":"boom","error":{"message":"m"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if pe := AsPayloadError(Event{Data: []byte(c.data)}); pe == nil {
				t.Fatalf("多锚点必须命中（不能只看首个出现位置）：%s", c.data)
			}
		})
	}
	// 第二个反例同时钉住"取值由 encoding/json 后值语义裁决"。
	if pe := AsPayloadError(Event{Data: []byte(`{"error":"boom","error":{"message":"m"}}`)}); pe == nil || pe.Message != "m" {
		t.Fatalf(`重复 error 键 ⇒ 内层后值 message="m"，得到 %#v`, pe)
	}
}

func TestAsPayloadErrorNotType(t *testing.T) { // T-notype
	// 判形要求锚点紧随 `:{` 或前 7 B 恰为 `"type":`；`"status":"error"` 两者都不满足
	// ⇒ 锚点出现不等于命中（M12b 的钉子）。
	if pe := AsPayloadError(Event{Data: []byte(`{"status":"error"}`)}); pe != nil {
		t.Fatalf(`{"status":"error"} 期望 nil，得到 %#v`, pe)
	}
}

// --- 4 KiB 扫判闸（spec r16 §3-1 ② / §5.2 T-gate 组） ---
//
// 夹具长度与锚点位置**全部写死**：`>` 与 `>=`、"判到底"与"头尾窗口"的差别只有这些
// 数字能分辨，用变量或不精确的 pad 会静默失去分辨力。

const (
	gateSize     = 4096 // 闸值：len(data) > 4096 的 data-only 帧不判
	gateAnchorAt = 2049 // ⑥ 的锚点绝对下标（中部；头尾窗口实现会漏）
)

// buildFrame 拼 head + pad + tail 并断言**恰好** size 字节的合法 JSON。
func buildFrame(t testing.TB, size int, head, tail string) []byte {
	t.Helper()
	n := size - len(head) - len(tail)
	if n < 0 {
		t.Fatalf("夹具超长：head+tail = %d > %d", len(head)+len(tail), size)
	}
	data := []byte(head + strings.Repeat("x", n) + tail)
	require.Equal(t, size, len(data), "夹具长度必须写死")
	require.True(t, json.Valid(data), "夹具必须是合法 JSON：%q", string(data[:64]))
	return data
}

// gateFrameNested 内含**嵌套**（非顶层）`"error":{…}`：预筛合格、顶层解析不命中。
func gateFrameNested(t testing.TB, size int) []byte {
	t.Helper()
	return buildFrame(t, size, `{"item":{"error":{"message":"tool boom"}},"pad":"`, `"}`)
}

// gateFrameTopErrorHead 顶层 `{"error":{…}}` 在**开头**。
func gateFrameTopErrorHead(t testing.TB, size int) []byte {
	t.Helper()
	return buildFrame(t, size, `{"error":{"message":"boom"},"pad":"`, `"}`)
}

// gateFrameTopErrorTail 顶层 `{"error":{…}}` 在**尾部**。
func gateFrameTopErrorTail(t testing.TB, size int) []byte {
	t.Helper()
	return buildFrame(t, size, `{"pad":"`, `","error":{"message":"tail"}}`)
}

// gateFrameTopErrorMid 顶层 `"error":{"message":"mid"}` 的**锚点绝对下标恰为 2049**：
// 前 2049 B 一个 pad 键、之后同样是两千多字节的 pad 键（两侧都有内容 ⇒ 头尾窗口都抓不到）。
func gateFrameTopErrorMid(t testing.TB, size int) []byte {
	t.Helper()
	const (
		head      = `{"p":"`
		errPart   = `"error":{"message":"mid"},`
		tailOpen  = `"q":"`
		tailClose = `"}`
	)
	pad1 := gateAnchorAt - len(head) - len(`",`)
	pad2 := size - gateAnchorAt - len(errPart) - len(tailOpen) - len(tailClose)
	if pad1 < 0 || pad2 < 0 {
		t.Fatalf("夹具尺寸不足：size=%d pad1=%d pad2=%d", size, pad1, pad2)
	}
	data := []byte(head + strings.Repeat("a", pad1) + `",` + errPart + tailOpen + strings.Repeat("b", pad2) + tailClose)
	require.Equal(t, size, len(data), "夹具长度必须写死")
	require.Equal(t, gateAnchorAt, bytes.Index(data, []byte(`"error"`)),
		"锚点绝对下标必须写死（否则边界钉子静默失去分辨力）")
	require.True(t, json.Valid(data), "夹具必须是合法 JSON：%q", string(data[:64]))
	return data
}

// gateFrameEventBig 配 `event: error` 的 128 KiB 帧，data 开头是 `{"message":"boom"}`。
func gateFrameEventBig(t testing.TB, size int) []byte {
	t.Helper()
	return buildFrame(t, size, `{"message":"boom","pad":"`, `"}`)
}

func TestAsPayloadErrorGate(t *testing.T) { // T-gate ①–⑥
	t.Run("①16 KiB 内容帧+嵌套 error ⇒ nil 且 0 alloc", func(t *testing.T) {
		ev := Event{Data: gateFrameNested(t, 16<<10)}
		if pe := AsPayloadError(ev); pe != nil {
			t.Fatalf("16 KiB data-only 帧不判，得到 %#v", pe)
		}
		allocs := testing.AllocsPerRun(50, func() { _ = AsPayloadError(ev) })
		t.Logf("16 KiB 内容帧：AllocsPerRun = %v（字节数 %d）", allocs, len(ev.Data))
		if allocs != 0 {
			t.Fatalf("16 KiB 内容帧 AllocsPerRun = %v，门禁要求 0（长度闸必须先于预筛与解析）", allocs)
		}
	})

	t.Run("②16 KiB 帧+顶层 error 在尾部 ⇒ nil", func(t *testing.T) {
		ev := Event{Data: gateFrameTopErrorTail(t, 16<<10)}
		if pe := AsPayloadError(ev); pe != nil {
			t.Fatalf(">4 KiB 的 data-only 帧不判（申报取舍），得到 %#v", pe)
		}
	})

	t.Run("③128 KiB 帧+顶层 error 在开头 ⇒ nil", func(t *testing.T) {
		ev := Event{Data: gateFrameTopErrorHead(t, 128<<10)}
		if pe := AsPayloadError(ev); pe != nil {
			t.Fatalf(">4 KiB 的 data-only 帧不判，得到 %#v", pe)
		}
	})

	t.Run("④128 KiB+event:error ⇒ hit 但不解析文案", func(t *testing.T) {
		ev := Event{Event: []byte("error"), Data: gateFrameEventBig(t, 128<<10)}
		pe := AsPayloadError(ev)
		if pe == nil {
			t.Fatal("event: error 不受长度闸约束，必须仍判 hit")
		}
		if pe.Message != "" || pe.Type != "" {
			t.Fatalf(">4 KiB 只判 hit 不解析文案，得到 %#v", pe)
		}
	})

	t.Run("⑤恰好 4096 B+嵌套 error ⇒ nil（走完整解析）", func(t *testing.T) {
		ev := Event{Data: gateFrameNested(t, gateSize)}
		if pe := AsPayloadError(ev); pe != nil {
			t.Fatalf("4096 B 走进解析路径但非顶层 ⇒ nil，得到 %#v", pe)
		}
	})

	t.Run("⑥(a)恰好 4096 B+顶层 error 锚点在 2049 ⇒ hit", func(t *testing.T) {
		ev := Event{Data: gateFrameTopErrorMid(t, gateSize)}
		// 夹具的两个数字都在此断言（"≈" 不可断言）：闸值与锚点绝对下标。
		require.Equal(t, 4096, len(ev.Data))
		require.Equal(t, 2049, bytes.Index(ev.Data, []byte(`"error"`)))
		pe := AsPayloadError(ev)
		require.NotNil(t, pe, "4096 B 必须判到底（`>` 不是 `>=`，也不能只扫头尾）")
		require.Equal(t, "mid", pe.Message)
	})

	t.Run("⑥(b)4097 B 同形 ⇒ nil 且 0 alloc", func(t *testing.T) {
		ev := Event{Data: gateFrameTopErrorMid(t, gateSize+1)}
		if pe := AsPayloadError(ev); pe != nil {
			t.Fatalf("4097 B 过闸 ⇒ nil，得到 %#v", pe)
		}
		allocs := testing.AllocsPerRun(50, func() { _ = AsPayloadError(ev) })
		t.Logf("4097 B 顶层 error：AllocsPerRun = %v（字节数 %d）", allocs, len(ev.Data))
		if allocs != 0 {
			t.Fatalf("4097 B AllocsPerRun = %v，门禁要求 0", allocs)
		}
	})
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

// --- 性能门禁（spec §3-5 / brief §5） ---

// TestAsPayloadErrorMissAllocs 未命中路径的硬门禁：单锚点单遍、零解析、零分配。
// 字节串在循环外预构造（AllocsPerRun 只计被测函数内的分配）。
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
			if pe := AsPayloadError(ev); pe != nil {
				t.Fatalf("期望 nil，得到 %#v", pe)
			}
			allocs := testing.AllocsPerRun(200, func() { _ = AsPayloadError(ev) })
			t.Logf("%s：AllocsPerRun = %v（字节数 %d）", c.name, allocs, len(ev.Data))
			if allocs != 0 {
				t.Fatalf("%s：AllocsPerRun = %v，门禁要求 0", c.name, allocs)
			}
		})
	}
}

// largeErrorPayload ~3.5 KiB 的 error 对象（大帧账目：probe.Error 按 error 对象大小
// 复制一次，受 MaxLineBytes 约束）。**必须 ≤ 4096 B**：超过 4 KiB 就过扫判闸、不再
// 进入解析路径（那一段由 BenchmarkAsPayloadErrorGate 负责，只做 bench 不断言）。
func largeErrorPayload() []byte {
	// 43 B 前缀 + 3584 B message + 3 B 收尾 = 3630 B。
	return []byte(`{"error":{"type":"server_error","message":"` + strings.Repeat("x", 3584) + `"}}`)
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
		{"大 error 对象（~3.5 KiB，仍走解析）", Event{Data: largeErrorPayload()}},
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

// BenchmarkAsPayloadErrorLargeHit 3.5 KiB 命中路径：预筛合格 ⇒ 整帧解析（唯一
// "扫描 + 解析都付"的形态，spec §3-5 的最坏路径 ~2.0 µs）。
func BenchmarkAsPayloadErrorLargeHit(b *testing.B) {
	benchAsPayloadError(b, string(largeErrorPayload()))
}

// BenchmarkAsPayloadErrorContent16K / Content1M >4 KiB 的**内容帧**（帧内无 `error`
// 子串，即生产上的普通 chunk 形态）：只付长度闸，成本与帧长解耦。
func BenchmarkAsPayloadErrorContent16K(b *testing.B) {
	benchAsPayloadError(b, string(buildFrame(b, 16<<10, `{"pad":"`, `"}`)))
}

func BenchmarkAsPayloadErrorContent1M(b *testing.B) {
	benchAsPayloadError(b, string(buildFrame(b, 1<<20, `{"pad":"`, `"}`)))
}

// BenchmarkAsPayloadErrorNested4K 恰好 4096 B 的嵌套 error 帧：预筛合格但非顶层
// ⇒ 走完整解析路径（最坏路径的边界值，spec §3-5 的 ~2.0 µs 对应本条）。
func BenchmarkAsPayloadErrorNested4K(b *testing.B) {
	benchAsPayloadError(b, string(gateFrameNested(b, gateSize)))
}

// BenchmarkAsPayloadErrorGate 64 KiB 内容帧（>4 KiB 闸直接返回，不扫不解析）。
// 只做 bench、不断言——命中路径的 allocs 账目由 largeErrorPayload（~3.5 KiB）承担。
// 64 KiB 帧已不属判据定义域（spec §6-1 申报），这里量的是"闸生效后的零成本"。
func BenchmarkAsPayloadErrorGate(b *testing.B) {
	benchAsPayloadError(b, `{"error":{"type":"server_error","message":"`+strings.Repeat("x", 64<<10)+`"}}`)
}
