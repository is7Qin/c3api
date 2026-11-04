// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseFullURLJoinsPathVerbatim 路径派生：path 是**完整尾段**，本层原样
// 拼接。是否带 /v1 由调用方决定（proxy.targetPath 给内置端点拼 /v1、custom
// 自定义路径原样透传），本层不做任何前缀判定——所以「不以 v1/ 开头」是合法输入，
// 不需要被纠正成 /v1/…。
func TestParseFullURLJoinsPathVerbatim(t *testing.T) {
	cases := []struct{ base, path, want string }{
		// 内置端点的完整尾段（/v1 由 proxy 层拼好）
		{"https://api.example.com", "v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com", "v1/responses", "https://api.example.com/v1/responses"},
		{"https://api.example.com", "v1/alpha/search", "https://api.example.com/v1/alpha/search"},
		{"https://api.example.com", "v1/images/generations", "https://api.example.com/v1/images/generations"},
		{"https://api.example.com", "v1/messages", "https://api.example.com/v1/messages"},

		// custom 自定义路径：原样到达上游，不补 /v1（这正是自定义的意义）
		{"https://api.example.com", "chat/completions", "https://api.example.com/chat/completions"},
		{"https://api.example.com", "messages", "https://api.example.com/messages"},
		{"https://api.example.com", "backend-api/codex/responses", "https://api.example.com/backend-api/codex/responses"},
		{"https://api.example.com", "v1foo/x", "https://api.example.com/v1foo/x"},

		// base 上的协议前缀继续生效（路径段不回退掉 base 的子路径）
		{"https://api.example.com/zen", "v1/messages", "https://api.example.com/zen/v1/messages"},
		{"https://api.example.com/zen", "acme/chat", "https://api.example.com/zen/acme/chat"},

		// query：切分后设 RawQuery，不得出现 %3F
		{"https://api.example.com", "v1/chat/completions?api-version=2024-10-21",
			"https://api.example.com/v1/chat/completions?api-version=2024-10-21"},
		{"https://api.example.com/zen", "v1/messages?beta=true",
			"https://api.example.com/zen/v1/messages?beta=true"},
	}
	for _, tc := range cases {
		u, err := parseFullURL(tc.base, tc.path)
		require.NoError(t, err, "%s + %s", tc.base, tc.path)
		require.Equal(t, tc.want, u.String(), "%s + %s", tc.base, tc.path)
	}
}
