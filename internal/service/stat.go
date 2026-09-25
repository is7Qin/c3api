// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 统计查询面（spec-stats-p1-backend §5）：/stats/trend、/stats/top、
// /stats/entity-trend、/stats/ttft 四端点 + 用户面 self 两方法的业务入口。
// 职责边界：本层只做参数校验/归一化（哨兵错误 → handler 400），聚合全部 SQL
// 下推（repository.StatRepo StatsTrend 族）——旧 QueryStats 的内存日聚合随
// cube v2 下推删除，不再有客户端合并路径。

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
	"github.com/is7qin/c3api/pkg/logx"
)

// 校验上限常量（spec §5 校验规则；TTFT 双分支各自独立上限——钉死）。统计面
// 的窗口上限已收敛到 domain.StatsKinds 的 CostCap（domain.Admit 判定），此处
// 只留路由观测面（routing.go/routing_frontier.go）共用的跨度上限。
const (
	// MaxStatsTrendSpan 路由观测窗口跨度上限（90 天）——与 KIND 矩阵的 cube
	// 成本上限是同一个 90d（单源：domain.MaxCubeSpan）。
	MaxStatsTrendSpan = domain.MaxCubeSpan

	// DefaultStatsTopLimit top 排行缺省条数（repo 层 ≤0 归一同值，双保险）。
	DefaultStatsTopLimit = 20

	// MaxStatsListLimit top 排行上限钳制（对齐 httpface.ClampLimit(200) 惯例
	// ——service 不 import handler 包，此处同语义本地化：超限裁剪不报错）。
	MaxStatsListLimit = 200

	// ttftQueryBudget TTFT 冷查询预算上界（实测最坏 ~7s；30s 为宽裕封顶
	// ——配合 WithoutCancel 脱钩 leader 取消，见 QueryStatsTTFT 注释）。
	ttftQueryBudget = 30 * time.Second
)

// statEntityTypes 实体类型白名单（与 repository.statEntityCols 键集一致——
// service 层前置拦截为 ErrInvalidInput(400)，repo 层查表失败显式报错双保险）。
var statEntityTypes = map[string]bool{"account": true, "user": true, "key": true}

// statTopByKeys top 排序键白名单（与 repository.statTopSortKeys 键集一致）。
var statTopByKeys = map[string]bool{"cost": true, "requests": true, "tokens": true}

// TrendQuery /stats/trend 入参（GroupID > 0 / Model 非空 = 过滤，零值不过滤）。
// Zone = handler 边界解析过的请求浏览器时区（nil = UTC；绝不接受未校验字符串）。
type TrendQuery struct {
	From        time.Time
	To          time.Time
	Granularity string // hour|day；空 = day
	GroupID     int64
	Model       string
	Zone        *time.Location
}

// TopQuery /stats/top 入参（EntityType ∈ account|user|key；By ∈ cost|requests|tokens）。
// 排行按实体维度分组、无时间桶——恒走 cube 绝对区间查询，Zone 不参与数值。
type TopQuery struct {
	From       time.Time
	To         time.Time
	EntityType string
	By         string
	Limit      int // ≤0 → 20；>200 裁剪到 200
}

// EntityTrendQuery /stats/entity-trend 入参（强制实体过滤 + 可选 Model）。Zone
// 语义同 TrendQuery。
type EntityTrendQuery struct {
	EntityType  string
	EntityID    int64
	From        time.Time
	To          time.Time
	Granularity string // hour|day；空 = day
	Model       string
	Zone        *time.Location
}

// TTFTQuery /stats/ttft 入参。EntityType 空 = 平台级 sketch 分支（cube hist
// 合并）；非空 = 实体级 exact 分支（usage_logs percentile_cont）。
type TTFTQuery struct {
	From       time.Time
	To         time.Time
	EntityType string
	EntityID   int64
	Model      string
}

// StatsRows 一次分组统计读的结果：桶 + 该次读取**实际使用**的执行计划（生效
// 窗口 + 实际存储）。泛型桶类型让 trend（*domain.StatBucket）与 entity-trend
// （*domain.EntityStatBucket）共用同一结果形状。
//
// Exec 随结果一起回传，是响应回显头（spec §4.4(b)）的**唯一**取值来源——handler
// 绝不重算 Admit（判定 owner 只有 domain.Admit 一处，重算即第二份判定，正是本
// spec 的头号病根）。
type StatsRows[B any] struct {
	Buckets []B
	Exec    domain.Exec
}

// QueryStatsTrend 时间趋势：domain.Admit 判定（窗口 → cube/raw → cost →
// coverage）→ 按 Exec.Storage 选 Cube/Raw 方法 → 降级 Warn（节流）。粒度白名单
// 在判定之后（与既有校验序一致）。
func (s *Service) QueryStatsTrend(ctx context.Context, q TrendQuery) (StatsRows[*domain.StatBucket], error) {
	exec, err := s.admitStats(domain.KindTrend, q.Zone, q.From, q.To)
	if err != nil {
		return StatsRows[*domain.StatBucket]{}, err
	}
	unit, err := normalizeGranularity(q.Granularity)
	if err != nil {
		return StatsRows[*domain.StatBucket]{}, err
	}
	var rows []*domain.StatBucket
	if exec.Storage == domain.StatsStorageCube {
		rows, err = s.store.StatsTrendCube(ctx, exec.From, exec.To, unit, q.GroupID, q.Model, exec.Zone)
	} else {
		rows, err = s.store.StatsTrendRaw(ctx, exec.From, exec.To, unit, q.GroupID, q.Model, exec.Zone)
	}
	return StatsRows[*domain.StatBucket]{Buckets: rows, Exec: exec}, err
}

// QueryStatsTop 实体排行（limit 归一化后透传；排序键/实体类型白名单前置拦截）。
// KindTop 无分组、只有 cube 一种候选存储（StatsKinds 的 Storages[0]），故执行
// 方法单一——窗口仍取 Exec（判定与执行同源）。
func (s *Service) QueryStatsTop(ctx context.Context, q TopQuery) ([]*domain.EntityStatBucket, error) {
	exec, err := s.admitStats(domain.KindTop, time.UTC, q.From, q.To)
	if err != nil {
		return nil, err
	}
	if !statEntityTypes[q.EntityType] {
		return nil, ErrInvalidInput
	}
	if !statTopByKeys[q.By] {
		return nil, ErrInvalidInput
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultStatsTopLimit
	}
	return s.store.StatsTop(ctx, exec.From, exec.To, q.EntityType, q.By, min(limit, MaxStatsListLimit))
}

// QueryEntityTrend 单实体时间趋势（实体类型白名单前置拦截；EntityID 合法性由
// 数据语义兜底——卷积表无 ID=0 行，零值自然返回空集）。判定/执行/告警同
// QueryStatsTrend。
func (s *Service) QueryEntityTrend(ctx context.Context, q EntityTrendQuery) (StatsRows[*domain.EntityStatBucket], error) {
	exec, err := s.admitStats(domain.KindEntityTrend, q.Zone, q.From, q.To)
	if err != nil {
		return StatsRows[*domain.EntityStatBucket]{}, err
	}
	unit, err := normalizeGranularity(q.Granularity)
	if err != nil {
		return StatsRows[*domain.EntityStatBucket]{}, err
	}
	if !statEntityTypes[q.EntityType] {
		return StatsRows[*domain.EntityStatBucket]{}, ErrInvalidInput
	}
	var rows []*domain.EntityStatBucket
	if exec.Storage == domain.StatsStorageCube {
		rows, err = s.store.StatsEntityTrendCube(ctx, exec.From, exec.To, unit, q.EntityType, q.EntityID, q.Model, exec.Zone)
	} else {
		rows, err = s.store.StatsEntityTrendRaw(ctx, exec.From, exec.To, unit, q.EntityType, q.EntityID, q.Model, exec.Zone)
	}
	return StatsRows[*domain.EntityStatBucket]{Buckets: rows, Exec: exec}, err
}

// QueryStatsTTFT TTFT 分位数卡片，双分支各自独立的 kind（成本/覆盖随行）：
//   - EntityType == ""：sketch 分支（cube hist 服务端合并），成本上限 = 矩阵的
//     cube 90d（2160 小时桶，与桶数上限是同一窗口的两种表述）；
//   - 非空：exact 分支（usage_logs percentile_cont），必须配 EntityID ≠ 0 且
//     entityType 过白名单，成本上限 = 矩阵的 raw 168h。
//
// **Admit 在 statsTTFTC 缓存之前**（spec §4.5 硬约束）：被拒绝的窗口不得命中
// 旧缓存。校验通过后经 TTL 缓存（验收遗留尾巴：exact 冷缓存 × 系统饱和排序致
// 负载 p99 5-6s；仪表盘同参轮询命中率天然高，陈旧 ≤30s 为展示面可接受语义
// ——overview 先例）。
func (s *Service) QueryStatsTTFT(ctx context.Context, q TTFTQuery) (*domain.TTFTSummary, error) {
	kind := domain.KindTTFTSketch
	if q.EntityType != "" {
		kind = domain.KindTTFTExact
	}
	exec, err := s.admitStats(kind, time.UTC, q.From, q.To)
	if err != nil {
		return nil, err
	}
	if q.EntityType != "" && (!statEntityTypes[q.EntityType] || q.EntityID == 0) {
		return nil, ErrInvalidInput
	}
	key := q.EntityType + "|" + strconv.FormatInt(q.EntityID, 10) + "|" + q.Model + "|" +
		strconv.FormatInt(q.From.Unix(), 10) + "|" + strconv.FormatInt(q.To.Unix(), 10)
	return statsTTFTC.fetch(key, func() (*domain.TTFTSummary, error) {
		// fn 由首个请求的 ctx 触发，但结果服务同键全部等待者——leader 取消
		// 不得连坐。脱钩后以 30s 预算封顶（冷查询最坏实测 ~7s；裸 WithoutCancel
		// 无界是 AGENTS.md 反模式 明令禁止形态）。
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ttftQueryBudget)
		defer cancel()
		if q.EntityType == "" {
			return s.store.StatsTTFTSketch(qctx, exec.From, exec.To, q.Model)
		}
		return s.store.StatsTTFTExact(qctx, exec.From, exec.To, q.EntityType, q.EntityID, q.Model)
	})
}

// UserStats 用户台自己的用量趋势：忽略调用方传入的任何 entity 参数，userID
// 钉死注入（JWT 身份即过滤条件，防越权只看 service 层这一道钉死）。
func (s *Service) UserStats(ctx context.Context, userID int64, q EntityTrendQuery) (StatsRows[*domain.EntityStatBucket], error) {
	q.EntityType = "user"
	q.EntityID = userID
	return s.QueryEntityTrend(ctx, q)
}

// UserStatsTTFT 用户台自己的 TTFT 卡片（self 钉死同 UserStats；恒走 exact 分支）。
func (s *Service) UserStatsTTFT(ctx context.Context, userID int64, q TTFTQuery) (*domain.TTFTSummary, error) {
	q.EntityType = "user"
	q.EntityID = userID
	return s.QueryStatsTTFT(ctx, q)
}

// StatsCapabilities 能力端点（spec §4.4(a)）的数据面：把 domain 的
// StatsKinds × Retention 机械投影回传。保留期取自本 Service 注入的那一份
// ——与 admitStats 传给 domain.Admit 的是**同一份内存**，故报告与实际判定不会
// 漂移（构造同源，不靠测试保证）。
func (s *Service) StatsCapabilities() domain.Capabilities {
	return domain.StatsCapabilities(s.retention)
}

// ——————— 统计窗口判定接线（spec §4.1 判定 / §4.3 可观测 / §4.4(d) 错误身份） ———————

// StatsWindowError 统计窗口拒绝的线缆载体——**类型别名**指向
// serviceerr.StatsWindowError（同一类型的第二个名字，不是第二套类型；与
// ErrInvalidInput 等哨兵的 re-export 同一手法）。载体必须住在叶子包里才对
// httpface 可见：httpface 不能 import internal/service（service → auth →
// httpface 已成环），详见该类型注释。
type StatsWindowError = serviceerr.StatsWindowError

// statsWindowErr domain 判定结果 → 线缆错误（nil 透传）。
func statsWindowErr(err *domain.StatsWindowError) error {
	if err == nil {
		return nil
	}
	return &StatsWindowError{StatsWindowError: *err}
}

// admitStats 判定 + 降级告警的**唯一接线点**：调 domain.Admit（唯一判定入口，
// 纯函数：保留期与时钟都以参数传入），拒绝时转成线缆载体错误，降级/位移时按
// 节流规则发 Warn。调用点因此在同一视线范围内完成「判定 → 读取 Exec.Storage →
// 执行 store 的 Cube/Raw 方法」。
func (s *Service) admitStats(kind domain.StatsKindID, zone *time.Location, from, to time.Time) (domain.Exec, error) {
	exec, werr := domain.Admit(kind, s.retention, zone, from, to, s.statsClock())
	if werr != nil {
		return domain.Exec{}, statsWindowErr(werr)
	}
	s.warnStatsPlan(kind, zone, from, exec)
	return exec, nil
}

// statsClock 判定与 Warn 节流共用的当前时间源（statsNow 注入；nil = time.Now）。
func (s *Service) statsClock() time.Time {
	if s.statsNow != nil {
		return s.statsNow()
	}
	return time.Now()
}

// warnStatsPlan 降级/位移告警（spec §4.3；Admit 自身不打日志——domain 零 logx
// 依赖，可观测与判定同位置=本包装）：
//   - Raw 降级：`stats: falling back to raw rows`，字段 reason（offset|dst|
//     span_below_grid）/kind/timezone/from/to/span_seconds；
//   - 窗口位移：`stats: window shifted to hour boundary`，字段 kind/timezone/
//     requested_from/effective_from/shift_seconds。
//
// 精确路径与拒绝路径都零 Warn（后者的可观测性由 400 机读字段承载，也不与节流
// 规则纠缠）。s.log 是可选字段（测试与降级路径可字面量构造 Service）——**必须
// 判空**：pkg/logx 的 Warn 直接解引用接收者。
func (s *Service) warnStatsPlan(kind domain.StatsKindID, zone *time.Location, reqFrom time.Time, exec domain.Exec) {
	if s.log == nil || exec.Reason == domain.StatsPlanExact {
		return
	}
	zoneName := zoneLabel(zone)
	if exec.Reason == domain.StatsPlanWindowShifted {
		if !s.warnAllowed(kind, zoneName, "shift") {
			return
		}
		s.log.Warn("stats: window shifted to hour boundary",
			logx.String("kind", string(kind)),
			logx.String("timezone", zoneName),
			logx.String("requested_from", reqFrom.UTC().Format(time.RFC3339)),
			logx.String("effective_from", exec.From.UTC().Format(time.RFC3339)),
			logx.Int64("shift_seconds", int64(exec.From.Sub(reqFrom)/time.Second)))
		return
	}
	if !s.warnAllowed(kind, zoneName, exec.Reason.String()) {
		return
	}
	s.log.Warn("stats: falling back to raw rows",
		logx.String("reason", exec.Reason.String()),
		logx.String("kind", string(kind)),
		logx.String("timezone", zoneName),
		logx.String("from", exec.From.UTC().Format(time.RFC3339)),
		logx.String("to", exec.To.UTC().Format(time.RFC3339)),
		logx.Int64("span_seconds", int64(exec.To.Sub(exec.From)/time.Second)))
}

// warnThrottleKey Warn 节流键：同 (kind, zone 名, reason) 每分钟至多一条。
type warnThrottleKey struct {
	kind   domain.StatsKindID
	zone   string
	reason string
}

// warnThrottleEvery 节流窗口（spec §4.3）：日志量级上界 = kind 数 × zone 数 ×
// reason 数 / 分钟，与请求量无关（Asia/Kolkata 整时区恒 raw 的部署不再按请求刷屏）。
const warnThrottleEvery = time.Minute

// warnAllowed 判定并登记节流。状态挂在 Service 上（**不用包级单例**：测试构造
// 大量 Service，包级节流态会在用例间泄漏，把"恰好一条 Warn"的断言变成顺序依赖
// 的 flake；节流态的生命周期本就该随 Service 而非随进程）。
func (s *Service) warnAllowed(kind domain.StatsKindID, zone, reason string) bool {
	now := s.statsClock().Unix()
	key := warnThrottleKey{kind: kind, zone: zone, reason: reason}
	s.warnThrottleMu.Lock()
	defer s.warnThrottleMu.Unlock()
	if s.warnThrottle == nil {
		s.warnThrottle = map[warnThrottleKey]int64{}
	}
	if last, ok := s.warnThrottle[key]; ok && now-last < int64(warnThrottleEvery/time.Second) {
		return false
	}
	s.warnThrottle[key] = now
	return true
}

// zoneLabel Warn 的 timezone 字段（nil = UTC；与 repository 的 zoneName 同语义）。
func zoneLabel(zone *time.Location) string {
	if zone == nil {
		return "UTC"
	}
	return zone.String()
}

// normalizeGranularity 粒度白名单归一化：空 = day（缺省）；hour/day 原样；
// 其余 → ErrInvalidInput（repo 层查表失败显式报错双保险）。
func normalizeGranularity(g string) (string, error) {
	switch g {
	case "":
		return "day", nil
	case "hour", "day":
		return g, nil
	default:
		return "", ErrInvalidInput
	}
}

// ResolveTimeZone 统计请求 `timezone` 查询参数边界解析（request-browser-timezone-stats）：
// 缺省/空 → UTC（兼容旧客户端）；IANA 名经 time.LoadLocation 校验；未知名 /
// Go 特有的 "Local"（无 PG 对应物，绝不能进 AT TIME ZONE 绑定）→
// ErrInvalidInput（httpface 400）。返回已解析 *time.Location（携带规范名，
// 下游只见类型化值，永不见原始串）；进程零全局态，Location 不可变。
func ResolveTimeZone(raw string) (*time.Location, error) {
	if raw == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(raw)
	if err != nil || loc == time.Local {
		return nil, fmt.Errorf("service: invalid timezone %q: %w", raw, ErrInvalidInput)
	}
	return loc, nil
}

// —— stats.ttft TTL 缓存（spec-ttft-cache-2026-08-23）——

// ttftCacheTTL 对齐 overview TTL 30s 先例：展示面数据陈旧上界。
const ttftCacheTTL = 30 * time.Second

// ttftCacheMaxEnt 条目上限：防变窗请求撑爆内存，满则整体重置（展示面粗粒度
// 防线——精确 LRU 的复杂度不为此处买单）。
const ttftCacheMaxEnt = 4096

type ttftCacheEntry struct {
	summary *domain.TTFTSummary
	expires time.Time
}

// ttftCacheCall 同键并发冷查询的去重句柄：等待方经 done close 的
// happens-before 语义读取结果，fn 只执行一次。
type ttftCacheCall struct {
	done    chan struct{}
	summary *domain.TTFTSummary
	err     error
}

// ttftCache 进程内 TTL 缓存 + inflight 去重。包级单例 statsTTFTC 挂载；
// now 可注入供测试推进时钟。
type ttftCache struct {
	ttl    time.Duration
	maxEnt int
	now    func() time.Time
	mu     sync.Mutex
	done   map[string]ttftCacheEntry
	calls  map[string]*ttftCacheCall
}

func newTTFTCache() *ttftCache {
	return &ttftCache{
		ttl:    ttftCacheTTL,
		maxEnt: ttftCacheMaxEnt,
		now:    time.Now,
		done:   map[string]ttftCacheEntry{},
		calls:  map[string]*ttftCacheCall{},
	}
}

// fetch 命中未过期缓存直接返回；未命中执行 fn（同键并发合并为单次）；仅
// 成功结果入缓存——瞬时 DB 抖动的错误不得钉死整个 TTL 窗口。
func (c *ttftCache) fetch(key string, fn func() (*domain.TTFTSummary, error)) (*domain.TTFTSummary, error) {
	c.mu.Lock()
	if e, ok := c.done[key]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return e.summary, nil
	}
	if call, ok := c.calls[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.summary, call.err
	}
	call := &ttftCacheCall{done: make(chan struct{})}
	c.calls[key] = call
	c.mu.Unlock()

	c.settle(key, call, fn)
	return call.summary, call.err
}

// settle 执行 fn 并收尾发布。发布顺序铁律：**字段写入必须全部先于
// close(done)** ——close 的 happens-before 边只覆盖此前写入，颠倒即等待方
// 读到撕裂/空值的数据竞争（RG 审计，-race 实测复现）。
// panic 兜底：store 层 panic 被 handler Recoverer 兜住时进程存活，
// 等待方不得永久阻塞在未 close 的 done 上——以错误形态传播给等待方后原样
// 重抛给 leader。
func (c *ttftCache) settle(key string, call *ttftCacheCall, fn func() (*domain.TTFTSummary, error)) {
	defer func() {
		if r := recover(); r != nil {
			c.finish(key, call, nil, fmt.Errorf("stats ttft: underlying query panic: %v", r))
			panic(r)
		}
		c.finish(key, call, call.summary, call.err)
	}()
	call.summary, call.err = fn()
}

// finish 收尾三步的唯一点：清 inflight → 成功才入缓存（含容量重置）→ 写字段
// → close 发布。
func (c *ttftCache) finish(key string, call *ttftCacheCall, summary *domain.TTFTSummary, err error) {
	c.mu.Lock()
	delete(c.calls, key)
	if err == nil {
		if len(c.done) >= c.maxEnt {
			c.done = map[string]ttftCacheEntry{}
		}
		c.done[key] = ttftCacheEntry{summary: summary, expires: c.now().Add(c.ttl)}
	}
	c.mu.Unlock()
	call.summary = summary
	call.err = err
	close(call.done)
}

var statsTTFTC = newTTFTCache()
