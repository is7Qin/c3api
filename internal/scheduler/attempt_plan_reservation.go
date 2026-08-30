// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

var reserveHook func() // ponytail: test hook for race barrier between concurrency CAS and state CAS

// NewAttemptPlan binds a compiled route to one immutable routing root. Dynamic
// account state remains shared by the captured account snapshots. An exact
// route miss falls back to the compiled default bucket (model "")—the same
// unknown-model semantics the legacy scan carries.
func (s *Scheduler) NewAttemptPlan(identity AttemptPlanIdentity, route RouteRef) (*AttemptPlan, error) {
	requestedModel := route.Model
	v := s.view.Load()
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
		route = RouteRefFor(route.GroupID, route.Format, "")
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
	p := NewAttemptPlan(identity, *decision)
	p.format = route.Format
	p.model = requestedModel
	p.operationTag = route.OperationTag
	for i := uint8(0); i < p.prefixCount; i++ {
		if !s.resolveCandidate(v, p, &p.candidates[i]) {
			continue
		}
	}
	return p, nil
}

// resolveCandidate fills one plan candidate's identity fields from the given
// routing root. Overflow candidates arrive unresolved (ID + lane only) and are
// resolved on demand at reserve time, so scanning a long tail costs nothing
// until a candidate is actually consulted.
func (s *Scheduler) resolveCandidate(v *RoutingView, p *AttemptPlan, c *attemptPlanCandidate) bool {
	if v == nil || v.static == nil {
		return false
	}
	c.account = v.static.byID[c.accountID]
	if c.account == nil {
		return false
	}
	c.static = c.account.static.Load()
	if c.static == nil || c.static.tpl == nil {
		return false
	}
	fingerprint, err := candidateFingerprint(&c.static.acc)
	if err != nil {
		return false
	}
	c.fingerprint = fingerprint
	resolved := p.model
	if p.identity.ApplyModelMapping {
		if mapped, ok := c.static.tpl.ModelMapping[p.model]; ok {
			resolved = mapped.MappedModel
			c.mappingMode = mapped.Mode
		}
	}
	c.quality = qualityClassHexForWithOp(domain.RequestFormat(p.format), resolved, domain.OperationTag(p.operationTag))
	c.templateID = c.static.tpl.ID
	c.requestedModel = p.model
	c.mappedModel = resolved
	c.routeClassID = p.identity.RouteClassID
	c.callerCategory = string(callerKindForFormat(domain.RequestFormat(p.format)))
	c.operationTag = p.operationTag
	c.lifecycleRevision = c.static.acc.LifecycleRevision
	c.routingGeneration = v.generation
	return true
}

// ReserveAttempt applies request-time health, latch, status, cooldown, and
// cluster-concurrency gates to the already compiled plan. Prefix candidates
// are additionally fenced against their captured static leaf: a leaf replaced
// after plan build (credential rotation, revision bump, removal) is rejected,
// never leased from stale identity.
func (s *Scheduler) ReserveAttempt(plan *AttemptPlan) (*Selection, Attempt, error) {
	if plan == nil {
		return nil, Attempt{}, ErrNoAvailable
	}
	v := s.view.Load()
	now := s.timeNow()
	instances := s.instancesN()
	cluster := s.concView.Load()
	var selected *Selection
	attempt, err := plan.reserve(func(candidate *attemptPlanCandidate) bool {
		if candidate.account == nil {
			if !s.resolveCandidate(v, plan, candidate) {
				return false
			}
		} else if v == nil || v.static == nil || v.static.byID[candidate.accountID] != candidate.account {
			return false // stale leaf after static replacement
		}
		a := candidate.account
		av := candidate.static
		if av == nil || av.tpl == nil || candidate.fingerprint == "" {
			return false
		}
		if s.latch != nil && s.latch.IsLatched(av.acc.ID, candidate.fingerprint) {
			return false
		}
		if s.health != nil && s.health.EffectiveState(av.acc.ID, candidate.quality, av.acc.LifecycleRevision) != StateReady {
			return false
		}
		st := a.statePtr()
		if st.status == domain.StatusDisabled || (st.cooldownUntil != nil && !st.cooldownUntil.Before(now)) {
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
		baseURL := av.tpl.BaseURL
		if av.acc.BaseURL != nil && *av.acc.BaseURL != "" {
			baseURL = *av.acc.BaseURL
		}
		selected = &Selection{
			AccountID: av.acc.ID, TemplateID: av.tpl.ID, BaseURL: baseURL,
			Format: domain.RequestFormat(plan.format), UpstreamKey: av.acc.UpstreamKey,
			CredentialType: av.tpl.CredentialType, Model: candidate.mappedModel,
			StripImageTools: av.tpl.StripImageTools, Ext: av.acc.Ext,
			CandidateFingerprint: candidate.fingerprint, lease: &leaseToken{acc: a},
			ModelMappingMode: candidate.mappingMode,
		}
		return true
	})
	if err != nil {
		return nil, Attempt{}, err
	}
	return selected, attempt, nil
}
