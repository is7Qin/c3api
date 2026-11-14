// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// TestSchedulerStatsExposesFingerprintSkips 钉住「指纹丢弃」这个观测面真的
// 出现在 API 里（spec 2026-11-14 §1）：计数器存在但没人读，等于盲区。
// 一次指纹丢弃 ⇒ SchedulerStats.FingerprintSkips == 1。
func TestSchedulerStatsExposesFingerprintSkips(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{acc(1, tp, 4)})

	before := s.Stats().(SchedulerStats)
	require.Zero(t, before.FingerprintSkips)

	// 让候选指纹算不出来：模板被摘掉（生产不变量下不会发生，测试专用叶子变更）。
	snap, ok := s.View().Account(1)
	require.True(t, ok)
	av := snap.static.Load()
	accNoTpl := av.acc
	accNoTpl.Template = nil
	snap.static.Store(newSnapshotStatic(accNoTpl, av.tpl, av.groupIDs, directEgress()))

	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom", "m")

	after := s.Stats().(SchedulerStats)
	require.Equal(t, uint64(1), after.FingerprintSkips,
		"指纹丢弃必须在 /ops 的 scheduler stats 里可见（非零 = 规则引擎完全没机会评估）")
	require.Equal(t, uint64(1), s.fingerprintSkips.Load(), "Stats 只读计数器，不改语义")

	// 其余字段保持原语义（本次改动不动任何既有计数）。
	require.Equal(t, before.CompileCap, after.CompileCap)
	require.Equal(t, before.DecisionGeneration, after.DecisionGeneration)
}
