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

// TestCapabilityNewDirections 新增两方向的接线进度账本（M2a：mess→chat 的**请求面**
// 已实现；响应面随 M2b、流式随 M3 打开）。每打开一面都必须同步改本表——它是
// "半成品窗口"的显式账本，不是恒真断言。
func TestCapabilityNewDirections(t *testing.T) {
	for pc, want := range map[domain.ProtocolConvert]DirectionCaps{
		domain.ProtocolConvertMessToChat: {Request: true},
		domain.ProtocolConvertRespToChat: {Request: true},
	} {
		require.Equal(t, want, Capability(pc), "%q 的能力三元组与接线进度不符", pc)
		// 三项未全真 ⇒ 仍未 GA：配置层必须继续 400，流式必须继续 fail-closed。
		require.False(t, want.Request && want.Response && want.Stream, "%q 尚未全真却当成 GA", pc)
		require.False(t, StreamSupported(pc), "%q StreamSupported 应为 false", pc)
	}
}

// TestUnsupportedDirectionSentinel 未接线方向必须返回 **可 errors.Is 的 sentinel**
// （调用方靠它区分"未支持方向"与"请求体非法"；字符串匹配会在改文案时静默失效）。
func TestUnsupportedDirectionSentinel(t *testing.T) {
	// 响应面：两个新方向都尚未接线（M2c 才打开）。
	for _, pc := range []domain.ProtocolConvert{
		domain.ProtocolConvertMessToChat,
		domain.ProtocolConvertRespToChat,
	} {
		_, err := ConvertResponse([]byte(`{"id":"x"}`), pc)
		require.ErrorIs(t, err, ErrUnsupportedDirection, "ConvertResponse(%q)", pc)
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
