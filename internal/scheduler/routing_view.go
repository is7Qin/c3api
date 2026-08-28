// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sync"
	"sync/atomic"
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
	return s.groups
}
func (s *StaticView) ByID() map[int64]*accountSnapshot {
	if s == nil {
		return nil
	}
	return s.byID
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
}

type decisionLeaf struct {
	status        string // placeholder; actual runtime state lives in sharedRuntime
	weight        int
	cooldownUntil *string
}

func (d *DecisionView) Generation() uint64 { return d.generation }

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
	return v.static.groups
}
func (v *RoutingView) ByID() map[int64]*accountSnapshot {
	if v == nil || v.static == nil {
		return nil
	}
	return v.static.byID
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
