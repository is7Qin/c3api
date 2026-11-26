// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpx

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyValueClean 是 P1 之前 pkg/aiclient 私有 valueClean 的判据副本（已删除的
// 历史实现，逐字节照抄）。它是「新判据与旧行为逐字节等价」的**冻结口径**：本文件的
// 表驱动等价测试拿它当 oracle，作用是防止 ValidHeaderValue 在搬家时被顺手改掉语义
// （spec request_patch §2.6）。不要在这里「优化」这个函数。
func legacyValueClean(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x7f || (c < 0x20 && c != '\t') {
			return false
		}
	}
	return true
}

// TestValidHeaderValueMatchesLegacyValueClean 等价门（spec §2.6 / 评审 M-e）：
// 对整个字节域逐项比较新旧判据。
//   - 0x00–0xff 每个单字节各测一次；
//   - 全部字节两两组合（65536 组）中的「首字节 + 尾字节」；
//   - 边界组合（含 0x09 TAB、0x20 SP、0x7e '~'、0x7f DEL、UTF-8 高位 0xc3 0xa9）。
func TestValidHeaderValueMatchesLegacyValueClean(t *testing.T) {
	for b := 0; b <= 0xff; b++ {
		s := string([]byte{byte(b)})
		require.Equal(t, legacyValueClean(s), ValidHeaderValue(s), "single byte %#02x", b)
	}
	for a := 0; a <= 0xff; a++ {
		for b := 0; b <= 0xff; b++ {
			s := string([]byte{byte(a), byte(b)})
			require.Equal(t, legacyValueClean(s), ValidHeaderValue(s), "bytes %#02x %#02x", a, b)
		}
	}
	// brief §2 的边界字节集（0x00/0x09/0x0a/0x1f/0x20/0x7e/0x7f/0xc3 0xa9）。
	boundary := []string{
		"a\x00b", "a\x09b", "a\x0ab", "a\x1fb", "a\x20b", "a\x7eb", "a\x7fb", "a\xc3\xa9b",
		"", "a", "a\x09b\x20c\x7ed", "\t", "\x09\x09", "\r\n", "\x7f\x7f",
		"caf\xc3\xa9", "\xe4\xb8\xad\xe6\x96\x87",
	}
	for _, s := range boundary {
		require.Equal(t, legacyValueClean(s), ValidHeaderValue(s), "boundary %q", s)
	}
}

// TestValidHeaderValueSemantics 行为清单（人类可读的判据陈述，与等价门互补）：
// TAB 与空格放行，控制字节与 DEL 拒绝，空串合法（Del 语义由 request_patch 解释），
// UTF-8 高位字节放行。
func TestValidHeaderValueSemantics(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{name: "empty is wire-legal", val: "", want: true},
		{name: "plain", val: "vibe-code/1.0", want: true},
		{name: "tab", val: "a\tb", want: true},
		{name: "space", val: "a b", want: true},
		{name: "utf8", val: "caf\xc3\xa9", want: true},
		{name: "del", val: "a\x7fb", want: false},
		{name: "nul", val: "a\x00b", want: false},
		{name: "cr", val: "a\rb", want: false},
		{name: "lf", val: "a\nb", want: false},
		{name: "vt", val: "a\x0bb", want: false},
		{name: "unit separator", val: "a\x1fb", want: false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, ValidHeaderValue(tc.val), tc.name)
	}
}

// TestValidHeaderName RFC 9110 token 判据：字母数字与 tchar 放行；空串、空格、
// 冒号、括号、分隔符、非 ASCII、CR/LF/TAB 一律拒绝。
func TestValidHeaderName(t *testing.T) {
	valid := []string{
		"Host", "X-Api-Key", "x-a", "Anthropic-Version", "OpenAI-Beta",
		"Responses-Websockets", "x_1", "a", "!#$%&'*+-.^`|~",
	}
	for _, name := range valid {
		require.True(t, ValidHeaderName(name), "合法头名 %q 必须放行", name)
	}
	invalid := []string{
		"", "X A", "X:A", "X\tA", "X\rA", "X\nA", "X(", "X)", "X/", "X@",
		"X[", "X]", "X{", "X}", "X\"", "X,", "X;", "X<", "X>", "X=", "X?",
		"Ünïcode", "caf\xc3\xa9", "X\x00A", "X\x7fA",
	}
	for _, name := range invalid {
		require.False(t, ValidHeaderName(name), "非法头名 %q 必须拒绝", name)
	}
	// 判据与 net/http 的规范化前置条件一致：CanonicalHeaderKey 对非法名原样返回，
	// 所以任何被本函数拒绝的名字都不会被「规范化后意外合法」。
	for _, name := range invalid {
		require.Equal(t, name, http.CanonicalHeaderKey(name), "非法名 %q 不得被规范化成另一个名字", name)
	}
}
