// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package domain

import "time"

// 路由质量观测窗口与观测保留截止的**单一来源**。
//
// 编译当前窗与 24h 基线窗是正确性输入（编译器分层 + 事故判定共用），四个消费
// 点必须同源、禁止各写字面量：
//   - repository 落库窗读（routing_quality_window.go 的 Q1/Q2 边界）
//   - scheduler 活体截断（quality_provider.go 的 live 行窗口）
//   - scheduler 内存累计（quality_stats.go 的 Current/Baseline 过滤）
//   - config 保留期地板校验（observation_retention_days 不得低于回看 + 边际）
//
// 常量定义在 domain（叶子包）而非 scheduler：`internal/scheduler` 已 import
// `internal/repository`（rule_persist.go），故 repository 无法反向引用 scheduler
// （Go 包级 import 环）。scheduler 侧以同值重导出
// `scheduler.BaselineLookback` / `scheduler.CurrentWindowLen`，spec §5.3 命名的
// 两个符号仍然存在且仍指向这里的唯一字面量。
const (
	// BaselineLookback 基线回看下界（事故判定与基线窗共用 24h）。
	BaselineLookback = 24 * time.Hour
	// CurrentWindowLen 当前窗长度（[M-CurrentWindowLen, M)）。
	CurrentWindowLen = 5 * time.Minute
)

// RoutingObservationCutoff 观测保留截止：保留 worker 与读面窗口守卫**同源**
// （同一份 observation_retention_days + 同一日历日换算）。
//
// 用 AddDate 而非 Add(-days*24h)：保留 worker 按**日历日** DROP 分区
// （repository.DropTablePartitionsBefore 以分区名日期判定），守卫必须与之逐字
// 一致，否则 DST/日界处守卫会放行已被 DROP 的窗口（静默残缺）。
func RoutingObservationCutoff(now time.Time, retentionDays int) time.Time {
	return now.AddDate(0, 0, -retentionDays)
}
