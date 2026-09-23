// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// A10（service 面）：观测窗口守卫。窗口起点早于观测保留截止 → ErrInvalidInput
// （httpface 映射 **HTTP 400**，状态码断言在 handler 侧）；起点恰为 cutoff →
// 通过；超界 **整窗拒绝**，不静默截断（截断后的聚合看起来正常却少了整段分钟）。
// 两个端点（/routing/flow、/routing/frontier）共用同一守卫。
func TestRoutingWindowGuardRetentionCutoff(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	fs := newFakeStore()
	svc := routingSvc(t, fs, plan)

	// 注入固定时钟 + 观测保留天数（与 retention worker 同源的口径）。
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	svc.routingRetentionDays = 7
	svc.statsNow = func() time.Time { return fixed }
	cutoff := domain.RoutingObservationCutoff(fixed, 7)
	require.Equal(t, time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC), cutoff, "cutoff = now - observation_retention_days（日历日）")

	// 起点恰为 cutoff：放行（边界含）。
	ok := RoutingFlowQuery{RouteID: idHex, From: cutoff, To: cutoff.Add(time.Hour)}
	_, err := svc.QueryRoutingFlow(context.Background(), ok)
	require.NoError(t, err, "from == cutoff must be accepted (inclusive boundary)")

	// 起点早于 cutoff 一分钟：整窗拒绝。
	tooOld := RoutingFlowQuery{RouteID: idHex, From: cutoff.Add(-time.Minute), To: cutoff.Add(time.Hour)}
	_, err = svc.QueryRoutingFlow(context.Background(), tooOld)
	require.ErrorIs(t, err, ErrInvalidInput, "flow: from before the retention cutoff must be rejected")

	// frontier 同一守卫。
	_, err = svc.QueryRoutingFrontier(context.Background(), RoutingFrontierQuery{
		RouteID: idHex, From: cutoff.Add(-time.Hour), To: cutoff.Add(time.Hour),
	})
	require.ErrorIs(t, err, ErrInvalidInput, "frontier: from before the retention cutoff must be rejected")
	_, err = svc.QueryRoutingFrontier(context.Background(), RoutingFrontierQuery{
		RouteID: idHex, From: cutoff, To: cutoff.Add(time.Hour),
	})
	require.NoError(t, err, "frontier: from == cutoff must be accepted")

	// 未装配（0，测试/降级路径）→ 不设守卫（生产装配由 config 地板 + main 覆盖）。
	svc.routingRetentionDays = 0
	_, err = svc.QueryRoutingFlow(context.Background(), tooOld)
	require.NoError(t, err, "no retention wiring means no guard")
}
