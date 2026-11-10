// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

// selectSessionForView binds a compiled route to one already-loaded immutable
// routing root. An exact route miss falls back to the compiled default bucket
// (model ""). The session is a stack value — never boxed.
//
// key-normalized lookup — the query key zeroes RouteClassID before map
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
// Boundary preserved: the (Selection, Attempt) VALUE shape is
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
// the generation fence consults the session-cached static verdict
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
	// admittedFinance 捕获**本次放行候选**的财务上下文：与准入判定同一次视图
	// 读取（§4.2/§4.6——归属随选中固定，收尾不再 Load/查 owner）。只在谓词真正
	// 放行（最后一个 return true 前）写入，避免被后续 CAS 失败候选污染。
	var admittedFinance domain.SupplierFinance
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
		// 健康判决按身份指纹 + K 双维围栏：指纹不匹配的记录不参与判决。
		// c.Fingerprint 在放行后恒等于当前身份指纹——放行前提是
		// planKeyOf(当前) == c.PlanKey，而指纹的每个入参都落在 planKey 内。
		if s.health != nil && s.health.EffectiveState(av.acc.ID, q, c.Fingerprint, c.IdentityRevision) != StateReady {
			return false
		}
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			return false
		}
		// 供给准入门（spec 2026-10-09 §4.6，I1）：带供应商归属的账号在财务
		// 快照未就绪（或该归属不在快照中）时暂不入调度——拒绝不耗 attempt，
		// 自然落到下一候选；未装配门（关闭态）恒放行（零值财务上下文）。
		// 放行时捕获**同一视图**的归属/分成/代数，随 Selection 携带。
		admittedFin, admitted := s.admitSupplier(av.acc.ID, av.acc.SupplierUserID)
		if !admitted {
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
		if s.reserveHook != nil {
			s.reserveHook()
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
		admittedFinance = admittedFin
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
		StripImageTools: curAv.tpl.StripImageTools, UpstreamPaths: curAv.tpl.UpstreamPaths, Ext: curAv.acc.Ext,
		// 传输是**模板原值**（不做归一）：消费方只调 Key()/Name()，Selection 保存
		// 原值才能让"配置写了什么"在响应头/日志里可辨认。
		CodexResponsesTransport: curAv.tpl.CodexResponsesTransport,
		CandidateFingerprint:    candidate.Fingerprint,
		// 租借同时冻结账号身份代际 K：本次请求的凭据投影（Selection.Credential）与
		// 由此产生的一切凭据事件都以它作来源版本（§3.3b-2），不再回读"当前"。
		lease:            &leaseToken{acc: cur, identityRevision: curAv.acc.IdentityRevision},
		ModelMappingMode: mappingMode,
		// 出口 client：快照期已解析好的指针（恒非 nil）——热路径一次字段读，
		// 没有"代理是否可用"的判断（那是装载/编译期的事实，不是每次选号的活）。
		Egress: curAv.egress,
		// 绑定行 id：与 Egress **同一次字段读**（同一来源 acc.ProxyID），只给
		// 失败面的 err_logs.message 用（`proxy_id=<id>`，spec §5.1）。不进决策：
		// 可用性位在 Routable，出口在 Egress。nil = 直连（失败日志不写 proxy_id）。
		ProxyID: curAv.acc.ProxyID,
	}
	// 认领会话（reserve 并发 CAS 已通过、Selection 装配点——**不在 predicate
	// 内**，避免后置校验异常路径泄漏会话）。仅 codex 凭据认领（claimCodexSession
	// 内判定），非 codex → nil。池注册表取自本视图静态根——与门禁 limit 同源。
	selected.session, selected.pool = s.claimCodexSession(v.static.identityPools, curAv.acc.ID, curAv.tpl.CredentialType)
	// 财务上下文随选中固定（供给准入门放行时同一次视图读取捕获；§4.2/§4.6）：
	// 收尾只按本值落账，不再 Load/查 owner。
	selected.SupplierFinance = admittedFinance
	return selected, attempt, nil
}
