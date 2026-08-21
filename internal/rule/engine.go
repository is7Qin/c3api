// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package rule 实现规则引擎（可编排状态管理）：事件 → 有界 channel → 规则 worker
// （priority 首中匹配）→ 状态/冷却/权重更新。scheduler.MarkResult 的硬编码状态机
// 由本包替代：scheduler 只做禁用守卫 + 事件投递（条件投递），动作应用经 SetApply
// 注册的回调完成（更新快照 + EWMA + 异步回写，属 scheduler 侧）。
package rule

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// Kind 事件类别（单一 kind 概念；连接级/5xx 分流由 scheduler.RuleKindOf 在
// 调用点完成——scheduler 不再有第二套枚举）。
type Kind int

const (
	KindOK Kind = iota
	Kind429
	Kind4xx
	Kind5xx
	KindNetwork // 连接级（code==0）事件——独立类型，不吃 5xx 冷却（用户裁决）
)

func (k Kind) String() string {
	switch k {
	case KindOK:
		return "ok"
	case Kind429:
		return "429"
	case Kind4xx:
		return "4xx"
	case Kind5xx:
		return "5xx"
	case KindNetwork:
		return "network"
	}
	return "unknown"
}

// kindFromString when.kind 字符串 → 事件类别；未知值返回负值（永不匹配，
// 非法 kind 在 ValidateWhen 已被拒绝）。
func kindFromString(s string) Kind {
	switch s {
	case "ok":
		return KindOK
	case "429":
		return Kind429
	case "4xx":
		return Kind4xx
	case "5xx":
		return Kind5xx
	case "network":
		return KindNetwork
	}
	return -1
}

// Event 请求结果事件（由 scheduler.MarkResult 构造投递）。
type Event struct {
	AccountID    int64
	TemplateID   int64
	GroupID      *int64
	Model        string
	Kind         Kind
	HTTPStatus   *int
	ErrorMessage string
	ResetAt      *time.Time
	OccurredAt   time.Time // 零值由引擎填充为当前时间
}

// ApplyFunc 动作应用回调（由 scheduler 注册）：st 为 nil = 不改状态（只改权重）；
// cooldownUntil 为 nil = 不设冷却；weight 为 nil = 不改权重。errMsg 为事件
// 错误文本（error_message_contains 已匹配；供 last_error 落库——部署故障
// 修复：scheduler 侧截断 500 后回写）。
type ApplyFunc func(aid int64, st *domain.AccountStatus, cooldownUntil *time.Time, weight *int, errMsg string)

// Config 引擎配置。
type Config struct {
	EventQueueSize int // 事件队列容量，默认 4096
}

// defaultWindowSeconds 未配 window_seconds 的规则默认统计窗口。
const defaultWindowSeconds = 60

// ruleDropWarnThreshold 事件丢弃累计告警阈值（热点修复 B，对齐 errlog 模式）：
// 丢弃计数 ≥ 阈值后 Warn 恰好一次（有界队列风暴丢弃的可观测面；队列排空后
// 边沿回落——每风暴一次，不刷屏）。默认 10_000（对齐 errlog 默认且风暴实测
// 12,355 恰可触发一次）。var（非 const）：测试注入小阈值。
var ruleDropWarnThreshold int64 = 10_000

// compiledRule 预编译规则：domain.Rule 纯数据 + 三 Set 只读视图。
// 编译后只读无锁，Classify 热路径零分配。
type compiledRule struct {
	domain.Rule
	httpStatusSet *intSet
	modelSet      *stringSet
	containsSet   *substringSet
}

func compileRule(r domain.Rule) (compiledRule, error) {
	httpSet, err := newIntSet(r.When.HTTPStatusIn)
	if err != nil {
		return compiledRule{}, err
	}
	mSet, err := newStringSet(r.When.ModelIn)
	if err != nil {
		return compiledRule{}, err
	}
	cSet, err := newSubstringSet(r.When.ErrorMessageContainsIn)
	if err != nil {
		return compiledRule{}, err
	}
	return compiledRule{Rule: r, httpStatusSet: httpSet, modelSet: mSet, containsSet: cSet}, nil
}

func compileRules(in []domain.Rule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(in))
	for _, r := range in {
		cr, err := compileRule(r)
		if err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, nil
}

// RuleEngine 规则引擎：加载 enabled 规则（priority 升序）、逐规则首中匹配、
// 窗口计数维护与 worker 消费循环（Name/Start/Close 见 worker.go）。
type RuleEngine struct {
	cfg   Config
	store repository.RuleStore
	log   *logx.Logger

	ch      chan Event
	dropped atomic.Uint64
	// warnDropped 丢弃告警边沿（热点修复 B）：≥ 阈值告警恰好一次；队列排空
	// 后回落（resetDropWarnIfDrained）——每风暴一次，不刷屏。
	warnDropped atomic.Bool

	apply   ApplyFunc
	applyMu sync.RWMutex

	rules   []compiledRule // enabled、priority 升序（预编译）
	rulesMu sync.RWMutex

	needsOK atomic.Bool

	wm windowMap

	timeNow   func() time.Time
	startOnce atomic.Bool
}

// New 只建结构（不加载规则、不注册 apply——分别由 Reload/SetApply 显式完成）。
func New(cfg Config, store repository.RuleStore, log *logx.Logger) *RuleEngine {
	q := cfg.EventQueueSize
	if q <= 0 {
		q = 4096
	}
	return &RuleEngine{
		cfg:     cfg,
		store:   store,
		log:     log,
		ch:      make(chan Event, q),
		timeNow: time.Now,
	}
}

// SetApply 注册动作应用回调（scheduler 构造期注入；可重复调用覆盖）。
func (e *RuleEngine) SetApply(fn ApplyFunc) {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	e.apply = fn
}

// NeedsOKEvents 规则表中是否存在需要 ok 事件投递的规则（when.kind 为 nil 或 "ok"）——
// scheduler 据此条件投递（C1：种子恢复规则 kind=ok 必须投递，否则成功恢复永不触发）。
func (e *RuleEngine) NeedsOKEvents() bool { return e.needsOK.Load() }

// Reload 全量加载 enabled 规则（priority 升序）；空表先写种子（seedRules）。
// 失败返回 error——规则表是状态管理唯一路径，main 收到错误即 fatalf。
func (e *RuleEngine) Reload(ctx context.Context) error {
	rules, err := e.store.ListRules(ctx, boolPtr(true))
	if err != nil {
		return fmt.Errorf("rule engine reload: %w", err)
	}
	if len(rules) == 0 {
		if err := e.seedRules(ctx); err != nil {
			return err
		}
		rules, err = e.store.ListRules(ctx, boolPtr(true))
		if err != nil {
			return fmt.Errorf("rule engine reload after seed: %w", err)
		}
	}
	// store 契约已保证 priority 升序；防御性再排一次。
	slices.SortFunc(rules, func(a, b domain.Rule) int { return a.Priority - b.Priority })

	needsOK := false
	maxWindow := 0
	for _, r := range rules {
		if r.When.Kind == nil || *r.When.Kind == "ok" {
			needsOK = true
		}
		if r.When.WindowSeconds != nil && *r.When.WindowSeconds > maxWindow {
			maxWindow = *r.When.WindowSeconds
		}
	}
	compiled, err := compileRules(rules)
	if err != nil {
		return err
	}
	e.rulesMu.Lock()
	e.rules = compiled
	e.rulesMu.Unlock()
	e.needsOK.Store(needsOK)
	// 窗口重建：覆盖规则集最大窗口；计数清零（重载语义，规则变更后旧计数不可信）。
	e.wm.reset(time.Duration(maxWindow)*time.Second, needsOK)
	return nil
}

// seedRules 规则表为空时写入种子规则（fresh setup 哲学，用户裁决；kind=error
// 旧规则不迁移——管理面重建；指针即意图 ResponseCode/CustomMessage nil=透传）：
//
//	seed-429（p10）      kind=429   → status=429 + cooldown 30s + ResponseCode nil(透429) + CustomMessage "rate limited"（码透文不透）
//	seed-4xx-400（p15）  kind=4xx + http_status=400 → ResponseCode nil + CustomMessage nil（400 全透；其余 4xx 默认归一 502——无规则即归一；直插 store，其 Then{} 与用户规则 Then{} 全透语义等价）
//	seed-5xx（p20）      kind=5xx   → status=unhealthy + cooldown 10m + 502/"Upstream request failed"（用户裁决归一）
//	seed-network（p25）  kind=network → status=unhealthy + cooldown 5s + 502/"Upstream request failed"
//	                       （连接级独立类型——原连接级 5s 语义，不吃 10m）
//	seed-ok（p30）       kind=ok    → status=active 无冷却（恢复）
//
// 启动 guard 检测旧列 Transmit：fresh setup 下旧列若存在则需重建 DB（本 Task 仅注释占位，真实 DB 检测后续承载）。
//
// 多实例种子幂等（设计文档 §1.5 / R2）：两实例同时空表启动 → 双双进入本方法，
// name/priority 唯一约束（ent schema 已有）保证只有一个实例的插入成功；失败方
// 收到 ErrConflict——忽略继续（各实例插入同一份种子，并集收敛为完整五份，
// Reload 随后重列规则集）。不做 SELECT 后再插的"先查后写"（查与写之间仍有
// 竞态窗口，唯一约束兜底才是治本）。
func (e *RuleEngine) seedRules(ctx context.Context) error {
	n, err := e.store.CountRules(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	seeds := []domain.Rule{
		{
			Name: "seed-429", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("429")},
			Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s"), CustomMessage: strPtr("rate limited")},
		},
		{
			Name: "seed-4xx-400", Enabled: true, Priority: 15,
			When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: intPtr(400)},
			Then: domain.RuleThen{}, // ResponseCode nil + CustomMessage nil = 全透（种子特例，直插）
		},
		{
			Name: "seed-5xx", Enabled: true, Priority: 20,
			When: domain.RuleWhen{Kind: strPtr("5xx")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("10m"), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")},
		},
		{
			Name: "seed-network", Enabled: true, Priority: 25,
			When: domain.RuleWhen{Kind: strPtr("network")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("5s"), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")},
		},
		{
			Name: "seed-ok", Enabled: true, Priority: 30,
			When: domain.RuleWhen{Kind: strPtr("ok")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusActive)},
		},
	}
	for _, s := range seeds {
		if _, err := e.store.CreateRule(ctx, s); err != nil {
			if errors.Is(err, repository.ErrConflict) {
				continue // 并发实例已插同名/同 priority 种子（R2）：跳过，并集收敛
			}
			return fmt.Errorf("create seed rule %s: %w", s.Name, err)
		}
	}
	return nil
}

// ReloadRules 规则表全量重载（invalidate.RulesReloader 适配，#14 T3a 装配
// invalidate.Config.Rules）：与 Reload 同一实现——重载清窗口计数，全实例同步
// 执行语义（设计文档 §1.5，NOTIFY Rules:true 远端变更触发）。
func (e *RuleEngine) ReloadRules(ctx context.Context) error { return e.Reload(ctx) }

// matchBasic 编译后热路径：1 级缩进 6 行单值+Set 早退，零分配。
func (e *RuleEngine) matchBasic(ev Event, r compiledRule) bool {
	if r.When.Kind != nil && kindFromString(*r.When.Kind) != ev.Kind {
		return false
	}
	if r.When.AccountID != nil && ev.AccountID != *r.When.AccountID {
		return false
	}
	if r.When.TemplateID != nil && ev.TemplateID != *r.When.TemplateID {
		return false
	}
	if r.When.GroupID != nil && (ev.GroupID == nil || *ev.GroupID != *r.When.GroupID) {
		return false
	}
	if r.When.HTTPStatus != nil && (ev.HTTPStatus == nil || *r.When.HTTPStatus != *ev.HTTPStatus) {
		return false
	}
	if r.httpStatusSet != nil && !r.httpStatusSet.contains(ev.HTTPStatus) {
		return false
	}
	if r.When.Model != nil && *r.When.Model != ev.Model {
		return false
	}
	if r.modelSet != nil && !r.modelSet.contains(ev.Model) {
		return false
	}
	if r.When.ErrorMessageContains != nil && !strings.Contains(ev.ErrorMessage, *r.When.ErrorMessageContains) {
		return false
	}
	if r.containsSet != nil && !r.containsSet.contains(ev.ErrorMessage) {
		return false
	}
	return true
}

// matchWindow 窗口条件判定（非热路径，次数/比例）。
func matchWindow(w domain.RuleWhen, wc windowSnapshot) bool {
	if w.Count429GE != nil && wc.t429 < *w.Count429GE {
		return false
	}
	if w.CountFailureGE != nil && wc.failure < *w.CountFailureGE {
		return false
	}
	if w.CountOKGE != nil && wc.ok < *w.CountOKGE {
		return false
	}
	if w.CountTotalGE != nil && wc.total() < *w.CountTotalGE {
		return false
	}
	if w.Ratio429GE != nil && !ratioPass(wc.t429, wc.total(), w.CountTotalGE, *w.Ratio429GE) {
		return false
	}
	if w.RatioFailureGE != nil && !ratioPass(wc.failure, wc.total(), w.CountTotalGE, *w.RatioFailureGE) {
		return false
	}
	return true
}

// HandleEvent 同步处理单个事件：窗口计数 → 逐规则 Match（首中）→ ApplyFunc。
// worker 消费循环与测试共用。命中不清零窗口计数（C2）——滑动自然衰减，
// 升级阶梯（如 60s 内 ≥5 error → 更重惩罚）不被低阈值规则清零阻断。
// 未命中仅更新计数。
func (e *RuleEngine) HandleEvent(ctx context.Context, ev Event) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = e.timeNow()
	}
	e.wm.Add(ev)

	e.rulesMu.RLock()
	rules := e.rules
	e.rulesMu.RUnlock()

	for _, r := range rules {
		wc := windowSnapshot{}
		if ruleNeedsWindow(r.When) {
			wc = e.wm.Snapshot(ev.AccountID, ruleWindowSeconds(r.When), ev.OccurredAt)
		}
		if !e.matchBasic(ev, r) {
			continue
		}
		if !matchWindow(r.When, wc) {
			continue
		}
		st, cd, w := Apply(r.Then, ev)
		e.applyMu.RLock()
		fn := e.apply
		e.applyMu.RUnlock()
		if fn != nil {
			fn(ev.AccountID, st, cd, w, ev.ErrorMessage)
		}
		return
	}
}

func boolPtr(b bool) *bool                                   { return &b }
func intPtr(v int) *int                                      { return &v }
func strPtr(s string) *string                                { return &s }
func statusPtr(s domain.AccountStatus) *domain.AccountStatus { return &s }

// Classify 事件分类决策（错误分支响应/投递决策——scheduler 包装调用，用户面
// err_logs 行级脱敏亦复用）：遍历 enabled 规则（priority 升序首中），首个
// "非窗口条件维度"（kind/http_status/message_contains/account/template/group/
// model）命中者决定结果。窗口条件规则（count_*/ratio_*，ruleNeedsWindow）依赖
// 历史计数，预判不可得——按"可能命中"保守处理（不参与判定，窗口阈值由 worker
// Match 精确裁决；prejudge 命中 → punish 保证事件投递，worker 再精确应用）。
// 指针即意图：then.ResponseCode nil=透传上游码，non-nil=覆写；then.CustomMessage nil=透传上游文，non-nil=覆写；头透传与 kind 解耦（ResponseCode==nil 才透）。
// 返回 then 值拷贝（调用方只读不得修改）与 punish（true = 命中规则有状态动作 Status/Weight/Cooldown 任一非 nil——应投递
// MarkResult；漏判 Cooldown 则 cooldown-only 规则永不投递，冷却静默丢弃，
// 2026-08-19 缺陷 1 根因）。无命中 → (domain.RuleThen{}, false)（默认归一 502+generic，安全默认
// ——不认识的错误不透传）。
// 零分配：仅读规则集切片（RLock 快照）+ 字符串比较。
func (e *RuleEngine) Classify(ev Event) (then domain.RuleThen, punish bool) {
	e.rulesMu.RLock()
	rules := e.rules
	e.rulesMu.RUnlock()
	for _, r := range rules {
		if !e.matchBasic(ev, r) {
			continue
		}
		return r.Then, r.Then.Status != nil || r.Then.Weight != nil || r.Then.Cooldown != nil
	}
	// 无规则命中 → 默认归一 502/"upstream rejected request"（安全默认，不透传）；ok 事件不归一（透传语义，成功不处理）
	if ev.Kind == KindOK {
		return domain.RuleThen{}, false
	}
	return domain.RuleThen{ResponseCode: intPtr(502), CustomMessage: strPtr("upstream rejected request")}, false
}

// UnifiedMessage 统一公式 msg=CustomMessage!=nil?*CustomMessage:upstream
// 响应与 sanitize 同源（I-3），代理日志保留原文边界另述
// TODO: upstream param unused — kept for formula parity (honest return would be upstream when CustomMessage==nil; callers currently handle passthrough separately)
func UnifiedMessage(then domain.RuleThen, upstream string) (string, bool) {
	if then.CustomMessage != nil {
		return *then.CustomMessage, true
	}
	return "", false
}

