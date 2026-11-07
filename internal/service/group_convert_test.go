// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestNormalizeProtocolConverts 集合归一与冲突判定：off 剔除、重复/非法 400、
// **同客户端格式两方向并存 400**（按 Client() 派生——chat×2 / mess×2 / resp×2
// 三对都要拦，不能只拦 chat 对）。
func TestNormalizeProtocolConverts(t *testing.T) {
	cases := []struct {
		name string
		in   []domain.ProtocolConvert
		ok   bool
		want []domain.ProtocolConvert
	}{
		{name: "nil → 空（不转换）", in: nil, ok: true, want: []domain.ProtocolConvert{}},
		{name: "仅 off → 空", in: []domain.ProtocolConvert{domain.ProtocolConvertOff}, ok: true, want: []domain.ProtocolConvert{}},
		{
			name: "不同客户端格式可并存",
			in:   []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp, domain.ProtocolConvertRespToMess},
			ok:   true,
			want: []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp, domain.ProtocolConvertRespToMess},
		},
		{name: "重复方向 → 400", in: []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertChatToResp}, ok: false},
		{name: "非法方向 → 400", in: []domain.ProtocolConvert{domain.ProtocolConvert("bogus")}, ok: false},
		{
			name: "chat 客户端两方向并存 → 400",
			in:   []domain.ProtocolConvert{domain.ProtocolConvertChatToResp, domain.ProtocolConvertChatToMess},
			ok:   false,
		},
		{
			name: "mess 客户端两方向并存 → 400",
			in:   []domain.ProtocolConvert{domain.ProtocolConvertMessToResp, domain.ProtocolConvertMessToChat},
			ok:   false,
		},
		{
			name: "resp 客户端两方向并存 → 400",
			in:   []domain.ProtocolConvert{domain.ProtocolConvertRespToMess, domain.ProtocolConvertRespToChat},
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProtocolConverts(tc.in)
			if !tc.ok {
				require.ErrorIs(t, err, ErrInvalidInput)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestNormalizeProtocolConvertsRejectsNotGA 能力表闸门：尚未 GA（三项未全真）
// 的方向不允许被配置——否则会得到"能配置、首请求 500 或流式空流"的半成品窗口。
//
// **M2/M3 打开能力时改本测试**：M1 阶段新方向全 false ⇒ 单方向配置也必须 400。
func TestNormalizeProtocolConvertsRejectsNotGA(t *testing.T) {
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertMessToChat,
		domain.ProtocolConvertRespToChat,
	} {
		_, err := normalizeProtocolConverts([]domain.ProtocolConvert{pc})
		require.ErrorIs(t, err, ErrInvalidInput, "未 GA 方向 %q 必须被配置层拒绝", pc)
	}
	// 反向：已 GA 的 4 个方向必须被接受（防止闸门写成"一律拒绝"）。
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertChatToResp,
		domain.ProtocolConvertMessToResp,
		domain.ProtocolConvertRespToMess,
		domain.ProtocolConvertChatToMess,
	} {
		_, err := normalizeProtocolConverts([]domain.ProtocolConvert{pc})
		require.NoError(t, err, "已 GA 方向 %q 必须被接受", pc)
	}
}
