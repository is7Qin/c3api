// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

var reserveHook func() // ponytail: test hook for race barrier between concurrency CAS and state CAS

// selectSessionForView binds a compiled route to one already-loaded immutable
// routing root. An exact route miss falls back to the compiled default bucket
// (model ""). The session is a stack value — never boxed.
//
// v4-S2: key-normalized lookup — the query key zeroes RouteClassID before map
// access (the old direct exact-match on the full key including hex is deleted,
// not kept as a fast path). RouteClassID is borrowed from the found
// RouteDecision (the stored per-route field IS the intern); the session never
// computes hex.
func selectSessionForView(identity AttemptPlanIdentity, route RouteRef, v *RoutingView) (AttemptPlan, error) {
	identity.RequestedModel = route.Model
	if v == nil || v.static == nil {
		return AttemptPlan{}, ErrGroupNotFound
	}
	if _, ok := v.static.groups[route.GroupID]; !ok {
		return AttemptPlan{}, ErrGroupNotFound
	}
	if v.decision == nil {
		// Static faces are warm but the compile lane has not published any
		// decision yet: compile lag, not an unroutable request.
		return AttemptPlan{}, ErrPlanNotReady
	}
	decision, ok := v.decision.routes[normRouteRef(route)]
	if !ok && route.Model != "" {
		decision, ok = v.decision.routes[RouteRefForOp(route.GroupID, route.Format, "", domain.OperationTag(route.OperationTag))]
	}
	if !ok || decision == nil {
		return AttemptPlan{}, ErrFormatUnavailable
	}
	if _, ok := parseRequestFormat(route.Format); !ok || route.OperationTag == "" {
		return AttemptPlan{}, ErrFormatUnavailable
	}
	identity.RouteClassID = decision.RouteClassID
	identity.RoutingGeneration = v.generation
	sess, err := newSelectSession(identity, decision)
	if err != nil {
		return AttemptPlan{}, err
	}
	sess.generation = v.generation
	if identity.HasAffinity {
		sess.ApplyCacheAffinity(identity.AffinityHash)
	}
	return sess, nil
}

// NewAttemptPlan binds a compiled route to one immutable routing root. An
// exact route miss falls back to the compiled default bucket (model "").
// Stack value return — the request path never boxes the session.
func (s *Scheduler) NewAttemptPlan(identity AttemptPlanIdentity, route RouteRef) (AttemptPlan, error) {
	return selectSessionForView(identity, route, s.view.Load())
}

func (s *Scheduler) NewAttemptPlanWithCacheAffinity(identity AttemptPlanIdentity, route RouteRef, keyHash uint64) (AttemptPlan, error) {
	plan, err := s.NewAttemptPlan(identity, route)
	if err != nil {
		return AttemptPlan{}, err
	}
	plan.ApplyCacheAffinity(keyHash)
	return plan, nil
}

// ReserveAttempt applies request-time health, latch, status and
// cluster-concurrency gates to the compiled plan. Candidates carry immutable
// metadata; the only per-candidate request work is O(1) gate checks and the
// single lease CAS. Stale candidates (current planKey mismatch after static
// replacement) are rejected, never leased.
//
// 门禁与 Selection 装配一律读**当前叶**：放行的前提已是"当前视图的逐账号
// planKey == c.PlanKey"，而 planKey 按定义就是门禁与 Selection 所读的全部静态
// 字段——于是对 planKey 覆盖的每个字段，"读当前叶"与"读计划冻结值"恒等；两者
// 的唯一差异恰好落在 payloadKey 上，而那组字段正是要取新值的载荷。
// runtime 在重载间共享同一个 *accountRuntime，故并发/状态读当前叶即读现值。
//
// Boundary preserved (v4-S2): the (Selection, Attempt) VALUE shape is
// unchanged — only the session carriage moved from heap box to stack value.
// The session pointer here is a stack pointer that never escapes: ReserveAttempt
// retains nothing across calls.
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
//
// v4-S3: the generation fence consults the session-cached static verdict
// (single fence-site entry) instead of scanning per attempt.
func (s *Scheduler) reserveOnView(plan *AttemptPlan, v *RoutingView) (*Selection, Attempt, error) {
	if plan == nil || plan.route == nil {
		return nil, Attempt{}, ErrNoAvailable
	}
	if v == nil || v.static == nil {
		return nil, Attempt{}, ErrAttemptsExhausted
	}
	if plan.reservationStarted && plan.generation != v.generation && !plan.cachedStaticVerdict(v) {
		return nil, Attempt{}, ErrAttemptsExhausted
	}
	instances := s.instancesN()
	cluster := s.concView.Load()
	applyMapping := plan.identity.ApplyModelMapping
	byID := v.static.byID
	facts := v.static.facts
	attempt, candidate, err := plan.reserve(func(c CompiledCandidate) bool {
		if c.Leaf == nil || c.Static == nil || c.Fingerprint == "" {
			return false
		}
		a := byID[c.AccountID]
		if a == nil {
			return false
		}
		av := a.static.Load()
		if av == nil {
			return false
		}
		// 计划判据取视图**发布时**按账号预计算的 planKey（与 baseURL/fingerprint
		// 同源的那份逐账号事实），不在每次预留尝试里现算：planKeyOf 要做规范序
		// 摘要（排序拷贝 + 多次 sha256），现算会给热路径加十余次分配。缺席
		// （账号不在视图/静态面缺失）⇒ 拒绝，与旧指针比较的缺席语义等价。
		fact, ok := facts[c.AccountID]
		if !ok || fact.planKey != c.PlanKey {
			return false
		}
		if av.tpl == nil {
			return false
		}
		if s.latch != nil && s.latch.IsLatched(av.acc.ID, c.Fingerprint, av.acc.IdentityRevision) {
			return false
		}
		q := c.Quality
		if !applyMapping {
			q = c.QualityRaw
		}
		if s.health != nil && s.health.EffectiveState(av.acc.ID, q, c.IdentityRevision) != StateReady {
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
	cur, ok := v.static.byID[candidate.AccountID]
	if !ok || cur == nil {
		return nil, Attempt{}, ErrAttemptsExhausted
	}
	curAv := cur.static.Load()
	if curAv == nil || curAv.tpl == nil {
		return nil, Attempt{}, ErrAttemptsExhausted
	}
	selected := &Selection{
		AccountID: curAv.acc.ID, TemplateID: curAv.tpl.ID, BaseURL: candidate.BaseURL,
		Format: domain.RequestFormat(plan.route.Format), UpstreamKey: curAv.acc.UpstreamKey,
		CredentialType: curAv.tpl.CredentialType, Model: mapped,
		StripImageTools: curAv.tpl.StripImageTools, Ext: curAv.acc.Ext,
		CandidateFingerprint: candidate.Fingerprint, lease: &leaseToken{acc: cur},
		ModelMappingMode: mappingMode,
	}
	return selected, attempt, nil
}
