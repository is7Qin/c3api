// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sync"
	"sync/atomic"

	"github.com/is7qin/c3api/internal/domain"
)

// StaticView holds immutable DB-derived state: groups and byID leaves.
// Published leaves never mutate; changed account gets new immutable leaf
// sharing separate runtime/concurrency state, old root stable.
type StaticView struct {
	generation uint64
	groups     map[int64]*groupSnapshot
	byID       map[int64]*accountSnapshot
}

func (s *StaticView) Generation() uint64 { return s.generation }
func (s *StaticView) Groups() map[int64]*groupSnapshot {
	if s == nil {
		return nil
	}
	out := make(map[int64]*groupSnapshot, len(s.groups))
	for k, v := range s.groups {
		out[k] = v
	}
	return out
}
func (s *StaticView) ByID() map[int64]*accountSnapshot {
	if s == nil {
		return nil
	}
	out := make(map[int64]*accountSnapshot, len(s.byID))
	for k, v := range s.byID {
		out[k] = v
	}
	return out
}

// DecisionView holds immutable decision state derived from compiler/runtime.
// Static updates retain latest DecisionView; decision submits rebase onto
// latest StaticView and replace only decision.
type DecisionView struct {
	generation uint64
	// decisions holds per-account decision snapshot (status/weight/cooldown)
	// For legacy builder, decisions are reflected via runtimeState but
	// DecisionView generation still tracks publish order.
	decisions map[int64]*decisionLeaf
	// routes holds per-route compiled decisions. Immutable after publish.
	routes map[RouteRef]*RouteDecision
}

type decisionLeaf struct {
	status        string // placeholder; actual runtime state lives in sharedRuntime
	weight        int
	cooldownUntil *string
}

// RouteRef identifies a compiled route including full route identity: group + format + model + operation + canonical RouteClassID.
// OperationTag and RouteClassID prevent collisions between Responses/WS/images/search sharing format/model.
type RouteRef struct {
	GroupID      int64
	Format       string
	Model        string
	OperationTag string
	RouteClassID string // hex of domain.RouteClassIDVal
}

// RouteDecision holds immutable per-route lane classification.
type RouteDecision struct {
	Primary  []int64
	Degraded []int64
	Explore  ExploreDecision
}

// ExploreDecision holds deterministic explore ordering.
type ExploreDecision struct {
	IDs        []int64
	Weights    map[int64]int
	Cumulative []uint64
	Total      uint64
	Fallback   []int64
}

func (d *DecisionView) Generation() uint64 { return d.generation }

// Routes returns immutable per-route decisions (nil if none). Deep copy of map and decisions.
func (d *DecisionView) Routes() map[RouteRef]*RouteDecision {
	if d == nil {
		return nil
	}
	out := make(map[RouteRef]*RouteDecision, len(d.routes))
	for k, v := range d.routes {
		out[k] = cloneRouteDecision(v)
	}
	return out
}

// Route returns decision for a specific route (compat: computes canonical identity when OperationTag/RouteClassID empty).
func (d *DecisionView) Route(groupID int64, format string, model string) (*RouteDecision, bool) {
	if d == nil || d.routes == nil {
		return nil, false
	}
	v, ok := d.routes[RouteRef{GroupID: groupID, Format: format, Model: model}]
	if ok {
		return cloneRouteDecision(v), true
	}
	rr := RouteRefFor(groupID, format, model)
	v, ok = d.routes[rr]
	if !ok {
		return nil, false
	}
	return cloneRouteDecision(v), true
}

func cloneRouteDecision(in *RouteDecision) *RouteDecision {
	if in == nil {
		return nil
	}
	out := &RouteDecision{Explore: cloneExploreDecision(in.Explore)}
	if in.Primary != nil {
		out.Primary = append([]int64(nil), in.Primary...)
	}
	if in.Degraded != nil {
		out.Degraded = append([]int64(nil), in.Degraded...)
	}
	return out
}

func cloneExploreDecision(in ExploreDecision) ExploreDecision {
	out := ExploreDecision{Total: in.Total}
	if in.IDs != nil {
		out.IDs = append([]int64(nil), in.IDs...)
	}
	if in.Weights != nil {
		out.Weights = make(map[int64]int, len(in.Weights))
		for k, v := range in.Weights {
			out.Weights[k] = v
		}
	}
	if in.Cumulative != nil {
		out.Cumulative = append([]uint64(nil), in.Cumulative...)
	}
	if in.Fallback != nil {
		out.Fallback = append([]int64(nil), in.Fallback...)
	}
	return out
}

// RouteRefFor builds canonical RouteRef for group/format/model (fails closed on invalid -> empty RouteClassID).
func RouteRefFor(groupID int64, format string, model string) RouteRef {
	op := operationTagForFormat(format)
	rc := ""
	if rf, ok := parseRequestFormat(format); ok && op != "" {
		if id, err := domain.RouteClassID(groupID, rf, model, op); err == nil {
			rc = domain.RouteClassIDHex(id)
		}
	}
	return RouteRef{GroupID: groupID, Format: format, Model: model, OperationTag: string(op), RouteClassID: rc}
}

// RouteRefForOp builds canonical RouteRef for group/format/model with explicit operation tag (images edits/generations separation).
func RouteRefForOp(groupID int64, format string, model string, op domain.OperationTag) RouteRef {
	rc := ""
	if rf, ok := parseRequestFormat(format); ok && op != "" && op.Valid() {
		if id, err := domain.RouteClassID(groupID, rf, model, op); err == nil {
			rc = domain.RouteClassIDHex(id)
		}
	}
	return RouteRef{GroupID: groupID, Format: format, Model: model, OperationTag: string(op), RouteClassID: rc}
}

func parseRequestFormat(s string) (domain.RequestFormat, bool) {
	rf := domain.RequestFormat(s)
	if !rf.Valid() {
		return "", false
	}
	return rf, true
}

func operationTagForFormat(format string) domain.OperationTag {
	switch domain.RequestFormat(format) {
	case domain.FormatOpenAIChat:
		return domain.OpChatCompletions
	case domain.FormatOpenAIResponses:
		return domain.OpResponses
	case domain.FormatOpenAIResponsesWS:
		return domain.OpResponsesWS
	case domain.FormatAnthropic:
		return domain.OpAnthropicMessages
	case domain.FormatOpenAIImages:
		return domain.OpImagesGenerations
	case domain.FormatOpenAISearch:
		return domain.OpSearch
	default:
		return ""
	}
}

// RoutingView explicitly holds immutable *StaticView and *DecisionView.
// Single atomic root; structurally shared.
type RoutingView struct {
	generation uint64
	static     *StaticView
	decision   *DecisionView
}

func (v *RoutingView) Generation() uint64 { return v.generation }
func (v *RoutingView) StaticView() *StaticView { return v.static }
func (v *RoutingView) DecisionView() *DecisionView { return v.decision }
func (v *RoutingView) Groups() map[int64]*groupSnapshot {
	if v == nil || v.static == nil {
		return nil
	}
	out := make(map[int64]*groupSnapshot, len(v.static.groups))
	for k, vv := range v.static.groups {
		out[k] = vv
	}
	return out
}
func (v *RoutingView) ByID() map[int64]*accountSnapshot {
	if v == nil || v.static == nil {
		return nil
	}
	out := make(map[int64]*accountSnapshot, len(v.static.byID))
	for k, vv := range v.static.byID {
		out[k] = vv
	}
	return out
}

type routingPublisher struct {
	mu    sync.Mutex
	sched *Scheduler
}

func newRoutingPublisher(s *Scheduler) *routingPublisher {
	return &routingPublisher{sched: s}
}

func (p *routingPublisher) storeLocked(staticView *StaticView, decisionView *DecisionView) {
	var gen uint64
	if cur := p.sched.view.Load(); cur != nil {
		gen = cur.generation + 1
	} else {
		gen = 1
	}
	// Ensure generation monotonic for sub-views.
	if staticView != nil && staticView.generation == 0 {
		staticView.generation = gen
	}
	if decisionView != nil && decisionView.generation == 0 {
		decisionView.generation = gen
	}
	nv := &RoutingView{generation: gen, static: staticView, decision: decisionView}
	p.sched.view.Store(nv)
	p.sched.gen.Store(gen)
}

func (p *routingPublisher) publishFull(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.sched.view.Load()
	var dec *DecisionView
	if cur != nil {
		dec = cur.decision
	}
	sv := &StaticView{groups: groups, byID: byID}
	p.storeLocked(sv, dec)
}

func (p *routingPublisher) publishStaticWithDecisionRetention(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.sched.view.Load()
	var dec *DecisionView
	if cur != nil {
		dec = cur.decision
	}
	sv := &StaticView{groups: groups, byID: byID}
	p.storeLocked(sv, dec)
}

func (p *routingPublisher) publishWithBase(baseGen uint64, build func(cur *RoutingView) *DecisionView) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.sched.view.Load()
	var curGen uint64
	if cur != nil {
		curGen = cur.generation
	}
	_ = curGen
	_ = baseGen
	// Rebase: always build decision from latest static, not stale base's static.
	// If caller captured stale RoutingView, we ignore its static and use cur.static.
	newDec := build(cur)
	var sv *StaticView
	if cur != nil {
		sv = cur.static
	}
	p.storeLocked(sv, newDec)
}

var _ = atomic.Pointer[RoutingView]{}
