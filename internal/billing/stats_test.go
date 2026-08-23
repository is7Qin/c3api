// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// /ops/workers billing flusher Stats 与真实状态一致性单测（F2 表面稳定裁决：
// FlusherStats 结构体与 Stats() 签名原样保留，字段语义重映射——PendingLogs =
// 当前 Unbilled 行数、Waterline 恒 0、Warned 恒 false；typed struct 断言）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlusherStats(t *testing.T) {
	store := newFakeLedgerStore()
	f := newFlusherWith(store, 1, map[int64]int64{1: 1_000_000})
	require.Zero(t, f.Stats().(FlusherStats).LastFlushUnixMs, "尚未消费 = 0")

	// 空游标周期：不推进 lastFlush（观测"最近何时真正消费过"）。
	f.consumeCycle(context.Background())
	st := f.Stats().(FlusherStats)
	require.Zero(t, st.LastFlushUnixMs, "空周期不推进 lastFlush")
	require.Zero(t, st.PendingLogs)
	require.Zero(t, st.PendingWaterline, "水线退役恒 0（TODO(T5) 契约换 lag 族）")
	require.False(t, st.Warned, "水线告警边沿退役恒 false")

	// 600 行 > 单批 LIMIT 500：一个周期消费 500，lag 探测刷新剩余 100——
	// PendingLogs 语义重映射真值 = 当前 Unbilled 行数。
	for i := 1; i <= 600; i++ {
		store.seedRow(int64(i), 1, 10, time.Now())
		store.setBalance(1, 1_000_000)
	}
	f.consumeCycle(context.Background())
	st = f.Stats().(FlusherStats)
	require.Equal(t, int64(100), st.PendingLogs, "PendingLogs = 当前 Unbilled 行数（批间 lag 探测）")
	require.Greater(t, st.LastFlushUnixMs, int64(0), "lastFlush = 最近成功消费时刻")
	require.Zero(t, st.quarantinedRows, "无隔离（非导出真值字段，同包直读）")

	// Close 排空至游标清空 → PendingLogs 归零。
	require.NoError(t, f.Close(context.Background()))
	st = f.Stats().(FlusherStats)
	require.Zero(t, st.PendingLogs, "排空后 Unbilled 归零")
}
