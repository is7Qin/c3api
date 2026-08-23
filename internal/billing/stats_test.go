// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// /ops/workers billing flusher Stats 与真实状态一致性单测（F2 ABI-4 终态：
// lag 族四字段——lag/unbilled/quarantine 每周期收尾 refreshLag 原子写，
// last_cycle = 最近成功消费时刻；typed struct 断言）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlusherStats(t *testing.T) {
	store := newFakeLedgerStore()
	f := newFlusherWith(store, 1, map[int64]int64{1: 1_000_000})
	require.Zero(t, f.Stats().(FlusherStats).LastCycleUnixMs, "尚未消费 = 0")

	// 空游标周期：不推进 lastFlush（观测"最近何时真正消费过"）。
	f.consumeCycle(context.Background())
	st := f.Stats().(FlusherStats)
	require.Zero(t, st.LastCycleUnixMs, "空周期不推进 lastFlush")
	require.Zero(t, st.UnbilledRows)
	require.Zero(t, st.LagMs, "游标空 = lag 0")
	require.Zero(t, st.QuarantinedRows)

	// 600 行 > 单批 LIMIT 500：一个周期消费 500，lag 探测刷新剩余 100——
	// UnbilledRows = 当前未扣费账本行数；行回填 1 分钟 → lag 稳健为正。
	for i := 1; i <= 600; i++ {
		store.seedRow(int64(i), 1, 10, time.Now().Add(-time.Minute))
		store.setBalance(1, 1_000_000)
	}
	f.consumeCycle(context.Background())
	st = f.Stats().(FlusherStats)
	require.Equal(t, int64(100), st.UnbilledRows, "UnbilledRows = 当前未扣费行数（批间 lag 探测）")
	require.Positive(t, st.LagMs, "lag = 探测时刻 now − 最老 unbilled 行 created_at")
	require.Greater(t, st.LastCycleUnixMs, int64(0), "lastFlush = 最近成功消费时刻")
	require.Zero(t, st.QuarantinedRows, "无隔离")

	// Close 排空至游标清空 → UnbilledRows/lag 归零。
	require.NoError(t, f.Close(context.Background()))
	st = f.Stats().(FlusherStats)
	require.Zero(t, st.UnbilledRows, "排空后 Unbilled 归零")
	require.Zero(t, st.LagMs, "排空后游标空 = lag 0")
}
