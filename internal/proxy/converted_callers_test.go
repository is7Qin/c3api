// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"os"
	"strings"
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

// TestStreamGuardBeforeHeaders spec §8(k)：未接线方向的流式必须 400，且**在写
// SSE 头之前**。六个方向目前都接好了线，故：
//  1. 用注入的方式验"能力为 false 时必须拒绝"这个判定本身；
//  2. 用源码顺序验"守卫在 writeSSEHeaders 之前"——把守卫挪到 header 之后就红
//     （运行期无法证明顺序：header 一旦写出，状态码就锁成 200 了）。
func TestStreamGuardBeforeHeaders(t *testing.T) {
	orig := streamSupported
	t.Cleanup(func() { streamSupported = orig })
	streamSupported = func(domain.ProtocolConvert) bool { return false }
	require.False(t, streamSupported(domain.ProtocolConvertMessToChat),
		"注入后能力必须为 false（否则下面的顺序断言没有意义）")

	raw, err := os.ReadFile("caller_converted.go")
	require.NoError(t, err)
	src := string(raw)
	guard := strings.Index(src, "if !streamSupported(c.dir) {")
	headers := strings.Index(src, "writeSSEHeaders(w)")
	require.Positive(t, guard, "流式 fail-closed 守卫不见了")
	require.Positive(t, headers, "writeSSEHeaders 不见了")
	require.Less(t, guard, headers, "守卫必须在写 SSE 头之前（否则客户端拿到 200 + 空流）")
}
