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

// LatchKey 已迁 internal/latch（根因重开：锁存一等组件；本包经
// Scheduler.latch 间接持有，不再自有类型）。

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
	// Baseline is the per-candidate PG-settled baseline (incidents only;
	// lanes read Quality). Nil = no baseline (all incident candidates abstain).
	Baseline map[CandidateQualityKey]Counts
	Prices   map[string]domain.ResolvedPrices
	// IncidentEval stamps one route's expose-only incident (nil = zero
	// incident; direct-compile tests). The compile lane always installs the
	// tracker closure so full and scoped fires share one transition function.
	IncidentEval IncidentEvalFunc
	// EvaluatedMinute is the provider's settled-boundary minute (unix) the
	// incident evidence cites. Zero when unwired.
	EvaluatedMinute int64
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
				// the table key stays normalized (no hex); the
				// decision value keeps rr.RouteClassID as the interned hex.
				key := normRouteRef(rr)
				dec, err := c.compileSingleRoute(in, gid, rk, op)
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
// shared per-route body behind both the full Compile loop and the
// scoped path, so scoped output equals the full-recompile oracle bit-for-bit
// by construction. Incident stamping rides the same shared body (full and
// scoped evaluate through the input's IncidentEval, frozen evaluations reuse
// stored lane state). Returns (nil, nil) when the route is gone from the
// static root (scoped caller drops it; the full loop skips it as before).
func (c *RoutingCompiler) compileSingleRoute(in CompilerInputs, gid int64, rk routeKey, op domain.OperationTag) (*RouteDecision, error) {
	static := in.Static
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
	var rcVal domain.RouteClassIDVal
	if rr.RouteClassID != "" {
		if v, err := domain.HexToID(rr.RouteClassID); err == nil {
			rcVal = domain.RouteClassIDVal(v)
		}
	}
	if len(filtered) == 0 {
		decision := &RouteDecision{Format: string(rk.format), RequestedModel: rk.model, RouteClassID: rr.RouteClassID, CallerCategory: string(callerKindForFormat(rk.format)), OperationTag: string(op)}
		if err := validateRouteDecision(decision); err != nil {
			return nil, err
		}
		decision.validated = true
		stampRouteIncident(decision, rr, nil, rcVal, in)
		if err := validateRouteIncident(decision.Incident); err != nil {
			return nil, err
		}
		return decision, nil
	}
	dec, err := compileRouteDecision(filtered, rk, rcVal, in.Quality, in.Prices, rr)
	if err != nil {
		return nil, err
	}
	stampRouteIncident(dec, rr, filtered, rcVal, in)
	if err := validateRouteIncident(dec.Incident); err != nil {
		return nil, err
	}
	return dec, nil
}

// stampRouteIncident resolves one route's pure incident vote and stamps it
// through the input evaluator (nil evaluator = zero incident). The lane state
// key is the normalized route ref, matching decision map keys and pruning.
func stampRouteIncident(decision *RouteDecision, rr RouteRef, facts []compilerCandidateFacts, rcVal domain.RouteClassIDVal, in CompilerInputs) {
	if decision == nil || in.IncidentEval == nil {
		return
	}
	vote := evaluateRouteIncident(facts, rcVal, in.Quality, in.Baseline)
	decision.Incident = in.IncidentEval(normRouteRef(rr), vote, in.EvaluatedMinute)
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
