// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package scheduler 实现内存优先的账号调度：规则驱动的状态管理（internal/rule 引擎
// 事件投递 + apply 回调）、选号（格式硬过滤 + 模型硬白名单 + 全模型账号 tier2 兜底
// + 预生成加权轮询序列）、并发槽、快照缓存与异步状态回写。规格 §5。单实例语义：运行时状态仅存内存。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

var (
	ErrGroupNotFound                = errors.New("scheduler: group not found")
	ErrFormatUnavailable            = errors.New("scheduler: no account for request format")
	ErrNoAvailable                  = errors.New("scheduler: no available account")
	ErrMissingCandidateFingerprint  = errors.New("scheduler: candidate fingerprint required")
	ErrCandidateFingerprintMismatch = errors.New("scheduler: candidate fingerprint mismatch")
	ErrMissingExpectedRevision      = errors.New("scheduler: expected revision required")
	ErrStaleFailureRevision         = errors.New("scheduler: stale failure revision")
)

type Config struct {
	DefaultMaxConcurrency int
	SyncInterval          time.Duration
	// GroupPub 状态回写成功后的组级 NOTIFY 发布器（#14 T3a：多实例传播——
	// 账号状态变更落库后广播受影响组，其余实例组级重载收敛分裂快照）。
	// 实现 = 装配侧 adapter（main 把 notify.Publisher 适配为
	// PublishGroups）；nil = 未装配（单实例/测试），no-op。
	GroupPub GroupChangePublisher
}

// GroupChangePublisher 组级 NOTIFY 发布面（设计文档 §1.3 / 必改 6）：
// apply 状态回写 DB 成功后发布受影响组 id——跨实例状态分裂的最大风险点
// （实例 A 禁号回写，实例 B 快照仍 active 继续选号）。计费/扣费路径不发布
// （scheduler 无扣费，全部是账号状态回写）。
type GroupChangePublisher interface {
	PublishGroups(ctx context.Context, gids []int64)
}

// Loader 是调度器的数据源（由 repository 实现）。
type Loader interface {
	LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error)
	LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error)
	UpdateAccountStatus(ctx context.Context, accountID int64, status domain.AccountStatus, cooldownUntil *time.Time, lastError *string, weight *int) error
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

type RuntimeInfo struct {
	Status        domain.AccountStatus
	CooldownUntil *time.Time
	Concurrency   int64
	ErrRate       float64
	ErrCount      int
}

type statusWrite struct {
	id       int64
	status   domain.AccountStatus
	cooldown *time.Time
	lastErr  *string
	weight   *int // 权重动作随状态同批回写（nil = 不动 weight）
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
	reloadMu  sync.Mutex // legacy alias to publisher.mu; publisher serializes (one Store)
	writeCh   chan statusWrite
	timeNow   func() time.Time
	startOnce atomic.Bool
	latch     *latchStore
	health    *RuntimeHealth
	// Compile lane (Task11 wiring): serial background compiler feeding the
	// single routingPublisher. Request path never touches these.
	compiler          routeCompiler
	qualityFn         func() map[CandidateQualityKey]CandidateQualityInput
	pricesFn          func() map[string]domain.ResolvedPrices
	compileCh         chan struct{}
	compileArmed      bool
	lastDecisionBytes []byte // compile-lane owned (single serial caller)
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
	snap, ok := v.ByID()[id]
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

// New 构造调度器并注册规则引擎的 apply 回调（动作应用 = 快照/EWMA/回写，见 apply）。
// ruleEngine 必须非 nil（状态管理唯一路径；main 在 Start 前显式 Reload）。
func New(cfg Config, loader Loader, ruleEngine *rule.RuleEngine, log *logx.Logger) *Scheduler {
	s := &Scheduler{
		cfg:       cfg,
		loader:    loader,
		rule:      ruleEngine,
		log:       log,
		writeCh:   make(chan statusWrite, 4096),
		timeNow:   time.Now,
		latch:     newLatchStore(),
		compiler:  NewRoutingCompiler(),
		compileCh: make(chan struct{}, 1),
	}
	s.publisher = newRoutingPublisher(s)
	ruleEngine.SetApply(s.apply)
	return s
}

// Name 满足 worker.Worker 契约（Global Constraints #5）。
func (s *Scheduler) Name() string { return "scheduler" }

// Start 启动定时同步与异步状态回写；重复 Start 幂等（返回错误）。
func (s *Scheduler) Start(ctx context.Context) error {
	if !s.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("scheduler: already started")
	}
	worker.GoLoop(ctx, "scheduler-sync", s.log, s.syncLoop)
	worker.GoLoop(ctx, "scheduler-writeback", s.log, s.writebackLoop)
	worker.GoLoop(ctx, "scheduler-compile", s.log, s.compileLoop)
	return nil
}

// Close 排空剩余状态回写（限时，复用 writebackLoop 的合并逻辑）；幂等，
// 满足 worker.Worker 契约。循环本身随 Start 的 ctx 取消而退出。
func (s *Scheduler) Close(ctx context.Context) error {
	done := make(chan struct{})
	worker.GoRecover("scheduler-close", s.log, func() {
		for {
			select {
			case w := <-s.writeCh:
				s.processWrite(w)
			default:
				close(done)
				return
			}
		}
	})
	select {
	case <-done:
	case <-ctx.Done():
		if s.log != nil {
			s.log.Warn("scheduler close timeout, dropping pending writebacks")
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
			if err := s.reload(ctx); err != nil && s.log != nil {
				s.log.Warn("scheduler sync failed", logx.Error(err))
			}
		}
	}
}

func (s *Scheduler) writebackLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-s.writeCh:
			s.processWrite(w)
		}
	}
}

// processWrite 处理一条状态回写：合并窗口内同一账号的重复写（幂等覆盖）后回写 DB。
// 合并语义：后写覆盖先写，但 weight 例外——后写若不带 weight（statusWrite.weight=nil，
// 纯状态动作），保留先前已入队的 weight（否则同账号 weight 写先入队、status 写后
// 入队时合并丢 weight，DB 不持久化 → ≤30s reload 后内存回退，weight 动作被静默撤销）。
// 全部回写成功后发布一次组级 NOTIFY（#14 T3a）：合并本批受影响组（去重，防载荷
// 膨胀超 R9 上限）——一次回写批次一条 NOTIFY（R3）。快照外账号（已移除）跳过：
// 无组可传播，其余实例经 ≤30s 全量同步 / 60s 兜底收敛。
func (s *Scheduler) processWrite(w statusWrite) {
	accs := map[int64]statusWrite{w.id: w}
	drain := true
	for drain {
		select {
		case w2 := <-s.writeCh:
			if prev, ok := accs[w2.id]; ok && w2.weight == nil && prev.weight != nil {
				w2.weight = prev.weight
			}
			accs[w2.id] = w2
		default:
			drain = false
		}
	}
	// 先回写 DB（锁外：持 reloadMu 做 DB 往返会阻塞重载），收集回写成功的账号。
	okIDs := make([]int64, 0, len(accs))
	for _, ww := range accs {
		if err := s.loader.UpdateAccountStatus(context.Background(), ww.id, ww.status, ww.cooldown, ww.lastErr, ww.weight); err != nil {
			if s.log != nil {
				s.log.Warn("account status writeback failed", logx.Int64("account_id", ww.id), logx.Error(err))
			}
			continue // 回写失败：DB 状态未变，无变更可传播
		}
		okIDs = append(okIDs, ww.id)
	}
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		if s.log != nil {
			s.log.Warn("scheduler writeback skipped: snapshot not loaded")
		}
		return
	}
	byID := v.ByID()
	gidSet := make(map[int64]struct{})
	for _, id := range okIDs {
		if as, ok := byID[id]; ok {
			for _, g := range as.static.Load().groupIDs {
				gidSet[g] = struct{}{}
			}
		}
	}
	if len(gidSet) > 0 && s.cfg.GroupPub != nil {
		gids := make([]int64, 0, len(gidSet))
		for g := range gidSet {
			gids = append(gids, g)
		}
		s.cfg.GroupPub.PublishGroups(context.Background(), gids)
	}
}

// reload 全量重建快照（启动/定时/InvalidateAll）— single publisher.
// Serializes ownership before DB load so Reload cannot overwrite InvalidateGroup;
// static update retains latest DecisionView.
func (s *Scheduler) reload(ctx context.Context) error {
	s.publisher.mu.Lock()
	defer s.publisher.mu.Unlock()
	m, err := s.loader.LoadGroupsAccounts(ctx)
	if err != nil {
		return err
	}
	cur := s.view.Load()
	var oldByID map[int64]*accountSnapshot
	var dec *DecisionView
	if cur != nil && cur.static != nil {
		oldByID = cur.static.byID
		dec = cur.decision
	}
	groups, byID := buildSnapshots(m, s.cfg.DefaultMaxConcurrency, oldByID)
	sv := &StaticView{groups: groups, byID: byID}
	s.publisher.storeLocked(sv, dec)
	if s.latch != nil {
		for id, as := range byID {
			av := as.static.Load()
			if av == nil {
				continue
			}
			curRev := av.acc.LifecycleRevision
			fp, err := candidateFingerprint(&av.acc)
			if err != nil {
				continue
			}
			s.latch.ClearIfRevisionGreater(id, curRev)
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
	s.RequestCompile()
	return nil
}

func (s *Scheduler) SetRuntimeHealth(h *RuntimeHealth) { s.health = h }

func (s *Scheduler) TryLatch(accountID int64, fingerprint string, revision int64) bool {
	if s.latch == nil {
		return false
	}
	return s.latch.TryAcquire(accountID, fingerprint, revision)
}

func (s *Scheduler) IsLatched(accountID int64) bool {
	if s.latch == nil {
		return false
	}
	v := s.view.Load()
	if v == nil {
		return s.latch.IsLatched(accountID, "")
	}
	byID := v.ByID()
	if byID == nil {
		return s.latch.IsLatched(accountID, "")
	}
	if as, ok := byID[accountID]; ok {
		fp, err := candidateFingerprint(&as.static.Load().acc)
		if err != nil {
			return false
		}
		return s.latch.IsLatched(accountID, fp)
	}
	return s.latch.IsLatched(accountID, "")
}

func (s *Scheduler) LatchStore() *latchStore { return s.latch }

// buildSnapshots 构建全量快照：**每账号一个共享实例**——多组账号在多个组
// 快照中引用同一实例（O2 评审实证修复）。发布后 leaves never mutate；
// 变更账号分配全新 immutable leaf，共享 separate runtime/concurrency state，
// old root stable。oldByID 来自旧 StaticView 的 byID（持 publisher.mu 读取安全）。
func buildSnapshots(m map[int64][]*domain.Account, defaultMax int, oldByID map[int64]*accountSnapshot) (map[int64]*groupSnapshot, map[int64]*accountSnapshot) {
	// First pass: collect per-account group membership and latest Account object.
	type accInfo struct {
		acc      *domain.Account
		groupIDs []int64
		firstGID int64
	}
	infoMap := make(map[int64]*accInfo)
	for gid, accs := range m {
		for _, a := range accs {
			if inf, ok := infoMap[a.ID]; ok {
				inf.groupIDs = append(inf.groupIDs, gid)
			} else {
				infoMap[a.ID] = &accInfo{acc: a, groupIDs: []int64{gid}, firstGID: gid}
			}
		}
	}
	byID := make(map[int64]*accountSnapshot, len(infoMap))
	for id, inf := range infoMap {
		a := inf.acc
		av := &snapshotStatic{acc: *a, tpl: a.Template, gid: inf.firstGID, groupIDs: append([]int64(nil), inf.groupIDs...)}
		if a.MaxConcurrency <= 0 {
			av.acc.MaxConcurrency = defaultMax
		}
		if old, exists := oldByID[id]; exists {
			oldAv := old.static.Load()
			sameBase := false
			if oldAv != nil {
				sameBase = (oldAv.acc.BaseURL == nil && av.acc.BaseURL == nil) || (oldAv.acc.BaseURL != nil && av.acc.BaseURL != nil && *oldAv.acc.BaseURL == *av.acc.BaseURL)
			}
			sameStatic := oldAv != nil && oldAv.acc.Weight == av.acc.Weight && oldAv.acc.MaxConcurrency == av.acc.MaxConcurrency && oldAv.tpl == av.tpl && groupsEqual(oldAv.groupIDs, av.groupIDs) && sameBase && oldAv.acc.UpstreamKey == av.acc.UpstreamKey && oldAv.acc.Ext == av.acc.Ext && oldAv.acc.LifecycleRevision == av.acc.LifecycleRevision
			if sameStatic {
				rt := old.runtime
				if curSt := rt.state.Load(); curSt != nil {
					next := *curSt
					next.status = a.Status
					if a.CooldownUntil != nil {
						next.cooldownUntil = a.CooldownUntil
					}
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
			} else {
				next = accState{status: domain.StatusActive}
			}
			next.status = a.Status
			if a.CooldownUntil != nil {
				next.cooldownUntil = a.CooldownUntil
			}
			rt.state.Store(&next)
			as := &accountSnapshot{runtime: rt}
			as.static.Store(av)
			byID[id] = as
		} else {
			st := &accState{status: a.Status, cooldownUntil: a.CooldownUntil}
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
// 重建路径调用（buildSnapshots/rebuildGroupLocked 均在 reloadMu 内），静态字段
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

// buildRoutes 预生成 (format, model) 调度路径：格式硬过滤（FormatSupports）与
// 模型硬白名单（Serves）都是静态信息，可完全在重建时计算。另为每个格式生成
// 默认回退桶（model == ""）：仅含全模型账号（无模型空间），请求模型未知时
// 兜底转发。
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
// 映射目标（上游模型名）不复查（pickFrom 内映射）。
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
			rt := &route{}
			if len(t1) > 0 {
				rt.tier1 = newWeightedSeq(t1)
			}
			if len(t2) > 0 {
				rt.tier2 = newWeightedSeq(t2)
			}
			routes[routeKey{format, model}] = rt
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
		routes[routeKey{format, ""}] = &route{tier2: newWeightedSeq(t2)}
	}
	return routes
}

// InvalidateGroup 组级定向重载（O2 接线矩阵：账号变更 → 受影响组）。与全量
// reload 同一"每账号共享实例"纪律：重载组的新实例同时替换 byID 与其账号的
// 其它组引用——Select（经组路由）与 Release（经 byID）必须命中同一计数器，
// 否则多组账号并发计数分裂漂移 → 槽位假满（O2 实证修复）。账号从组移除且
// 不再属于任何组 → 从 byID 移除；仍属其它组 → 保留实例并摘除本组引用。
// 静态字段（含 groupIDs）在 snapshotStatic 不可变视图中：写经 reloadMu +
// 原子指针发布（buildSnapshots/本方法 copy-modify-Store），读经 atomic.Load()
// （processWrite 发布收集仍持 reloadMu——评审 M-1 纪律，无锁外裸读）。
func (s *Scheduler) InvalidateGroup(groupID int64) {
	s.publisher.mu.Lock()
	defer s.publisher.mu.Unlock()
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
	if cur != nil && cur.static != nil {
		m = cur.static.groups
		byID = cur.static.byID
	} else {
		m = map[int64]*groupSnapshot{}
		byID = map[int64]*accountSnapshot{}
	}
	// byID 兼作复用查询源（oldByID）：组级重载同样复用旧实例——errRate/errCount
	// 跨组级 NOTIFY 重载保留（A-2 M-4），静态字段 DB 权威同步。持 reloadMu 读取安全。
	gs, _ := buildSnapshots(map[int64][]*domain.Account{groupID: accs}, s.cfg.DefaultMaxConcurrency, byID)
	newAccs := gs[groupID].accounts
	// 直接复用 buildSnapshots 产出的快照：accounts 与 routes 一并生效，
	// 避免组级重载后 routes 为 nil（Select 预生成路径断裂）。
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
			newStatic := &snapshotStatic{acc: ost.acc, tpl: ost.tpl, gid: ost.gid, groupIDs: newGids}
			newLeaf := &accountSnapshot{runtime: os.runtime}
			newLeaf.static.Store(newStatic)
			newByID[ost.acc.ID] = newLeaf
			// Record for other group replacement.
			for _, og := range newGids {
				removedOtherRefs[og] = append(removedOtherRefs[og], newLeaf)
			}
		}
		// Apply other-group replacements for removed accounts.
		for og, leaves := range removedOtherRefs {
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
		repl := make([]*accountSnapshot, len(ref.gs.accounts))
		copy(repl, ref.gs.accounts)
		for _, ns := range newAccs {
			if i, ok := ref.idx[ns.static.Load().acc.ID]; ok {
				repl[i] = ns
			}
		}
		newM[og] = &groupSnapshot{accounts: repl, routes: buildRoutes(repl)}
	}
	sv := &StaticView{groups: newM, byID: newByID}
	var dec *DecisionView
	if cur != nil {
		dec = cur.decision
	}
	s.publisher.storeLocked(sv, dec)
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
	byID := v.ByID()
	as, exists := byID[accountID]
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
	a, ok := v.ByID()[accountID]
	if !ok {
		return RuntimeInfo{}, false
	}
	st := a.statePtr()
	return RuntimeInfo{
		Status: st.status, CooldownUntil: st.cooldownUntil,
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
	if a, ok := v.ByID()[accountID]; ok {
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
// 快照/EWMA/组路由/DB 回写全部由规则命中后的 apply 回调完成（本方法不再触碰状态）。
// kind 直接收 rule.Kind（单一 kind 概念——scheduler 不再有第二套枚举；连接级/
// 5xx 分流由调用点 RuleKindOf 完成）。
func (s *Scheduler) MarkResult(accountID int64, kind rule.Kind, resetAt *time.Time, httpStatus int, errMsg string, model string) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return
	}
	a, ok := v.ByID()[accountID]
	if !ok {
		return
	}
	// 禁用账号的防复活守卫：管理端禁用后（InvalidateGroup 以 disabled 重载
	// 快照），在途请求完成时不得投递事件把状态重置回 active 并回写 DB——否则
	// 禁用被静默抹除、30s 同步后账号复现（评审发现）。禁用账号不参与选号，
	// err/429 分支同样不可能合法触发于其上，统一在此短路（不投递）。
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
		AccountID:            accountID,
		TemplateID:           av.acc.TemplateID,
		GroupID:              groupIDPtr(av.gid),
		Kind:                 kind,
		HTTPStatus:           hp,
		Model:                model,
		ErrorMessage:         errMsg,
		ResetAt:              resetAt,
		OccurredAt:           s.timeNow(),
		CandidateFingerprint: fp,
	}
	s.rule.Enqueue(ev)
}

// FailAccount 账号失效摘除（SDK 接入 T1——统一失效回调处理链第二步，
// sdkbridge.HandleFailure 调用；冷面低频）：快照置 StatusDisabled + last_error
// 审计（失效原因摘要，域内截断 500）+ 阻塞入队回写 loader 持久化（重启快照
// 重载后仍摘除——pickFrom 只跳 disabled 不查 failed_at，仅内存摘除会复活）。
// 复用既有机制：pickFrom 过滤器（selection.go 只跳 disabled）与 MarkResult
// 防复活守卫（置位后快照为 disabled，在途请求结果回流短路不投递规则事件）。
// 与规则引擎动作（apply）同构但**不投递规则事件**：直接置快照 + 回写——失效
// 是 SDK 上报的既成事实，不参与规则状态机（规则可能把它恢复 active）。
// 回写经既有 writeback 合并管道（后写覆盖先写）落库，与在途 apply 回写不乱序；
// 阻塞入队区别于普通回写的"队列满丢弃"策略：失效回写必须落库（丢弃 = 重启
// 复活），冷面阻塞可接受。writebackLoop 未启动（未 Start）时入队不阻塞（缓冲
// 4096）；进程退出竞态（循环已死且队列满）才阻塞——可接受。
func (s *Scheduler) FailAccount(accountID int64, reason string) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return
	}
	a, ok := v.ByID()[accountID]
	if !ok {
		return // 快照外账号（已移除/未知）：无状态可改，不投递回写（同 apply）
	}
	// copy-on-write CAS（与 apply 同构）：快照置位对并发转换（apply）串行化——
	// 本 CAS 先成功 → apply 的 CAS 读到 disabled 早退，不复活；apply 先成功 →
	// 本 CAS 在 disabled 之上覆盖 disabled，终态确定。disabled 幂等早退：账号
	// 已 disabled（规则动作先置）时直接返回——新语义：首个置位者写 lastError
	// 审计，已 disabled 的后续失效上报不重复覆盖审计与回写（终态 disabled 不变，
	// 仅审计内容与旧"最后写者"语义不同）。cur 恒非 nil（构造即初始化）。
	now := s.timeNow()
	for {
		cur := a.runtime.state.Load()
		if cur.status == domain.StatusDisabled {
			return
		}
		st := *cur
		st.status = domain.StatusDisabled
		if t := domain.TruncateErrMsg(reason); t != "" {
			st.lastError = &t
		}
		st.lastUsedAt = &now
		if !a.runtime.state.CompareAndSwap(cur, &st) {
			continue // 并发转换已发生——重读重试（disabled 对双方都是吸收态，必然终止）
		}
		s.writeCh <- statusWrite{id: accountID, status: st.status, cooldown: st.cooldownUntil, lastErr: st.lastError, weight: nil}
		return
	}
}

func (s *Scheduler) failureEvent(accountID int64, kind rule.Kind, errMsg string) rule.Event {
	v := s.view.Load()
	if v == nil {
		return rule.Event{AccountID: accountID, Kind: kind, ErrorMessage: errMsg}
	}
	byID := v.ByID()
	if byID == nil {
		return rule.Event{AccountID: accountID, Kind: kind, ErrorMessage: errMsg}
	}
	a, ok := byID[accountID]
	if !ok {
		return rule.Event{AccountID: accountID, Kind: kind, ErrorMessage: errMsg}
	}
	av := a.static.Load()
	fp, _ := candidateFingerprint(&av.acc)
	return rule.Event{AccountID: accountID, TemplateID: av.acc.TemplateID, GroupID: groupIDPtr(av.gid), Kind: kind, ErrorMessage: errMsg, CandidateFingerprint: fp, ExpectedRevision: av.acc.LifecycleRevision}
}

// RuleKindOf 连接级/5xx 事件分流（单点 helper，分流外移到调用点——9 处
// 跨包引用：failoverLoop 5xx/0 分支、ws_relay 中继失联/心跳错误、caller 各
// statusOf(err)==0 调用点）：code==0 → network（独立冷却不吃 5xx 10m）；
// ≥500 → 5xx；1-499 为不可达防御（调用点恒 0/≥500，4xx 走骨架透传不至此）
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
// （true = 命中规则有状态动作——Status/Weight/Cooldown 任一非 nil，应投递
// MarkResult 让 worker 精确应用——含窗口条件规则的"可能命中"保守判定）。
// 头透传与 kind 解耦：ResponseCode==nil 且上游带 Retry-After/X-Retry-After 才透，否则不透不伪造。
// 快照未加载/账号快照外 → (domain.RuleThen{}, false)
// （对齐 MarkResult 早退语义——请求路径不可达；本地拒绝不进本机制）。
func (s *Scheduler) Classify(ev rule.Event) (then domain.RuleThen, punish bool) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return domain.RuleThen{}, false
	}
	a, ok := v.ByID()[ev.AccountID]
	if !ok {
		return domain.RuleThen{}, false
	}
	av := a.static.Load() // 静态字段视图一次取用（评审 Critical 修复）
	ev.TemplateID = av.acc.TemplateID
	ev.GroupID = groupIDPtr(av.gid)
	return s.rule.Classify(ev)
}

func groupIDPtr(gid int64) *int64 {
	if gid <= 0 {
		return nil
	}
	return &gid
}

func strPtr(s string) *string { return &s }

// errMsgOr 错误文本回退：errMsg 非空（且截断后非空）用它，否则用默认文案
// （旧语义：429/error 状态机的硬编码 last_error；无文本事件保持原样）。
func errMsgOr(def, errMsg string) string {
	if t := domain.TruncateErrMsg(errMsg); t != "" {
		return t
	}
	return def
}

// FlushRules 同步处理规则引擎队列中的全部事件（仅测试与优雅关闭用）：
// MarkResult 为异步投递，需要立即断言快照的测试先排空队列。
func (s *Scheduler) FlushRules() {
	s.rule.Flush(context.Background())
}

// apply 是规则引擎的动作应用回调（New 时注册）：更新快照状态/冷却/权重、
// EWMA（仅状态类动作）、权重变更时重建组路由（weightedSeq 预生成缓存）、
// 异步 DB 回写。st 为 nil = 只改权重/冷却，不动状态与 EWMA。
// errMsg 为事件错误文本（部署故障修复）：429/unhealthy 落 last_error 用——
// 有文本用文本（域内截断 500），无文本回退既有硬编码文案（旧语义不变）。
func (s *Scheduler) apply(aid int64, st *domain.AccountStatus, cooldownUntil *time.Time, weight *int, errMsg string) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		if s.log != nil {
			s.log.Warn("scheduler apply skipped: snapshot not loaded")
		}
		return
	}
	byID := v.ByID()
	a, ok := byID[aid]
	if !ok {
		return // 快照外账号（已移除/未知）：无状态可改，不投递回写
	}
	// 防复活（T1 P1 评审——探针确定性复现）：disabled 快照的事件回流不得覆盖
	// 状态——规则事件可能在失效/禁用置位**之前**已入队（MarkResult 守卫只拦
	// 置位后的新事件，拦不住入队在先的），apply 照常覆盖会把快照与 DB 回写
	// 重置回 active/unhealthy，账号重新可调度。与 MarkResult 防复活守卫同哲学：
	// disabled 后规则动作整体失效（含 cooldown/weight，且不投递回写——避免
	// 旧状态经合并"后写覆盖先写"覆盖 DB 的 disabled）。
	//
	// copy-on-write CAS：守卫并入转换原子性——读-改-写整体对并发转换
	// （FailAccount/另一 apply）串行化，disabled 检查后到 Store 之间无窗口；
	// CAS 失败 = 并发转换已发生，重读重试（disabled 对双方都是吸收态，重试
	// 必然终止）。cur 恒非 nil（构造即初始化，无 nil 分支）。
	now := s.timeNow()
	var next accState // CAS 成功后持有（enqueueWrite 用）
	for {
		cur := a.runtime.state.Load()
		if cur.status == domain.StatusDisabled {
			return
		}
		next = *cur
		if st != nil {
			next.status = *st
			switch *st {
			case domain.Status429:
				next.errCount++
				next.lastError = strPtr(errMsgOr("upstream 429 rate limited", errMsg))
			case domain.StatusUnhealthy:
				next.errCount++
				next.lastError = strPtr(errMsgOr("upstream error", errMsg))
			case domain.StatusActive:
				if cur.cooldownUntil != nil && !cur.cooldownUntil.Before(now) {
					return
				}
				next.errCount = 0
				next.lastError = nil
			}
			rateDelta := 0.0
			if *st == domain.Status429 || *st == domain.StatusUnhealthy {
				rateDelta = 1
			}
			old := float64(a.runtime.errRate.Load()) / errRateScale
			rate := 0.2*rateDelta + 0.8*old
			a.runtime.errRate.Store(uint64(rate * errRateScale))
		}
		if cooldownUntil != nil {
			next.cooldownUntil = cooldownUntil
		}
		next.lastUsedAt = &now
		if a.runtime.state.CompareAndSwap(cur, &next) {
			break
		}
	}
	if weight != nil {
		s.publisher.mu.Lock()
		curView := s.view.Load()
		if curView != nil && curView.static != nil {
			av := a.static.Load()
			nv := *av
			nv.acc.Weight = *weight
			newLeaf := &accountSnapshot{runtime: a.runtime}
			newLeaf.static.Store(&nv)
			oldByID := curView.static.byID
			oldGroups := curView.static.groups
			newByID := make(map[int64]*accountSnapshot, len(oldByID))
			for k, vv := range oldByID {
				if k == aid {
					newByID[k] = newLeaf
				} else {
					newByID[k] = vv
				}
			}
			newGroups := make(map[int64]*groupSnapshot, len(oldGroups))
			for gid, gs := range oldGroups {
				repl := make([]*accountSnapshot, len(gs.accounts))
				copy(repl, gs.accounts)
				needs := false
				for i, acc := range repl {
					if acc.static.Load().acc.ID == aid {
						repl[i] = newLeaf
						needs = true
					}
				}
				if needs {
					newGroups[gid] = &groupSnapshot{accounts: repl, routes: buildRoutes(repl)}
				} else {
					newGroups[gid] = gs
				}
			}
			sv := &StaticView{groups: newGroups, byID: newByID}
			s.publisher.storeLocked(sv, curView.decision)
		}
		s.publisher.mu.Unlock()
	}
	// 回写前复查 disabled（防 active 回写覆盖 FailAccount 并发置位）——仅对
	// 非 disabled 动作生效：本 apply 动作即 disabled 时必须照常回写（否则
	// disabled 只活内存 ≤30s，全量同步拉回 DB 旧值复活——规则禁用失效，bug
	// 2026-08-18）。st == nil（纯 weight/冷却动作）保留复查（disabled 后规则
	// 动作整体失效语义）。
	// 原 gate M1 设计备注（位置钉扎：weight 锁区之后、紧邻 enqueueWrite）：
	// CAS 成功后本 apply 的 active 回写仍可能晚于 FailAccount 的 disabled 回写
	// 入队（writeback 合并"后写覆盖先写"→ DB 落 active → 重载复活）——复查与
	// 入队指令相邻关闭全部实际可达窗口（此间 FailAccount 完成 CAS+入队则其入队
	// 必然晚于本入队 → 通道序 [active, disabled] 合并取 disabled）；残余窗口
	// （CAS+阻塞入队整体落进复查-入队间隙）为 spec M1b 明示接受（-race 实证可
	// 复现、DB 可短暂落 active）。≤30s 全量同步是坏 DB 写的显形机制而非自愈
	// 承诺；真正兜底是复查-入队相邻 + 内存终态恒 disabled。
	if (st == nil || *st != domain.StatusDisabled) && a.statePtr().status == domain.StatusDisabled {
		return
	}
	s.enqueueWrite(aid, next, weight)
}

// rebuildGroup 重建单组路由的公开包装（持锁委托 rebuildGroupLocked）。
func (s *Scheduler) rebuildGroup(groupID int64) {
	s.publisher.mu.Lock()
	defer s.publisher.mu.Unlock()
	s.rebuildGroupLocked(groupID)
}

// rebuildGroupLocked 重建单组路由（须持 publisher.mu 调用；不碰 DB/账号列表）：
func (s *Scheduler) rebuildGroupLocked(groupID int64) {
	v := s.view.Load()
	if v == nil || v.StaticView() == nil {
		return
	}
	m := v.Groups()
	gs, ok := m[groupID]
	if !ok {
		return
	}
	newM := make(map[int64]*groupSnapshot, len(m))
	for k, vv := range m {
		newM[k] = vv
	}
	newM[groupID] = &groupSnapshot{accounts: gs.accounts, routes: buildRoutes(gs.accounts)}
	sv := &StaticView{groups: newM, byID: v.ByID()}
	s.publisher.storeLocked(sv, v.decision)
	s.RequestCompile()
}

func (s *Scheduler) enqueueWrite(id int64, st accState, weight *int) {
	select {
	case s.writeCh <- statusWrite{id: id, status: st.status, cooldown: st.cooldownUntil, lastErr: st.lastError, weight: weight}:
	default:
		// 队列满：丢弃 DB 回写（内存状态已生效，重启后由下一次请求重新判定）
	}
}
