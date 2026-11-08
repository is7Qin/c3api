// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
)

// TestCodexResponsesTransportTruthTable 是 6×4 真值表（spec §8.2 domain 行）：
// 输入 {"", "http", "ws", "WS", "websocket", " "} × {Valid, AllowedFor,
// Key, Name}。表本身就是契约——任何"顺手加个归一"的改动都会在这里显形。
//
// 关键两列：
//   - Key：计划/缓存键维度，"" 与 "http" **同键**（留空改成显式 http 是空改动，
//     不该换叶换计划）；非法值 → ""（默认面）。
//   - Name：对外观测（响应头），非法值 → "http"（绝不把脏值透给实验仪器）。
func TestCodexResponsesTransportTruthTable(t *testing.T) {
	cases := []struct {
		in      CodexResponsesTransport
		valid   bool
		allowed bool // codex 类型（responses 出站面可达）
		key     string
		name    string
		why     string
	}{
		{in: "", valid: true, allowed: true, key: "", name: "http", why: "零值 = 未配置 = 默认 http"},
		{in: "http", valid: true, allowed: true, key: "", name: "http", why: "显式 http 与默认同键、同名"},
		{in: "ws", valid: true, allowed: true, key: "ws", name: "ws", why: "唯一真正换传输的值"},
		{in: "WS", valid: false, allowed: false, key: "", name: "http", why: "不做大小写归一（回落会让两臂都落 http）"},
		{in: "websocket", valid: false, allowed: false, key: "", name: "http", why: "不做别名归一"},
		{in: " ", valid: false, allowed: false, key: "", name: "http", why: "空白不是未配置"},
	}
	for _, tc := range cases {
		// 子测试名用 %q：空串/空格子测试名在 `go test -run` 里不可读也不可区分
		// （"" 与 " " 都会退化成看不见的名字）。
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			require.Equal(t, tc.valid, tc.in.Valid(), "Valid(%q): %s", string(tc.in), tc.why)
			require.Equal(t, tc.allowed, tc.in.AllowedFor(credential.TypeCodexOAuth), "AllowedFor(codex-oauth, %q)", string(tc.in))
			require.Equal(t, tc.allowed, tc.in.AllowedFor(credential.TypeCodexPAT), "AllowedFor(codex-pat, %q)", string(tc.in))
			require.Equal(t, tc.key, tc.in.Key(), "Key(%q): %s", string(tc.in), tc.why)
			require.Equal(t, tc.name, tc.in.Name(), "Name(%q): %s", string(tc.in), tc.why)
		})
	}
}

// TestCodexResponsesTransportAllowedForNonCodex 非 codex 类型只接受 ""：
// "http" 与 "ws" 一样是永不生效的配置（都是 400 的对象），非法字面量也不放行。
func TestCodexResponsesTransportAllowedForNonCodex(t *testing.T) {
	nonCodex := []credential.Type{
		credential.TypeAPIKey, credential.TypeResponsesSpecial, credential.TypeCustom,
	}
	for _, ct := range nonCodex {
		require.True(t, CodexResponsesTransportDefault.AllowedFor(ct), "%s 接受空值（未配置）", ct)
		require.False(t, CodexResponsesTransportHTTP.AllowedFor(ct), "%s 不接受显式 http", ct)
		require.False(t, CodexResponsesTransportWS.AllowedFor(ct), "%s 不接受 ws", ct)
		require.False(t, CodexResponsesTransport("WS").AllowedFor(ct), "%s 不接受非法字面量", ct)
	}
	// 空类型（未初始化）同样不放行任何非空值
	require.True(t, CodexResponsesTransportDefault.AllowedFor(credential.Type("")))
	require.False(t, CodexResponsesTransportWS.AllowedFor(credential.Type("")))
}

// TestCodexResponsesTransportWSPredicate 钉死 "Name()==\"ws\"" ⟺ "值是 WS"
// 这一条**唯一**的等价（spec §8.2：这条断言本身就是"不可能静默分歧"的机器证明）。
//
// 跨层的那一半在 sdkbridge：`Name()=="ws" ⟺ codexTransport(t)==codexsdk.TransportWS`
// （codexTransport 属桥层、本批次不在范围内；两处断言共用同一个谓词 t==WS，
// 因此任一侧把 ws 折成 http 都会让对应断言变红）。这里先钉住 domain 半边：
// 除 WS 之外**任何**输入都不得让 Name 报 ws（含脏值 "WS"/"websocket"/" "），
// 否则"报 ws 且 200"就不再等价于"升级成功"。
func TestCodexResponsesTransportWSPredicate(t *testing.T) {
	for _, in := range []CodexResponsesTransport{"", "http", "ws", "WS", "websocket", " "} {
		require.Equal(t, in == CodexResponsesTransportWS, in.Name() == string(CodexResponsesTransportWS),
			"Name()==ws 必须恰好等价于值是 ws: %q", string(in))
	}
}

// TestCodexResponsesTransportKeyNameAgree 两个归一方法的唯一分歧点必须是
// "http"/""（一个换键、一个不换键），其余输入完全一致——三份归一 = 三份静默
// 分歧，这里把"只允许这一处分歧"钉住。与 sdkbridge 的 codexTransport 的等价
// 断言（Name()=="ws" ⟺ TransportWS）分处两包，互为交叉证明。
func TestCodexResponsesTransportKeyNameAgree(t *testing.T) {
	for _, in := range []CodexResponsesTransport{"", "http", "ws", "WS", "websocket", " "} {
		if in.Name() == string(CodexResponsesTransportWS) {
			require.Equal(t, string(CodexResponsesTransportWS), in.Key(), "Name=ws 必须 Key=ws: %q", string(in))
		} else {
			require.Empty(t, in.Key(), "Name=http 时 Key 必须是默认面（空串）: %q", string(in))
		}
	}
}
