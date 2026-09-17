// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// denyKeyCount 清单长度的显式断言（防清单扩容这类越界改动静默通过：改
// relayDeny 必须同时显式改本常量，让那次扩容在 diff 里可见）。
const denyKeyCount = 29

// TestRelayHeadersAllowSide R1（允许面）：自定义头与协议协商头原样送达 +
// relayDeny 键形防线（写错大小写＝能编译但永不命中，本断言是唯一机器防线）。
func TestRelayHeadersAllowSide(t *testing.T) {
	in := http.Header{
		"X-Opencode-Session":         {"oc-1"},
		"X-Claude-Code-Session-Id":   {"cc-1"},
		"Session-Id":                 {"s-1"},
		"Anthropic-Version":          {"2023-06-01"},
		"Openai-Beta":                {"advanced-tool-use-2025-11-18"},
		"Originator":                 {"codex_cli_rs"},
		"User-Agent":                 {"curl/8.7.1"},
		"X-Request-Id":               {"req-1"},
		"X-Stainless-Retry-Count":    {"0"},
		"Anthropic-Dangerous-Skills": {"preview"},
	}
	out := RelayHeaders(in)

	for k, want := range in {
		got, ok := out[k]
		require.True(t, ok, "允许面头 %s 必须出现在 relay 产物", k)
		require.Equal(t, want, got, "允许面头 %s 值必须原样", k)
		require.Equal(t, http.CanonicalHeaderKey(k), k, "用例构造必须用规范形键")
	}

	for k := range relayDeny {
		require.Equal(t, http.CanonicalHeaderKey(k), k,
			"relayDeny 键 %q 必须是 http.CanonicalHeaderKey 规范形（普通 map 查表，非规范形＝静默永不命中）", k)
	}
	require.Len(t, relayDeny, denyKeyCount)
}

// TestRelayHeadersValueHygiene R2（值卫生）：含 CR/LF/NUL/DEL/任一 <0x20 且非
// TAB 的单个值被丢；a\tb / a b 保留；整键值全脏则该键不出现；其余键保留。
func TestRelayHeadersValueHygiene(t *testing.T) {
	in := http.Header{
		"X-CR":    {"a\rb"},
		"X-LF":    {"a\nb"},
		"X-NUL":   {"a\x00b"},
		"X-DEL":   {"a\x7fb"},
		"X-VT":    {"a\x0bb"},
		"X-BEL":   {"a\x07b"},
		"X-BS":    {"a\x08b"},
		"X-FF":    {"a\x0cb"},
		"X-ESC":   {"a\x1bb"},
		"X-Tab":   {"a\tb"},
		"X-Space": {"a b"},
		"X-Mixed": {"good", "bad\r\nvalue", "also good"},
	}
	out := RelayHeaders(in)

	for _, k := range []string{"X-CR", "X-LF", "X-NUL", "X-DEL", "X-VT", "X-BEL", "X-BS", "X-FF", "X-ESC"} {
		require.Empty(t, out.Get(k), "脏值 %s 必须被丢", k)
		_, ok := out[k]
		require.False(t, ok, "脏值键 %s 必须整键不出现（map 槽位级断言）", k)
	}
	require.Equal(t, []string{"a\tb"}, out["X-Tab"], "TAB 是合法头值字节，必须保留")
	require.Equal(t, []string{"a b"}, out["X-Space"], "空格是合法头值字节，必须保留")
	require.Equal(t, []string{"good", "also good"}, out["X-Mixed"], "只丢脏的那一个值，同键其余值保留")

	require.True(t, valueClean("a\tb"))
	require.True(t, valueClean("a b"))
	require.False(t, valueClean("a\x7f"))
}

// TestRelayHeadersDenySide R3a（剔除面 + 形状行为清单）：遍历 relayDeny 全部
// 29 键逐个断言被剔，并钉住计划 §1 原型实测的 8 项行为。
func TestRelayHeadersDenySide(t *testing.T) {
	// ① nil → 非 nil 空 map（rawPostCT 随后 req.Header.Set，nil map 会 panic）。
	nilOut := RelayHeaders(nil)
	require.NotNil(t, nilOut, "RelayHeaders(nil) 必须返回非 nil map")
	require.Len(t, nilOut, 0)
	nilOut.Set("Content-Type", "application/json") // 不得 panic

	in := http.Header{"X-Sentinel": {"keep"}}
	for k := range relayDeny {
		in[k] = []string{"client-value"}
	}
	out := RelayHeaders(in)
	require.Equal(t, []string{"keep"}, out["X-Sentinel"], "允许面哨兵键不得被误剔")

	for k := range relayDeny {
		require.Empty(t, out.Get(k), "deny 键 %s 不得达上游", k)
		_, ok := out[k]
		require.False(t, ok, "deny 键 %s 必须不存在于 relay 产物（map 槽位级断言）", k)
	}
	require.Len(t, out, 1)

	// ② 多值键在规范槽位下两值俱全。
	multi := RelayHeaders(http.Header{"openai-beta": {"a", "b"}})
	require.Equal(t, []string{"a", "b"}, multi["Openai-Beta"], "多值必须保真且落在规范槽位")
	_, ok := multi["openai-beta"]
	require.False(t, ok, "输出键必须是规范形")

	// ③ 非规范入站键提升为规范形。
	noncanon := RelayHeaders(http.Header{"x-noncanonical": {"1"}})
	require.Equal(t, []string{"1"}, noncanon["X-Noncanonical"], "非规范入站键必须提升为规范形输出键")
	_, ok = noncanon["x-noncanonical"]
	require.False(t, ok, "非规范槽位不得残留")

	// ④ 干净分支共享入站 slice（值不深拷），且出栈侧 Set 不回写 in。
	shared := http.Header{"X-Session-Id": {"s-1"}}
	relayed := RelayHeaders(shared)
	require.Same(t, &shared["X-Session-Id"][0], &relayed["X-Session-Id"][0],
		"干净值必须共享入站底层数组（值拷贝 0 是本设计的性能前提）")
	relayed.Set("X-Session-Id", "gateway-overwrite")
	require.Equal(t, []string{"s-1"}, shared["X-Session-Id"], "出栈侧 Set 不得回写入站 map")

	// ⑤ 脏值分支必须 v[:0:0]：cap>len 时 append 若复用入站底层数组，会写进
	// 入站槽位的下一个位置（in 的 len 不变⇒当下不可见，但数组已被污染）。
	backing := make([]string, 2, 4)
	backing[0] = "bad\r\nvalue"
	backing[1] = "good"
	dirtyIn := http.Header{"X-Mixed-Values": backing}
	dirtyOut := RelayHeaders(dirtyIn)
	require.Equal(t, []string{"good"}, dirtyOut["X-Mixed-Values"])
	require.Equal(t, "bad\r\nvalue", dirtyIn["X-Mixed-Values"][0],
		"脏值分支不得回写入站底层数组（必须 v[:0:0]，写 v[:0] 会就地污染入站第 0 槽）")
	require.NotSame(t, &dirtyIn["X-Mixed-Values"][0], &dirtyOut["X-Mixed-Values"][0],
		"脏值分支必须落在新分配的底层数组上")

	// ⑥ relay 产物上 Del 真正命中手工构造的非规范键（根治「假绿」）。
	rogue := RelayHeaders(http.Header{"OpenAI-Beta": {"rogue"}})
	require.Equal(t, []string{"rogue"}, rogue["Openai-Beta"], "手工构造的非规范键也必须落到规范槽位")
	rogue.Del("OpenAI-Beta")
	_, ok = rogue["Openai-Beta"]
	require.False(t, ok, "Del 必须真正删掉 relay 产物里的 beta 头（输出键形不统一时此删除会落空）")

	// ⑦ 空值 slice（仅手工构造可得）走全净分支原样保留，不特判。
	empty := RelayHeaders(http.Header{"X-Empty": {}})
	v, ok := empty["X-Empty"]
	require.True(t, ok, "空值 slice 的键必须保留")
	require.Len(t, v, 0)
}

// inboundFootprintCanonical 入站足迹 8 键（规范形，spec §10 增补裁决）：描述
// **入站**连接/对端的头，网关向上游一律剔除。
//
// 三处与本表常见写法不同，别“顺手修正”：`X-Real-Ip`（不是 `X-Real-IP`）、
// `Cf-Connecting-Ip`（不是 `CF-Connecting-IP`）、`True-Client-Ip`（不是
// `True-Client-IP`）—— RelayHeaders 用 `CanonicalHeaderKey(k)` 查表（普通 map，
// 不像 Header.Get 会规范化实参），清单里写成常见写法能编译但永不命中。
var inboundFootprintCanonical = []string{
	"X-Forwarded-For", "X-Real-Ip", "Cf-Connecting-Ip", "True-Client-Ip",
	"Forwarded", "X-Forwarded-Host", "X-Forwarded-Proto", "Via",
}

// TestRelayHeadersInboundFootprint R7（spec §10）：客户端 IP / 转发路径类头不得
// 达上游；两向夹住「非规范形写法」这个静默失效点（清单侧不得收录常见写法，
// 入站侧不论怎么写都要被规范化后命中）。
func TestRelayHeadersInboundFootprint(t *testing.T) {
	in := http.Header{"X-Opencode-Session": {"oc-1"}} // 哨兵：允许面不受影响
	for _, k := range inboundFootprintCanonical {
		require.Equal(t, k, http.CanonicalHeaderKey(k), "本表必须是规范形拼写")
		in[k] = []string{"203.0.113.9"}
	}
	out := RelayHeaders(in)
	for _, k := range inboundFootprintCanonical {
		require.Empty(t, out.Get(k), "入站足迹头 %s 不得达上游（spec §10）", k)
		_, ok := out[k]
		require.False(t, ok, "入站足迹头 %s 必须整键不出现（map 槽位级断言）", k)
	}
	require.Equal(t, []string{"oc-1"}, out["X-Opencode-Session"], "哨兵键不得被误剔")
	require.Len(t, out, 1)

	// 清单侧：常见（非规范形）写法**不得**被收录 —— 收录即静默失效。
	// 与 R1 的「清单每键都是规范形」遍历互为反向夹逼。
	for _, wrong := range []string{"X-Real-IP", "CF-Connecting-IP", "True-Client-IP"} {
		_, present := relayDeny[wrong]
		require.False(t, present, "清单不得收录非规范形写法 %q（查表用规范形，收录即永不命中）", wrong)
	}

	// 入站侧：拼写不敏感（规范化在查表之前完成）⇒ 常见写法的入站键同样被剔。
	rogue := http.Header{}
	for _, wrong := range []string{"X-Real-IP", "CF-Connecting-IP", "True-Client-IP"} {
		rogue[wrong] = []string{"203.0.113.9"}
	}
	require.Len(t, RelayHeaders(rogue), 0, "非规范形拼写的入站足迹头必须被规范化后命中清单")
}

// BenchmarkRelayHeaders 精确门：20 键 × 1 值 allocs/op ≤ 4；20 键 × 8 值的
// B/op ≤ 前者 ×1.2（守住「值不深拷」——深拷会多出整条值数组）。
func BenchmarkRelayHeaders(b *testing.B) {
	one := make(http.Header, 20)
	many := make(http.Header, 20)
	for i := 0; i < 20; i++ {
		k := http.CanonicalHeaderKey("X-Bench-Key-" + string(rune('a'+i)))
		one[k] = []string{"value"}
		many[k] = []string{"v0", "v1", "v2", "v3", "v4", "v5", "v6", "v7"}
	}
	b.Run("20keys_1value", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			RelayHeaders(one)
		}
	})
	b.Run("20keys_8values", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			RelayHeaders(many)
		}
	})
}
