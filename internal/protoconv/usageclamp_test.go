// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// usage 口径与钳 0（spec §5.4 + §8(e)）：chat 的 prompt_tokens 是**含缓存**的
// gross，Anthropic 的 input_tokens 是**净额**。三项恒写（含 0），病态上游
// （cached+write > gross）钳 0 且必须计数，绝不允许负值进账。
//
// 这条不依赖 golden：直接断言字面量与丢弃计数。

func TestChatToMessUsageNetAndClamp(t *testing.T) {
	// chatToMessResponse 的 dir 与"客户端→上游"方向同名（响应面复用同一方向值），
	// 所以 mess 客户端的响应面 key 是 mess_to_chat|...。
	const dropKey = "mess_to_chat|resp|usage_negative_clamped"

	for _, tc := range []struct {
		name       string
		usage      string // 空串 = 响应里完全没有 usage
		wantIn     int64
		wantCached int64
		wantCreate int64
		wantOut    int64
		wantClamps int64
	}{
		{
			name:       "net_after_cache_deduction",
			usage:      `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}`,
			wantIn:     50,
			wantCached: 40,
			wantCreate: 10,
			wantOut:    20,
		},
		{
			name:   "no_cache_no_deduction",
			usage:  `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}`,
			wantIn: 100, wantCached: 0, wantCreate: 0, wantOut: 20,
		},
		{
			name:       "over_deduction_clamped_to_zero",
			usage:      `{"prompt_tokens":10,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}`,
			wantIn:     0,
			wantCached: 40,
			wantCreate: 10,
			wantOut:    20,
			wantClamps: 1,
		},
		{
			// 上游没给 usage：三项恒写 0，绝不留缺键（缺键即漏计费）。
			name: "missing_usage_writes_zeros", usage: "",
		},
		{
			// 显式全 0：同样是三项恒写，且不得被当成"缺失"。
			name:  "explicit_zero_writes_zeros",
			usage: `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetDropCounts()
			t.Cleanup(ResetDropCounts)

			body := `{"id":"c_1","object":"chat.completion","model":"m","choices":[` +
				`{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]`
			if tc.usage != "" {
				body += `,"usage":` + tc.usage
			}
			body += `}`

			got, err := ConvertResponse([]byte(body), domain.ProtocolConvertMessToChat)
			require.NoError(t, err)

			u := gjson.GetBytes(got, "usage")
			require.True(t, u.Exists(), "usage 必须存在")
			for _, key := range []string{
				"input_tokens", "cache_read_input_tokens",
				"cache_creation_input_tokens", "output_tokens",
			} {
				require.True(t, u.Get(key).Exists(), "usage.%s 必须恒写（含 0）", key)
			}
			require.Equal(t, tc.wantIn, u.Get("input_tokens").Int(), "input_tokens 必须是净额")
			require.Equal(t, tc.wantCached, u.Get("cache_read_input_tokens").Int())
			require.Equal(t, tc.wantCreate, u.Get("cache_creation_input_tokens").Int())
			require.Equal(t, tc.wantOut, u.Get("output_tokens").Int())
			require.Equal(t, tc.wantClamps, DropCounts()[dropKey], "超额扣减必须钳 0 并计数")
		})
	}
}

// TestChatToRespUsageKeepsReasoningTokens Responses 有 reasoning_tokens 承载位
// ⇒ 必须搬真值；chat 缺该字段时写 0（官方 schema 必填）。
func TestChatToRespUsageKeepsReasoningTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int64
	}{
		{
			name: "transfers_reasoning_tokens",
			body: `{"id":"c_1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":3,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":7}}}`,
			want: 7,
		},
		{
			name: "absent_reasoning_writes_zero",
			body: `{"id":"c_1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":3,"completion_tokens":9}}`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConvertResponse([]byte(tc.body), domain.ProtocolConvertRespToChat)
			require.NoError(t, err)
			require.Equal(t, tc.want,
				gjson.GetBytes(got, "usage.output_tokens_details.reasoning_tokens").Int(),
				"Responses 的 reasoning_tokens 必须搬真值（缺省才写 0）")
		})
	}
}

// TestChatToRespUsageZeroMatrix Responses 目标的用量键必须**恒写**（含 0）：
// official schema 里 usage 是必填对象，缺键会让客户端 SDK 解析失败。
func TestChatToRespUsageZeroMatrix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"missing_usage", ""},
		{"explicit_zero", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`},
		{"only_prompt", `{"prompt_tokens":7,"completion_tokens":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"c_1","object":"chat.completion","model":"m","choices":[` +
				`{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]`
			if tc.usage != "" {
				body += `,"usage":` + tc.usage
			}
			body += `}`

			got, err := ConvertResponse([]byte(body), domain.ProtocolConvertRespToChat)
			require.NoError(t, err)
			for _, key := range []string{
				"usage.input_tokens", "usage.output_tokens", "usage.total_tokens",
				"usage.input_tokens_details.cached_tokens",
				"usage.input_tokens_details.cache_write_tokens",
				"usage.output_tokens_details.reasoning_tokens",
			} {
				require.True(t, gjson.GetBytes(got, key).Exists(), "%s 必须恒写（含 0）", key)
				require.Equal(t, gjson.Number, gjson.GetBytes(got, key).Type, "%s 必须是数字", key)
			}
			require.True(t, gjson.GetBytes(got, "output").IsArray(), "output 必须始终是数组")
		})
	}
}

// TestMessTargetReasoningTokensDropCounted Messages 协议没有 reasoning_tokens 的
// 承载位 ⇒ 有**实际消耗**才算"丢了信息"。值为 0 不构成信息丢失、不计数 ——
// 流式与非流式共用同一份判据（messUsageOf），这条钉住"非零才计"。
func TestMessTargetReasoningTokensDropCounted(t *testing.T) {
	const dropKey = "mess_to_chat|resp|reasoning_tokens"
	for _, tc := range []struct {
		name string
		body string
		want int64
	}{
		{
			name: "nonzero_counted",
			body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":3,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":7}}}`,
			want: 1,
		},
		{
			name: "zero_not_counted",
			body: `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":3,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":0}}}`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetDropCounts()
			t.Cleanup(ResetDropCounts)
			_, err := ConvertResponse([]byte(tc.body), domain.ProtocolConvertMessToChat)
			require.NoError(t, err)
			require.Equal(t, tc.want, DropCounts()[dropKey],
				"reasoning_tokens 的丢弃计数（非零才计）")
		})
	}
}
