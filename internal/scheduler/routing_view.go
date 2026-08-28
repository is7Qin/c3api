// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sync"
	"sync/atomic"
)

type RoutingView struct {
	generation uint64
	groups     map[int64]*groupSnapshot
	byID       map[int64]*accountSnapshot
}

func (v *RoutingView) Generation() uint64 { return v.generation }
func (v *RoutingView) Groups() map[int64]*groupSnapshot {
	if v == nil {
		return nil
	}
	return v.groups
}
func (v *RoutingView) ByID() map[int64]*accountSnapshot {
	if v == nil {
		return nil
	}
	return v.byID
}

type routingPublisher struct {
	mu    sync.Mutex
	sched *Scheduler
}

func newRoutingPublisher(s *Scheduler) *routingPublisher {
	return &routingPublisher{sched: s}
}

func (p *routingPublisher) storeLocked(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) {
	var gen uint64
	if cur := p.sched.view.Load(); cur != nil {
		gen = cur.generation + 1
	} else {
		gen = 1
	}
	nv := &RoutingView{generation: gen, groups: groups, byID: byID}
	p.sched.view.Store(nv)
	p.sched.gen.Store(gen)
}

func (p *routingPublisher) publishFull(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storeLocked(groups, byID)
}

func (p *routingPublisher) publishWithBase(baseGen uint64, build func() (map[int64]*groupSnapshot, map[int64]*accountSnapshot)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.sched.view.Load()
	var curGen uint64
	if cur != nil {
		curGen = cur.generation
	}
	if curGen != baseGen {
		_ = curGen
	}
	groups, byID := build()
	p.storeLocked(groups, byID)
}

var _ = atomic.Pointer[RoutingView]{}
