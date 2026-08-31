// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// /ops/workers scheduler Stats 与真实状态一致性单测（编译道观测：pending/cap
// 零成本采集；typed struct 非 map 契约）。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

func TestSchedulerStats(t *testing.T) {
	s := New(testCfg(), newMemLoader(nil), rule.New(rule.Config{}, &fakeRuleStore{}, nil), nil)
	st := s.Stats().(SchedulerStats)
	require.Zero(t, st.CompilePending, "未触发编译：pending 0")
	require.Equal(t, 1, st.CompileCap, "编译触发通道 cap 1（trailing-edge 合并）")
	require.Zero(t, st.DecisionRoutes, "未编译：决策路由 0")

	// 武装编译道 + 同步编译 → 决策视图发布，路由数 > 0、世代 > 0。
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s2 := New(testCfg(), m, rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil), nil)
	require.NoError(t, s2.reload(context.Background()))
	wireSources(s2, nil, nil)
	s2.compileOnce()
	st2 := s2.Stats().(SchedulerStats)
	require.Greater(t, st2.DecisionRoutes, 0, "编译后发布决策路由")
	require.Greater(t, st2.DecisionGeneration, uint64(0))
	require.NotZero(t, st2.LastCompileOKUnixMs, "成功编译时刻已记录")
}
