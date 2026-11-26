// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package codexproto_test

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/codexproto"
)

// jwts 拼一个三段式 JWT 外壳（不签名不加密——解析方不验签，第二段即 payload）。
func jwts(payloadJSON string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payloadJSON)) + ".s"
}

// TestDeriveAccountIDMatchesSDK 是本包**存在的前提条件**：同一段 JWT claims 解析
// 逻辑在仓库里有两份（本包 + codexsdk.AccountIDFromToken），它们必须对同一组输入
// 产出完全相同的 (id, ok)。
//
// 这是"两份实现必须一致"的**唯一机械约束**——不是口头约定、不是注释提醒。改本包
// 而不改 SDK 侧（或反之）让语义分叉，这条测试立刻红。反过来说：这条测试通过就
// 证明薄包 SDK 换不来任何行为差异。
//
// 用例覆盖 SDK 实现的每个失败面：三段式判定、base64url 解码、JSON 解码、claims
// 路径命名空间、空白裁剪、空值。
func TestDeriveAccountIDMatchesSDK(t *testing.T) {
	cases := []struct{ name, token string }{
		{"normal", jwts(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acc-1"}}`)},
		{"space padded", jwts(`{"https://api.openai.com/auth":{"chatgpt_account_id":"  acc-2  "}}`)},
		{"empty id", jwts(`{"https://api.openai.com/auth":{"chatgpt_account_id":""}}`)},
		{"whitespace only id", jwts(`{"https://api.openai.com/auth":{"chatgpt_account_id":"   "}}`)},
		{"missing claims", jwts(`{}`)},
		{"wrong namespace", jwts(`{"auth":{"chatgpt_account_id":"acc-3"}}`)},
		{"extra claims are ignored", jwts(`{"exp":1,"https://api.openai.com/auth":{"chatgpt_account_id":"acc-4","email":"x@y"}}`)},
		{"not a jwt", "opaque-refresh-token"},
		{"two parts", "h.s"},
		{"four parts", "h.s.s.s"},
		{"empty", ""},
		{"bad base64", "h.!!!.s"},
		{"not json payload", "h." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := codexproto.DeriveAccountID(tc.token)
			wantID, wantOK := codexsdk.AccountIDFromToken(tc.token)
			require.Equal(t, wantOK, gotOK, "ok 必须与 SDK 一致")
			require.Equal(t, wantID, gotID, "id 必须与 SDK 一致")
		})
	}
}

// TestDeriveAccountIDNotPanic 任何畸形输入都不 panic（管理面会在保存/导入时对用户
// 提交的任意字符串调用它）。
func TestDeriveAccountIDNotPanic(t *testing.T) {
	for _, tok := range []string{"", ".", "..", "a.b.c", "\x00.\xff.\x00"} {
		require.NotPanics(t, func() {
			id, ok := codexproto.DeriveAccountID(tok)
			require.False(t, ok)
			require.Empty(t, id)
		})
	}
}
