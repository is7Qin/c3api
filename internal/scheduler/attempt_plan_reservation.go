// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

var reserveHook func() // ponytail: test hook for race barrier between concurrency CAS and state CAS


// NewAttemptPlan binds a compiled route to one immutable routing root. Dynamic
// account state remains shared by the captured account snapshots.
func (s *Scheduler) NewAttemptPlan(identity AttemptPlanIdentity, route RouteRef) (*AttemptPlan, error) {
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
	p.model = route.Model
	for i := uint8(0); i < p.candidateCount; i++ {
		candidate := &p.candidates[i]
		candidate.account = v.static.byID[candidate.accountID]
		if candidate.account == nil {
			continue
		}
		candidate.static = candidate.account.static.Load()
		if candidate.static == nil || candidate.static.tpl == nil {
			continue
		}
		fingerprint, err := candidateFingerprint(&candidate.static.acc)
		if err != nil {
			continue
		}
		candidate.fingerprint = fingerprint
		resolved := route.Model
		if mapped, ok := candidate.static.tpl.ModelMapping[route.Model]; ok {
			resolved = mapped
		}
		candidate.quality = qualityClassHexForWithOp(domain.RequestFormat(route.Format), resolved, domain.OperationTag(route.OperationTag))
	}
	return p, nil
}

// ReserveAttempt applies request-time health, latch, status, cooldown, and
// cluster-concurrency gates to the already compiled plan.
func (s *Scheduler) ReserveAttempt(plan *AttemptPlan) (*Selection, Attempt, error) {
	if plan == nil {
		return nil, Attempt{}, ErrNoAvailable
	}
	now := s.timeNow()
	instances := s.instancesN()
	cluster := s.concView.Load()
	var selected *Selection
	attempt, err := plan.reserve(func(candidate attemptPlanCandidate) bool {
		a := candidate.account
		if a == nil {
			return false
		}
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
		mapped := plan.model
		if model, ok := av.tpl.ModelMapping[plan.model]; ok {
			mapped = model
		}
		baseURL := av.tpl.BaseURL
		if av.acc.BaseURL != nil && *av.acc.BaseURL != "" {
			baseURL = *av.acc.BaseURL
		}
		selected = &Selection{
			AccountID: av.acc.ID, TemplateID: av.tpl.ID, BaseURL: baseURL,
			Format: domain.RequestFormat(plan.format), UpstreamKey: av.acc.UpstreamKey,
			CredentialType: av.tpl.CredentialType, Model: mapped,
			StripImageTools: av.tpl.StripImageTools, Ext: av.acc.Ext,
			CandidateFingerprint: candidate.fingerprint, lease: &leaseToken{acc: a},
		}
		return true
	})
	if err != nil {
		return nil, Attempt{}, err
	}
	return selected, attempt, nil
}
