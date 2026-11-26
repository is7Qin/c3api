// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/credential"
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

// planAllCandidatesNativeContinuation 报告计划的候选车道里的候选是否**全部**原生
// 支持 previous_response_id 的服务端续接。
//
// 语义（与它取代的旧判定逐条等价，除了"通道"换成"能力"）：
//   - 只看计划真正会用的三张表：Primary、Explore.Ordered、Degraded；
//   - 全为真 ⇒ 调用方**不解析**绑定：codex 不写续接绑定，这组请求不可能命中；
//     previous_response_id 留在请求里交给 SDK，续接存储故障时同样放行（不 503）；
//   - 有任何候选为假 ⇒ 调用方照旧先 contResolve（fail-closed：宁可多查一次 Redis、
//     宁可 410/503，也不静默丢上下文）。
//
// 为什么判据是**能力位**而不是通道：旧实现问"是不是 codex 通道"，那在今天就等于
// "能不能续接"。两条轴只是**当前**取值一致——一个未来的 codex 通道类型可以不支持
// 服务端续接（例如换一种上游鉴权方式、读不回 response id）。判据读
// credential.Type.SupportsContinuation（逐 Type caps 的能力列），故这种分化无需
// 改调度层。
//
// 空表按"全部支持"返回 true：没有候选的计划不可能派发（预留必然失败），这一位对它
// 没有意义；反过来返回 false 只会让不可能发生的路径多查一次 Redis。注意这条**只对
// 编译产物**成立——手工拼装的路由决策走的是 RoutingView 上的零值（保守方向），见
// RouteDecision.ContinuationNativeOnly 的注释。
//
// 纯函数 + 显式能力读投影（capable），不是包级可变状态：
//   - 生产唯一调用点是**编译期**（assembleRouteDecision 传
//     credential.Type.SupportsContinuation），结论落在 RouteDecision 的不可变位上；
//   - 测试用它注入"codex 通道但不支持续接"的未来类型替身（capsTable 包私有，
//     跨包注册不了），证明这条判定真的读能力而不是读通道。
func planAllCandidatesNativeContinuation(primary, ordered, degraded []CompiledCandidate, capable func(credential.Type) bool) bool {
	for _, cs := range [][]CompiledCandidate{primary, ordered, degraded} {
		for i := range cs {
			if !capable(cs[i].PlanKey.credentialType) {
				return false
			}
		}
	}
	return true
}

// skipContinuationFor 判断一次派发是否**完全跳过**续接解析（不只是"不需要原生续接"）。
//
// 对**表内**类型，语义是两条能力位的合取取反：
//   - 没有原生续接能力（!SupportsContinuation）——"为什么不需要解析"的一半；
//   - 也不吃 c3api 的绑定账本（!UsesStaticKey）——另一半。绑定账本里的 id 由
//     静态 key 通道的响应产生（c3api 为该账号写的 response id），所以只有静态通道
//     类型的 previous_response_id 可能有绑定；codex/bps 的上游 id 不在账本里。
//
// 今天唯一为真的是 BPS（无原生续接、也无账本）。**必须**写成两个能力位的合取，
// 不能写成通道投影（`ChannelOf(t) == ChannelBPS`）：那会把"今天恰好只有 BPS 同时缺
// 这两条"固化成一个通道判定，未来的 codex 通道变体（不支持续接、也不产生账本 id）
// 会被误判成"要解析" ——反过来，任何新增的静态通道类型也会因为通道名不等于 BPS 被
// 误判成"要解析"。能力位是逐 Type 声明的，新增类型必须显式回答这两问。
//
// **表外类型（含 CredentialType == "")必须 fail-closed（返回 false）**，不能靠上面的
// 合取：表外类型两个能力位都是 false ⇒ 合取为 true ⇒ 会把"缺失绑定 ⇒ 410"悄悄变成
// "丢掉 previous_response_id 放行"，方向是 fail-open。这条路径**可达**：
// routing_compiler_candidates.go 的候选过滤只拒 `CredentialType != "" && !Valid()`，
// 空类型模板照常成为候选并产出 Selection。语义与 service/format_gate.go 同一选择
// （"表外历史 credential_type 因此 fail-closed——这是刻意的行为选择"）：
// 不猜未知类型的能力，把请求交给绑定账本，缺失/过期照旧 410 / Redis 故障 503。
//
// 构造期一次定型：调用点在预留装配点，结果写进 Selection.SkipContinuation，数据面
// 只读一位（不查任何表）。
func skipContinuationFor(t credential.Type) bool {
	if !t.Valid() {
		return false // 表外/空类型：不猜能力，交给绑定账本 ⇒ 缺失 410（fail-closed）
	}
	return !t.SupportsContinuation() && !t.UsesStaticKey()
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
		// 使用时刻的观测写：独立原子字上一次 Store（0 alloc）。它不是 accState
		// 的一部分——写 state 要 copy-modify-CAS，复制品取地址（`&next`）逃逸，
		// 每请求多一次堆分配，而这只是管理端展示用的观测值。
		a.runtime.markUsed(s.timeNow())
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
	// 凭据通道在**装配点算一次**（一次 map 查表）：Selection 的续接政策位与
	// claimCodexSession 的分支共用同一个值，热路径不重复查表。
	credCh := credential.ChannelOf(curAv.tpl.CredentialType)
	// 唯一的生产装配点：契约字段 + 私有句柄一次装配。Handle 指向包装自身——
	// 与嵌入的 Selection 同属一个堆对象（&selected.Selection 内指针），所以
	// 「契约在外、私有状态在背」不额外分配（272→288 档，见 size gate）。
	selected := &selection{
		Selection: Selection{
			AccountID: curAv.acc.ID, TemplateID: curAv.tpl.ID, BaseURL: candidate.BaseURL,
			Format: domain.RequestFormat(plan.route.Format), UpstreamKey: curAv.acc.UpstreamKey,
			CredentialType: curAv.tpl.CredentialType, Model: mapped,
			StripImageTools: curAv.tpl.StripImageTools, Upstream: curAv.tpl.Upstream, Ext: curAv.acc.Ext,
			// 传输是**模板原值**（不做归一）：消费方只调 Key()/Name()，Selection 保存
			// 原值才能让"配置写了什么"在响应头/日志里可辨认。
			CodexResponsesTransport: curAv.tpl.CodexResponsesTransport,
			CandidateFingerprint:    candidate.Fingerprint,
			ModelMappingMode:        mappingMode,
			// 出口 client：快照期已解析好的指针（恒非 nil）——热路径一次字段读，
			// 没有"代理是否可用"的判断（那是装载/编译期的事实，不是每次选号的活）。
			Egress: curAv.egress,
			// 绑定行 id：与 Egress **同一次字段读**（同一来源 acc.ProxyID），只给
			// 失败面的 err_logs.message 用（`proxy_id=<id>`，spec §5.1）。不进决策：
			// 可用性位在 Routable，出口在 Egress。nil = 直连（失败日志不写 proxy_id）。
			ProxyID: curAv.acc.ProxyID,
			// 会话路径的账号级事实：快照构建期已定型（同 egress），这里只拷字段。
			CacheDomain:    curAv.cacheDomain,
			InstallationID: curAv.installationID,
			Conversation:   plan.identity.Conversation,
			// 续接派发政策位（构造期定型，构造期一次赋值）：判据**不是通道**，而是两条
			// 逐 Type 能力位的合取（见 skipContinuationFor 的注释）——"没有原生续接，
			// 也不吃绑定账本"才跳过解析，此时 previous_response_id 原样留给 SDK 丢弃
			// （c3api 不做 body 工作）。
			SkipContinuation: skipContinuationFor(curAv.tpl.CredentialType),
		},
		// 租借同时冻结账号身份代际 K：本次请求的凭据投影（Handle.Credential）与
		// 由此产生的一切凭据事件都以它作来源版本（§3.3b-2），不再回读"当前"。
		lease: &leaseToken{acc: cur, identityRevision: curAv.acc.IdentityRevision},
	}
	// 认领会话（reserve 并发 CAS 已通过、Selection 装配点——**不在 predicate
	// 内**，避免后置校验异常路径泄漏会话）。仅 codex 凭据认领（claimCodexSession
	// 内判定），非 codex → nil。池注册表取自本视图静态根——与门禁 limit 同源。
	// 会话路径（Conversation）不认领：身份由 SDK 会话簿合成，槽位池无占用、无推进。
	if !plan.identity.Conversation {
		selected.session, selected.pool = s.claimCodexSession(v.static.identityPools, curAv.acc.ID, credCh)
	}
	// 财务上下文随选中固定（供给准入门放行时同一次视图读取捕获；§4.2/§4.6）：
	// 收尾只按本值落账，不再 Load/查 owner。
	selected.SupplierFinance = admittedFinance
	selected.Handle = selected
	return &selected.Selection, attempt, nil
}
