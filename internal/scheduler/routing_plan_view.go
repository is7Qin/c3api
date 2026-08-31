// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"encoding/binary"
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

// RoutingPlan is the read-only projection of the CURRENT published routing
// plan (Todo 17 explanation lane). One immutable RoutingView root is read
// exactly once; everything below is a defensive deep copy — callers may hold,
// sort and serialize it freely, published views never alias into it.
// No request hashing, no reservation, no recompile, no dynamic filtering:
// the projection reports what IS published, not what would be published.
type RoutingPlan struct {
	Generation uint64
	Routes     []RoutingPlanRoute // deterministic order (lessRouteRef)
}

// RoutingPlanRoute is one published route: full identity plus lane candidate
// orders preserved exactly as compiled (primary/explore/degraded order is
// semantic, never re-sorted here).
type RoutingPlanRoute struct {
	Ref        RouteRef
	Primary    []int64
	Explore    ExploreDecision // defensive copy (IDs order + weights + cumulative)
	Degraded   []int64
	Candidates []RoutingPlanCandidate // union of lane accounts, ascending AccountID
}

// RoutingPlanCandidate is the static identity of one lane candidate:
// template/account/revision metadata, fingerprint (real one, "" when
// underivable), the rollup-join identity fingerprint, mapped model for this
// route's requested model and the derived quality class.
type RoutingPlanCandidate struct {
	AccountID                int64
	TemplateID               int64
	LifecycleRevision        int64
	UpstreamCostMultiplierBp int
	Fingerprint              string // real candidate fingerprint hex ("" if underivable)
	IdentityFingerprint      string // rollup join identity hex (compiler synthesis rule)
	MappedModel              string
	QualityClassID           string // hex quality class for (format, mapped model, op)
}

// CurrentRoutingPlan projects the currently published RoutingView root into
// an immutable snapshot. Nil scheduler / unpublished view yields an empty
// plan (Generation 0, non-nil empty Routes) — never nil, so callers cannot
// confuse "no view yet" with a stale generation.
func (s *Scheduler) CurrentRoutingPlan() *RoutingPlan {
	plan := &RoutingPlan{Routes: []RoutingPlanRoute{}}
	if s == nil {
		return plan
	}
	v := s.view.Load() // single read of the immutable root
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
		route := RoutingPlanRoute{
			Ref:      ref,
			Primary:  rd.Primary,
			Explore:  rd.Explore,
			Degraded: rd.Degraded,
		}
		route.Candidates = routePlanCandidates(v, ref)
		plan.Routes = append(plan.Routes, route)
	}
	return plan
}

// planCandidates builds the defensive candidate metadata for the union of
// lane accounts (primary + explore IDs + degraded), ascending AccountID.
// Accounts missing from the static leaves (decision rebased over a removal)
// still appear with identity-only fields — the projection never drops a
// published candidate reference.
func routePlanCandidates(v *RoutingView, ref RouteRef) []RoutingPlanCandidate {
	seen := make(map[int64]bool, len(v.decision.routes[ref].Primary))
	ids := make([]int64, 0, 16)
	collect := func(lane []int64) {
		for _, id := range lane {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	rd := v.decision.routes[ref]
	collect(rd.Primary)
	collect(rd.Explore.IDs)
	collect(rd.Degraded)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]RoutingPlanCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, routePlanCandidate(v, ref, id))
	}
	return out
}

func routePlanCandidate(v *RoutingView, ref RouteRef, id int64) RoutingPlanCandidate {
	c := RoutingPlanCandidate{AccountID: id}
	acc := domain.Account{ID: id} // missing leaf still joins on the ID-synthesis identity
	var tpl *domain.Template
	if v.static != nil {
		if snap, ok := v.static.byID[id]; ok && snap != nil {
			if av := snap.static.Load(); av != nil {
				acc = av.acc
				tpl = av.tpl
			}
		}
	}
	c.TemplateID = acc.TemplateID
	c.LifecycleRevision = acc.LifecycleRevision
	c.UpstreamCostMultiplierBp = acc.UpstreamCostMultiplierBp
	if fp, err := CandidateFingerprint(&acc); err == nil {
		c.Fingerprint = fp
	}
	c.IdentityFingerprint = domain.CandidateFPHex(candidateIdentityFingerprint(&acc))
	c.MappedModel = resolveMappedModel(tpl, ref.Model)
	c.QualityClassID = qualityClassHexForWithOp(domain.RequestFormat(ref.Format), c.MappedModel, domain.OperationTag(ref.OperationTag))
	return c
}

// resolveMappedModel applies the template mapping for the requested model
// (same rule as the compiler health-key derivation; empty requested model =
// default bucket has no mapping).
func resolveMappedModel(tpl *domain.Template, requested string) string {
	if tpl == nil || requested == "" {
		return requested
	}
	if e, ok := tpl.ModelMapping[requested]; ok {
		return e.MappedModel
	}
	return requested
}

// candidateIdentityFingerprint is the canonical rollup-join identity: the real
// candidate fingerprint when derivable, else big-endian account ID bytes —
// the single synthesis rule shared by the compiler quality-key lookup and the
// RoutingPlan projection.
func candidateIdentityFingerprint(a *domain.Account) domain.CandidateFingerprintVal {
	if fp, err := CandidateFingerprint(a); err == nil && fp != "" {
		if v, err := domain.HexToID(fp); err == nil {
			return domain.CandidateFingerprintVal(v)
		}
	}
	var b [32]byte
	binary.BigEndian.PutUint64(b[:8], uint64(a.ID))
	return domain.CandidateFingerprintVal(b)
}

// lessRouteRef is the total order over full route identity (shared by the
// publish byte guard and the plan projection route ordering).
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
