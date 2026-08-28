// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package invalidate

// /ops/workers invalidate Stats 与真实状态一致性单测（脏状态 atomic.Pointer
// 零锁读；typed struct 断言）。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDebouncerStats(t *testing.T) {
	d := New(Config{Sched: &recSched{}, Clients: &recClients{}, Auth: &recAuth{}})
	st := d.Stats().(DebouncerStats)
	require.False(t, st.Dirty, "初始不脏")
	require.Equal(t, DefaultWindow.Milliseconds(), st.WindowMs)

	d.Users()
	st = d.Stats().(DebouncerStats)
	require.True(t, st.Dirty)
	require.Equal(t, "users", st.DirtyKinds)
	require.Zero(t, st.DirtyGroups)

	// 组级定向（独立去抖器）：Kinds 空、Groups 计数真实。
	d2 := New(Config{Sched: &recSched{}, Clients: &recClients{}, Auth: &recAuth{}})
	d2.Accounts([]int64{7, 8}, false)
	st = d2.Stats().(DebouncerStats)
	require.True(t, st.Dirty)
	require.Empty(t, st.DirtyKinds, "纯组级定向无 full 脏位")
	require.Equal(t, 2, st.DirtyGroups)

	// 多脏位合并命名。
	d.Rules()
	st = d.Stats().(DebouncerStats)
	require.Equal(t, "users,rules", st.DirtyKinds)

	// flush 消费后复位。
	d.flush()
	st = d.Stats().(DebouncerStats)
	require.False(t, st.Dirty, "flush 后不脏")
	require.Zero(t, st.DirtyGroups)
}
