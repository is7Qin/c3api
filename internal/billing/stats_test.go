// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// /ops/workers billing flusher Stats 与真实状态一致性单测（F2 ABI-4 终态：
// lag 族四字段——lag/unbilled/quarantine 每周期收尾 refreshLag 原子写，
// last_cycle = 最近成功消费时刻；typed struct 断言）。F2-opt D2 排空节奏：
// 单周期全量消费（一批一 tick 断言废除）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlusherStats(t *testing.T) {
	restoreLagThrottle(t, 0)
	restoreLagSlowEvery(t, 1) // 禁节流：每周期探测刷新（节流行为归 throttle 专测）
	store := newFakeLedgerStore()
	f := newFlusherWith(store, 1, map[int64]int64{1: 1_000_000})
	require.Zero(t, f.Stats().(FlusherStats).LastCycleUnixMs, "尚未消费 = 0")

	// 空游标周期：不推进 lastFlush（观测"最近何时真正消费过"）。
	f.consumeCycle(context.Background(), false)
	st := f.Stats().(FlusherStats)
	require.Zero(t, st.LastCycleUnixMs, "空周期不推进 lastFlush")
	require.Zero(t, st.UnbilledRows)
	require.Zero(t, st.LagMs, "游标空 = lag 0")
	require.Zero(t, st.QuarantinedRows)

	// 600 行积压：锁外观测周期——Stats/lag 真值照常刷新（wave3 D-B：UnbilledRows
	// 降级占位恒 0；行回填 1 分钟 → lag 稳健为正）。
	for i := 1; i <= 600; i++ {
		store.seedRow(int64(i), 1, 10, time.Now().Add(-time.Minute))
		store.setBalance(1, 1_000_000)
	}
	store.holdLock()
	f.consumeCycle(context.Background(), false)
	st = f.Stats().(FlusherStats)
	require.Zero(t, st.UnbilledRows, "D-B 降级：UnbilledRows 占位恒 0（精确 COUNT 已删）")
	require.Positive(t, st.LagMs, "lag = 探测时刻 now − 最老 unbilled 行 created_at")
	require.Zero(t, st.LastCycleUnixMs, "锁外周期不消费")
	store.releaseLock()

	// 排空式消费（D2）：单周期全量清空 600 行——一批一 tick 节奏废除。
	f.consumeCycle(context.Background(), false)
	st = f.Stats().(FlusherStats)
	require.Zero(t, st.UnbilledRows, "排空式循环单周期全量消费")
	require.Greater(t, st.LastCycleUnixMs, int64(0), "lastFlush = 最近成功消费时刻")
	require.Zero(t, st.QuarantinedRows, "无隔离")

	// Close 排空至游标清空 → UnbilledRows/lag 归零。
	require.NoError(t, f.Close(context.Background()))
	st = f.Stats().(FlusherStats)
	require.Zero(t, st.UnbilledRows, "排空后 Unbilled 归零")
	require.Zero(t, st.LagMs, "排空后游标空 = lag 0")
}
