// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// —— 统计窗口规划器（spec-stats-window-plan-2026-09-25 §4.1/§4.5）——
//
// 三个正交问题各自归属一次（本文件是它们的**唯一**内存形态）：
//
//	Expressiveness（cube 能否表达） zone + 窗口位置 → ZoneCubeReason（statszone.go）
//	Cost（扫不扫得起）             kind + storage + 请求跨度 → StatsKind.CostCap
//	Coverage（数据还在不在）       storage 读的表 + 保留期   → StatsKind.Tables × Retention
//
// 判定、执行选择（Cube/Raw）、能力报告三者同源于 StatsKinds 这张表：本文件是
// 「窗口由哪套存储服务」与「窗口能否被完整服务」两个判定的**单一归属**。
// Admit 是唯一判定入口且是**纯函数**（retention 与 now 都以参数传入——不读配置、
// 不读全局、不打日志：internal/domain 零 logx 依赖，降级 Warn 由 service 侧调用
// 包装发出）。

import (
	"fmt"
	"time"
)

// GroupingMode 读形状的分组模式：无分组（绝对区间）或按请求时区分组。
type GroupingMode uint8

const (
	GroupingNone  GroupingMode = iota // 无分组：数值与请求时区无关
	GroupingZoned                     // 按请求时区分组（本地桶界）
)

// String 线缆词表（P4 能力端点的 grouping 字段）。
func (g GroupingMode) String() string {
	if g == GroupingZoned {
		return "zoned"
	}
	return "none"
}

// StatsStorage 读路径实际使用的存储：规范 UTC 小时卷积表或原始行表。
type StatsStorage uint8

const (
	StatsStorageCube StatsStorage = iota // usage_stats/usage_entity_stats 卷积表
	StatsStorageRaw                      // usage_logs/err_logs 原始行
)

func (s StatsStorage) String() string {
	if s == StatsStorageRaw {
		return "raw"
	}
	return "cube"
}

// StatsPlanReason 规划层原因枚举（内部命名空间；与 HTTP 线缆 reason 串的映射见
// spec §4.4(d) 映射表）。String 即线缆词表——日志字段与 400 机读字段同源。
type StatsPlanReason uint8

const (
	StatsPlanExact         StatsPlanReason = iota // 无需降级
	StatsPlanWindowShifted                        // 界不齐，已对齐到整点以换取 cube（窗口微移，§4.2）
	StatsPlanOffset                               // 时区偏移非整小时（:30/:45）
	StatsPlanDST                                  // 窗口内偏移跳变（DST/一次性）
	StatsPlanSpanBelowGrid                        // 请求跨度 < 网格单位 1h，cube 无法表达
)

func (r StatsPlanReason) String() string {
	switch r {
	case StatsPlanExact:
		return "exact"
	case StatsPlanWindowShifted:
		return "window_shifted"
	case StatsPlanOffset:
		return "offset"
	case StatsPlanDST:
		return "dst"
	case StatsPlanSpanBelowGrid:
		return "span_below_grid"
	}
	return "unknown"
}

// StatsWindowReject 拒绝原因（线缆 reason 串的唯一来源，spec §4.4(d)）。
type StatsWindowReject uint8

const (
	StatsRejectWindowInvalid StatsWindowReject = iota // window_invalid：必填/倒序/时长串不可解析
	StatsRejectWindowTooLong                          // window_too_long：cost 步（span > CostCap）
	StatsRejectRawHorizon                             // raw_horizon：coverage 步（读原始行表）
	StatsRejectCubeHorizon                            // cube_horizon：coverage 步（读卷积表）
	// StatsRejectWindowAmbiguous window_ambiguous：`from`/`to`/`window` 三者的
	// 「恰择一」未被满足（P3 相对窗口）。它由**请求边界**的形态解析产生
	// （httpface.ResolveStatsWindow），不是 Admit 的窗口判定——Admit 只接受已定的
	// (from, to) 对，判不了"调用方给了哪种形态"（wire 形态活在 domain 之外）。
	StatsRejectWindowAmbiguous
)

func (r StatsWindowReject) String() string {
	switch r {
	case StatsRejectWindowInvalid:
		return "window_invalid"
	case StatsRejectWindowTooLong:
		return "window_too_long"
	case StatsRejectRawHorizon:
		return "raw_horizon"
	case StatsRejectCubeHorizon:
		return "cube_horizon"
	case StatsRejectWindowAmbiguous:
		return "window_ambiguous"
	}
	return "unknown"
}

// StatsKindID 读形状标识（键 = spec §2 消费者表的一行；10 个 kind = 13 个端点）。
// 取值即能力端点（P4）JSON 的 kinds 键。
type StatsKindID string

const (
	// KindTrend /stats/trend（管理台趋势）。
	KindTrend StatsKindID = "trend"
	// KindEntityTrend /stats/entity-trend 与 /api/user/stats（同读路径）。
	KindEntityTrend StatsKindID = "entity_trend"
	// KindSummary /overview 的 summary 单行区间（服务端自算的今日窗）。
	KindSummary StatsKindID = "summary"
	// KindDays /overview 的 trend 日桶。
	KindDays StatsKindID = "days"
	// KindTop /stats/top（无时间分组，恒 cube）。
	KindTop StatsKindID = "top"
	// KindTTFTSketch /stats/ttft 的平台级草图分支（无实体过滤，恒 cube）。
	KindTTFTSketch StatsKindID = "ttft_sketch"
	// KindTTFTExact /stats/ttft 与 /api/user/stats/ttft 的实体级精确分支（恒原始行）。
	KindTTFTExact StatsKindID = "ttft_exact"
	// KindUsageAgg /accounts/usage 的区间聚合（恒原始行）。
	KindUsageAgg StatsKindID = "usage_agg"
	// KindUsageList /usage_logs 与 /api/user/usage_logs 的 keyset 分页（恒原始行）。
	KindUsageList StatsKindID = "usage_list"
	// KindErrlogList /err_logs 与 /api/user/err_logs 的 keyset 分页（恒原始行）。
	KindErrlogList StatsKindID = "errlog_list"
)

// 成本上限（cost 步的取值来源；**保留期无关的纯常量**——扫 10 天日志的成本与
// 保留了多少天日志无关）。coverage 由 Retention 独立承载（§4.5）。
const (
	// MaxCubeSpan 卷积表读路径的成本上限（90d = 2160h）：cube 查询按小时桶扫描，
	// 90d × 维度基数是交互式端点的合理上界。sketch 分支的桶数上限（2160 =
	// 90d × 24 小时桶）是同一窗口的另一种表述。
	MaxCubeSpan = 90 * 24 * time.Hour
	// MaxGroupedRawSpan 分组原始行读路径的成本上限（8d 纯常量，即文档对外承诺
	// 的窗口上限——它不再是「保留期 +1 天」的派生值）。
	MaxGroupedRawSpan = 8 * 24 * time.Hour
	// MaxExactTTFTSpan 实体级精确 TTFT 的成本上限（168h = 7d）：打 usage_logs
	// 原始行 percentile_cont，无预聚合保护，窗口必须远小于走 cube 的 sketch 分支。
	MaxExactTTFTSpan = 168 * time.Hour
)

// RetentionTable coverage 依据的表标识。
type RetentionTable uint8

const (
	TableUsageLogs  RetentionTable = iota // usage_logs
	TableErrLogs                          // err_logs
	TableUsageStats                       // usage_stats
)

// Retention 各表的**部署**保留天数（由 service 从 ServiceDeps 注入，domain 里
// 不读配置）。Days(t) <= 0 ⇒ 该表守卫关闭（分区保留被禁用）。
type Retention struct {
	Log    int // usage_logs ← usage.log_retention_days
	ErrLog int // err_logs ← usage.errlog_retention_days
	Stats  int // usage_stats ← usage.stats_retention_days
}

// Days t 表的部署保留天数（未登记的表 → 0 = 守卫关闭）。
func (r Retention) Days(t RetentionTable) int {
	switch t {
	case TableUsageLogs:
		return r.Log
	case TableErrLogs:
		return r.ErrLog
	case TableUsageStats:
		return r.Stats
	}
	return 0
}

// StatsKind 一行的全部事实。
type StatsKind struct {
	Grouping GroupingMode
	// Storages 候选存储按偏好序：zoned=[cube,raw]，cube 恒=[cube]，raw 恒=[raw]。
	Storages []StatsStorage
	// CostCap[storage] = 该形状扫该存储的最大跨度（保留期无关纯常量）。
	// 0 = 无上限（logs 分页：keyset + LIMIT，构造上有界）。
	CostCap map[StatsStorage]time.Duration
	// Tables[storage] = coverage 依据的表（floor = max 各表 floor）。
	Tables map[StatsStorage][]RetentionTable
}

// StatsKinds KIND 矩阵 as code（spec §4.1 逐行；每种读形状声明一次，再无处写分支）。
// 判定（Admit）、执行选择（service 的 Cube/Raw 调用）、能力报告（P4 端点）全部
// 读这一份内存——数值不一致在构造上不可能。
var StatsKinds = map[StatsKindID]StatsKind{
	KindTrend:       {Grouping: GroupingZoned, Storages: []StatsStorage{StatsStorageCube, StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageCube: MaxCubeSpan, StatsStorageRaw: MaxGroupedRawSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageCube: {TableUsageStats}, StatsStorageRaw: {TableUsageLogs, TableErrLogs}}},
	KindEntityTrend: {Grouping: GroupingZoned, Storages: []StatsStorage{StatsStorageCube, StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageCube: MaxCubeSpan, StatsStorageRaw: MaxGroupedRawSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageCube: {TableUsageStats}, StatsStorageRaw: {TableUsageLogs, TableErrLogs}}},
	KindSummary:     {Grouping: GroupingZoned, Storages: []StatsStorage{StatsStorageCube, StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageCube: MaxCubeSpan, StatsStorageRaw: MaxGroupedRawSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageCube: {TableUsageStats}, StatsStorageRaw: {TableUsageLogs, TableErrLogs}}},
	KindDays:        {Grouping: GroupingZoned, Storages: []StatsStorage{StatsStorageCube, StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageCube: MaxCubeSpan, StatsStorageRaw: MaxGroupedRawSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageCube: {TableUsageStats}, StatsStorageRaw: {TableUsageLogs, TableErrLogs}}},
	KindTop:         {Grouping: GroupingNone, Storages: []StatsStorage{StatsStorageCube}, CostCap: map[StatsStorage]time.Duration{StatsStorageCube: MaxCubeSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageCube: {TableUsageStats}}},
	KindTTFTSketch:  {Grouping: GroupingNone, Storages: []StatsStorage{StatsStorageCube}, CostCap: map[StatsStorage]time.Duration{StatsStorageCube: MaxCubeSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageCube: {TableUsageStats}}},
	KindTTFTExact:   {Grouping: GroupingNone, Storages: []StatsStorage{StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageRaw: MaxExactTTFTSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageRaw: {TableUsageLogs}}},
	KindUsageAgg:    {Grouping: GroupingNone, Storages: []StatsStorage{StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageRaw: MaxCubeSpan}, Tables: map[StatsStorage][]RetentionTable{StatsStorageRaw: {TableUsageLogs}}},
	KindUsageList:   {Grouping: GroupingNone, Storages: []StatsStorage{StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageRaw: 0}, Tables: map[StatsStorage][]RetentionTable{StatsStorageRaw: {TableUsageLogs}}},
	KindErrlogList:  {Grouping: GroupingNone, Storages: []StatsStorage{StatsStorageRaw}, CostCap: map[StatsStorage]time.Duration{StatsStorageRaw: 0}, Tables: map[StatsStorage][]RetentionTable{StatsStorageRaw: {TableErrLogs}}},
}

// Exec 可执行计划（判定 + 存储已决）：真正要读的绝对半开区间 [From, To) 与
// 实际使用的存储。Zone 原样透传（nil = UTC，repository 入口 locOrUTC 兜底）。
type Exec struct {
	Storage StatsStorage
	From    time.Time
	To      time.Time
	Zone    *time.Location
	Reason  StatsPlanReason
}

// StatsWindowError Admit 的拒绝载体（domain 侧最小形态：判定事实 + P4 机读字段
// 的来源）。哨兵语义（ErrInvalidInput / HTTP 400）不在此层——service.StatsWindowError
// 嵌入本类型并把 Unwrap 接到既有校验哨兵（spec §4.4(d)：一个哨兵 + 一个载体）。
type StatsWindowError struct {
	Kind          StatsKindID
	Reject        StatsWindowReject
	Storage       StatsStorage
	LimitSeconds  int64 // cost：上限秒数（0 = 不适用）
	Cutoff        time.Time
	RetentionDays int // coverage：判定所依据的最保守保留天数（0 = 不适用）
	EffectiveFrom time.Time
	EffectiveTo   time.Time
}

func (e *StatsWindowError) Error() string {
	switch e.Reject {
	case StatsRejectWindowInvalid:
		// 一类一文案：`window_invalid` 覆盖"窗口请求本身非法"的三个子情形
		// ——必填缺失（step 1）、倒序（step 1）、`window` 时长串不可解析
		// （请求边界的形态解析）。文本对三者都成立，具体是哪一个由机读
		// `reason` + 请求本身可见；不为子情形增设字段（那会让载体承载展示细节）。
		return "service: stats window invalid: from/to must be set and from < to, or window must be a duration string"
	case StatsRejectWindowTooLong:
		return fmt.Sprintf("service: stats window too long: %s %s supports windows up to %s",
			e.Kind, e.Storage, time.Duration(e.LimitSeconds)*time.Second)
	case StatsRejectRawHorizon, StatsRejectCubeHorizon:
		return fmt.Sprintf("service: stats %s window starts before retained partitions (cutoff %s, retention %dd)",
			e.Storage, e.Cutoff.UTC().Format(time.RFC3339), e.RetentionDays)
	case StatsRejectWindowAmbiguous:
		// 边界形态冲突：不是某个窗口的错，而是"给的形态不是一个窗口"。
		return "service: stats window ambiguous: require from+to, or window alone"
	}
	return fmt.Sprintf("service: stats window rejected (%s)", e.Reject)
}

// Admit 唯一的窗口判定入口：kind 矩阵 + 部署保留期 + 请求窗口 → 可执行计划，或
// 拒绝（*StatsWindowError）。纯函数（ret/now 都是参数），不发日志。
//
// 顺序固定（cost 先于 coverage——「扫不起」是部署无关的事实，「数据不在」随保留期
// 浮动；先报前者故报告顺序稳定）：
//  1. validate：零值/倒序 → window_invalid
//  2. zoned ? zone-branch（三条判定，见 admitZoned） : Fixed{Storages[0]}
//  3. cost：请求跨度 > CostCap[kind][storage]（0 = 无上限）→ window_too_long
//  4. coverage：exec.From < max(floor(ret.Days(t), now)) over Tables[kind][storage]
//     → raw_horizon / cube_horizon；该 storage 的表全部关闭保留期 ⇒ 整体跳过
//  5. 返回（降级/位移的 Warn 由 service 侧调用包装发出，含节流）
func Admit(kind StatsKindID, ret Retention, zone *time.Location, from, to, now time.Time) (Exec, *StatsWindowError) {
	k, ok := StatsKinds[kind]
	if !ok { // 未登记的 kind = 编程错误（10 个声明全覆盖）；防御性拒绝而非 panic
		return Exec{}, &StatsWindowError{Kind: kind, Reject: StatsRejectWindowInvalid}
	}
	reqSpan := to.Sub(from)
	if from.IsZero() || to.IsZero() || reqSpan <= 0 {
		return Exec{}, &StatsWindowError{Kind: kind, Reject: StatsRejectWindowInvalid,
			EffectiveFrom: from, EffectiveTo: to}
	}
	var exec Exec
	if k.Grouping == GroupingZoned {
		exec = admitZoned(zone, from, to)
	} else {
		exec = Exec{Storage: k.Storages[0], From: from, To: to, Zone: zone, Reason: StatsPlanExact}
	}
	// cost 作用在**请求跨度**（用户请求了多少）；coverage 作用在**生效窗口**
	// （exec.From/To——真正要读的区间）。两个问题必须分别问（§4.2）。
	if cap := k.CostCap[exec.Storage]; cap > 0 && reqSpan > cap {
		return Exec{}, &StatsWindowError{Kind: kind, Reject: StatsRejectWindowTooLong,
			Storage: exec.Storage, LimitSeconds: int64(cap / time.Second),
			EffectiveFrom: exec.From, EffectiveTo: exec.To}
	}
	if days, ok := coverageDays(k.Tables[exec.Storage], ret); ok {
		cutoff := floor(days, now)
		if exec.From.Before(cutoff) {
			reject := StatsRejectRawHorizon
			if exec.Storage == StatsStorageCube {
				reject = StatsRejectCubeHorizon
			}
			return Exec{}, &StatsWindowError{Kind: kind, Reject: reject, Storage: exec.Storage,
				Cutoff: cutoff, RetentionDays: days, EffectiveFrom: exec.From, EffectiveTo: exec.To}
		}
	}
	return exec, nil
}

// admitZoned zone-branch（spec §4.1 三条判定逐条）：
//  1. 双界齐整点且偏移恒整点无跳变 → Cube{[from,to), Exact}
//  2. 请求跨度 >= 1h 时取 cand = [ceilHour(from), ceilHour(to))，cand 精确
//     → Cube{cand, WindowShifted}（窗口两端各自向后微移 < 1h，N1；跨度不越上限
//     由网格整除性保证，N3；跨度 < 1h 不做对齐故永不为空读，N5）
//  3. 否则 → Raw{[from,to), reason}
//
// 第 3 条的原因在 cand 上派生（穷尽 {Offset, DST}——cand 双界恒整点，故界的
// 前提不可能失败）；跨度 < 1h 时不对齐，原因恒为 SpanBelowGrid。
func admitZoned(zone *time.Location, from, to time.Time) Exec {
	if ZoneCubeVerdict(zone, from, to) == ZoneCubeReusable {
		return Exec{Storage: StatsStorageCube, From: from, To: to, Zone: zone, Reason: StatsPlanExact}
	}
	if to.Sub(from) >= time.Hour {
		candFrom, candTo := ceilHour(from), ceilHour(to)
		r := ZoneCubeVerdict(zone, candFrom, candTo)
		if r == ZoneCubeReusable {
			return Exec{Storage: StatsStorageCube, From: candFrom, To: candTo, Zone: zone, Reason: StatsPlanWindowShifted}
		}
		return Exec{Storage: StatsStorageRaw, From: from, To: to, Zone: zone, Reason: rawPlanReason(r)}
	}
	return Exec{Storage: StatsStorageRaw, From: from, To: to, Zone: zone, Reason: StatsPlanSpanBelowGrid}
}

// rawPlanReason 第 3 条的原因派生：Exact 已在第 2 条返回、cand 双界恒整点故
// Unaligned 不可达——switch 的 default 给出确定性回落（不产生第三类原因）。
func rawPlanReason(r ZoneCubeReason) StatsPlanReason {
	if r == ZoneCubeDST {
		return StatsPlanDST
	}
	return StatsPlanOffset
}

// ceilHour t 两端**向后**取整到 UTC 整点（t 已整点则不变）。
//
// 亚秒分量必须参与取整：`Math.ceil(ms/HOUR)*HOUR`（前端现状形态，spec §4.2 的
// 等价物）对 10:00:00.001 给出 11:00，而 `time.Unix(((t.Unix()+3599)/3600)*3600, 0)`
// 会先丢掉亚秒再判整点，把它算成"已整点"从而回落 10:00——**比请求下界更早**，
// 直接违反 N1（两端只向后）并丢掉该小时最后一段。故此处先按纳秒分量把秒向后
// 进位，再做整点取整（正时刻；Unix 秒非负区间内与前端逐位等价）。
func ceilHour(t time.Time) time.Time {
	secs := t.Unix()
	if t.Nanosecond() != 0 {
		secs++ // 亚秒分量：秒也向后进一位（否则会被误判为"已整点"）
	}
	if secs%3600 == 0 {
		return time.Unix(secs, 0).UTC()
	}
	return time.Unix(((secs+3599)/3600)*3600, 0).UTC()
}

// WindowFromDuration 相对窗口 → 绝对窗口对（P3 `?window=<dur>`，spec §7.3）：
// `to = ceilHour(now)`、`from = to − d`。**两处都用同一个 ceilHour**（不是
// 复制一份取整算术）——这正是"双界恒整点 ⇒ 按定义命中 Admit 第 1 条（精确）
// ⇒ 零对齐位移、零桶丢失"在实现上的保证；若调用方各自取整，该承诺就只是巧合。
//
// 纯函数：时钟由参数传入（服务端自持，不读墙钟）。`d <= 0` 不在此校验——时长串
// 的解析与合法性是请求边界的事（time.ParseDuration 只产出正值），本函数不假装
// 判定（判定 owner 只有 Admit）。
func WindowFromDuration(d time.Duration, now time.Time) (from, to time.Time) {
	to = ceilHour(now)
	return to.Add(-d), to
}

// floor 表保留截止（coverage 的判定下界）：now 倒退 days 个日历日后的 UTC 日界。
//
// 与 retention worker 的 DROP 边界**逐位同形**：DROP 侧传入未截断的
// now.AddDate(0,0,-days)（internal/usage/retention.go），而
// DropTablePartitionsBefore 内部截断到 UTC 日（internal/repository/partition.go
// 的 cut := cutoff.UTC().Truncate(24 * time.Hour)）——故此处同样截断，
// 守卫与 DROP 之间不存在 ≤24h 偏斜。
func floor(days int, now time.Time) time.Time {
	return now.AddDate(0, 0, -days).UTC().Truncate(24 * time.Hour)
}

// coverageDays 该读路径涉及的表里守卫开启（Days > 0）的**最保守**保留天数。
// floor 对天数单调递减 ⇒ max(floor(d_i)) = floor(min(d_i))，故读 N 张表取最小
// 天数即得 max-of-floors（统计面 raw 读 usage_logs+err_logs，故等价于
// min(log, errlog)）。
//
// ok=false = Tables 里所有表都关闭保留期 ⇒ coverage 步整体跳过（不存在可比的
// floor，不产生 horizon 错误）。
func coverageDays(tables []RetentionTable, ret Retention) (int, bool) {
	days, ok := 0, false
	for _, t := range tables {
		d := ret.Days(t)
		if d <= 0 {
			continue
		}
		if !ok || d < days {
			days, ok = d, true
		}
	}
	return days, ok
}

// —— P4 能力投影（spec §4.4(a)）——
//
// 能力端点是**本文件的机械投影**：判定（Admit）、执行选择（service 的
// Cube/Raw 调用）、能力报告（HttpHandler 的 JSON）三处读的是同一份内存
// （StatsKinds），故数值不一致在构造上不可能——不需要"改注入看端点变不变"
// 这类用例去保证一致性。

// KindCapability 一个 kind 的能力投影。
type KindCapability struct {
	Grouping GroupingMode
	Storages []StatsStorage
	// CostCapSeconds[storage] = 该形状扫该存储的最大跨度秒数（0 = 无上限）。
	CostCapSeconds map[StatsStorage]int64
	// CoverageDays[storage] = 该存储实际读表的保留天数（读多表取最保守者）。
	// 0 = 涉及的表都未启用分区保留 ⇒ 覆盖守卫关闭（无覆盖下限）。
	CoverageDays map[StatsStorage]int
}

// Capabilities 能力端点响应的数据面（bucket_grid_seconds + kinds）。
type Capabilities struct {
	// BucketGridSeconds 卷积表桶网格单位（秒）：恒 1 小时（判定里的对齐单位）。
	BucketGridSeconds int64
	Kinds             map[StatsKindID]KindCapability
}

// StatsCapabilities 把 StatsKinds 矩阵与部署保留期投影成能力报告。
// 纯函数（ret 是参数）——与 Admit 读同一份矩阵、同一份保留期，故端点报出的
// 数字与守卫实际使用的数字同源。
//
// cost 与 coverage 在此处**各自独立投影**（正是 §4.5 拆两步的可见后果）：
// 保留期调短只改 CoverageDays，CostCapSeconds 恒为常量。
func StatsCapabilities(ret Retention) Capabilities {
	out := Capabilities{
		BucketGridSeconds: int64(time.Hour / time.Second),
		Kinds:             make(map[StatsKindID]KindCapability, len(StatsKinds)),
	}
	for id, k := range StatsKinds {
		kc := KindCapability{
			Grouping:       k.Grouping,
			Storages:       append([]StatsStorage(nil), k.Storages...),
			CostCapSeconds: make(map[StatsStorage]int64, len(k.Storages)),
			CoverageDays:   make(map[StatsStorage]int, len(k.Storages)),
		}
		for _, s := range k.Storages {
			kc.CostCapSeconds[s] = int64(k.CostCap[s] / time.Second)
			// 全部表关闭保留期 ⇒ 0（守卫关闭，不是"只能查 0 天"）。
			days, _ := coverageDays(k.Tables[s], ret)
			kc.CoverageDays[s] = days
		}
		out.Kinds[id] = kc
	}
	return out
}
