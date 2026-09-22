// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"strconv"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
)

// routing.go — intelligent-routing 观测面（admin API）：flow /
// frontier / plan 三个只读端点。纯转换/委托层：参数直进 service（窗口 ≤90d、
// route 目录校验、limit 钳制全在 service），错误走 httpface.WriteServiceErr
// 唯一映射表；空集合恒为 []（service 侧已保证非 nil，转换层再兜投影 nil 切片）。

// GetRoutingFlow 路由 flow 聚合 — ServerInterface。
func (h *AdminAPI) GetRoutingFlow(w http.ResponseWriter, r *http.Request, params GetRoutingFlowParams) {
	res, err := h.svc.QueryRoutingFlow(r.Context(), service.RoutingFlowQuery{
		RouteID: params.Route,
		From:    params.From,
		To:      params.To,
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIRoutingFlow(res))
}

// GetRoutingFrontier 质量-成本前沿 — ServerInterface。limit 缺省/越界由
// service 归一钳制（≤0→200，>200→200），handler 不重复实现。
func (h *AdminAPI) GetRoutingFrontier(w http.ResponseWriter, r *http.Request, params GetRoutingFrontierParams) {
	res, err := h.svc.QueryRoutingFrontier(r.Context(), service.RoutingFrontierQuery{
		RouteID: params.Route,
		From:    params.From,
		To:      params.To,
		Limit:   deref(params.Limit),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIRoutingFrontier(res))
}

// GetRoutingPlan 当前发布计划解释 — ServerInterface（无参数：只有当前快照，
// 不提供历史 generation 查询面）。
func (h *AdminAPI) GetRoutingPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := h.svc.RoutingPlanExplanation()
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIRoutingPlan(plan))
}

func toAPIRoutingFlow(res *service.RoutingFlowResult) RoutingFlowResponse {
	out := RoutingFlowResponse{
		RouteClassId:                 res.RouteClassID,
		PlanGeneration:               int64(res.PlanGeneration),
		Lanes:                        make([]RoutingFlowLane, 0, len(res.Lanes)),
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
				Ordinal:              int(e.Ordinal),
				Lane:                 e.Lane,
				AccountId:            e.AccountID,
				PreviousAccountId:    e.PreviousAccountID,
				PreviousOutcome:      e.PreviousOutcome,
				TransitionReason:     e.TransitionReason,
				Outcome:              e.Outcome,
				IsTerminal:           e.IsTerminal,
				Generation:           e.Generation,
				CandidateFingerprint: e.CandidateFingerprint,
				ChainCount:           e.ChainCount,
			})
		}
		out.Lanes = append(out.Lanes, apiLane)
	}
	return out
}

func toAPIRoutingFrontier(res *service.RoutingFrontierResult) RoutingFrontierResponse {
	out := RoutingFrontierResponse{
		RouteClassId:   res.RouteClassID,
		PlanGeneration: int64(res.PlanGeneration),
		Candidates:     make([]RoutingFrontierCandidate, 0, len(res.Candidates)),
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

func toAPIRoutingPlan(plan *scheduler.RoutingPlan) RoutingPlanResponse {
	out := RoutingPlanResponse{Generation: int64(plan.Generation), Routes: make([]RoutingPlanRoute, 0, len(plan.Routes))}
	for _, rt := range plan.Routes {
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
			Candidates: make([]RoutingPlanCandidate, 0, len(rt.Candidates)),
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
