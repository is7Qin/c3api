// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package scheduler 实现内存优先的账号调度：规则驱动的事件投递（internal/rule
// 引擎——typed Throttle/FailAccount 动作）、选号（执行预编译路由计划：lane 顺序、
// 完整唯一 overflow 尾、reservation reject 不耗 attempt）、并发槽、快照缓存。
// 规格 §5。单实例语义：运行时状态仅存内存。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

var (
	ErrGroupNotFound     = errors.New("scheduler: group not found")
	ErrFormatUnavailable = errors.New("scheduler: no account for request format")
	// ErrPlanNotReady is the compile-lag sentinel: the group exists in the
	// static view but no compiled decision has published yet (static staged
	// ahead of the compile lane, e.g. cold start before the first paired
	// publish). Distinct from ErrFormatUnavailable (genuinely unsupported
	// format/model — no static bucket, never routable): not-ready retries
	// after ~1s (503 + Retry-After), unroutable does not (404).
	ErrPlanNotReady                 = errors.New("scheduler: routing plan not ready")
	ErrNoAvailable                  = errors.New("scheduler: no available account")
	ErrMissingCandidateFingerprint  = errors.New("scheduler: candidate fingerprint required")
	ErrCandidateFingerprintMismatch = errors.New("scheduler: candidate fingerprint mismatch")
	ErrMissingExpectedRevision      = errors.New("scheduler: expected revision required")
)

type Config struct {
	SyncInterval time.Duration
	// StalenessProbe 是 C1 backstop 探针的 tuple 供应商（repo 层实现，
	// 如 GroupRepo.CompileStalenessSnapshot；接口在 compile_backstop.go
	// 定义，赋值即满足，无需命名类型）。构造期传入，nil = 不接线
	// （backstop 保持 fail-safe 全量 reload）。装配后不可变—— probes
	// 不支持运行时替换（换源 = 重建 Scheduler）。
	StalenessProbe stalenessQuerier
}

// Loader 是调度器的数据源（由 repository 实现）。
type Loader interface {
	LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error)
	LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error)
}

type leaseToken struct {
	acc      *accountSnapshot
	released atomic.Bool
}

type Selection struct {
	AccountID            int64
	TemplateID           int64
	BaseURL              string
	Format               domain.RequestFormat
	UpstreamKey          string
	CredentialType       credential.Type
	Model                string // 已应用模型映射
	StripImageTools      bool
	Ext                  *domain.AccountExt
	CandidateFingerprint string
	lease                *leaseToken
	ModelMappingMode     domain.ModelMappingMode
}

func (s *Selection) Release() {
	if s == nil || s.lease == nil {
		return
	}
	if s.lease.released.CompareAndSwap(false, true) {
		s.lease.acc.runtime.concurrency.Add(-1)
	}
}

func (s *Selection) LeaseAccountForTest() *accountSnapshot {
	if s == nil || s.lease == nil {
		return nil
	}
	return s.lease.acc
}

// RuntimeInfo 账号运行时视图（管理端展示 + overview 聚合）。Status = 运行时
// 调度状态（active/disabled——disabled 来自 FailAccount/failed_at 装载；临时
// 健康细分状态在 RuntimeHealth，不在此重复）；ErrRate/ErrCount 为运行时观测
// 投影（legacy 状态机写点已随 cutover 删除，值由后续质量统计道供给）。
type RuntimeInfo struct {
	Status      domain.AccountStatus
	Concurrency int64
	ErrRate     float64
	ErrCount    int
}

type Scheduler struct {
	cfg       Config
	loader    Loader
	rule      *rule.RuleEngine
	log       *logx.Logger
	view      atomic.Pointer[RoutingView]
	gen       atomic.Uint64
	publisher *routingPublisher
	// concView 集群账号并发视图（concsync.go worker 换入的第二 atomic 快照，
	// spec conc-share-borrow-account）：超份额借位判定的对账聚合。nil / 陈旧 =
	// 无共识 = fail-open 全额本地语义（结构性质，非错误分支）。
	concView atomic.Pointer[clusterView]
	// instN 集群实例数 N 提供者（装配期 SetInstancesProvider 注入；nil → N=1，
	// 见 concsync.go instancesN）。与 proxy.InstancesProvider 同名异包自持。
	instN     atomic.Pointer[InstancesProvider]
	timeNow   func() time.Time
	startOnce atomic.Bool
	latch     *latch.LatchStore
	health    *RuntimeHealth
	// Compile lane (Task11 wiring): serial background compiler feeding the
	// single routingPublisher. Request path never touches these.
	// sources 是 Start 期结构注入的编译双源（W3-T1；nil = 未装配，armed 门
	// no-op）：装配期一次性写入（Start 存入后起循环），此后只读。
	compiler           routeCompiler
	sources            *CompilerSources
	compileCh          chan struct{}
	decisionEnc        decisionEncoder // compile-lane owned scratch (serial caller; buffers reused across fires)
	lastDecisionBytes  []byte          // compile-lane owned retain copy of the last published encoding (capacity reused via append)
	lastCompiledStatic *StaticView     // compile-lane owned; bytes alone omit static identity
	// v5 event-driven compile lane (single mechanism replacing the
	// unconditional rebuild; owners+lifecycles in compile_event.go):
	scopeCh       chan scopedCompileReq                         // compile-lane-owned, fire-owned payloads
	scopeOverflow atomic.Bool                                   // set on scopeCh drop, consumed per fire
	lastQuality   map[CandidateQualityKey]CandidateQualityInput // compile-lane-owned dynamic baselines
	lastBaseline  map[CandidateQualityKey]Counts                // compile-lane-owned incident baseline
	lastPrices    map[string]domain.ResolvedPrices
	lastProbe     atomic.Pointer[compileProbeCounts] // tick-lane staleness baseline (refresh-first)
	// stalenessProbe supplies the O(1) §4 counter tuple for the backstop;
	// nil = unwired → fail-safe full reload preserves the SLO by construction.
	stalenessProbe func(context.Context) (compileProbeCounts, error)
	fallbackCount  atomic.Uint64
	lastFallback   atomic.Pointer[fallbackReason]
	// skipCount counts no-work fires (fireSkip): the wake's entire input was
	// identical to the last successful compile, so the compile was elided.
	// Distinct from fallbackCount (which counts real FULL recomputes).
	skipCount atomic.Uint64
	// compileDone 监督循环完成信号（Start 存入，Close join——同 runtime-health /
	// conc-sync 停机纪律）；compileOKMs/compileErrMs 编译道新鲜度观测
	//（atomic，Stats 冷路径读；unix-ms，0 = 从未发生）。
	compileDone  atomic.Pointer[<-chan struct{}]
	compileOKMs  atomic.Int64
	compileErrMs atomic.Int64
	// Incident lane (serial compile lane only; expose-only, never forks
	// routing): per-route state machine + evaluation counters (atomics for
	// the Stats cold path; unix-ms, 0 = 从未发生）。
	incidents      *incidentTracker
	incidentActive atomic.Int64
	incidentEvalMs atomic.Int64
	// lastFireMinute tracks the UTC minute of the last compileOnce fire
	// (unix, 0 = never). The sync tick fires the lane when the wall minute
	// advanced without any fire — windowed inputs are a function of M, so a
	// quiet boundary crossing can newly expose settled minutes with no other
	// trigger. Conditional (lane-quiet boundary only) + byte-guarded publish:
	// never an unconditional periodic recompile.
	lastFireMinute atomic.Int64
}

// View returns current RoutingView root (single atomic root; structurally shared StaticView+DecisionView).
func (s *Scheduler) View() *RoutingView { return s.view.Load() }

// ProbeAccount 返回健康探测用的账号快照拷贝（选号门与探测读同一权威视图：
// Template/Ext/revision 齐备；只读拷贝，不暴露发布视图指针）。账号缺失
// （已删/未加载）返回 ok=false——探测侧 fail-closed。
func (s *Scheduler) ProbeAccount(id int64) (*domain.Account, bool) {
	v := s.view.Load()
	if v == nil {
		return nil, false
	}
	snap, ok := v.Account(id)
	if !ok {
		return nil, false
	}
	av := snap.static.Load()
	if av == nil {
		return nil, false
	}
	acc := av.acc
	return &acc, true
}

// New 构造调度器。ruleEngine 必须非 nil（事件投递面；main 在 Start 前显式 Reload）。
// h 为运行时健康投影（构造注入，无 Set* 回填）：nil 允许——无健康面场景
// （测试/纯选号）refill 热路径经 s.health != nil 守卫跳过健康门。
// latch/hub 为锁存一等组件（main 拥有、构造出借，无回填）：latch nil 则自建
// （保测试兼容）；hub 非 nil 即在 New 内订阅 onRuleFailure（首个 MarkResult
// 前完成——装配序保证），nil = 不订阅（无规则摘除面的纯选号测试）。
// cfg.StalenessProbe 为空保持探针解线（backstop fail-safe 全量 reload）。
func New(cfg Config, loader Loader, ruleEngine *rule.RuleEngine, h *RuntimeHealth, log *logx.Logger, latchStore *latch.LatchStore, hub *latch.Hub) *Scheduler {
	if latchStore == nil {
		latchStore = latch.NewLatchStore()
	}
	s := &Scheduler{
		cfg:       cfg,
		loader:    loader,
		rule:      ruleEngine,
		health:    h,
		log:       log,
		timeNow:   time.Now,
		latch:     latchStore,
		compiler:  NewRoutingCompiler(),
		compileCh: make(chan struct{}, 1),
		scopeCh:   make(chan scopedCompileReq, scopeChCap),
		incidents: newIncidentTracker(),
	}
	if hub != nil {
		hub.Subscribe(s.onRuleFailure)
	}
	s.publisher = newRoutingPublisher(s)
	if cfg.StalenessProbe != nil {
		q := cfg.StalenessProbe
		s.stalenessProbe = func(ctx context.Context) (compileProbeCounts, error) {
			snap, err := q.CompileStalenessSnapshot(ctx)
			if err != nil {
				return compileProbeCounts{}, err
			}
			return snapshotToProbeCounts(snap), nil
		}
	}
	return s
}

// Name 满足 worker.Worker 契约（Global Constraints #5）。
func (s *Scheduler) Name() string { return "scheduler" }

// Start 启动定时同步；编译源是 Start 期依赖（W3-T1 结构注入，取代已删的
// 双 setter）：src 非 nil 即武装编译道（reload/分钟边界/质量/价格触发经
// armed 门放行），nil = 未装配 legacy 形态（触发 no-op，绝不发布编译视
// 图）。重复 Start 幂等（返回错误）。
func (s *Scheduler) Start(ctx context.Context, src *CompilerSources) error {
	if !s.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("scheduler: already started")
	}
	s.sources = src
	worker.GoLoop(ctx, "scheduler-sync", s.log, s.syncLoop)
	compileDone := worker.GoLoop(ctx, "scheduler-compile", s.log, s.compileLoop)
	s.compileDone.Store(&compileDone)
	return nil
}

// Close join 编译道循环（限时，同 conc-sync/runtime-health 停机纪律——Close
// 后不再有编译发布）；幂等，满足 worker.Worker 契约。循环本身随 Start 的 ctx
// 取消而退出。
func (s *Scheduler) Close(ctx context.Context) error {
	if d := s.compileDone.Load(); d != nil {
		select {
		case <-*d:
		case <-ctx.Done():
			if s.log != nil {
				s.log.Warn("scheduler close timeout, compile loop still running")
			}
		}
	}
	return nil
}

func (s *Scheduler) syncLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// v5-C1: the tick is a staleness backstop (O(1) probe, full work
			// only on mismatch). The unconditional reload-on-every-tick
			// default path is DELETED outright.
			s.backstopTick(ctx)
			// Windowed inputs are a function of M: a lane-quiet boundary
			// crossing can newly expose settled minutes with no other
			// trigger, so the tick fires the lane then (conditional +
			// byte-guarded, never periodic-unconditional).
			s.fireOnMinuteAdvance()
		}
	}
}

// fireOnMinuteAdvance signals the compile lane when the UTC minute advanced
// since the last compileOnce fire. Busy lanes never trip it (every fire
// restamps); quiet lanes fire at most once per boundary. Pre-arming it is a
// no-op via RequestCompile's armed gate.
func (s *Scheduler) fireOnMinuteAdvance() {
	m := s.timeNow().UTC().Truncate(time.Minute).Unix()
	if m <= s.lastFireMinute.Load() {
		return
	}
	s.RequestCompile()
}

// reload 全量重建快照（启动/定时/InvalidateAll）— single publisher.
// Serializes ownership before DB load so Reload cannot overwrite InvalidateGroup;
// the new static root is built from the newest staged-or-published root and
// staged as pending — the published pair is never paired with a stale
// decision (atomic publication: the compile lane publishes the matched pair).
func (s *Scheduler) reload(ctx context.Context) error {
	s.publisher.mu.Lock()
	defer s.publisher.mu.Unlock()
	// v5-C1: refresh-first baseline (never after — see refreshProbeBaseline).
	s.refreshProbeBaseline(ctx)
	m, err := s.loader.LoadGroupsAccounts(ctx)
	if err != nil {
		return err
	}
	cur := s.view.Load()
	var oldByID map[int64]*accountSnapshot
	if s.publisher.pending != nil {
		oldByID = s.publisher.pending.byID
	} else if cur != nil && cur.static != nil {
		oldByID = cur.static.byID
	}
	groups, byID := buildSnapshots(m, oldByID)
	sv := newStaticView(groups, byID)
	if s.latch != nil {
		for id, as := range byID {
			av := as.static.Load()
			if av == nil {
				continue
			}
			fp, err := candidateFingerprint(&av.acc)
			if err != nil {
				continue
			}
			// 指纹变更 = 新候选人 ⇒ 清旧锁存。K 变更**无需**写侧动作：读侧
			// IsLatched 比 (指纹, K)，旧代际条目天然不匹配。
			s.latch.ClearIfFingerprintChanged(id, fp)
		}
		if oldByID != nil {
			for id := range oldByID {
				if _, ok := byID[id]; !ok {
					s.latch.Clear(id)
				}
			}
		}
	}
	// 身份代际 K 推进 ⇒ 清该账号的健康记录（含 Redis 侧墓碑）。K 变了意味着
	// 身份写入发生，旧 K 下的健康事实不再适用。**只清内存视图不够**：Sync 按
	// 活动 ZSET 重建，未过期 OPEN 记录仅在被墓碑标注时才丢弃（故原语内写墓碑）。
	// 与 latch 同一收敛点：快照重建后、发布前，且同样以 oldByID 为对照——
	// 这样"K 推进"只在一个地方判定，不散落成多个可能分歧的判断。
	if s.health != nil && oldByID != nil {
		for id, as := range byID {
			old, ok := oldByID[id]
			if !ok {
				continue
			}
			av, oldAv := as.static.Load(), old.static.Load()
			if av == nil || oldAv == nil || av.acc.IdentityRevision == oldAv.acc.IdentityRevision {
				continue
			}
			if _, err := s.health.ClearAccount(ctx, id); err != nil && s.log != nil {
				s.log.Warn("health clear on identity revision advance failed", logx.Int64("account_id", id), logx.Error(err))
			}
		}
	}
	s.publisher.stageLocked(sv)
	// v5-C1/C2: a full staging covers every route — the lane takes the
	// full-fidelity fallback. Scope-first, then wake.
	s.enqueueCompileScope(nil, nil, scopeCauseFullStage)
	s.RequestCompile()
	return nil
}

func (s *Scheduler) TryLatch(accountID int64, fingerprint string, identityRevision int64) bool {
	if s.latch == nil {
		return false
	}
	return s.latch.TryAcquire(accountID, fingerprint, identityRevision)
}

func (s *Scheduler) IsLatched(accountID int64) bool {
	if s.latch == nil {
		return false
	}
	v := s.view.Load()
	if v == nil {
		return s.latch.IsLatched(accountID, "", 0)
	}
	if as, ok := v.Account(accountID); ok {
		av := as.static.Load()
		if av == nil {
			return false
		}
		fp, err := candidateFingerprint(&av.acc)
		if err != nil {
			return false
		}
		return s.latch.IsLatched(accountID, fp, av.acc.IdentityRevision)
	}
	return s.latch.IsLatched(accountID, "", 0)
}

func (s *Scheduler) LatchStore() *latch.LatchStore { return s.latch }

// runtimeStatusFor 从可持久生命周期字段推导账号装载时的运行时初始状态：
// failed_at 置位 = 运行时 disabled（SDK/rule 判死的持久事实；恢复唯一入口
// /recover 清 failed_at + revision +1，重载即回 active）。持久 status 列已
// 随 cutover 删除——运行时状态机不再从 DB 镜像复活。
func runtimeStatusFor(a *domain.Account) domain.AccountStatus {
	if a.FailedAt != nil {
		return domain.StatusDisabled
	}
	return domain.StatusActive
}

// 叶复用的判据 = staticKeyOf(old) == staticKeyOf(new)，即「叶上被消费的事实是否变了」。
//
// 这里曾有一个候选设计：让叶对象身份**永久稳定**、只原子换 static 指针。已证伪——
// hasStaticChange（attempt_plan_exec.go:378）比较的正是**叶指针**：叶变了 ⇒ 在途
// plan 跳过该账号、未变的账号继续预留（spec §5.7(b)「态 2」）。若叶指针永不改变，
// hasStaticChange 恒假 ⇒ 任何 generation 变化都直接 ErrAttemptsExhausted
// （attempt_plan_reservation.go:106）⇒ 在途 plan **永不能续跑**，比「不复用」更糟。
// 故叶身份必须随「消费面是否变化」而动。
//
// 由此 staticKey 的字段集判据是 A2 的「消费面 ⊆ 键字段集」：**凡从叶消费的事实
// 都必须入键**，凭据值（含 OAuth token）亦然——否则凭据轮转后键判等、复用旧叶、
// 上游用旧令牌（实测 TestInvalidateAccountReloadsExt）。而「token 刷新不该改身份」
// 由 (I,K) 围栏（P1/P2/P3）另行承担：**两件事不共用一个比较**。

// buildSnapshots 构建全量快照：**每账号一个共享实例**——多组账号在多个组
// 快照中引用同一实例（O2 评审实证修复）。发布后 leaves never mutate；
// 变更账号分配全新 immutable leaf，共享 separate runtime/concurrency state，
// old root stable。oldByID 来自旧 StaticView 的 byID（持 publisher.mu 读取安全）。
func buildSnapshots(m map[int64][]*domain.Account, oldByID map[int64]*accountSnapshot) (map[int64]*groupSnapshot, map[int64]*accountSnapshot) {
	// First pass: collect per-account group membership and latest Account object.
	type accInfo struct {
		acc      *domain.Account
		groupIDs []int64
	}
	infoMap := make(map[int64]*accInfo)
	for gid, accs := range m {
		for _, a := range accs {
			if inf, ok := infoMap[a.ID]; ok {
				inf.groupIDs = append(inf.groupIDs, gid)
			} else {
				infoMap[a.ID] = &accInfo{acc: a, groupIDs: []int64{gid}}
			}
		}
	}
	byID := make(map[int64]*accountSnapshot, len(infoMap))
	for id, inf := range infoMap {
		a := inf.acc
		// gid 必须是**确定性**派生：它随快照进入事件投递归组与规则事件
		// （scheduler.go:975 groupIDPtr(av.eventGID())）。此前取「首个出现组」=
		// map 迭代序首元素，同一份 DB 数据在不同进程/不同重载下可得不同
		// gid —— 组归属是静态事实，不得有这种自由度。多组账号取最小
		// 组 ID（min 与迭代序无关，且与组集合一一对应）。
		// 入快照的账号 max_concurrency 由写面保证 ≥1（创建默认 + 校验拒绝），
		// 此处不再静默钳制——落库异常值应显形而非被快照掩盖。
		av := &snapshotStatic{acc: *a, tpl: a.Template, groupIDs: append([]int64(nil), inf.groupIDs...)}
		if old, exists := oldByID[id]; exists {
			oldAv := old.static.Load()
			// 静态事实比较经 staticKeyOf（值类型，`==` 算子）——此前的
			// 手写布尔链含 `oldAv.tpl == av.tpl` 与 `oldAv.acc.Ext == av.Ext`
			// 两处**指针比较**：tpl/Ext 每次重载都新建对象，地址恒不等，
			// 故整条链恒假 ⇒ 复用分支从不命中，静态字段列表整体成为惰性
			// 装饰。staticKeyOf 把切片/映射/指针事实一律折叠为规范序摘要，
			// 结构上杜绝该缺陷（守卫见 static_key_test.go）。
			//
			sameStatic := oldAv != nil && staticKeyOf(oldAv) == staticKeyOf(av)
			if sameStatic {
				// 静态未变：runtime 整体保留（errRate/errCount/并发跨重载连续，
				// A-2 M-4；status 唯一例外——failed_at 是持久事实，重载按
				// failed_at 收敛，未失效账号的内存 disabled 不复活）。
				rt := old.runtime
				if curSt := rt.state.Load(); curSt != nil {
					next := *curSt
					next.status = runtimeStatusFor(a)
					rt.state.Store(&next)
				}
				byID[id] = old
				continue
			}
			rt := old.runtime
			curSt := rt.state.Load()
			var next accState
			if curSt != nil {
				next = *curSt
			}
			next.status = runtimeStatusFor(a)
			rt.state.Store(&next)
			as := &accountSnapshot{accountID: av.acc.ID, runtime: rt}
			as.static.Store(av)
			byID[id] = as
		} else {
			st := &accState{status: runtimeStatusFor(a)}
			byID[id] = newAccountSnapshot(av, st)
		}
	}
	groups := make(map[int64]*groupSnapshot, len(m))
	for gid, accs := range m {
		gs := &groupSnapshot{}
		for _, a := range accs {
			as := byID[a.ID]
			gs.accounts = append(gs.accounts, as)
		}
		gs.routes = buildRoutes(gs.accounts)
		groups[gid] = gs
	}
	return groups, byID
}

// modelSet 组内所有账号模板的可服务模型并集（桶 key 的模型空间）。
// 重建路径调用（buildSnapshots 在 publisher.mu 内），静态字
// 经视图读取（评审 Critical 修复后实例不再保留裸字段）。
func modelSet(accs []*accountSnapshot) map[string]struct{} {
	set := make(map[string]struct{})
	for _, a := range accs {
		tpl := a.static.Load().tpl
		if tpl == nil {
			continue
		}
		for _, m := range tpl.Models {
			set[m] = struct{}{}
		}
		for _, list := range tpl.FormatModels {
			for _, m := range list {
				set[m] = struct{}{}
			}
		}
		for m := range tpl.ModelMapping {
			set[m] = struct{}{}
		}
	}
	return set
}

// buildRoutes 生成 (format, model) 桶键集：格式硬过滤（FormatSupports）与
// 模型硬白名单（Serves）都是静态信息，可完全在重建时计算。另为每个格式生成
// 默认回退桶（model == ""）：仅含全模型账号（无模型空间），请求模型未知时
// 兜底转发。桶键集是编译车道的枚举域（编译器经 fullCandidateUnion 重算候选，
// 不消费序列——legacy 加权预生成序列已随 cutover 删除）。
//
// 分桶语义（模板模型硬白名单，用户裁决 2026-08-18）：
//   - Serves(model) 命中 → tier1（不变）；
//   - 未命中 + 模板有模型空间（Models/FormatModels/ModelMapping 任一非空 =
//     白名单账号）→ 跳过（不建路由 → Select 404，不再 tier2 兜底转发）；
//   - 未命中 + 无模型空间（全模型账号）→ tier2（兜底保留）。
//
// 边界：仅配置 format_models/mapping、Models 空的账号同样归白名单账号——其
// 未列模型的格式（FormatModels 未覆盖但 supported_formats 含）上不建任何路由
// → 404（旧行为该格式全模型 tier2 转发，随白名单语义收窄）。
//
// mapping 交互：白名单只查请求模型（mapping key 即白名单别名，∈ Serves 空间），
// 映射目标（上游模型名）不复查（选号内映射）。
func buildRoutes(accs []*accountSnapshot) map[routeKey]*route {
	routes := make(map[routeKey]*route)
	formats := []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS, domain.FormatOpenAIImages, domain.FormatAnthropic}
	for model := range modelSet(accs) {
		for _, format := range formats {
			var t1, t2 []*accountSnapshot
			for _, a := range accs {
				tpl := a.static.Load().tpl
				if tpl == nil || !tpl.FormatSupports(format, model) {
					continue
				}
				if tpl.Serves(model) {
					t1 = append(t1, a)
				} else if !tpl.HasModelSpace() {
					t2 = append(t2, a) // 全模型账号：tier2 兜底
				}
				// 白名单账号未命中 → 跳过（不建路由 → 404）
			}
			if len(t1) == 0 && len(t2) == 0 {
				continue
			}
			routes[routeKey{format, model}] = &route{}
		}
	}
	for _, format := range formats {
		var t2 []*accountSnapshot
		for _, a := range accs {
			tpl := a.static.Load().tpl
			if tpl == nil || !slices.Contains(tpl.SupportedFormats, format) {
				continue
			}
			if tpl.HasModelSpace() {
				continue // 白名单账号不参与未知模型回落（默认桶仅全模型账号）
			}
			t2 = append(t2, a)
		}
		if len(t2) == 0 {
			continue
		}
		routes[routeKey{format, ""}] = &route{}
	}
	return routes
}

// InvalidateGroup 组级定向重载（O2 接线矩阵：账号变更 → 受影响组）。与全量
// reload 同一"每账号共享实例"纪律：重载组的新实例同时替换 byID 与其账号的
// 其它组引用——Select（经组路由）与 Release（经 byID）必须命中同一计数器，
// 否则多组账号并发计数分裂漂移 → 槽位假满（O2 实证修复）。账号从组移除且
// 不再属于任何组 → 从 byID 移除；仍属其它组 → 保留实例并摘除本组引用。
// 新静态根基于最新 staged-or-published 根合并后 stage 为 pending（原子发布：
// 编译车道发布配对），已发布的完整 pair 在此期间保持可见。
// 静态字段（含 groupIDs）在 snapshotStatic 不可变视图中：写经 publisher.mu +
// 原子指针发布（buildSnapshots/本方法 copy-modify-Store），读经 atomic.Load()
// （发布收集仍持 publisher.mu——评审 M-1 纪律，无锁外裸读）。
func (s *Scheduler) InvalidateGroup(groupID int64) {
	s.publisher.mu.Lock()
	defer s.publisher.mu.Unlock()
	// v5-C1: refresh-first baseline (never after — see refreshProbeBaseline).
	s.refreshProbeBaseline(context.Background())
	accs, err := s.loader.LoadGroupAccounts(context.Background(), groupID)
	if err != nil {
		if s.log != nil {
			s.log.Warn("group reload failed", logx.Int64("group_id", groupID), logx.Error(err))
		}
		return
	}
	cur := s.view.Load()
	var m map[int64]*groupSnapshot
	var byID map[int64]*accountSnapshot
	if s.publisher.pending != nil {
		m = s.publisher.pending.groups
		byID = s.publisher.pending.byID
	} else if cur != nil && cur.static != nil {
		m = cur.static.groups
		byID = cur.static.byID
	} else {
		m = map[int64]*groupSnapshot{}
		byID = map[int64]*accountSnapshot{}
	}
	// byID 兼作复用查询源（oldByID）：组级重载同样复用旧实例——errRate/errCount
	// 跨组级 NOTIFY 重载保留（A-2 M-4），静态字段 DB 权威同步。持 publisher.mu 读取安全。
	gs, _ := buildSnapshots(map[int64][]*domain.Account{groupID: accs}, byID)
	newAccs := gs[groupID].accounts
	// 直接复用 buildSnapshots 产出的快照：accounts 与 routes 一并生效，
	// 避免组级重载后 routes 为 nil（编译车道枚举域断裂）。
	newM := make(map[int64]*groupSnapshot, len(m))
	for k, v := range m {
		newM[k] = v
	}
	newM[groupID] = gs[groupID]
	newByID := make(map[int64]*accountSnapshot, len(byID)+len(newAccs))
	for k, v := range byID {
		newByID[k] = v
	}
	// 从组移除的账号（旧组有、新组无）：仍属其它组 → 保留实例并摘本组引用；
	// 已不属于任何组 → 从 byID 删除（其它组引用随实例保留/删除，路由无需重建）。
	// 评审 M-2：先建 新组账号ID 索引再单遍扫描——嵌套循环对 50k 大组批量删
	// 25k 是 ≈1.25e9 次比较 ≈1s 停顿（去抖单 goroutine 内拉大所有失效延迟/
	// 新用户 402 窗口），索引后 O(旧组大小)。
	// v5-C2: staged groups bound the scoped fire — the reloaded group plus
	// every other group sharing its accounts (their snapshots are rebuilt
	// with the new leaves, so their routes must recompute too).
	scopeGroups := []int64{groupID}
	if old, ok := m[groupID]; ok {
		newIDs := make(map[int64]struct{}, len(newAccs))
		for _, ns := range newAccs {
			newIDs[ns.static.Load().acc.ID] = struct{}{}
		}
		// Track leaves needing other-group replacement for removed-but-still-present accounts.
		removedOtherRefs := make(map[int64][]*accountSnapshot)
		for _, os := range old.accounts {
			ost := os.static.Load()
			if _, stillIn := newIDs[ost.acc.ID]; stillIn {
				continue
			}
			newGids := removeGid(append([]int64(nil), ost.groupIDs...), groupID)
			if len(newGids) == 0 {
				delete(newByID, ost.acc.ID)
				continue
			}
			// Changed account gets new immutable static leaf sharing separate runtime, old root stable.
			// gid 必须随 groupIDs 一起重派生：组被移除后，旧 gid 可能已不在
			// newGids 中（原 gid 恰为被移除组，且是当时的最小值）——
			// 沿用 ost.gid 会让 gid ∉ groupIDs，破坏「gid = min(groupIDs)」
			// 不变量，事件投递归组会指向一个该账号已不属于的组。
			newStatic := &snapshotStatic{acc: ost.acc, tpl: ost.tpl, groupIDs: newGids}
			newLeaf := &accountSnapshot{accountID: ost.acc.ID, runtime: os.runtime}
			newLeaf.static.Store(newStatic)
			newByID[ost.acc.ID] = newLeaf
			// Record for other group replacement.
			for _, og := range newGids {
				removedOtherRefs[og] = append(removedOtherRefs[og], newLeaf)
			}
		}
		// Apply other-group replacements for removed accounts.
		for og, leaves := range removedOtherRefs {
			scopeGroups = append(scopeGroups, og)
			ogp, ok := newM[og]
			if !ok {
				continue
			}
			repl := make([]*accountSnapshot, len(ogp.accounts))
			copy(repl, ogp.accounts)
			leafByID := make(map[int64]*accountSnapshot, len(leaves))
			for _, lf := range leaves {
				leafByID[lf.static.Load().acc.ID] = lf
			}
			for i, oas := range repl {
				if nl, ok := leafByID[oas.static.Load().acc.ID]; ok {
					repl[i] = nl
				}
			}
			newM[og] = &groupSnapshot{accounts: repl, routes: buildRoutes(repl)}
		}
	}
	// 新实例替换 byID + 其它组引用（多组账号：旧实例在其它组路由中的位置换成
	// 新实例并重建该组路由——共享实例纪律；单组账号 otherGids 为空，零开销）。
	// 评审 M-2：其它组引用替换同禁嵌套扫描——每其它组先建 账号ID→位置 索引
	// （O(该组大小)），替换 O(1)，总量 O(受影响组账号和)。
	type ogRef struct {
		gs  *groupSnapshot
		idx map[int64]int
	}
	otherRefs := make(map[int64]*ogRef)
	for _, ns := range newAccs {
		var otherGids []int64
		nst := ns.static.Load()
		if oa, ok := byID[nst.acc.ID]; ok {
			// Preserve other groupIDs from old immutable root.
			for _, g := range oa.static.Load().groupIDs {
				if g != groupID {
					otherGids = append(otherGids, g)
				}
			}
			// Share runtime: new leaf already shares oa.runtime via buildSnapshots,
			// no extra Store needed. Ensure pointer sharing.
			if ns.runtime != oa.runtime {
				ns.runtime = oa.runtime
			}
		}
		// New leaf is local (not yet published), safe to mutate static before publish.
		nns := *nst
		nns.groupIDs = append([]int64{groupID}, otherGids...)
		ns.static.Store(&nns)
		newByID[nst.acc.ID] = ns
		for _, og := range otherGids {
			if _, ok := otherRefs[og]; ok {
				continue
			}
			ogp, ok := newM[og]
			if !ok {
				continue
			}
			ref := &ogRef{gs: ogp, idx: make(map[int64]int, len(ogp.accounts))}
			for i, oas := range ogp.accounts {
				ref.idx[oas.static.Load().acc.ID] = i
			}
			otherRefs[og] = ref
		}
	}
	for og, ref := range otherRefs {
		scopeGroups = append(scopeGroups, og)
		repl := make([]*accountSnapshot, len(ref.gs.accounts))
		copy(repl, ref.gs.accounts)
		for _, ns := range newAccs {
			if i, ok := ref.idx[ns.static.Load().acc.ID]; ok {
				repl[i] = ns
			}
		}
		newM[og] = &groupSnapshot{accounts: repl, routes: buildRoutes(repl)}
	}
	sv := newStaticView(newM, newByID)
	s.publisher.stageLocked(sv)
	// v5-C1/C2: this staging touches exactly scopeGroups — the lane recomputes
	// only their routes. Scope-first, then wake.
	s.enqueueCompileScope(scopeGroups, nil, scopeCauseGroup)
	s.RequestCompile()
}

// InvalidateAccount 单账号快照失效（SDK 接入 T5 §1 P3-3——轮转回写后同步
// AccountExt 内存快照：下个会话重载新凭据，避免旧令牌 401 额外往返）。复用
// 既有组级定向重载（InvalidateGroup——账号所属各组并集去重；旋转低频事件，
// 组级重载成本可接受）。快照外账号（已移除/未知）→ no-op。与失效上报不同
// 步（sdkbridge 轮转回调内调用；重载失败由 InvalidateGroup 内部 Warn 记录，
// 不阻断——令牌已落库，下个会话经适配层 Auth 内存新 at 自愈）。
func (s *Scheduler) InvalidateAccount(accountID int64) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return
	}
	as, exists := v.Account(accountID)
	if !exists {
		return
	}
	gids := append([]int64(nil), as.static.Load().groupIDs...)
	seen := make(map[int64]struct{}, len(gids))
	for _, g := range gids {
		if _, dup := seen[g]; dup {
			continue
		}
		seen[g] = struct{}{}
		s.InvalidateGroup(g)
	}
}

// minGID 返回组 ID 集合的最小值——snapshotStatic.gid 的确定性派生。
// 空集合返回 0（不变量：账号至少属于一个组，调用点已保证非空；
// 0 作为哨兵，不会与真实组 ID 混淆——组 ID 为数据库自增正数）。
func minGID(ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	m := ids[0]
	for _, v := range ids[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func groupsEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[int64]int, len(a))
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		if c, ok := m[v]; !ok || c == 0 {
			return false
		}
		m[v]--
	}
	return true
}

// removeGid 摘除 groupIDs 中的指定组（实例共享纪律：组级重载的从组移除路径）。
func removeGid(gids []int64, gid int64) []int64 {
	out := gids[:0]
	for _, g := range gids {
		if g != gid {
			out = append(out, g)
		}
	}
	return out
}

func (s *Scheduler) InvalidateAll() {
	if err := s.reload(context.Background()); err != nil && s.log != nil {
		s.log.Warn("scheduler reload failed", logx.Error(err))
	}
}

// Loader 暴露数据源（测试注入用）。
func (s *Scheduler) Loader() Loader { return s.loader }

// InvalidateAllSync 同步全量重载（测试与启动用）。
func (s *Scheduler) InvalidateAllSync() error { return s.reload(context.Background()) }

// InvalidateAllSyncCtx 同步全量重载（响应 ctx 取消；#14 T3a 评审 M-2：notify
// Dispatcher.FullRefresh 用——断线重连的全量刷新不得耗尽停机预算）。
func (s *Scheduler) InvalidateAllSyncCtx(ctx context.Context) error { return s.reload(ctx) }

// Runtime 供管理端展示运行时视图。快照未加载（启动中/首刷失败）→ 返回
// false（同 Select 模式——裸断言在此 panic，Warn-and-serve 语义下管理端应见
// 未就绪而非进程崩溃）。
func (s *Scheduler) Runtime(accountID int64) (RuntimeInfo, bool) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return RuntimeInfo{}, false
	}
	a, ok := v.Account(accountID)
	if !ok {
		return RuntimeInfo{}, false
	}
	st := a.statePtr()
	return RuntimeInfo{
		Status:      st.status,
		Concurrency: a.runtime.concurrency.Load(),
		ErrRate:     float64(a.runtime.errRate.Load()) / errRateScale,
		ErrCount:    st.errCount,
	}, true
}

// AccountRuntime 账号运行时视图（/api/admin/overview 聚合专用；含账号名——err_top
// 为账号维度，name = 账号名）。与 RuntimeInfo 同源（快照 EWMA/并发原子读），
// 仅多带聚合所需字段。
type AccountRuntime struct {
	AccountID      int64
	Name           string
	Status         domain.AccountStatus
	MaxConcurrency int
	Concurrency    int64
	ErrRate        float64
	ErrCount       int
}

// Runtimes 全部账号运行时快照（overview 聚合面：账号健康分布/并发水位/err_top
// 与账号列表运行时视图同源）。遍历 byID 快照 map（整体原子换入不可变）零锁；
// 冷面调用（管理端聚合 + TTL 缓存摊薄），不涉请求热路径。快照未加载 → nil。
func (s *Scheduler) Runtimes() []AccountRuntime {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return nil
	}
	byID := v.ByID()
	out := make([]AccountRuntime, 0, len(byID))
	for id, a := range byID {
		av := a.static.Load()
		st := a.statePtr()
		out = append(out, AccountRuntime{
			AccountID:      id,
			Name:           av.acc.Name,
			Status:         st.status,
			MaxConcurrency: av.acc.MaxConcurrency,
			Concurrency:    a.runtime.concurrency.Load(),
			ErrRate:        float64(a.runtime.errRate.Load()) / errRateScale,
			ErrCount:       st.errCount,
		})
	}
	return out
}

// Release legacy ID-based release (kept for existing tests that use ID path).
// New code should use Selection.Release which releases exact object via leaseToken.
func (s *Scheduler) Release(accountID int64) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return
	}
	if a, ok := v.Account(accountID); ok {
		a.runtime.concurrency.Add(-1)
	}
}

// ReleaseSelection releases exact leased object idempotently (preferred).
func (s *Scheduler) ReleaseSelection(sel *Selection) {
	if sel != nil {
		sel.Release()
	}
}

// MarkResult 请求结果回流：禁用守卫（同步短路）+ 条件投递（C1）→ 规则引擎异步处理。
// 动作应用全部由规则命中后的 typed sink 完成（本方法不触碰状态）。
// kind 直接收 rule.Kind（单一 kind 概念——scheduler 不再有第二套枚举；连接级/
// 5xx 分流由调用点 RuleKindOf 完成）。
func (s *Scheduler) MarkResult(accountID int64, kind rule.Kind, resetAt *time.Time, httpStatus int, errMsg string, model string) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return
	}
	a, ok := v.Account(accountID)
	if !ok {
		return
	}
	// 禁用账号的防复活守卫：失效/禁用置位后（failed_at 装载或 FailAccount 内存
	// 置 disabled），在途请求完成时不得投递事件——规则可能把它恢复。禁用账号
	// 不参与选号，err/429 分支同样不可能合法触发于其上，统一在此短路（不投递）。
	if a.statePtr().status == domain.StatusDisabled {
		return
	}
	// 条件投递（C1）：规则表无 kind=nil/ok 规则时 ok 事件不投递
	// （无恢复规则时成功结果不影响任何状态，省队列与处理开销）。
	if kind == rule.KindOK && !s.rule.NeedsOKEvents() {
		return
	}
	var hp *int
	if httpStatus > 0 {
		hp = &httpStatus
	}
	av := a.static.Load() // 静态字段视图一次取用（评审 Critical 修复）
	fp, _ := candidateFingerprint(&av.acc)
	ev := rule.Event{
		AccountID:                accountID,
		TemplateID:               av.acc.TemplateID,
		GroupID:                  groupIDPtr(av.eventGID()),
		ExpectedIdentityRevision: av.acc.IdentityRevision,
		Kind:                     kind,
		HTTPStatus:               hp,
		Model:                    model,
		ErrorMessage:             errMsg,
		ResetAt:                  resetAt,
		OccurredAt:               s.timeNow(),
		CandidateFingerprint:     fp,
	}
	s.rule.Enqueue(ev)
}

// FailAccount 账号失效摘除（SDK 接入 T1——统一失效回调处理链第二步，
// sdkbridge.HandleFailure 调用；冷面低频）：快照置 StatusDisabled（运行时
// 摘除）。持久化事实 = failed_at（同链第一步 FailAccountCAS/SetAccountFailed
// 已落库；重启快照重载经 runtimeStatusFor 仍摘除——恢复唯一入口 /recover
// fenced 端点清 failed_at + revision +1）。复用既有机制：选号 disabled 过滤
// 与 MarkResult 防复活守卫（置位后快照为 disabled，在途请求结果回流短路不
// 投递规则事件）。与规则引擎动作同构但**不投递规则事件**：失效是 SDK 上报
// 的既成事实，不参与规则判定（规则可能把它恢复）。
func (s *Scheduler) FailAccount(accountID int64) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return
	}
	a, ok := v.Account(accountID)
	if !ok {
		return // 快照外账号（已移除/未知）：无状态可改
	}
	// copy-on-write CAS：快照置位对并发转换串行化。disabled 幂等早退：账号
	// 已 disabled（失效上报重复到达）时直接返回——终态 disabled 不变。
	// cur 恒非 nil（构造即初始化）。失效原因审计落库由失效链 DB 步负责
	// （last_error 列），内存态不再保留副本。
	now := s.timeNow()
	for {
		cur := a.runtime.state.Load()
		if cur.status == domain.StatusDisabled {
			return
		}
		st := *cur
		st.status = domain.StatusDisabled
		st.lastUsedAt = &now
		if !a.runtime.state.CompareAndSwap(cur, &st) {
			continue // 并发转换已发生——重读重试（disabled 对双方都是吸收态，必然终止）
		}
		return
	}
}

func (s *Scheduler) failureEvent(accountID int64, kind rule.Kind, errMsg string) rule.Event {
	v := s.view.Load()
	if v == nil {
		return rule.Event{AccountID: accountID, Kind: kind, ErrorMessage: errMsg}
	}
	a, ok := v.Account(accountID)
	if !ok {
		return rule.Event{AccountID: accountID, Kind: kind, ErrorMessage: errMsg}
	}
	av := a.static.Load()
	fp, _ := candidateFingerprint(&av.acc)
	return rule.Event{AccountID: accountID, TemplateID: av.acc.TemplateID, GroupID: groupIDPtr(av.eventGID()), Kind: kind, ErrorMessage: errMsg, CandidateFingerprint: fp, ExpectedIdentityRevision: av.acc.IdentityRevision}
}

// RuleKindOf 连接级/5xx 事件分流（单点 helper，分流外移到调用点——9 处
// 跨包引用：failoverLoop 5xx/0 分支、ws_relay 中继失联/心跳错误、caller 各
// statusOf(err)==0 调用点）：code==0 → network（独立 kind，规则窗口与 5xx 各自
// 判定）；≥500 → 5xx；1-499 为不可达防御（调用点恒 0/≥500，4xx 走骨架透传不至此）
// → 5xx。429/4xx/ok 调用点显式传 rule.Kind429/Kind4xx/KindOK。
func RuleKindOf(httpStatus int) rule.Kind {
	if httpStatus == 0 {
		return rule.KindNetwork
	}
	return rule.Kind5xx
}

// Classify 错误事件分类决策（failoverLoop 错误分支调用；对齐 MarkResult 模式
// 的 scheduler 包装）：快照取 TemplateID/GroupID（对齐 MarkResult——调用方
// 事件构造无快照访问）后委托规则引擎首中分类。返回 then（ResponseCode nil=透传上游码，CustomMessage nil=透传上游文，指针即意图）与 punish
// （true = 命中规则有惩罚动作——Throttle 或 FailAccount，应投递
// MarkResult 让 worker 精确应用——含窗口条件规则的"可能命中"保守判定）。
// 头透传与 kind 解耦：ResponseCode==nil 且上游带 Retry-After/X-Retry-After 才透，否则不透不伪造。
// 快照未加载/账号快照外 → (domain.RuleThen{}, false)
// （对齐 MarkResult 早退语义——请求路径不可达；本地拒绝不进本机制）。
func (s *Scheduler) Classify(ev rule.Event) (then domain.RuleThen, punish bool) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return domain.RuleThen{}, false
	}
	a, ok := v.Account(ev.AccountID)
	if !ok {
		return domain.RuleThen{}, false
	}
	av := a.static.Load() // 静态字段视图一次取用（评审 Critical 修复）
	ev.TemplateID = av.acc.TemplateID
	ev.GroupID = groupIDPtr(av.eventGID())
	return s.rule.Classify(ev)
}

func groupIDPtr(gid int64) *int64 {
	if gid <= 0 {
		return nil
	}
	return &gid
}

// FlushRules 同步处理规则引擎队列中的全部事件（仅测试与优雅关闭用）：
// MarkResult 为异步投递，需要立即断言快照的测试先排空队列。
func (s *Scheduler) FlushRules() {
	s.rule.Flush(context.Background())
}
