// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

// CandidateQualityInput is per-candidate window stats.
type CandidateQualityInput struct {
	Counts            Counts
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// CandidateQualityKey is canonical (RouteClassID, CandidateFingerprint).
type CandidateQualityKey struct {
	RouteClassID domain.RouteClassIDVal
	Fingerprint  domain.CandidateFingerprintVal
}

// LatchKey preserves account+fingerprint+revision identity.
type LatchKey struct {
	AccountID   int64
	Fingerprint string
	Revision    int64
}

// CompilerInputs is immutable deterministic inputs.
type CompilerInputs struct {
	Static  *StaticView
	Quality map[CandidateQualityKey]CandidateQualityInput
	Prices  map[string]domain.ResolvedPrices
	Health  map[HealthKey]HealthState
	Latched map[LatchKey]bool
}

// RoutingCompiler is stateless compiler.
type RoutingCompiler struct{}

func NewRoutingCompiler() *RoutingCompiler { return &RoutingCompiler{} }

// Compile produces immutable DecisionView.
func (c *RoutingCompiler) Compile(in CompilerInputs) (*DecisionView, error) {
	if in.Static == nil {
		return &DecisionView{routes: map[RouteRef]*RouteDecision{}}, nil
	}
	groupIDs := sortedGroupIDs(in.Static.groups)
	routes := make(map[RouteRef]*RouteDecision)
	for _, gid := range groupIDs {
		gs := in.Static.groups[gid]
		if gs == nil {
			continue
		}
		for _, rk := range sortedRouteKeys(gs.routes) {
			route := gs.routes[rk]
			if route == nil {
				continue
			}
			ops := operationTagsForFormat(rk.format)
			for _, op := range ops {
				rr := canonicalRouteRefWithOp(gid, rk, op)
				candidates := fullCandidateUnion(gs, rk)
				filtered := filterCandidates(candidates, in.Health, in.Latched, rk, op)
				if len(filtered) == 0 {
					routes[rr] = &RouteDecision{Explore: ExploreDecision{Weights: map[int64]int{}, Fallback: []int64{}}}
					continue
				}
				var rcVal domain.RouteClassIDVal
				if rr.RouteClassID != "" {
					if v, err := domain.HexToID(rr.RouteClassID); err == nil {
						rcVal = domain.RouteClassIDVal(v)
					}
				}
				dec := compileRouteDecision(filtered, rk, rcVal, in.Quality, in.Prices)
				routes[rr] = dec
			}
		}
	}
	return &DecisionView{routes: routes}, nil
}

func sortedGroupIDs(m map[int64]*groupSnapshot) []int64 {
	ids := make([]int64, 0, len(m))
	for gid := range m {
		ids = append(ids, gid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sortedRouteKeys(m map[routeKey]*route) []routeKey {
	keys := make([]routeKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if string(keys[i].format) != string(keys[j].format) {
			return string(keys[i].format) < string(keys[j].format)
		}
		return keys[i].model < keys[j].model
	})
	return keys
}

func canonicalRouteRef(gid int64, rk routeKey) RouteRef {
	op := operationTagForFormat(string(rk.format))
	return canonicalRouteRefWithOp(gid, rk, op)
}

func canonicalRouteRefWithOp(gid int64, rk routeKey, op domain.OperationTag) RouteRef {
	rc := ""
	if rf, ok := parseRequestFormat(string(rk.format)); ok && op != "" {
		if id, err := domain.RouteClassID(gid, rf, rk.model, op); err == nil {
			rc = domain.RouteClassIDHex(id)
		}
	}
	return RouteRef{GroupID: gid, Format: string(rk.format), Model: rk.model, OperationTag: string(op), RouteClassID: rc}
}

func operationTagsForFormat(f domain.RequestFormat) []domain.OperationTag {
	switch f {
	case domain.FormatOpenAIImages:
		return []domain.OperationTag{domain.OpImagesGenerations, domain.OpImagesEdits}
	default:
		if op := operationTagForFormat(string(f)); op != "" {
			return []domain.OperationTag{op}
		}
		return nil
	}
}
