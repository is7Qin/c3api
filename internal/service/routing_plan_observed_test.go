// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// service lane：/routing/plan 的 observed 窗口过滤。计划内路由 ∩ 观察集合，
// search 再叠加；route 绕过窗口；plan_total_routes 与 total_routes 语义分离；
// 形状校验（恰给一端 → 400）先于 route 分支；planTotal==0 短路不查 reader。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

func observedID(t *testing.T, hexStr string) domain.RouteClassIDVal {
	t.Helper()
	raw, err := domain.HexToID(hexStr)
	require.NoError(t, err)
	return domain.RouteClassIDVal(raw)
}

func TestQueryRoutingPlan_ObservedWindowFiltersAndIntersectsSearch(t *testing.T) {
	plan := multiRoutePlan(t, 4) // m0..m3
	fs := newFakeStore()
	fs.routingObservedRows = []domain.RouteClassIDVal{
		observedID(t, plan.Routes[1].Ref.RouteClassID),
		observedID(t, plan.Routes[3].Ref.RouteClassID),
	}
	svc := routingSvc(t, fs, plan)
	from, to := routingBase, routingBase.Add(time.Hour)

	res, err := svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{ObservedFrom: &from, ObservedTo: &to, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Equal(t, int64(4), res.PlanTotalRoutes, "plan_total_routes = 未过滤总数")
	require.Equal(t, int64(2), res.TotalRoutes, "total_routes = 窗口内有流量的过滤后数")
	require.Len(t, res.Routes, 2)
	require.Equal(t, "m1", res.Routes[0].Route.Ref.Model)
	require.Equal(t, "m3", res.Routes[1].Route.Ref.Model)

	// search 与窗口取交集。
	filtered, err := svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{Search: "m3", ObservedFrom: &from, ObservedTo: &to, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Equal(t, int64(4), filtered.PlanTotalRoutes)
	require.Equal(t, int64(1), filtered.TotalRoutes)
	require.Equal(t, "m3", filtered.Routes[0].Route.Ref.Model)

	// search 无命中：plan_total 仍 > 0，total 0。
	none, err := svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{Search: "zzz", ObservedFrom: &from, ObservedTo: &to, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Equal(t, int64(4), none.PlanTotalRoutes)
	require.Zero(t, none.TotalRoutes)
	require.Empty(t, none.Routes)
}

func TestQueryRoutingPlan_ObservedWindowRouteBypassesFilter(t *testing.T) {
	plan := multiRoutePlan(t, 4)
	fs := newFakeStore()
	fs.routingObservedRows = []domain.RouteClassIDVal{observedID(t, plan.Routes[3].Ref.RouteClassID)}
	svc := routingSvc(t, fs, plan)
	from, to := routingBase, routingBase.Add(time.Hour)

	// 目标路由不在观察集合内：route 直查仍返回该条（绕过窗口），total 恒 1。
	target := plan.Routes[0].Ref.RouteClassID
	res, err := svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{Route: target, ObservedFrom: &from, ObservedTo: &to, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Equal(t, int64(4), res.PlanTotalRoutes)
	require.Equal(t, int64(1), res.TotalRoutes)
	require.Len(t, res.Routes, 1)
	require.Equal(t, target, res.Routes[0].Route.Ref.RouteClassID)
	require.Zero(t, fs.routingObservedCalls, "route 分支绕过 reader 查询")
}

func TestQueryRoutingPlan_ObservedWindowEmptyPlanShortCircuits(t *testing.T) {
	plan := &scheduler.RoutingPlan{Generation: 1} // 空计划
	fs := newFakeStore()
	fs.routingObservedRows = []domain.RouteClassIDVal{observedID(t, fpHex(0x11))}
	svc := routingSvc(t, fs, plan)
	from, to := routingBase, routingBase.Add(time.Hour)

	res, err := svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{ObservedFrom: &from, ObservedTo: &to, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Zero(t, res.PlanTotalRoutes)
	require.Zero(t, res.TotalRoutes)
	require.Empty(t, res.Routes)
	require.Zero(t, fs.routingObservedCalls, "空计划必无窗口内流量：短路不查 reader")
}

func TestQueryRoutingPlan_ObservedWindowShapeValidation(t *testing.T) {
	plan := multiRoutePlan(t, 2)
	svc := routingSvc(t, newFakeStore(), plan)
	ctx := context.Background()
	from, to := routingBase, routingBase.Add(time.Hour)

	// 恰给一端（含 route + 半窗口组合）→ ErrInvalidInput，先于 route 分支。
	_, err := svc.QueryRoutingPlan(ctx, RoutingPlanQuery{ObservedFrom: &from})
	require.ErrorIs(t, err, ErrInvalidInput, "from 单独给出")
	_, err = svc.QueryRoutingPlan(ctx, RoutingPlanQuery{ObservedTo: &to})
	require.ErrorIs(t, err, ErrInvalidInput, "to 单独给出")
	_, err = svc.QueryRoutingPlan(ctx, RoutingPlanQuery{Route: plan.Routes[0].Ref.RouteClassID, ObservedFrom: &from})
	require.ErrorIs(t, err, ErrInvalidInput, "route + 半窗口同样 400")

	// 窗口非法：to<=from / >90d。
	same := routingBase
	_, err = svc.QueryRoutingPlan(ctx, RoutingPlanQuery{ObservedFrom: &same, ObservedTo: &same})
	require.ErrorIs(t, err, ErrInvalidInput, "to<=from")
	tooWide := routingBase.Add(90*24*time.Hour + time.Nanosecond)
	_, err = svc.QueryRoutingPlan(ctx, RoutingPlanQuery{ObservedFrom: &same, ObservedTo: &tooWide})
	require.ErrorIs(t, err, ErrInvalidInput, ">90d")

	// 恰为 90d 放行（边界含）。
	exact := routingBase.Add(90 * 24 * time.Hour)
	_, err = svc.QueryRoutingPlan(ctx, RoutingPlanQuery{ObservedFrom: &same, ObservedTo: &exact})
	require.NoError(t, err, "exactly 90d is allowed")
}

func TestQueryRoutingPlan_ObservedWindowRetentionCutoff(t *testing.T) {
	plan := multiRoutePlan(t, 2)
	svc := routingSvc(t, newFakeStore(), plan)
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	svc.routingRetentionDays = 7
	svc.statsNow = func() time.Time { return fixed }
	cutoff := domain.RoutingObservationCutoff(fixed, 7)

	// 起点恰为 cutoff：放行（边界含）。
	okFrom, okTo := cutoff, cutoff.Add(time.Hour)
	_, err := svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{ObservedFrom: &okFrom, ObservedTo: &okTo})
	require.NoError(t, err, "from == cutoff must be accepted")

	// 起点早于 cutoff 一分钟：整窗拒绝。
	badFrom, badTo := cutoff.Add(-time.Minute), cutoff.Add(time.Hour)
	_, err = svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{ObservedFrom: &badFrom, ObservedTo: &badTo})
	require.ErrorIs(t, err, ErrInvalidInput, "起点早于保留 cutoff → 400")
}

func TestQueryRoutingPlan_ObservedWindowNotWired(t *testing.T) {
	plan := multiRoutePlan(t, 2)
	svc := New(Deps{Store: &fakeStoreNoFacts{}, Scheduler: &fakeRoutingSched{plan: plan}, Invalidate: NopInvalidator{}, Publisher: nil, RuleReload: nil, Auth: nil, Log: nil, EmailCodeStore: testEmailCodes})
	from, to := routingBase, routingBase.Add(time.Hour)
	_, err := svc.QueryRoutingPlan(context.Background(), RoutingPlanQuery{ObservedFrom: &from, ObservedTo: &to})
	require.ErrorIs(t, err, errRoutingNotWired)
}
