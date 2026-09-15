// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestNotifyRoutingDirtyReloadRepublishesCompiledPlan 是 NOTIFY routing dirty
// 消费端契约（cmd/server dispatcher 测试的 scheduler 侧对偶）：静态重载——全量
// （invalidate 去抖器 Templates/FullRefresh 路径）与组级定向（Accounts(gids)
// 路径）——必须武装编译道并重编译决策视图。NOTIFY 仅承载 admin/static 变更，
// routing dirty 不单独广播，由静态重载尾部的 RequestCompile 收敛。
//
// Given: armed 编译道（无消费者，cap-1 合并触发以 CompilePending 可见）；
// When: loader 新增组 20 账号 → reload（全量）；随后组内加账号 → InvalidateGroup；
// Then: 两条重载路径各武装一次触发，compileOnce 后发布视图覆盖新路由/新账号。
// 另见缺陷 B 修复：质量 influx（quality-sync PG 落库边界构造器注入 onPersisted）
// 与定价写面（ServiceDeps.CompileNotify + 变化门控）各有专用事件触发，不再
// 依赖静态重载尾部收敛；backstop 探针仍只覆盖静态聚合（质量/价格差由 lane-local
// diff 定作用域）。
func TestNotifyRoutingDirtyReloadRepublishesCompiledPlan(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {accWithEnabled(1, tpl, true, 10000)}})
	s := newSched(t, m)
	wireSources(s, nil, map[string]domain.ResolvedPrices{"m": {InputPerM: pricePtr(1000)}})
	require.Zero(t, s.Stats().(SchedulerStats).CompilePending, "构造期 reload 未 armed，触发不得入队")

	// 远端静态变更（NOTIFY → 去抖器 → 全量重载消费面）：组 20 新增账号。
	setGroup(m, 20, accWithEnabled(2, tpl, true, 10000))
	require.NoError(t, s.reload(context.Background()))
	require.Equal(t, 1, s.Stats().(SchedulerStats).CompilePending, "全量静态重载必须武装 routing 重编译触发")

	s.compileOnce()
	_, ok := s.View().DecisionView().Routes()[RouteRefFor(20, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok, "重编译后发布视图必须覆盖新组路由")

	// 组级定向重载（Change.Groups 映射面）同样进入 routing dirty。
	setGroup(m, 20, accWithEnabled(2, tpl, true, 10000), accWithEnabled(3, tpl, true, 10000))
	s.InvalidateGroup(20)
	require.Equal(t, 1, s.Stats().(SchedulerStats).CompilePending, "组级定向重载同样必须武装重编译")

	s.compileOnce()
	rd, ok := s.View().DecisionView().Routes()[RouteRefFor(20, string(domain.FormatOpenAIChat), "m")]
	require.True(t, ok)
	require.Len(t, allLaneIDs(rd), 2, "重编译后新账号进入计划")
}

// setGroup 替换 memLoader 组账号集（模拟管理面写库后的远端观察）。
func setGroup(m *memLoader, gid int64, accs ...*domain.Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byGroup[gid] = accs
}
