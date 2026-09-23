// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"strconv"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// routing.go — intelligent-routing 观测面（admin API）：flow /
// frontier / plan 三个只读端点。纯转换/委托层：参数直进 service（窗口 ≤90d、
// route 目录校验、limit 钳制全在 service），错误走 httpface.WriteServiceErr
// 唯一映射表；空集合恒为 []（service 侧已保证非 nil，转换层再兜投影 nil 切片）。

// GetRoutingFlow 路由 flow 聚合 — ServerInterface。
func (h *AdminAPI) GetRoutingFlow(w http.ResponseWriter, r *http.Request, params GetRoutingFlowParams) {
	res, err := h.svc.QueryRoutingFlow(r.Context(), service.RoutingFlowQuery{
		RouteID:  params.Route,
		From:     params.From,
		To:       params.To,
		Offset:   deref(params.Offset),
		Limit:    deref(params.Limit),
		Accounts: deref(params.Accounts),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIRoutingFlow(res))
}

// GetRoutingFrontier 质量-成本前沿 — ServerInterface。limit 缺省/越界由
// service 归一钳制（≤0→20，>200→200），handler 不重复实现。
func (h *AdminAPI) GetRoutingFrontier(w http.ResponseWriter, r *http.Request, params GetRoutingFrontierParams) {
	res, err := h.svc.QueryRoutingFrontier(r.Context(), service.RoutingFrontierQuery{
		RouteID: params.Route,
		From:    params.From,
		To:      params.To,
		Limit:   deref(params.Limit),
		Offset:  deref(params.Offset),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIRoutingFrontier(res))
}

// GetRoutingPlan 当前发布计划解释 — ServerInterface（无历史 generation 查询
// 面；search/route/分页见契约）。分页与切片归一全在 service，handler 只做
// 「缺省指针 → 契约默认」的取值。
func (h *AdminAPI) GetRoutingPlan(w http.ResponseWriter, r *http.Request, params GetRoutingPlanParams) {
	res, err := h.svc.QueryRoutingPlan(service.RoutingPlanQuery{
		Search:           deref(params.Search),
		Route:            deref(params.Route),
		Offset:           deref(params.Offset),
		Limit:            deref(params.Limit),
		CandidatesOffset: deref(params.CandidatesOffset),
		CandidatesLimit:  planCandidatesLimit(params.CandidatesLimit),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIRoutingPlan(res))
}

// planCandidatesLimit candidates_limit 缺省 → 契约默认；显式 0 = 不返回候选
// （路由选择器取轻量列表）。两者语义不同，必须区分。
func planCandidatesLimit(v *int) int {
	if v == nil {
		return service.RoutingPlanCandidatesDefault
	}
	return *v
}

func toAPIRoutingFlow(res *service.RoutingFlowResult) RoutingFlowResponse {
	out := RoutingFlowResponse{
		RouteClassId:                 res.RouteClassID,
		PlanGeneration:               int64(res.PlanGeneration),
		Lanes:                        make([]RoutingFlowLane, 0, len(res.Lanes)),
		TotalEdges:                   res.TotalEdges,
		StaleGenerationPresent:       res.StaleGenerationPresent,
		Sankey:                       toAPIRoutingFlowGraph(res.Sankey),
		FirstDispatchChains:          res.FirstDispatchChains,
		TerminalChains:               res.TerminalChains,
		IncompleteChainDropped:       res.IncompleteChainDropped,
		FlowOverflowDroppedChains:    res.FlowOverflowDroppedChains,
		ProcessCrashLossUnobservable: res.ProcessCrashLossUnobservable,
	}
	for _, lane := range res.Lanes {
		apiLane := RoutingFlowLane{Ordinal: int(lane.Ordinal), Lane: lane.Lane, Edges: make([]RoutingFlowEdge, 0, len(lane.Edges))}
		for _, e := range lane.Edges {
			apiLane.Edges = append(apiLane.Edges, RoutingFlowEdge{
				Ordinal:           int(e.Ordinal),
				Lane:              e.Lane,
				AccountId:         e.AccountID,
				PreviousAccountId: e.PreviousAccountID,
				PreviousOutcome:   e.PreviousOutcome,
				TransitionReason:  e.TransitionReason,
				Outcome:           e.Outcome,
				IsTerminal:        e.IsTerminal,
				MinGeneration:     e.MinGeneration,
				ChainCount:        e.ChainCount,
			})
		}
		out.Lanes = append(out.Lanes, apiLane)
	}
	return out
}

// toAPIRoutingFlowGraph 桑基图边集投影（service → 契约类型；nil 集合兜 []）。
func toAPIRoutingFlowGraph(g service.RoutingFlowGraph) RoutingFlowGraph {
	out := RoutingFlowGraph{
		Edges:          make([]RoutingFlowGraphEdge, 0, len(g.Edges)),
		AccountLimit:   g.AccountLimit,
		FoldedAccounts: g.FoldedAccounts,
		Folded:         g.Folded,
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, RoutingFlowGraphEdge{
			Ordinal:           int(e.Ordinal),
			Lane:              e.Lane,
			AccountId:         e.AccountID,
			Folded:            e.Folded,
			Outcome:           e.Outcome,
			IsTerminal:        e.IsTerminal,
			ChainCount:        e.ChainCount,
			TransitionReasons: nonNilStrings(e.TransitionReasons),
			PreviousOutcomes:  nonNilStrings(e.PreviousOutcomes),
			PreviousAccounts:  nonNilIDs(e.PreviousAccounts),
		})
	}
	return out
}

func toAPIRoutingFrontier(res *service.RoutingFrontierResult) RoutingFrontierResponse {
	out := RoutingFrontierResponse{
		RouteClassId:    res.RouteClassID,
		PlanGeneration:  int64(res.PlanGeneration),
		Candidates:      make([]RoutingFrontierCandidate, 0, len(res.Candidates)),
		TotalCandidates: res.TotalCandidates,
	}
	for _, c := range res.Candidates {
		out.Candidates = append(out.Candidates, RoutingFrontierCandidate{
			CandidateFingerprint: c.CandidateFingerprint,
			Known:                c.Known,
			AccountId:            c.AccountID,
			TemplateId:           c.TemplateID,
			IdentityRevision:     c.IdentityRevision,
			QualityClassId:       c.QualityClassID,
			MappedModel:          c.MappedModel,
			Attempts:             c.Attempts,
			Successes:            c.Successes,
			SuccessLcb:           c.SuccessLCB,
			SuccessUcb:           c.SuccessUCB,
			TtftLcb:              c.TTFTLCB,
			TtftUcb:              c.TTFTUCB,
			TtftKnown:            c.TTFTKnown,
			CostPerSuccess:       c.CostPerSuccess,
			CostKnown:            c.CostKnown,
			Insufficient:         c.Insufficient,
			OnFrontier:           c.OnFrontier,
		})
	}
	return out
}

func toAPIRoutingPlan(res *service.RoutingPlanResult) RoutingPlanResponse {
	out := RoutingPlanResponse{
		Generation:  int64(res.Generation),
		Routes:      make([]RoutingPlanRoute, 0, len(res.Routes)),
		TotalRoutes: res.TotalRoutes,
	}
	for _, view := range res.Routes {
		rt := view.Route
		out.Routes = append(out.Routes, RoutingPlanRoute{
			Ref: RoutingPlanRef{
				GroupId:      rt.Ref.GroupID,
				Format:       rt.Ref.Format,
				Model:        rt.Ref.Model,
				OperationTag: rt.Ref.OperationTag,
				RouteClassId: rt.Ref.RouteClassID,
			},
			Primary:  nonNilIDs(rt.Primary),
			Degraded: nonNilIDs(rt.Degraded),
			Explore: RoutingPlanExplore{
				Ids:        nonNilIDs(rt.Explore.IDs),
				Weights:    exploreWeights(rt.Explore.Weights),
				Cumulative: nonNilCumulative(rt.Explore.Cumulative),
				Total:      int64(rt.Explore.Total),
				Fallback:   nonNilIDs(rt.Explore.Fallback),
			},
			Candidates:      make([]RoutingPlanCandidate, 0, len(rt.Candidates)),
			CandidatesTotal: view.CandidatesTotal,
			Incident: RoutingPlanIncident{
				Active:          rt.Incident.Active,
				Kind:            rt.Incident.Kind,
				Comparable:      rt.Incident.Comparable,
				Degraded:        rt.Incident.Degraded,
				Domains:         rt.Incident.Domains,
				EvaluatedMinute: rt.Incident.EvaluatedMinute,
			},
		})
		r := &out.Routes[len(out.Routes)-1]
		for _, c := range rt.Candidates {
			r.Candidates = append(r.Candidates, RoutingPlanCandidate{
				AccountId:                c.AccountID,
				TemplateId:               c.TemplateID,
				IdentityRevision:         c.IdentityRevision,
				UpstreamCostMultiplierBp: c.UpstreamCostMultiplierBp,
				Fingerprint:              c.Fingerprint,
				IdentityFingerprint:      c.IdentityFingerprint,
				MappedModel:              c.MappedModel,
				QualityClassId:           c.QualityClassID,
			})
		}
	}
	return out
}

func nonNilIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func nonNilCumulative(cum []uint64) []int64 {
	out := make([]int64, 0, len(cum))
	for _, v := range cum {
		out = append(out, int64(v))
	}
	return out
}

func exploreWeights(w map[int64]int) map[string]int {
	out := make(map[string]int, len(w))
	for id, v := range w {
		out[strconv.FormatInt(id, 10)] = v
	}
	return out
}
