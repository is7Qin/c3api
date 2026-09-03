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
)

// 校验上限常量（spec §5 校验规则；TTFT 双分支各自独立上限——Momus M5 钉死）。
const (
	// MaxStatsTrendSpan trend/top/entity-trend 共用窗口跨度上限（90 天）：
	// cube 查询按小时桶扫描，90d × 维度基数是交互式端点的合理上界。
	MaxStatsTrendSpan = 90 * 24 * time.Hour

	// MaxStatsSketchBuckets sketch 分支桶数上限 = 2160（= 90d × 24 小时桶，
	// 与 MaxStatsTrendSpan 自洽——同一窗口两种表述）。sketch 走 cube hist
	// 服务端合并（array_agg 带回逐行直方图），桶数直接决定合并成本。
	MaxStatsSketchBuckets = 2160

	// MaxStatsTTFTExactSpan exact 分支窗口跨度上限（168h = 7 天）：打
	// usage_logs 原始行 percentile_cont，无预聚合保护，窗口必须远小于走
	// 预聚合 cube 的 sketch 分支。
	MaxStatsTTFTExactSpan = 168 * time.Hour

	// DefaultStatsTopLimit top 排行缺省条数（repo 层 ≤0 归一同值，双保险）。
	DefaultStatsTopLimit = 20

	// MaxStatsListLimit top 排行上限钳制（对齐 httpface.ClampLimit(200) 惯例
	// ——service 不 import handler 包，此处同语义本地化：超限裁剪不报错）。
	MaxStatsListLimit = 200

	// ttftQueryBudget TTFT 冷查询预算上界（P3 实测最坏 ~7s；30s 为宽裕封顶
	// ——配合 WithoutCancel 脱钩 leader 取消，见 QueryStatsTTFT 注释）。
	ttftQueryBudget = 30 * time.Second

	// MaxStatsRawSpan 浏览器时区原始行分组路径窗口上限的缺省值（step 7 裁决：
	// 支持 horizon 对齐既有关于保留期，宁可 400 不静默残缺）。非精确时区或
	// 窗口界劈开卷积行的分组读走 usage_logs + err_logs 原始行（repository
	// stat_raw_read.go）——受 usage.log_retention_days（默认 30d）与
	// usage.errlog_retention_days（默认 7d，日级分区 + 1 天 DST/日界余量）
	// 双重约束；取两者都完整覆盖的最保守窗口 8 天（7d errlog + 1d DST 日历
	// 余量——overview 7 日窗在 fall-back 日本地跨度 24h+1h×7 ≤ 8d）。实际
	// horizon 由 Service.statsRawSpan 承载（New 缺省本值，main 经
	// SetStatsRawSpan(min(log, errlog) 正保留) 换算部署配置——配置更短
	// 则更严，配置更长不放水超出本文档化的保守窗口之外仍按 days+1 计）。恒
	// 整点无 DST 时区且双界对齐（含 UTC 缺省）走 cube 重组，窗口维持
	// MaxStatsTrendSpan（90d，cube 保留 180d）。部署若把 errlog/usage 保留期
	// 调低，老桶自然缺行——OpenAPI 描述与 repository 注释均文档化该耦合。
	MaxStatsRawSpan = 8 * 24 * time.Hour
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

// QueryStatsTrend 时间趋势（校验顺序：必填 → to>from → 跨度 → 粒度白名单 →
// 非 cube 时区原始行 horizon）。
func (s *Service) QueryStatsTrend(ctx context.Context, q TrendQuery) ([]*domain.StatBucket, error) {
	if err := validateStatsWindow(q.From, q.To, MaxStatsTrendSpan); err != nil {
		return nil, err
	}
	if err := s.validateZoneSpan(q.Zone, q.From, q.To); err != nil {
		return nil, err
	}
	unit, err := normalizeGranularity(q.Granularity)
	if err != nil {
		return nil, err
	}
	return s.store.StatsTrend(ctx, q.From, q.To, unit, q.GroupID, q.Model, q.Zone)
}

// QueryStatsTop 实体排行（limit 归一化后透传；排序键/实体类型白名单前置拦截）。
func (s *Service) QueryStatsTop(ctx context.Context, q TopQuery) ([]*domain.EntityStatBucket, error) {
	if err := validateStatsWindow(q.From, q.To, MaxStatsTrendSpan); err != nil {
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
	return s.store.StatsTop(ctx, q.From, q.To, q.EntityType, q.By, min(limit, MaxStatsListLimit))
}

// QueryEntityTrend 单实体时间趋势（实体类型白名单前置拦截；EntityID 合法性由
// 数据语义兜底——卷积表无 ID=0 行，零值自然返回空集）。
func (s *Service) QueryEntityTrend(ctx context.Context, q EntityTrendQuery) ([]*domain.EntityStatBucket, error) {
	if err := validateStatsWindow(q.From, q.To, MaxStatsTrendSpan); err != nil {
		return nil, err
	}
	if err := s.validateZoneSpan(q.Zone, q.From, q.To); err != nil {
		return nil, err
	}
	unit, err := normalizeGranularity(q.Granularity)
	if err != nil {
		return nil, err
	}
	if !statEntityTypes[q.EntityType] {
		return nil, ErrInvalidInput
	}
	return s.store.StatsEntityTrend(ctx, q.From, q.To, unit, q.EntityType, q.EntityID, q.Model, q.Zone)
}

// QueryStatsTTFT TTFT 分位数卡片，双分支独立上限（Momus M5）：
//   - EntityType == ""：sketch 分支（cube hist 服务端合并），桶数 ≤
//     MaxStatsSketchBuckets；
//   - 非空：exact 分支（usage_logs percentile_cont），必须配 EntityID ≠ 0 且
//     entityType 过白名单，跨度 ≤ MaxStatsTTFTExactSpan。
//
// 校验通过后经 statsTTFTC TTL 缓存（P3 验收遗留尾巴：exact 冷缓存 × 系统饱和
// 排序致负载 p99 5-6s；仪表盘同参轮询命中率天然高，陈旧 ≤30s 为展示面可
// 接受语义——overview 先例）。
func (s *Service) QueryStatsTTFT(ctx context.Context, q TTFTQuery) (*domain.TTFTSummary, error) {
	if q.EntityType == "" {
		if err := validateStatsWindow(q.From, q.To, MaxStatsSketchBuckets*time.Hour); err != nil {
			return nil, err
		}
	} else {
		if err := validateStatsWindow(q.From, q.To, MaxStatsTTFTExactSpan); err != nil {
			return nil, fmt.Errorf("self/entity ttft window exceeds %s exact-track limit: %w", MaxStatsTTFTExactSpan, err)
		}
		if !statEntityTypes[q.EntityType] || q.EntityID == 0 {
			return nil, ErrInvalidInput
		}
	}
	key := q.EntityType + "|" + strconv.FormatInt(q.EntityID, 10) + "|" + q.Model + "|" +
		strconv.FormatInt(q.From.Unix(), 10) + "|" + strconv.FormatInt(q.To.Unix(), 10)
	return statsTTFTC.fetch(key, func() (*domain.TTFTSummary, error) {
		// M2：fn 由首个请求的 ctx 触发，但结果服务同键全部等待者——leader 取消
		// 不得连坐。脱钩后以 30s 预算封顶（冷查询最坏实测 ~7s；裸 WithoutCancel
		// 无界是 AGENTS.md 反模式 #5 明令禁止形态）。
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ttftQueryBudget)
		defer cancel()
		if q.EntityType == "" {
			return s.store.StatsTTFTSketch(qctx, q.From, q.To, q.Model)
		}
		return s.store.StatsTTFTExact(qctx, q.From, q.To, q.EntityType, q.EntityID, q.Model)
	})
}

// UserStats 用户台自己的用量趋势：忽略调用方传入的任何 entity 参数，userID
// 钉死注入（JWT 身份即过滤条件，防越权只看 service 层这一道钉死）。
func (s *Service) UserStats(ctx context.Context, userID int64, q EntityTrendQuery) ([]*domain.EntityStatBucket, error) {
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

// validateStatsWindow 统计窗口校验（spec §5 顺序：必填 → to>from → 跨度）。
// 违规一律 ErrInvalidInput（errors 包既有校验哨兵族成员，httpface 映射 400
// ——spec 行文中的 "ErrInvalidArgument" 即此哨兵，不另立重复语义的新哨兵）。
func validateStatsWindow(from, to time.Time, max time.Duration) error {
	if from.IsZero() || to.IsZero() {
		return ErrInvalidInput
	}
	if !to.After(from) {
		return ErrInvalidInput
	}
	if to.Sub(from) > max {
		return ErrInvalidInput
	}
	return nil
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

// validateZoneSpan 原始行分组路径双闸门（step 7 裁决 + 保留兜底）：窗口无法
// 由 cube 精确重组（界非 UTC 整点劈开卷积小时行，或 DST 偏移漂移 / :30/:45
// 偏移，domain.ZoneCubeExact == false——与 repository 读面路由同一谓词，校验
// 与执行永不判岐）时，读走 usage_logs/err_logs 原始行，受保留期双重约束：
//   - 窗口跨度 > s.statsRawSpan（缺省 MaxStatsRawSpan，main 按双表最小正保留
//     配置换算）→ 显式 400；statsRawSpan == 0（保留禁用）= 不限；
//   - from 早于保证存留的 UTC 分区 cutoff（now−statsRawRetentionDays 的日界
//     截断，与 retention worker DROP 同一保守语义）→ 显式 400——短历史窗口
//     即使不触跨度上限，起点分区也可能已被 DROP，静默缺行比超窗更危险；
//     保留禁用（days <= 0）跳过该兜底；to 超出 now 的未来窗不因此拒绝。
//
// cube 可精确窗口恒零成本放行。绝不静默返回残缺桶。
func (s *Service) validateZoneSpan(zone *time.Location, from, to time.Time) error {
	if domain.ZoneCubeExact(zone, from, to) {
		return nil
	}
	if s.statsRawSpan > 0 && to.Sub(from) > s.statsRawSpan {
		return fmt.Errorf("service: timezone %v grouping over raw logs supports windows up to %s: %w", zone, s.statsRawSpan, ErrInvalidInput)
	}
	if s.statsRawRetentionDays > 0 {
		now := time.Now
		if s.statsNow != nil {
			now = s.statsNow
		}
		cutoff := now().AddDate(0, 0, -s.statsRawRetentionDays).UTC().Truncate(24 * time.Hour)
		if from.Before(cutoff) {
			return fmt.Errorf("service: timezone %v grouping over raw logs starts before retained partitions (cutoff %s, retention %dd): %w",
				zone, cutoff.Format(time.RFC3339), s.statsRawRetentionDays, ErrInvalidInput)
		}
	}
	return nil
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
// 读到撕裂/空值的数据竞争（RG 审计 B1，-race 实测复现）。
// panic 兜底（M1）：store 层 panic 被 handler Recoverer 兜住时进程存活，
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
