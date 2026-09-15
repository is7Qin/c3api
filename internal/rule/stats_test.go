// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

// /ops/workers rule-engine Stats 与真实状态一致性单测（队列占用 + 丢弃计数；
// typed struct 断言；atomic.Uint64 → int64 JSON 类型验证）。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuleEngineStats(t *testing.T) {
	e := New(Config{EventQueueSize: 2}, newFakeRuleStore(), nil, nil, nil)
	st := e.Stats().(RuleEngineStats)
	require.Zero(t, st.Queued)
	require.Equal(t, 2, st.QueueCap)

	for i := 0; i < 5; i++ {
		e.Enqueue(Event{AccountID: int64(i)})
	}
	st = e.Stats().(RuleEngineStats)
	require.Equal(t, 2, st.Queued, "队列占用 = 实际积压")
	require.Equal(t, int64(3), st.AdmissionDropped, "溢出丢弃 = 5 投 2 收")
	require.Equal(t, ruleDropWarnThreshold, st.DropWarnThreshold)
	require.Equal(t, uint64(3), e.dropped.Load(), "Stats 与内部计数同源")
}
