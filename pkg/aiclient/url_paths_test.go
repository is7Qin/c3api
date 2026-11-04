// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseFullURLPathAndQuery 路径派生：默认端点逐字节不变；query 不被转义；
// base 上的协议前缀（/zen）继续生效；v1/ 精确前缀规则不被放宽。
func TestParseFullURLPathAndQuery(t *testing.T) {
	cases := []struct{ base, path, want string }{
		// 现网默认：必须与改动前逐字节一致
		{"https://api.example.com", "chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com", "responses", "https://api.example.com/v1/responses"},
		{"https://api.example.com", "alpha/search", "https://api.example.com/v1/alpha/search"},
		{"https://api.example.com", "images/generations", "https://api.example.com/v1/images/generations"},
		{"https://api.example.com", "images/edits", "https://api.example.com/v1/images/edits"},
		{"https://api.example.com", "v1/messages", "https://api.example.com/v1/messages"},
		{"https://api.example.com", "v1/responses", "https://api.example.com/v1/responses"},

		// base 协议前缀继续生效
		{"https://api.example.com/zen", "chat/completions", "https://api.example.com/zen/v1/chat/completions"},
		{"https://api.example.com/zen", "v1/messages", "https://api.example.com/zen/v1/messages"},

		// query：切分后设 RawQuery，不得出现 %3F
		{"https://api.example.com", "chat/completions?api-version=2024-10-21",
			"https://api.example.com/v1/chat/completions?api-version=2024-10-21"},
		{"https://api.example.com/zen", "v1/messages?beta=true",
			"https://api.example.com/zen/v1/messages?beta=true"},

		// v1/ 是精确前缀：v1foo/x 仍补 /v1
		{"https://api.example.com", "v1foo/x", "https://api.example.com/v1/v1foo/x"},
	}
	for _, tc := range cases {
		u, err := parseFullURL(tc.base, tc.path)
		require.NoError(t, err, "%s + %s", tc.base, tc.path)
		require.Equal(t, tc.want, u.String(), "%s + %s", tc.base, tc.path)
	}
}
