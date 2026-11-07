// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestCapabilityTableComplete 能力表必须逐一登记 domain 的每个方向：漏登记
// 会退化成零值（三项 false）→ 配置层 400 + 流式 400，表现为"这个方向不可用"
// 而没有任何编译期或启动期报错，属于最难定位的一类缺口。
func TestCapabilityTableComplete(t *testing.T) {
	all := domain.AllProtocolConverts()
	require.Len(t, dirCaps, len(all), "能力表条目数必须等于方向全集")
	for _, pc := range all {
		_, ok := dirCaps[pc]
		require.True(t, ok, "方向 %q 未登记能力表（会退化成三项 false）", pc)
	}
}

// TestCapabilityExistingDirectionsGA 既有 4 方向早已上线，三项恒 true——
// 任何一次改动把它们置 false 都会让存量组配置立刻 400。
func TestCapabilityExistingDirectionsGA(t *testing.T) {
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertChatToResp,
		domain.ProtocolConvertMessToResp,
		domain.ProtocolConvertRespToMess,
		domain.ProtocolConvertChatToMess,
	} {
		c := Capability(pc)
		require.True(t, c.Request, "%q 请求面能力丢失", pc)
		require.True(t, c.Response, "%q 响应面能力丢失", pc)
		require.True(t, c.Stream, "%q 流式能力丢失", pc)
		require.True(t, StreamSupported(pc), "%q StreamSupported 不一致", pc)
	}
}

// TestCapabilityNewDirections 两个新方向三项全真（M2 请求/响应面 + M3 流式）。
// 这张表是"接线进度"的显式账本：任何一项回到 false 都必须同步改这里（并说明
// 为什么退回去），不是恒真断言。
func TestCapabilityNewDirections(t *testing.T) {
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertMessToChat,
		domain.ProtocolConvertRespToChat,
	} {
		c := Capability(pc)
		require.Equal(t, DirectionCaps{Request: true, Response: true, Stream: true}, c,
			"%q 的能力三元组与接线进度不符", pc)
		require.True(t, StreamSupported(pc), "%q StreamSupported 与能力表不一致", pc)
		// 三项全真 ⇒ 已 GA：配置层不再拒绝（service 的闸门读同一张表）。
		require.True(t, c.Request && c.Response && c.Stream, "%q 三项应全真", pc)
	}
}

// TestUnsupportedDirectionSentinel 未接线方向必须返回 **可 errors.Is 的 sentinel**
// （调用方靠它区分"未支持方向"与"请求体非法"；字符串匹配会在改文案时静默失效）。
func TestUnsupportedDirectionSentinel(t *testing.T) {
	// 未登记的方向值必须返回 sentinel（两个新方向已全接线，故改用非法方向值：
	// sentinel 的意义是"配置/部署与实现不一致"，不是"某个方向名"）。
	for _, f := range []func([]byte, domain.ProtocolConvert) ([]byte, error){ConvertRequest, ConvertResponse} {
		_, err := f([]byte(`{"id":"x"}`), domain.ProtocolConvert("no_such_direction"))
		require.ErrorIs(t, err, ErrUnsupportedDirection)
	}
	// 已接线方向绝不能返回该 sentinel（否则会把正常请求误判成"方向不支持"）。
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertChatToResp,
		domain.ProtocolConvertMessToResp,
		domain.ProtocolConvertRespToMess,
		domain.ProtocolConvertChatToMess,
		domain.ProtocolConvertMessToChat,
		domain.ProtocolConvertRespToChat,
	} {
		_, err := ConvertRequest([]byte(`{"model":"m","messages":[]}`), pc)
		require.False(t, errors.Is(err, ErrUnsupportedDirection), "已接线方向 %q 不应返回 ErrUnsupportedDirection", pc)
	}
}
