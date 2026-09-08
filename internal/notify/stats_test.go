// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

// /ops/workers notify listener Stats 与真实状态一致性单测（存活标志；typed
// struct 断言）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestListenerStats(t *testing.T) {
	l := NewListener(ListenerConfig{
		DSN:        "postgres://fake",
		Dispatcher: &fakeDisp{},
		Connect: func(ctx context.Context, dsn string) (Conn, error) {
			return newFakeConn(), nil
		},
	})
	require.False(t, l.Stats().(ListenerStats).Running, "未 Start 不存活")
	require.False(t, l.Stats().(ListenerStats).Ready, "未 Start 未建立 LISTEN 基线")

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, l.Start(ctx))
	require.Eventually(t, func() bool { return l.Stats().(ListenerStats).Running },
		time.Second, 5*time.Millisecond, "Start 后监听循环存活")
	require.Eventually(t, func() bool { return l.Stats().(ListenerStats).Ready },
		time.Second, 5*time.Millisecond, "LISTEN + FullRefresh 后 ready")

	cancel()
	require.NoError(t, l.Close(context.Background()), "Close 用独立 ctx（已取消的 Start ctx 不可复用）")
	require.Eventually(t, func() bool { return !l.Stats().(ListenerStats).Running },
		time.Second, 5*time.Millisecond, "循环退出后复位（原子读零锁）")
	require.False(t, l.Stats().(ListenerStats).Ready, "循环退出后 ready 复位")
}
