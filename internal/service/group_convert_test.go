// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
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

// TestNormalizeProtocolConvertsCapabilityGate 能力表闸门：尚未 GA（三项未全真）
// 的方向不允许被配置——否则会得到"能配置、首请求 500 或流式空流"的半成品窗口。
//
// 六个方向现在全部 GA，故拒绝分支靠**注入**一个非 GA 方向来验（能力查询走
// convertCapability 间接层，与 proxy 的 convertRequest 同款），而不是让这段
// 判据在生产里变成死代码。
func TestNormalizeProtocolConvertsCapabilityGate(t *testing.T) {
	all := domain.AllProtocolConverts()
	require.Len(t, all, 6, "方向全集变了：GA 账本必须同步")
	for _, pc := range all {
		_, err := normalizeProtocolConverts([]domain.ProtocolConvert{pc})
		require.NoError(t, err, "已 GA 方向 %q 必须被接受", pc)
	}

	// 注入"流式尚未接线"的方向：必须被拒（且不是靠 client/target 冲突之类别的理由）。
	orig := convertCapability
	t.Cleanup(func() { convertCapability = orig })
	convertCapability = func(pc domain.ProtocolConvert) protoconv.DirectionCaps {
		c := orig(pc)
		if pc == domain.ProtocolConvertMessToChat {
			c.Stream = false
		}
		return c
	}
	_, err := normalizeProtocolConverts([]domain.ProtocolConvert{domain.ProtocolConvertMessToChat})
	require.ErrorIs(t, err, ErrInvalidInput, "未 GA 方向必须被配置层拒绝")

	// 三项全真才放行：只差 Response 也一样拒。
	convertCapability = func(pc domain.ProtocolConvert) protoconv.DirectionCaps {
		c := orig(pc)
		c.Response = false
		return c
	}
	_, err = normalizeProtocolConverts([]domain.ProtocolConvert{domain.ProtocolConvertMessToChat})
	require.ErrorIs(t, err, ErrInvalidInput, "缺任一能力项都必须拒绝")
}
