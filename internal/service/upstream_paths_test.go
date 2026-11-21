// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestValidateUpstreamPathsMatrix custom 路径校验：白名单 + 段规则 + 键/格式门。
// 每个拒绝项都对应一类真实绕过（分隔符变体、百分号编码、query 走私），
// 不允许用黑名单思路放宽。
func TestValidateUpstreamPathsMatrix(t *testing.T) {
	accept := []map[string]string{
		nil,
		{},
		{"openai-chat": "chat/completions"},
		{"openai-chat": "/chat/completions"}, // 至多一个前导 / 被归一化
		{"openai-responses": "responses"},
		{"openai-responses-ws": "v1/responses"},
		{"anthropic": "v1/messages"},
		{"openai-search": "alpha/search"},
		{"openai-images.generations": "images/generations"},
		{"openai-images.edits": "images/edits"},
		{"openai-chat": "chat/completions?api-version=2024-10-21"},
		{"anthropic": "v1/messages?beta=true&x=a-b_c.d~e"},
		{"openai-chat": "chat/completions?x=%2e%2e"}, // query 值里的点段无害（非路径）
		{"openai-chat": "  chat/completions  "},      // 首尾空白被裁掉
		// 长度上限作用在归一后的值：剥掉前导 / 后恰好 1024 仍合法（顺序：
		// trim → 剥一个 / → 长度 → 白名单）
		{"openai-chat": "/" + strings.Repeat("a", 1024)},
	}
	for _, tc := range accept {
		_, err := validateUpstreamPaths(tc)
		require.NoError(t, err, "应接受: %#v", tc)
	}

	reject := []map[string]string{
		{"unknown-key": "chat/completions"},
		{"openai-chat": ""},
		{"openai-chat": "/"},
		{"openai-chat": "chat/completions/"},             // 尾段 /
		{"openai-chat": "a//b"},                          // 空段
		{"openai-chat": "../admin"},                      // 上跳段
		{"openai-chat": "a/../../b"},                     // 中段上跳
		{"openai-chat": "a/./b"},                         // 当前段
		{"openai-chat": "chat%2Fcompletions"},            // path 不允许百分号（fail-closed）
		{"openai-chat": "a\\..\\..\\admin"},              // 反斜杠分隔变体
		{"openai-chat": "..;/admin"},                     // 分号参数绕过
		{"openai-chat": "//evil.com/a"},                  // 双斜杠
		{"openai-chat": "http://evil.com/a"},             // scheme 走私
		{"openai-chat": "chat/completions#frag"},         // fragment
		{"openai-chat": "chat/completions?"},             // 空 query
		{"openai-chat": "chat/completions?x=%2f"},        // query 解码出 '/' → 白名单拒（防路径走私）
		{"openai-chat": "chat/completions?x=%3a%2f%2fh"}, // query 解码出 ':' '/'（防 URL 走私）
		{"openai-chat": "chat/completions?x=%zz"},        // 非法百分号转义
		{"openai-chat": "chat/\ncompletions"},            // 内嵌控制字符（首尾空白按设计裁掉）
		{"openai-chat": strings.Repeat("a", 1025)},       // 超长
		{"openai-chat": "chat/completions?x=" + strings.Repeat("a", 1024)},
	}
	for _, tc := range reject {
		_, err := validateUpstreamPaths(tc)
		require.ErrorIs(t, err, ErrInvalidInput, "应拒绝: %#v", tc)
	}

	// 键与 supported_formats **不联动**：非当前格式的键是死配置（永不生效），
	// 但必须允许保存——否则收窄格式后任何一次普通保存都会把残留覆盖永久删掉。
	for _, paths := range []map[string]string{
		{"openai-chat": "chat/completions"},
		{"anthropic": "v1/messages"},
		{"openai-images.edits": "images/edits"},
	} {
		_, err := validateUpstreamPaths(paths)
		require.NoError(t, err, "死配置必须可保存: %#v", paths)
	}
	// 未知键仍然拒（只认 7 键全集）。
	_, err := validateUpstreamPaths(map[string]string{"bogus-key": "x"})
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestValidateTemplateKeepsEcosystemFormatLimit custom 与 api_key 的「全部格式都收」
// 由 domain 的格式矩阵 + validateFormatsForType 承担，这里不再逐行复述；
// 本用例只守**本函数**独有的判断：生态类型（ResponsesSpecial）仍然只允许
// resp/resp-ws/images/search，chat 被拒——这次放开不能顺带削弱既有约束。
func TestValidateTemplateKeepsEcosystemFormatLimit(t *testing.T) {
	require.ErrorIs(t, validateTemplate(&domain.Template{
		Name: "special-chat", BaseURL: "https://u",
		CredentialType:   credential.TypeResponsesSpecial,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	}), ErrInvalidInput)
}

// TestValidateTemplateCustomAcceptsAllFormats 走的是**接线**而不是裸谓词：
// validateTemplate → dedupFormats → validateFormatsForType → domain.FormatsAllowedFor。
// custom 收窄（例如只认 anthropic）时这条断言必须红——矩阵用例只钉谓词，钉不住
// 模板校验有没有真的把 custom 交给它。
func TestValidateTemplateCustomAcceptsAllFormats(t *testing.T) {
	require.NoError(t, validateTemplate(&domain.Template{
		Name:           "custom-all",
		CredentialType: credential.TypeCustom,
		SupportedFormats: []domain.RequestFormat{
			domain.FormatOpenAIChat,
			domain.FormatOpenAIResponses,
			domain.FormatOpenAIResponsesWS,
			domain.FormatOpenAIImages,
			domain.FormatOpenAISearch,
			domain.FormatAnthropic,
		},
	}))
}

// TestValidateUpstreamPathsNormalizes 校验必须**回写归一结果**：只判定不归一，
// "/v1/messages" 与带空白的值会通过校验但在上游拼成 /v1/v1/messages 或 %20…。
func TestValidateUpstreamPathsNormalizes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/v1/messages", "v1/messages"},
		{"  chat/completions  ", "chat/completions"},
		{" /responses", "responses"},
		{"v1/messages?beta=true", "v1/messages?beta=true"},
	}
	for _, tc := range cases {
		got, err := validateUpstreamPaths(map[string]string{"anthropic": tc.in})
		require.NoError(t, err, "in=%q", tc.in)
		require.Equal(t, tc.want, got["anthropic"], "落库值必须已归一（in=%q）", tc.in)
	}
}
