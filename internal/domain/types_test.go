// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestTemplateFormatSupports(t *testing.T) {
	tpl := &Template{
		SupportedFormats: []RequestFormat{FormatOpenAIChat, FormatAnthropic},
		Models:           []string{"gpt-4o", "claude-3"},
		FormatModels:     map[RequestFormat][]string{FormatOpenAIChat: {"gpt-4o"}},
	}
	require.True(t, tpl.FormatSupports(FormatOpenAIChat, "gpt-4o"))
	require.False(t, tpl.FormatSupports(FormatOpenAIChat, "claude-3"), "配置了格式 → 仅列表内模型")
	require.True(t, tpl.FormatSupports(FormatAnthropic, "gpt-4o"), "未配置格式 → 全部模型")
	require.False(t, tpl.FormatSupports(FormatOpenAIResponses, "gpt-4o"), "格式不在 supported")
	require.True(t, tpl.Serves("gpt-4o"))
	require.False(t, tpl.Serves("nonexistent"))
	require.Equal(t, []RequestFormat{FormatOpenAIChat, FormatAnthropic}, tpl.FormatsFor())
}

func TestTemplateServes(t *testing.T) {
	tpl := &Template{
		Models:       []string{"gpt-4o"},
		FormatModels: map[RequestFormat][]string{FormatOpenAIResponses: {"o3"}},
		ModelMapping: map[string]ModelMappingEntry{"claude-sonnet": {MappedModel: "claude-sonnet-4-5", Mode: ModelMappingModeExplicit}},
	}
	require.True(t, tpl.Serves("gpt-4o"), "serves models")
	require.True(t, tpl.Serves("o3"), "serves format_models list values")
	require.True(t, tpl.Serves("claude-sonnet"), "serves mapping keys")
	require.False(t, tpl.Serves("nope"))
}

func TestRequestFormatValid(t *testing.T) {
	for _, f := range []RequestFormat{
		FormatOpenAIChat, FormatOpenAIResponses, FormatOpenAIResponsesWS, FormatAnthropic,
		FormatOpenAIImages, // spec §4.3：openai-images（images 端点落库 format）
		FormatOpenAISearch, // spec 2026-08-13：openai-search（search 端点落库 format——本 task 只扩枚举）
	} {
		require.True(t, f.Valid(), "format %s should be valid", f)
	}
	require.False(t, RequestFormat("gemini").Valid())
	require.False(t, RequestFormat("openai-images-extra").Valid())
}

func TestTruncateErrMsg(t *testing.T) {
	// 短文本原样返回（零分配路径；全部 ASCII 错误文案 < 500 字符）
	require.Equal(t, "", TruncateErrMsg(""))
	require.Equal(t, "boom", TruncateErrMsg("boom"))
	require.Equal(t, strings.Repeat("a", ErrMsgMaxLen), TruncateErrMsg(strings.Repeat("a", ErrMsgMaxLen)))
	// 超限按 500 字符截断
	require.Equal(t, strings.Repeat("a", ErrMsgMaxLen), TruncateErrMsg(strings.Repeat("a", 600)))
	// 多字节 UTF-8 不拆断：600 个「界」= 1800 字节 → 截 500 字符（1500 字节）
	got := TruncateErrMsg(strings.Repeat("界", 600))
	require.Equal(t, 500, utf8.RuneCountInString(got))
	require.True(t, utf8.ValidString(got), "截断不得产生非法 UTF-8")
	// 字节超限但字符数未超限 → 原样返回
	require.Equal(t, strings.Repeat("界", 300), TruncateErrMsg(strings.Repeat("界", 300)))
}
