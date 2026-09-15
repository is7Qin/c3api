// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

// RoutingPlan is the read-only projection of the CURRENT published routing
// plan. One immutable RoutingView root is read exactly once; everything below
// is a defensive deep copy.
type RoutingPlan struct {
	Generation uint64
	Routes     []RoutingPlanRoute
}

// RoutingPlanRoute is one published route: full identity plus lane orders
// preserved exactly as compiled.
type RoutingPlanRoute struct {
	Ref        RouteRef
	Primary    []int64
	Explore    ExploreIDs
	Degraded   []int64
	Candidates []RoutingPlanCandidate
	// Incident is the expose-only mark projected from the route decision.
	Incident RouteIncident
}

// ExploreIDs is the ops-face projection of explore ordering (IDs only).
// ExploreBP (the steady-state exploration share in basis points) is
// deliberately NOT projected: it is an internal select-path tuning parameter,
// and exposing it would churn the openapi contract (RoutingPlanExplore +
// regen) and the console schema for no ops need.
type ExploreIDs struct {
	IDs        []int64
	Weights    map[int64]int
	Cumulative []uint64
	Total      uint64
	Fallback   []int64
}

// RoutingPlanCandidate is the static identity of one lane candidate.
type RoutingPlanCandidate struct {
	AccountID                int64
	TemplateID               int64
	LifecycleRevision        int64
	UpstreamCostMultiplierBp int
	Fingerprint              string
	IdentityFingerprint      string
	MappedModel              string
	QualityClassID           string
}

func (s *Scheduler) CurrentRoutingPlan() *RoutingPlan {
	plan := &RoutingPlan{Routes: []RoutingPlanRoute{}}
	if s == nil {
		return plan
	}
	v := s.view.Load()
	if v == nil {
		return plan
	}
	plan.Generation = v.generation
	if v.decision == nil || len(v.decision.routes) == 0 {
		return plan
	}
	refs := make([]RouteRef, 0, len(v.decision.routes))
	for k := range v.decision.routes {
		refs = append(refs, k)
	}
	sort.Slice(refs, func(i, j int) bool { return lessRouteRef(refs[i], refs[j]) })
	plan.Routes = make([]RoutingPlanRoute, 0, len(refs))
	for _, ref := range refs {
		rd := cloneRouteDecision(v.decision.routes[ref])
		// v4-S2: the table key stays normalized; the projected Ref carries
		// the interned per-route hex so the admin API bytes are unchanged.
		ref.RouteClassID = rd.RouteClassID
		route := RoutingPlanRoute{
			Ref: ref, Primary: compiledIDs(rd.Primary),
			Explore: ExploreIDs{
				IDs: compiledIDs(rd.Explore.Ordered), Weights: rd.Explore.Weights,
				Cumulative: append([]uint64(nil), rd.Explore.Cumulative...),
				Total:      rd.Explore.Total, Fallback: fallbackIDs(rd.Explore),
			},
			Degraded: compiledIDs(rd.Degraded),
			Incident: rd.Incident,
		}
		route.Candidates = routePlanCandidates(rd, v.static.facts)
		plan.Routes = append(plan.Routes, route)
	}
	return plan
}

func fallbackIDs(explore ExploreDecision) []int64 {
	if explore.Fallback == nil {
		return nil
	}
	out := make([]int64, 0, len(explore.Fallback))
	for _, idx := range explore.Fallback {
		if int(idx) < len(explore.Ordered) {
			out = append(out, explore.Ordered[idx].AccountID)
		}
	}
	return out
}

func compiledIDs(cs []CompiledCandidate) []int64 {
	if cs == nil {
		return nil
	}
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.AccountID
	}
	return out
}

func routePlanCandidates(rd *RouteDecision, facts map[int64]compilerAccountFacts) []RoutingPlanCandidate {
	byID := make(map[int64]CompiledCandidate, 8)
	for _, c := range rd.Primary {
		if _, ok := byID[c.AccountID]; !ok {
			byID[c.AccountID] = c
		}
	}
	for _, c := range rd.Explore.Ordered {
		if _, ok := byID[c.AccountID]; !ok {
			byID[c.AccountID] = c
		}
	}
	for _, c := range rd.Degraded {
		if _, ok := byID[c.AccountID]; !ok {
			byID[c.AccountID] = c
		}
	}
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]RoutingPlanCandidate, 0, len(ids))
	for _, id := range ids {
		c := byID[id]
		rpc := RoutingPlanCandidate{
			AccountID: id,
		}
		if fact, ok := facts[id]; ok && fact.account == c.Leaf && fact.static == c.Static {
			rpc.TemplateID = c.TemplateID
			rpc.LifecycleRevision = c.LifecycleRevision
			rpc.Fingerprint = c.Fingerprint
			rpc.MappedModel = c.MappedModel
			rpc.QualityClassID = c.Quality
			rpc.UpstreamCostMultiplierBp = fact.upstreamCostMultiplierBp
		}
		rpc.IdentityFingerprint = domain.CandidateFPHex(candidateIdentityFingerprint(c.Fingerprint, c.AccountID))
		out = append(out, rpc)
	}
	return out
}

func lessRouteRef(a, b RouteRef) bool {
	if a.GroupID != b.GroupID {
		return a.GroupID < b.GroupID
	}
	if a.Format != b.Format {
		return a.Format < b.Format
	}
	if a.Model != b.Model {
		return a.Model < b.Model
	}
	if a.OperationTag != b.OperationTag {
		return a.OperationTag < b.OperationTag
	}
	return a.RouteClassID < b.RouteClassID
}
