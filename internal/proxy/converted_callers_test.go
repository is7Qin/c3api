// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestConvCallersComplete 方向 → convertedCaller 的接线必须覆盖全部方向：
// 缺键会让 forwardRoute 拿到 nil UpstreamCaller（failover 骨架直接 panic），
// 而编译器完全帮不上忙——只有穷举断言能挡住"新增方向忘了在这里加一行"。
func TestConvCallersComplete(t *testing.T) {
	p := newConvertedTestProxyAccs(t, map[int64][]*domain.Account{}, nil)
	all := domain.AllProtocolConverts()
	require.Len(t, p.convCallers, len(all), "convCallers 条目数必须等于方向全集")
	for _, pc := range all {
		c, ok := p.convCallers[pc]
		require.True(t, ok, "方向 %q 未接线 convertedCaller（运行期会 nil caller panic）", pc)
		require.NotNil(t, c, "方向 %q 的 caller 是 nil", pc)
	}
}

// TestConvertedCallerIsChatSource 上游=chat 的判定只对两个新方向成立：判错会
// 让既有 4 方向走进 chat 分支（流式字节变化 / 用量改走 chat 解析）。
func TestConvertedCallerIsChatSource(t *testing.T) {
	for _, pc := range domain.AllProtocolConverts() {
		got := isChatSource(pc)
		want := pc == domain.ProtocolConvertMessToChat || pc == domain.ProtocolConvertRespToChat
		require.Equal(t, want, got, "isChatSource(%q)", pc)
	}
	require.False(t, isChatSource(domain.ProtocolConvertOff))
}

// TestConvertedTargetProtocols 目标协议派生：chat 目标只应由两个新方向产生
// （既有方向不得因为改动而改指 chat）。
func TestConvertedTargetProtocols(t *testing.T) {
	for _, pc := range domain.AllProtocolConverts() {
		tgt := pc.Target()
		require.NotEqual(t, domain.RequestFormat(""), tgt, "方向 %q 缺目标协议", pc)
		if tgt == domain.FormatOpenAIChat {
			require.True(t, isChatSource(pc), "目标 chat 的方向 %q 必须是 chat 源（convertedCaller 的 chat 分支依赖它）", pc)
		}
	}
	_ = credential.TypeAPIKey // 保持 import 一致（helper 依赖 credential 类型）
}
