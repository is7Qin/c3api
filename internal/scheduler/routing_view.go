// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"maps"
	"sync"

	"github.com/is7qin/c3api/internal/domain"
)

// cloneSnapMap 浅拷贝快照 map（语义与原各手写循环逐字一致：恒返回非 nil
// map，nil 输入得空 map；调用方依赖非 nil——见 routing_compiler_extra_test.go:90
// 的 NotNil 断言，故不用 maps.Clone 其 nil→nil 语义）。
func cloneSnapMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// StaticView holds immutable DB-derived state: groups and byID leaves.
// Published leaves never mutate; changed account gets new immutable leaf
// sharing separate runtime/concurrency state, old root stable.
type StaticView struct {
	generation uint64
	groups     map[int64]*groupSnapshot
	byID       map[int64]*accountSnapshot
	facts      map[int64]compilerAccountFacts
	// routeIndex maps events to affected routes. Born at stage with
	// the root, dies with it, read-only between (compile-lane-owned).
	routeIndex *compileRouteIndex
}

func (s *StaticView) Generation() uint64 { return s.generation }
func (s *StaticView) Groups() map[int64]*groupSnapshot {
	if s == nil {
		return nil
	}
	return cloneSnapMap(s.groups)
}
func (s *StaticView) ByID() map[int64]*accountSnapshot {
	if s == nil {
		return nil
	}
	return cloneSnapMap(s.byID)
}

func (s *StaticView) Account(id int64) (*accountSnapshot, bool) {
	if s == nil {
		return nil, false
	}
	a, ok := s.byID[id]
	return a, ok
}

// DecisionView holds immutable decision state derived from compiler/runtime.
type DecisionView struct {
	generation uint64
	routes     map[RouteRef]*RouteDecision
	// whole marks view-WHOLENESS (v5 scope-loss fix, §5.2): true ONLY when
	// every route was recomputed by a full-compile/full-fallback fire; a
	// scoped-carry publish marks false (partial) by conservative default —
	// even a scoped fire that recomputed everything stays partial unless it
	// proves full coverage (proof rule: affected ⊇ the target index's full
	// route set; the lane does not attempt the proof). Orthogonal to
	// atomicity: publish stays one-generation matched-pair atomic either
	// way. Owner: compile lane (publisher pair). Lifecycle: born at publish,
	// immutable afterwards (read lock-free by the backstop tick).
	whole bool
}

// RouteRef identifies a compiled route: group + format + model + operation + canonical RouteClassID.
type RouteRef struct {
	GroupID      int64
	Format       string
	Model        string
	OperationTag string
	RouteClassID string
}

// RouteDecision is the immutable published route: compiled candidates own all
// request-independent metadata; the request path only carries a cursor.
type RouteDecision struct {
	Format              string
	RequestedModel      string
	RouteClassID        string
	CallerCategory      string
	OperationTag        string
	Primary             []CompiledCandidate
	Degraded            []CompiledCandidate
	Explore             ExploreDecision
	CacheDomainRing     CacheDomainRing
	CacheDomainAccounts []CacheDomainAccount
	// Incident is the expose-only mark (detect + surface: never reorders
	// lanes, never throttles, never probes). Zero = no incident.
	Incident RouteIncident
	// validated is set only by the compiler. Defensive clones intentionally
	// clear it so callers cannot mutate a clone past the plan boundary.
	validated bool
}

// ExploreDecision holds deterministic explore ordering over compiled candidates.
type ExploreDecision struct {
	Ordered    []CompiledCandidate
	Weights    map[int64]int
	Cumulative []uint64
	Total      uint64
	// Fallback indexes Ordered. The compiler rejects explore tables larger than
	// uint16, so every index is in range for the immutable canonical table.
	Fallback []uint16
	// ExploreBP is the compiled steady-state exploration share in basis
	// points (0..10000) from ExploreBP(eligible, unknown, primaryCount):
	// 10000 when no Primary serves, 100 when nothing is unknown, capped at
	// 500 otherwise. The select path serves the explore sample first with
	// probability ExploreBP/10000 (canonical lane hash), primary-first
	// otherwise. Zero (hand-built/test decisions) means primary-first.
	ExploreBP int
}

func (d *DecisionView) Generation() uint64 { return d.generation }

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

func (d *DecisionView) Route(groupID int64, format string, model string) (*RouteDecision, bool) {
	if d == nil || d.routes == nil {
		return nil, false
	}
	// Single canonical accessor: published keys always carry the
	// format-derived OperationTag (compiler/publish normalize), so a raw
	// empty-op probe could only hit keys that can never be published.
	v, ok := d.routes[RouteRefFor(groupID, format, model)]
	if !ok {
		return nil, false
	}
	return cloneRouteDecision(v), true
}

func cloneRouteDecision(in *RouteDecision) *RouteDecision {
	if in == nil {
		return nil
	}
	out := &RouteDecision{
		Format:         in.Format,
		RequestedModel: in.RequestedModel, RouteClassID: in.RouteClassID,
		CallerCategory: in.CallerCategory, OperationTag: in.OperationTag,
		Primary: cloneCompiled(in.Primary), Degraded: cloneCompiled(in.Degraded),
		Explore:             cloneExploreDecision(in.Explore),
		CacheDomainRing:     cloneCacheDomainRing(in.CacheDomainRing),
		CacheDomainAccounts: append([]CacheDomainAccount(nil), in.CacheDomainAccounts...),
		Incident:            in.Incident,
		validated:           false,
	}
	return out
}

func cloneCacheDomainRing(in CacheDomainRing) CacheDomainRing {
	out := CacheDomainRing{}
	if in.Nodes != nil {
		out.Nodes = append([]CacheDomainNode(nil), in.Nodes...)
	}
	if in.Domains != nil {
		out.Domains = append([]string(nil), in.Domains...)
	}
	return out
}

func cloneExploreDecision(in ExploreDecision) ExploreDecision {
	out := ExploreDecision{Total: in.Total, Ordered: cloneCompiled(in.Ordered), ExploreBP: in.ExploreBP}
	if in.Fallback != nil {
		out.Fallback = append([]uint16(nil), in.Fallback...)
	}
	if in.Weights != nil {
		out.Weights = maps.Clone(in.Weights)
	}
	if in.Cumulative != nil {
		out.Cumulative = append([]uint64(nil), in.Cumulative...)
	}
	return out
}

// key-normalized route lookup — the key carries NO RouteClassID hex.
// The hex lived here as a per-request recompute (sha256 + hex encode on every
// select); the steady-state path only borrows the interned per-route hex from
// the found RouteDecision. Publish sites birth the intern once per route via
// routeClassHex; map keys on both sides stay normalized.
func RouteRefFor(groupID int64, format string, model string) RouteRef {
	return RouteRef{GroupID: groupID, Format: format, Model: model, OperationTag: string(operationTagForFormat(format))}
}

func RouteRefForOp(groupID int64, format string, model string, op domain.OperationTag) RouteRef {
	return RouteRef{GroupID: groupID, Format: format, Model: model, OperationTag: string(op)}
}

// normRouteRef zeroes RouteClassID before map access: the single
// normalization spelling shared by query and publish sites, so a key carrying
// a stale hex can never miss the normalized table.
func normRouteRef(r RouteRef) RouteRef {
	r.RouteClassID = ""
	return r
}

// routeClassHex births the interned per-route hex ONCE per published route
// : steady-state selection borrows RouteDecision.RouteClassID and
// never computes.
func routeClassHex(groupID int64, format string, model string, op domain.OperationTag) string {
	rf, ok := parseRequestFormat(format)
	if !ok || op == "" || !op.Valid() {
		return ""
	}
	id, err := domain.RouteClassID(groupID, rf, model, op)
	if err != nil {
		return ""
	}
	return domain.RouteClassIDHex(id)
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
type RoutingView struct {
	generation uint64
	static     *StaticView
	decision   *DecisionView
}

func (v *RoutingView) Generation() uint64          { return v.generation }
func (v *RoutingView) StaticView() *StaticView     { return v.static }
func (v *RoutingView) DecisionView() *DecisionView { return v.decision }
func (v *RoutingView) Groups() map[int64]*groupSnapshot {
	if v == nil || v.static == nil {
		return nil
	}
	return cloneSnapMap(v.static.groups)
}
func (v *RoutingView) ByID() map[int64]*accountSnapshot {
	if v == nil || v.static == nil {
		return nil
	}
	return cloneSnapMap(v.static.byID)
}

// byIDReadOnly / groupsReadOnly 返回内部不可变 map 本身（零拷贝，只读）。
// 架构不变量：已发布静态视图从不原地写（copy-modify-Store；叶子 runtime 计数
// 走原子量），因此包内热路径（per-tick 账号并发同步、per-request 模型列表）
// 可安全只读遍历/单键查。公共 ByID()/Groups() 保留浅拷贝契约（防包外误改，
// red 测试钉住）；此处刻意不返回给包外调用方。
func (v *RoutingView) byIDReadOnly() map[int64]*accountSnapshot {
	if v == nil || v.static == nil {
		return nil
	}
	return v.static.byID
}

func (v *RoutingView) groupsReadOnly() map[int64]*groupSnapshot {
	if v == nil || v.static == nil {
		return nil
	}
	return v.static.groups
}

func (v *RoutingView) Account(id int64) (*accountSnapshot, bool) {
	if v == nil || v.static == nil {
		return nil, false
	}
	return v.static.Account(id)
}

type routingPublisher struct {
	mu    sync.Mutex
	sched *Scheduler
	// pending is the staged static root awaiting a paired compile+publish.
	// Control plane only (reload/InvalidateGroup stage, the serial compile
	// lane consumes); guarded by mu. Nil means the published view is current.
	pending *StaticView
}

func newRoutingPublisher(s *Scheduler) *routingPublisher { return &routingPublisher{sched: s} }

// stageLocked records sv as the staged root awaiting a paired compile. The
// published pair is left untouched when complete; otherwise the static root
// is published alone (nil decision, fail-closed) so static faces stay warm
// until the first paired publish. Caller must hold p.mu.
func (p *routingPublisher) stageLocked(sv *StaticView) {
	p.pending = sv
	cur := p.sched.view.Load()
	if cur == nil || cur.static == nil || cur.decision == nil {
		p.publishInitialStaticLocked(sv)
	}
}

// publishInitialStaticLocked publishes a static-only view (nil decision,
// fail-closed) for initial startup so static faces stay warm until the first
// paired publish. The root must be unpublished; its generation is assigned
// once here and never mutated afterwards. Caller must hold p.mu.
func (p *routingPublisher) publishInitialStaticLocked(sv *StaticView) {
	var gen uint64
	if cur := p.sched.view.Load(); cur != nil {
		gen = cur.generation + 1
	} else {
		gen = 1
	}
	sv.generation = gen
	nv := &RoutingView{generation: gen, static: sv, decision: nil}
	p.sched.view.Store(nv)
	p.sched.gen.Store(gen)
}

// publishPairLocked publishes a matched static+decision pair under one fresh
// generation shared by both roots and the view, then clears the staged root.
// A published root is never mutated: when the staged root is already the
// published static-only root, a fresh wrapper sharing the same immutable
// maps is published instead (leaf pointers identical), otherwise the
// unpublished staged root gets its generation assigned once. Returns the
// published static root for byte-cache identity. Caller must hold p.mu with
// pending == staticView (verified by the compile lane before publishing).
func (p *routingPublisher) publishPairLocked(staticView *StaticView, decisionView *DecisionView) *StaticView {
	cur := p.sched.view.Load()
	var gen uint64
	if cur != nil {
		gen = cur.generation + 1
	} else {
		gen = 1
	}
	if cur != nil && cur.static == staticView {
		staticView = &StaticView{generation: gen, groups: staticView.groups, byID: staticView.byID, facts: staticView.facts, routeIndex: staticView.routeIndex}
	} else {
		staticView.generation = gen
	}
	if decisionView != nil {
		decisionView.generation = gen
	}
	nv := &RoutingView{generation: gen, static: staticView, decision: decisionView}
	p.sched.view.Store(nv)
	p.sched.gen.Store(gen)
	p.pending = nil
	return staticView
}

func (p *routingPublisher) publishWithBase(baseGen uint64, baseStatic *StaticView, build func(cur *RoutingView) *DecisionView) bool {
	p.mu.Lock()
	cur := p.sched.view.Load()
	if cur == nil || cur.static == nil || cur.generation != baseGen || cur.static != baseStatic {
		p.mu.Unlock()
		p.sched.RequestCompile()
		return false
	}
	defer p.mu.Unlock()
	newDec := build(cur)
	// Decision-only refresh on the same root: the fresh decision gets the new
	// generation once; the published static root is never touched.
	gen := cur.generation + 1
	newDec.generation = gen
	nv := &RoutingView{generation: gen, static: cur.static, decision: newDec}
	p.sched.view.Store(nv)
	p.sched.gen.Store(gen)
	return true
}

func (s *Scheduler) PublishDecisionForTest(route RouteRef, decision *RouteDecision) {
	// the publish key stays normalized; the intern is born per route below.
	route = normRouteRef(route)
	// Flush any staged static root through the compile lane first so the
	// test decision pairs with the freshest static root.
	s.publisher.mu.Lock()
	staged := s.publisher.pending != nil
	s.publisher.mu.Unlock()
	if staged {
		s.compileOnce()
	}
	base := s.View()
	if base == nil {
		return
	}
	s.publisher.publishWithBase(base.Generation(), base.StaticView(), func(cur *RoutingView) *DecisionView {
		var routes map[RouteRef]*RouteDecision
		if cur != nil && cur.decision != nil {
			routes = cloneSnapMap(cur.decision.routes)
		} else {
			routes = make(map[RouteRef]*RouteDecision)
		}
		if decision == nil {
			delete(routes, route)
		} else {
			prepared := cloneRouteDecision(decision)
			fill := func(in []CompiledCandidate, lane AttemptLane) []CompiledCandidate {
				out := make([]CompiledCandidate, 0, len(in))
				for _, candidate := range in {
					if account, ok := cur.static.byID[candidate.AccountID]; ok && account != nil {
						facts := buildCandidateFacts([]*accountSnapshot{account}, cur.static.facts, routeKey{format: domain.RequestFormat(route.Format), model: route.Model}, domain.OperationTag(route.OperationTag))
						out = append(out, compileCandidate(facts[0], lane))
					} else {
						candidate.Lane = lane
						out = append(out, candidate)
					}
				}
				return out
			}
			prepared.Format = route.Format
			prepared.RequestedModel = route.Model
			// birth the interned hex once per published route; the query
			// key stays normalized (RouteClassID "").
			prepared.RouteClassID = routeClassHex(route.GroupID, route.Format, route.Model, domain.OperationTag(route.OperationTag))
			prepared.CallerCategory = string(callerKindForFormat(domain.RequestFormat(route.Format)))
			prepared.OperationTag = route.OperationTag
			prepared.Primary = fill(prepared.Primary, AttemptLanePrimary)
			prepared.Explore.Ordered = fill(prepared.Explore.Ordered, AttemptLaneExplore)
			prepared.Degraded = fill(prepared.Degraded, AttemptLaneDegraded)
			routes[route] = prepared
		}
		return &DecisionView{routes: routes}
	})
}
