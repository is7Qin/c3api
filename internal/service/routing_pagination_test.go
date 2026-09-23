// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 路由观测面的分页与桑基折叠：两条不变量必须被钉死——
//  1. 分页只影响边表/候选表的可见行，守恒计数、total 与 sankey 恒在完整集合上算。
//  2. 折叠只合并账号，层内 chain_count 求和不变（守恒不破）。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// --- buildFlowSankey: 每层 top-N + 「其他」折叠 ---

func TestBuildFlowSankey_PerLayerTopNAndOtherBucket(t *testing.T) {
	var edges []RoutingFlowEdge
	for i, cc := range []int64{50, 40, 30, 20, 10} {
		edges = append(edges, RoutingFlowEdge{
			Ordinal: 1, Lane: "primary", AccountID: int64(i + 1),
			Outcome: "success", IsTerminal: true, ChainCount: cc,
			TransitionReason: "initial",
		})
	}

	g := buildFlowSankey(edges, 2)
	require.Equal(t, 2, g.AccountLimit)
	require.True(t, g.Folded)
	require.Equal(t, int64(3), g.FoldedAccounts, "5 账号留 2 → 折叠 3")

	byAcct := make(map[int64]RoutingFlowGraphEdge, len(g.Edges))
	for _, e := range g.Edges {
		byAcct[e.AccountID] = e
	}
	require.Contains(t, byAcct, int64(1), "链数最多者保留")
	require.Contains(t, byAcct, int64(2))
	require.NotContains(t, byAcct, int64(3), "第 3 名起折叠")
	require.True(t, byAcct[0].Folded, "「其他」节点 account_id=0")
	require.Equal(t, int64(60), byAcct[0].ChainCount, "折叠桶 = 30+20+10")
	require.Empty(t, byAcct[0].PreviousAccounts, "折叠节点不收集前驱账号（会膨胀）")
	require.Equal(t, []string{"initial"}, byAcct[0].TransitionReasons, "小枚举照常收集")

	var sum int64
	for _, e := range g.Edges {
		sum += e.ChainCount
	}
	require.Equal(t, int64(150), sum, "层内链数和守恒（折叠不失真）")

	// limit ≥ 账号数 → 不折叠，账号全保留。
	full := buildFlowSankey(edges, 99)
	require.False(t, full.Folded)
	require.Zero(t, full.FoldedAccounts)
	require.Len(t, full.Edges, 5)
	require.Equal(t, int64(150), func() int64 {
		var s int64
		for _, e := range full.Edges {
			s += e.ChainCount
		}
		return s
	}())
}

func TestBuildFlowSankey_FoldIsPerLayer(t *testing.T) {
	// 两个层各自 3 个账号（同账号 id 在两层的权重不同）：limit=1 → 每层各留 1、
	// 各折叠 2 → 共折叠 4。折叠按 (ordinal, lane) 层独立进行。
	var edges []RoutingFlowEdge
	for _, layer := range []struct {
		ord  int16
		lane string
	}{{1, "primary"}, {2, "degraded"}} {
		for i, cc := range []int64{30, 20, 10} {
			edges = append(edges, RoutingFlowEdge{
				Ordinal: layer.ord, Lane: layer.lane, AccountID: int64(i + 1),
				Outcome: "success", IsTerminal: true, ChainCount: cc,
			})
		}
	}
	g := buildFlowSankey(edges, 1)
	require.Equal(t, int64(4), g.FoldedAccounts, "两层各折叠 2")

	perLayerKept := map[sankeyLayerKey]int{}
	perLayerOther := map[sankeyLayerKey]int64{}
	for _, e := range g.Edges {
		lk := sankeyLayerKey{e.Ordinal, e.Lane}
		if e.Folded {
			perLayerOther[lk] += e.ChainCount
			continue
		}
		perLayerKept[lk]++
	}
	require.Len(t, perLayerKept, 2, "两层各自保留")
	for lk, n := range perLayerKept {
		require.Equal(t, 1, n, "每层只留 1 个账号")
		require.Equal(t, int64(30), perLayerOther[lk], "每层折叠桶 = 20+10")
	}
}

func TestBuildFlowSankey_DeterministicAndTieBreak(t *testing.T) {
	// 同链数按账号升序决胜；输出顺序对同一输入字节级确定。
	edges := []RoutingFlowEdge{
		{Ordinal: 1, Lane: "primary", AccountID: 9, Outcome: "success", IsTerminal: true, ChainCount: 10},
		{Ordinal: 1, Lane: "primary", AccountID: 4, Outcome: "success", IsTerminal: true, ChainCount: 10},
		{Ordinal: 1, Lane: "primary", AccountID: 7, Outcome: "success", IsTerminal: true, ChainCount: 10},
	}
	a := buildFlowSankey(edges, 1)
	b := buildFlowSankey([]RoutingFlowEdge{edges[2], edges[0], edges[1]}, 1)
	require.Equal(t, a, b, "输入顺序不影响输出")

	kept := int64(-1)
	for _, e := range a.Edges {
		if !e.Folded {
			kept = e.AccountID
		}
	}
	require.Equal(t, int64(4), kept, "同链数 → 账号升序保留最小者")
	require.Equal(t, int64(2), a.FoldedAccounts)
}

// --- flow: 分页与守恒/桑基解耦 ---

func TestRoutingFlow_PaginationDecoupledFromTotalsAndSankey(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	fs := newFakeStore()
	rows := make([]repository.RoutingFlowStat, 0, 6)
	for i := 0; i < 6; i++ {
		rows = append(rows, repository.RoutingFlowStat{
			Ordinal: 1, Lane: "primary", AccountID: int64(i%3 + 1),
			Outcome: "success", IsTerminal: true, MinGeneration: 7,
			ChainCount: int64(i + 1),
		})
	}
	fs.routingFlowRows = rows
	svc := routingSvc(t, fs, plan)
	window := RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour)}

	full := window
	full.Limit = 100
	all, err := svc.QueryRoutingFlow(context.Background(), full)
	require.NoError(t, err)
	require.Equal(t, int64(6), all.TotalEdges)
	require.Len(t, all.Lanes[0].Edges, 6)

	page := window
	page.Limit, page.Offset = 2, 2
	res, err := svc.QueryRoutingFlow(context.Background(), page)
	require.NoError(t, err)

	var pageEdges int
	for _, l := range res.Lanes {
		pageEdges += len(l.Edges)
	}
	require.Equal(t, 2, pageEdges, "一页只含 limit 条边")

	require.Equal(t, int64(6), res.TotalEdges, "total_edges 不受分页影响")
	require.Equal(t, all.FirstDispatchChains, res.FirstDispatchChains, "守恒左端不受分页影响")
	require.Equal(t, all.TerminalChains, res.TerminalChains, "守恒右端不受分页影响")
	require.Equal(t, all.Sankey, res.Sankey, "sankey 在完整边集上折叠，不随翻页变化")

	// offset 越界 → 空页，但 total 与守恒仍在。
	over := window
	over.Limit, over.Offset = 10, 1000
	o, err := svc.QueryRoutingFlow(context.Background(), over)
	require.NoError(t, err)
	require.Empty(t, o.Lanes)
	require.Equal(t, int64(6), o.TotalEdges)
	require.Equal(t, all.FirstDispatchChains, o.FirstDispatchChains)
}

func TestRoutingFlow_AccountsParamFoldsGraphOnly(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	fs := newFakeStore()
	rows := make([]repository.RoutingFlowStat, 0, 3)
	for i := 0; i < 3; i++ {
		rows = append(rows, repository.RoutingFlowStat{
			Ordinal: 1, Lane: "primary", AccountID: int64(i + 1),
			Outcome: "success", IsTerminal: true, MinGeneration: 7,
			ChainCount: int64(30 - i*10),
		})
	}
	fs.routingFlowRows = rows
	svc := routingSvc(t, fs, plan)

	res, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{
		RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Accounts: 1,
	})
	require.NoError(t, err)
	require.True(t, res.Sankey.Folded)
	require.Equal(t, int64(2), res.Sankey.FoldedAccounts)
	require.Equal(t, 1, res.Sankey.AccountLimit)

	// 折叠只作用于 sankey：边表仍是完整的三条（此处未分页）。
	require.Len(t, res.Lanes[0].Edges, 3, "边表不受 accounts 参数影响")
	require.Equal(t, int64(3), res.TotalEdges)
}

// --- plan: 搜索 + 路由分页 + 候选分页 ---

func multiRoutePlan(t *testing.T, n int) *scheduler.RoutingPlan {
	t.Helper()
	routes := make([]scheduler.RoutingPlanRoute, 0, n)
	for i := 0; i < n; i++ {
		rc, err := domain.RouteClassID(int64(i+1), domain.FormatOpenAIChat, fmt.Sprintf("m%d", i), domain.OpChatCompletions)
		require.NoError(t, err)
		cands := make([]scheduler.RoutingPlanCandidate, 0, 5)
		for c := 0; c < 5; c++ {
			cands = append(cands, scheduler.RoutingPlanCandidate{
				AccountID:           int64(c + 1),
				IdentityFingerprint: fpHex(byte(c + 1)),
			})
		}
		routes = append(routes, scheduler.RoutingPlanRoute{
			Ref: scheduler.RouteRef{
				GroupID: int64(i + 1), Format: string(domain.FormatOpenAIChat),
				Model: fmt.Sprintf("m%d", i), OperationTag: string(domain.OpChatCompletions),
				RouteClassID: domain.RouteClassIDHex(rc),
			},
			Candidates: cands,
		})
	}
	return &scheduler.RoutingPlan{Generation: 9, Routes: routes}
}

func TestQueryRoutingPlan_SearchPaginationAndCandidates(t *testing.T) {
	plan := multiRoutePlan(t, 6)
	svc := routingSvc(t, newFakeStore(), plan)

	// 路由分页：total_routes 为过滤后总数，不受分页影响。
	page, err := svc.QueryRoutingPlan(RoutingPlanQuery{Limit: 2, CandidatesLimit: 1})
	require.NoError(t, err)
	require.Equal(t, uint64(9), page.Generation)
	require.Equal(t, int64(6), page.TotalRoutes)
	require.Len(t, page.Routes, 2)
	require.Equal(t, "m0", page.Routes[0].Route.Ref.Model)

	second, err := svc.QueryRoutingPlan(RoutingPlanQuery{Limit: 2, Offset: 2, CandidatesLimit: 1})
	require.NoError(t, err)
	require.Len(t, second.Routes, 2)
	require.Equal(t, "m2", second.Routes[0].Route.Ref.Model, "offset 在过滤后的列表上切片")

	// 搜索在切片前生效。
	found, err := svc.QueryRoutingPlan(RoutingPlanQuery{Search: "m3", CandidatesLimit: 1})
	require.NoError(t, err)
	require.Equal(t, int64(1), found.TotalRoutes)
	require.Len(t, found.Routes, 1)
	require.Equal(t, "m3", found.Routes[0].Route.Ref.Model)

	require.Zero(t, func() int64 {
		none, err := svc.QueryRoutingPlan(RoutingPlanQuery{Search: "zzz-no-such-model"})
		require.NoError(t, err)
		require.Empty(t, none.Routes)
		return none.TotalRoutes
	}(), "无命中 → 空列表 + total 0")

	// 候选分页：candidates_total 为完整候选数，page 只含一页。
	one, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: plan.Routes[3].Ref.RouteClassID, CandidatesLimit: 2})
	require.NoError(t, err)
	require.Len(t, one.Routes, 1)
	require.Equal(t, int64(5), one.Routes[0].CandidatesTotal)
	require.Len(t, one.Routes[0].Route.Candidates, 2)

	candPage, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: plan.Routes[3].Ref.RouteClassID, CandidatesLimit: 2, CandidatesOffset: 4})
	require.NoError(t, err)
	require.Len(t, candPage.Routes[0].Route.Candidates, 1, "尾页只剩 1 条")
	require.Equal(t, int64(5), candPage.Routes[0].CandidatesTotal)

	// candidates_limit=0 = 不返回候选（路由选择器取轻量列表），但 total 仍在。
	light, err := svc.QueryRoutingPlan(RoutingPlanQuery{Limit: 6, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Len(t, light.Routes, 6)
	for _, r := range light.Routes {
		require.Empty(t, r.Route.Candidates)
		require.Equal(t, int64(5), r.CandidatesTotal)
	}

	// 路由校验：非法 hex → ErrInvalidInput；合法但不在目录 → ErrNotFound。
	_, err = svc.QueryRoutingPlan(RoutingPlanQuery{Route: "nothex"})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.QueryRoutingPlan(RoutingPlanQuery{Route: fpHex(0xff)})
	require.ErrorIs(t, err, ErrNotFound)
}

// --- flow: 完整集口径（A1/A3，B 期重基线为布尔）---
//
// fixture 含旧代际行：plan generation 为 7，min_generation=6 的行即"窗口内含
// 非当前计划代际的链"。B 期退役链次占比（stale_chains/total_chains），改出精确
// 布尔 stale_generation_present——行级谓词，无归属误差。A3 的 total_edges 子句
// （行单位）保留。

func staleFixtureRows(t *testing.T) []repository.RoutingFlowStat {
	t.Helper()
	rows := []repository.RoutingFlowStat{
		// min_gen-7 当前代：ordinal1 首发两行（acct1 5 链、acct2 3 链）+ ordinal2 terminal。
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, MinGeneration: 7, ChainCount: 5},
		{Ordinal: 1, Lane: "primary", AccountID: 2, Outcome: "5xx", IsTerminal: false, MinGeneration: 7, ChainCount: 3},
		{Ordinal: 2, Lane: "degraded", AccountID: 2, PreviousAccountID: ptrInt64(2), PreviousOutcome: "5xx", TransitionReason: "failover", Outcome: "success", IsTerminal: true, MinGeneration: 7, ChainCount: 3},
		// min_gen-6 旧代际行：首发 + terminal 各一。
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, MinGeneration: 6, ChainCount: 2},
		{Ordinal: 2, Lane: "degraded", AccountID: 3, PreviousAccountID: ptrInt64(1), PreviousOutcome: "success", TransitionReason: "drain", Outcome: "success", IsTerminal: true, MinGeneration: 6, ChainCount: 4},
	}
	return rows
}

// flatEdges 把 lanes 按原分组序展平（页内 order-segment 比对用）。
func flatEdges(lanes []RoutingFlowLane) []RoutingFlowEdge {
	var out []RoutingFlowEdge
	for _, l := range lanes {
		out = append(out, l.Edges...)
	}
	return out
}

func TestRoutingFlow_CompleteSetTotalsAndStaleBoolean(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	fs := newFakeStore()
	fs.routingFlowRows = staleFixtureRows(t)
	svc := routingSvc(t, fs, plan)
	full, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Limit: 200})
	require.NoError(t, err)

	// A3（total_edges 子句保留）：完整行数，与分页无关。
	require.Equal(t, int64(5), full.TotalEdges)
	// A15/A1：混代际 fixture（gen-6 行 + gen-7 行）→ 布尔为真；断言**布尔值**
	// 而非链数——链数在该行不可精确拆分（这正是退役占比的原因）。
	require.True(t, full.StaleGenerationPresent, "前置：fixture 须含旧代际行，否则 A1 空洞成立")

	// A1：两页的 sankey/total_edges/布尔恒等（完整集上算，与页无关）。
	p1, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Limit: 2, Offset: 0})
	require.NoError(t, err)
	p2, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, full.Sankey, p1.Sankey)
	require.Equal(t, full.Sankey, p2.Sankey)
	require.Equal(t, full.TotalEdges, p1.TotalEdges)
	require.Equal(t, full.TotalEdges, p2.TotalEdges)
	require.Equal(t, full.StaleGenerationPresent, p1.StaleGenerationPresent)
	require.Equal(t, full.StaleGenerationPresent, p2.StaleGenerationPresent)
	require.Equal(t, full.FirstDispatchChains, p1.FirstDispatchChains)
	require.Equal(t, full.TerminalChains, p2.TerminalChains)

	// A15 负方向：仅当前代际（plan generation 7）→ 布尔为假。
	fs.routingFlowRows = []repository.RoutingFlowStat{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, MinGeneration: 7, ChainCount: 5},
		{Ordinal: 2, Lane: "degraded", AccountID: 2, PreviousAccountID: ptrInt64(1), PreviousOutcome: "success", TransitionReason: "retry", Outcome: "success", IsTerminal: true, MinGeneration: 7, ChainCount: 3},
	}
	cur, err := svc.QueryRoutingFlow(context.Background(), RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Limit: 200})
	require.NoError(t, err)
	require.False(t, cur.StaleGenerationPresent, "current-generation-only rows must not read as stale")
}

// --- flow: lanes 切片正确性（A24）---

func TestRoutingFlow_LanesSliceMatchesCompleteOrder(t *testing.T) {
	plan, idHex, _ := routingFixturePlan()
	fs := newFakeStore()
	fs.routingFlowRows = staleFixtureRows(t)
	svc := routingSvc(t, fs, plan)
	ctx := context.Background()
	base := RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour)}

	full, err := svc.QueryRoutingFlow(ctx, RoutingFlowQuery{RouteID: idHex, From: routingBase, To: routingBase.Add(time.Hour), Limit: 200})
	require.NoError(t, err)
	all := flatEdges(full.Lanes)
	require.Len(t, all, 5)

	q := base
	q.Limit, q.Offset = 2, 1
	page, err := svc.QueryRoutingFlow(ctx, q)
	require.NoError(t, err)
	got := flatEdges(page.Lanes)
	require.Len(t, got, 2)
	require.Equal(t, all[1:3], got, "页内 offset/limit 取出完整序的同一段")

	q2 := base
	q2.Limit, q2.Offset = 2, 3
	page2, err := svc.QueryRoutingFlow(ctx, q2)
	require.NoError(t, err)
	got2 := flatEdges(page2.Lanes)
	require.Len(t, got2, 2)
	require.Equal(t, all[3:5], got2)
	require.NotEqual(t, got, got2, "不同的页内 offset 不得返回同一批行")
}

// --- sankey: 折叠边细节（A6 补全：全空 reason 用例）---

func TestBuildFlowSankey_FoldedEdgeSets(t *testing.T) {
	edges := []RoutingFlowEdge{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, ChainCount: 50, TransitionReason: "initial"},
		{Ordinal: 1, Lane: "primary", AccountID: 2, Outcome: "success", IsTerminal: true, ChainCount: 30, TransitionReason: "failover"},
		{Ordinal: 1, Lane: "primary", AccountID: 3, Outcome: "success", IsTerminal: true, ChainCount: 10, TransitionReason: "failover"},
	}
	g := buildFlowSankey(edges, 1)
	require.True(t, g.Folded)
	require.Equal(t, int64(2), g.FoldedAccounts)
	var folded *RoutingFlowGraphEdge
	for i := range g.Edges {
		if g.Edges[i].Folded {
			folded = &g.Edges[i]
		}
	}
	require.NotNil(t, folded)
	require.Empty(t, folded.PreviousAccounts, "折叠节点 previous_accounts 恒为空")
	require.Equal(t, []string{"failover"}, folded.TransitionReasons, "reason 并集（acct2/acct3 均为 failover）")

	// 全空 reason 的合法输入 → 空集（不得断言非空）。
	emptyReason := []RoutingFlowEdge{
		{Ordinal: 1, Lane: "primary", AccountID: 1, Outcome: "success", IsTerminal: true, ChainCount: 50},
		{Ordinal: 1, Lane: "primary", AccountID: 2, Outcome: "success", IsTerminal: true, ChainCount: 30},
	}
	g2 := buildFlowSankey(emptyReason, 1)
	require.True(t, g2.Folded)
	var folded2 *RoutingFlowGraphEdge
	for i := range g2.Edges {
		if g2.Edges[i].Folded {
			folded2 = &g2.Edges[i]
		}
	}
	require.NotNil(t, folded2)
	require.Empty(t, folded2.TransitionReasons, "全空 reason → 空集")
	require.Empty(t, folded2.PreviousAccounts)
}

// --- plan: 搜索四字段 + 大小写不敏感（A9）---

func searchPlan() *scheduler.RoutingPlan {
	mk := func(group int64, format, model, tag string) scheduler.RoutingPlanRoute {
		rc, err := domain.RouteClassID(group, domain.RequestFormat(format), model, domain.OperationTag(tag))
		if err != nil {
			panic(err)
		}
		return scheduler.RoutingPlanRoute{
			Ref: scheduler.RouteRef{GroupID: group, Format: format, Model: model, OperationTag: tag, RouteClassID: domain.RouteClassIDHex(rc)},
		}
	}
	return &scheduler.RoutingPlan{Generation: 3, Routes: []scheduler.RoutingPlanRoute{
		mk(101, "openai-chat", "AlphaModel", "chat_completions"),
		mk(202, "anthropic", "beta", "anthropic_messages"),
		mk(303, "openai-responses", "gamma", "responses"),
	}}
}

func TestQueryRoutingPlan_SearchAllFields(t *testing.T) {
	svc := routingSvc(t, newFakeStore(), searchPlan())
	search := func(term string) *RoutingPlanResult {
		t.Helper()
		res, err := svc.QueryRoutingPlan(RoutingPlanQuery{Search: term, CandidatesLimit: 0})
		require.NoError(t, err)
		return res
	}

	byModel := search("alphamodel")
	require.Equal(t, int64(1), byModel.TotalRoutes)
	require.Len(t, byModel.Routes, 1)
	require.Equal(t, "AlphaModel", byModel.Routes[0].Route.Ref.Model)

	byFormat := search("ANTHROPIC")
	require.Equal(t, int64(1), byFormat.TotalRoutes, "format 大小写不敏感")
	require.Equal(t, int64(202), byFormat.Routes[0].Route.Ref.GroupID)

	byTag := search("Responses")
	require.Equal(t, int64(1), byTag.TotalRoutes, "operation_tag 大小写不敏感")

	byGroup := search("303")
	require.Equal(t, int64(1), byGroup.TotalRoutes, "group_id 十进制串")
	require.Equal(t, "gamma", byGroup.Routes[0].Route.Ref.Model)

	all := search("")
	require.Equal(t, int64(3), all.TotalRoutes, "空 search = 不过滤")

	none := search("zzz-no-such")
	require.Equal(t, int64(0), none.TotalRoutes)
	require.Empty(t, none.Routes)
}

// --- plan: candidates_limit 三态 + 封顶（A10）---

func TestQueryRoutingPlan_CandidatesLimitStates(t *testing.T) {
	plan := multiRoutePlan(t, 2)
	svc := routingSvc(t, newFakeStore(), plan)

	// 服务层默认：RoutingPlanCandidatesDefault（handler 负责把缺席的 nil 解析为
	// 该默认；服务层 0 的语义是"不返回候选"，不是"缺席"）。
	absent, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: plan.Routes[0].Ref.RouteClassID, CandidatesLimit: RoutingPlanCandidatesDefault})
	require.NoError(t, err)
	require.Len(t, absent.Routes[0].Route.Candidates, 5)
	require.Equal(t, int64(5), absent.Routes[0].CandidatesTotal)

	// 显式 0 → 空数组，但 candidates_total 仍完整。
	zero, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: plan.Routes[0].Ref.RouteClassID, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Empty(t, zero.Routes[0].Route.Candidates)
	require.Equal(t, int64(5), zero.Routes[0].CandidatesTotal)

	// 负数 → 默认 20。
	neg, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: plan.Routes[0].Ref.RouteClassID, CandidatesLimit: -1})
	require.NoError(t, err)
	require.Len(t, neg.Routes[0].Route.Candidates, 5)

	// >200 → 200（此处造 250 候选的路由验证封顶）。
	big := multiRoutePlan(t, 1)
	big.Routes[0].Candidates = make([]scheduler.RoutingPlanCandidate, 250)
	for i := range big.Routes[0].Candidates {
		big.Routes[0].Candidates[i] = scheduler.RoutingPlanCandidate{AccountID: int64(i + 1), IdentityFingerprint: fpHex(byte(i))}
	}
	bigSvc := routingSvc(t, newFakeStore(), big)
	capped, err := bigSvc.QueryRoutingPlan(RoutingPlanQuery{CandidatesLimit: 500})
	require.NoError(t, err)
	require.Len(t, capped.Routes[0].Route.Candidates, 200, ">200 钳到 200")
	require.Equal(t, int64(250), capped.Routes[0].CandidatesTotal)
}

// --- plan: route 优先于 search（A11）---

func TestQueryRoutingPlan_RouteBeatsSearch(t *testing.T) {
	plan := multiRoutePlan(t, 4)
	svc := routingSvc(t, newFakeStore(), plan)
	target := plan.Routes[2].Ref.RouteClassID

	res, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: target, Search: "m0", CandidatesLimit: 0})
	require.NoError(t, err)
	require.Equal(t, int64(1), res.TotalRoutes, "route 给定时 search 被忽略")
	require.Len(t, res.Routes, 1)
	require.Equal(t, target, res.Routes[0].Route.Ref.RouteClassID)
}

// --- plan: 空计划（A14）---

func TestQueryRoutingPlan_EmptyPlan(t *testing.T) {
	svc := routingSvc(t, newFakeStore(), &scheduler.RoutingPlan{Routes: []scheduler.RoutingPlanRoute{}})
	res, err := svc.QueryRoutingPlan(RoutingPlanQuery{CandidatesLimit: 0})
	require.NoError(t, err)
	require.Equal(t, uint64(0), res.Generation)
	require.Equal(t, int64(0), res.TotalRoutes)
	require.Empty(t, res.Routes)

	lo, hi := normalizePage(0, 20, 20, 200, 0)
	require.Equal(t, 0, lo)
	require.Equal(t, 0, hi)
}

// --- plan: routes 与 candidates 双轴切片（A25）---

func TestQueryRoutingPlan_TwoAxisSlicing(t *testing.T) {
	plan := multiRoutePlan(t, 6)
	svc := routingSvc(t, newFakeStore(), plan)

	// routes 轴：offset 页 == 过滤后全量序同一段。
	page, err := svc.QueryRoutingPlan(RoutingPlanQuery{Limit: 2, Offset: 2, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Equal(t, int64(6), page.TotalRoutes)
	require.Len(t, page.Routes, 2)
	require.Equal(t, "m2", page.Routes[0].Route.Ref.Model)
	require.Equal(t, "m3", page.Routes[1].Route.Ref.Model)

	page2, err := svc.QueryRoutingPlan(RoutingPlanQuery{Limit: 2, Offset: 4, CandidatesLimit: 0})
	require.NoError(t, err)
	require.Len(t, page2.Routes, 2)
	require.Equal(t, "m4", page2.Routes[0].Route.Ref.Model)
	require.NotEqual(t, page.Routes[0].Route.Ref.RouteClassID, page2.Routes[0].Route.Ref.RouteClassID)

	// candidates 轴：candidates_offset 页 == 该路由候选全量序同一段。
	target := plan.Routes[1].Ref.RouteClassID
	c1, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: target, CandidatesLimit: 2, CandidatesOffset: 1})
	require.NoError(t, err)
	require.Len(t, c1.Routes[0].Route.Candidates, 2)
	require.Equal(t, int64(2), c1.Routes[0].Route.Candidates[0].AccountID)
	require.Equal(t, int64(3), c1.Routes[0].Route.Candidates[1].AccountID)
	require.Equal(t, int64(5), c1.Routes[0].CandidatesTotal)

	c2, err := svc.QueryRoutingPlan(RoutingPlanQuery{Route: target, CandidatesLimit: 2, CandidatesOffset: 3})
	require.NoError(t, err)
	require.Len(t, c2.Routes[0].Route.Candidates, 2)
	require.Equal(t, int64(4), c2.Routes[0].Route.Candidates[0].AccountID)
}
