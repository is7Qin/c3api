// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShutdownChainOrder 是 P5 唯一的**行为**证据：把四个副作用点换成记录器，
// 断言 shutdown 按「HTTP 优雅窗口 → 强断长连接 → 等在途归零 → 尾部排空」执行。
//
// 为什么需要它：AST 门只能证明「这五个调用按这个顺序写着」，证明不了运行期真的
// 走到；而顺序错了的后果是计费丢窗口（rec 在 billing 之前关），没有异常、没有
// 日志，只有对账时才发现。
func TestShutdownChainOrder(t *testing.T) {
	var seq []string
	r := &appRuntime{
		// log 为 nil：Sync 是空操作（logx 空 logger 静默），这里只关心四个副作用点。
		shutdownHTTP: func(context.Context) { seq = append(seq, "http") },
		closeWS:      func() { seq = append(seq, "ws") },
		waitInflight: func(context.Context) { seq = append(seq, "inflight") },
		tail:         func(context.Context) { seq = append(seq, "tail") },
	}

	r.shutdown(context.Background())

	require.Equal(t, []string{"http", "ws", "inflight", "tail"}, seq,
		"停机链顺序：先优雅窗口收尾 → 再强断长连接（触发计费收尾）→ 等在途归零 → 最后反向排空 + 尾部")
}
