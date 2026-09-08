// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

var reserveHook func() // ponytail: test hook for race barrier between concurrency CAS and state CAS

// newPlanForView binds a compiled route to one already-loaded immutable
// routing root. An exact route miss falls back to the compiled default bucket
// (model "").
func newPlanForView(identity AttemptPlanIdentity, route RouteRef, v *RoutingView) (*AttemptPlan, error) {
	identity.RequestedModel = route.Model
	if v == nil || v.static == nil {
		return nil, ErrGroupNotFound
	}
	if _, ok := v.static.groups[route.GroupID]; !ok {
		return nil, ErrGroupNotFound
	}
	if v.decision == nil {
		return nil, ErrFormatUnavailable
	}
	decision, ok := v.decision.routes[route]
	if !ok && route.Model != "" {
		route = RouteRefForOp(route.GroupID, route.Format, "", domain.OperationTag(route.OperationTag))
		decision, ok = v.decision.routes[route]
	}
	if !ok || decision == nil {
		return nil, ErrFormatUnavailable
	}
	if _, ok := parseRequestFormat(route.Format); !ok || route.OperationTag == "" {
		return nil, ErrFormatUnavailable
	}
	identity.RouteClassID = route.RouteClassID
	identity.RoutingGeneration = v.generation
	p, err := NewAttemptPlan(identity, decision)
	if err != nil {
		return nil, err
	}
	p.generation = v.generation
	if identity.HasAffinity {
		p.ApplyCacheAffinity(identity.AffinityHash)
	}
	return p, nil
}

// NewAttemptPlan binds a compiled route to one immutable routing root. An
// exact route miss falls back to the compiled default bucket (model "").
func (s *Scheduler) NewAttemptPlan(identity AttemptPlanIdentity, route RouteRef) (*AttemptPlan, error) {
	return newPlanForView(identity, route, s.view.Load())
}

func (s *Scheduler) NewAttemptPlanWithCacheAffinity(identity AttemptPlanIdentity, route RouteRef, keyHash uint64) (*AttemptPlan, error) {
	plan, err := s.NewAttemptPlan(identity, route)
	if err != nil {
		return nil, err
	}
	plan.ApplyCacheAffinity(keyHash)
	return plan, nil
}

// ReserveAttempt applies request-time health, latch, status and
// cluster-concurrency gates to the compiled plan. Candidates carry immutable
// metadata; the only per-candidate request work is O(1) gate checks and the
// single lease CAS. Stale leaves (pointer mismatch after static replacement)
// are rejected, never leased.
func (s *Scheduler) ReserveAttempt(plan *AttemptPlan) (*Selection, Attempt, error) {
	if plan == nil || plan.route == nil {
		return nil, Attempt{}, ErrNoAvailable
	}
	return s.reserveOnView(plan, s.view.Load())
}

// reserveOnView runs the reservation gates against one already-loaded view.
// A decision-generation mismatch is tolerated only before the plan has ever
// successfully reserved (reservationStarted): the initial reservation lands
// on the current leaves even if a decision-only republish slipped in. Once
// execution started, a decision-only republish invalidates the session. A
// static replacement remains eligible for the existing leaf fence, so
// in-flight plans can move to the current leaf instead of losing their
// fallback tail.
func (s *Scheduler) reserveOnView(plan *AttemptPlan, v *RoutingView) (*Selection, Attempt, error) {
	if plan == nil || plan.route == nil {
		return nil, Attempt{}, ErrNoAvailable
	}
	if v == nil || v.static == nil {
		return nil, Attempt{}, ErrAttemptsExhausted
	}
	if plan.reservationStarted && plan.generation != v.generation && !plan.hasStaticChange(v) {
		return nil, Attempt{}, ErrAttemptsExhausted
	}
	instances := s.instancesN()
	cluster := s.concView.Load()
	applyMapping := plan.identity.ApplyModelMapping
	attempt, candidate, err := plan.reserve(func(c CompiledCandidate) bool {
		if c.Leaf == nil || c.Static == nil || c.Fingerprint == "" {
			return false
		}
		if v == nil || v.static == nil || v.static.byID[c.AccountID] != c.Leaf {
			return false
		}
		a := c.Leaf
		av := c.Static
		if av.tpl == nil {
			return false
		}
		if s.latch != nil && s.latch.IsLatched(av.acc.ID, c.Fingerprint) {
			return false
		}
		q := c.Quality
		if !applyMapping {
			q = c.QualityRaw
		}
		if s.health != nil && s.health.EffectiveState(av.acc.ID, q, c.LifecycleRevision) != StateReady {
			return false
		}
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			return false
		}
		cur := a.runtime.concurrency.Load()
		limit := int64(av.acc.MaxConcurrency)
		if cur >= int64(concShare(int(limit), instances)) && (cur >= limit || !concAllows(cluster, av.acc.ID, limit, cur+1)) {
			return false
		}
		if !a.runtime.concurrency.CompareAndSwap(cur, cur+1) {
			return false
		}
		if reserveHook != nil {
			reserveHook()
		}
		used := s.timeNow()
		for {
			curSt := a.runtime.state.Load()
			if curSt == nil {
				break
			}
			next := *curSt
			next.lastUsedAt = &used
			if a.runtime.state.CompareAndSwap(curSt, &next) {
				break
			}
		}
		return true
	})
	if err != nil {
		return nil, Attempt{}, err
	}
	mapped := candidate.MappedModel
	mappingMode := candidate.MappingMode
	if !applyMapping {
		mapped = candidate.RequestedModel
		mappingMode = domain.ModelMappingModeInvalid
	}
	av := candidate.Static
	a := candidate.Leaf
	selected := &Selection{
		AccountID: av.acc.ID, TemplateID: av.tpl.ID, BaseURL: candidate.BaseURL,
		Format: domain.RequestFormat(plan.route.Format), UpstreamKey: av.acc.UpstreamKey,
		CredentialType: av.tpl.CredentialType, Model: mapped,
		StripImageTools: av.tpl.StripImageTools, Ext: av.acc.Ext,
		CandidateFingerprint: candidate.Fingerprint, lease: &leaseToken{acc: a},
		ModelMappingMode: mappingMode,
	}
	return selected, attempt, nil
}
