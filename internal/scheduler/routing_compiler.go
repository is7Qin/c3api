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
//
// v5-§5.1A (COMPILED-HEALTH-FREE): Health/Latched fields are DELETED —
// compiled-health snapshots invalidated in-flight plans via generation churn
// while serving never needed them (reserveOnView owns latch + EffectiveState
// + StatusDisabled per attempt, strictly fresher). Positive trigger rule =
// static + price + quality ONLY.
type CompilerInputs struct {
	Static  *StaticView
	Quality map[CandidateQualityKey]CandidateQualityInput
	Prices  map[string]domain.ResolvedPrices
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
				// v4-S2: the table key stays normalized (no hex); the
				// decision value keeps rr.RouteClassID as the interned hex.
				key := normRouteRef(rr)
				dec, err := c.compileSingleRoute(in.Static, gid, rk, op, in.Quality, in.Prices)
				if err != nil {
					return nil, err
				}
				if dec == nil {
					continue
				}
				routes[key] = dec
			}
		}
	}
	return &DecisionView{routes: routes}, nil
}

// compileSingleRoute compiles exactly one route (group × routeKey × op) — the
// shared per-route body behind both the full Compile loop and the v5-C2
// scoped path, so scoped output equals the full-recompile oracle bit-for-bit
// by construction. Returns (nil, nil) when the route is gone from the static
// root (scoped caller drops it; the full loop skips it as before).
func (c *RoutingCompiler) compileSingleRoute(static *StaticView, gid int64, rk routeKey, op domain.OperationTag, quality map[CandidateQualityKey]CandidateQualityInput, prices map[string]domain.ResolvedPrices) (*RouteDecision, error) {
	gs := static.groups[gid]
	if gs == nil {
		return nil, nil
	}
	route := gs.routes[rk]
	if route == nil {
		return nil, nil
	}
	rr := canonicalRouteRefWithOp(gid, rk, op)
	candidates := fullCandidateUnion(gs, static.facts, rk)
	facts := buildCandidateFacts(candidates, static.facts, rk, op)
	filtered := filterCandidates(facts)
	if len(filtered) == 0 {
		decision := &RouteDecision{Format: string(rk.format), RequestedModel: rk.model, RouteClassID: rr.RouteClassID, CallerCategory: string(callerKindForFormat(rk.format)), OperationTag: string(op)}
		if err := validateRouteDecision(decision); err != nil {
			return nil, err
		}
		decision.validated = true
		return decision, nil
	}
	var rcVal domain.RouteClassIDVal
	if rr.RouteClassID != "" {
		if v, err := domain.HexToID(rr.RouteClassID); err == nil {
			rcVal = domain.RouteClassIDVal(v)
		}
	}
	return compileRouteDecision(filtered, rk, rcVal, quality, prices, rr)
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
