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

	// BaselineTruncateAttempts 基线窗截断阈值：按分钟从新到老累计到该值即停
	// （该分钟整分钟计入，更老的排除）。repository 的截断 SQL 与 scheduler 的
	// hot 候选判定（当前窗 attempts ≥ 该值）必须同源，故落在 domain。
	BaselineTruncateAttempts = 30

	// BaselineProbeLen 基线窗**首轮探测**长度——注意它不是回看下界。
	//
	// 截断谓词（累计 ≥ BaselineTruncateAttempts 即停）在「按分钟从新到老」的序上
	// 是**单调停止条件**，因此它定义的是整段回看的一个**前缀**。用整段 24h 去算
	// 这个前缀，等于把「前缀长度」当成常量：读 H·1435·N 行只用 H·k·N 行。
	// 实测（.omo/evidence/routing-read-amplification/prefix-scan.txt，H=5000、N=3）：
	// 整段回看 19,459 ms vs 最近 30m 442 ms（44×），**结果逐字段相等**。
	// 故首轮只探最近 BaselineProbeLen，前缀内未达阈值者才回落整段回看
	// （见 repository.PartitionRepo.QueryBaselineTruncated）。
	BaselineProbeLen = 30 * time.Minute
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

// RoutingPartitionStats 路由观测面兜底统计的**共享形状**：聚合告警口径
// （Count/Oldest）+ 分表诊断口径（QualityCount/FlowCount/QualityOldest/FlowOldest）
// + 快照交接状态行数（SnapshotRows）+ 无法解析名的分区数（UndatedCount）。
//
// **为什么定义在 domain（叶子包）而不是 repository**：usage 的 PartitionManager
// 是**窄接缝**（保留策略只需 DROP/预建，不感知分区表内部 DDL）。若该接缝的签名引用
// repository 的类型，usage 就被迫 import repository——而此前 usage 在生产代码里对
// repository **零依赖**。放叶子包则两侧同源、互不依赖（与本文件上方常量注释同一条
// 依赖方向纪律）。
//
// 聚合值是告警，分表值是诊断——一表 DROP 失败而另一表健康时，聚合只显示漂移，
// 分表值才指出故障表。Oldest 零值 = 该表无可解析分区（空表 / 全为无名分区）。
// SnapshotRows 是 routing_flow_snapshot_state 的当前总行数（普通表，无分区可
// DROP，DELETE 失败会无界增长，故与分区数同轮观测）。UndatedCount 是名解析失败的
// 分区数：它们既不被计数也不被 DROP（误删未知日期分区等于丢未知数据），只能靠本数
// 暴露后人工介入。
type RoutingPartitionStats struct {
	Count         int
	Oldest        time.Time
	QualityCount  int
	FlowCount     int
	QualityOldest time.Time
	FlowOldest    time.Time
	SnapshotRows  int
	UndatedCount  int
}
