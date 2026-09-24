// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：原子读既有计数器 + len(channel)（零锁零分配，O(1)）。

// RecorderStats usage 明细/额度 worker 状态（spec 2026-08-14：统计桶机制整体
// 删除——stat_buckets_created 观测随之消失）。
type RecorderStats struct {
	PendingLogs      int64 `json:"pending_logs"`      // 尚未落库的明细条数
	PendingWaterline int64 `json:"pending_waterline"` // 水线（包级 var 直读）
	Warned           bool  `json:"warned"`            // 水线告警边沿是否置位
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (r *Recorder) Stats() any {
	return RecorderStats{
		PendingLogs:      r.pendingN.Load(),
		PendingWaterline: pendingWaterline,
		Warned:           r.warned.Load(),
	}
}

// ErrLogWorkerStats err_logs 落盘 worker 状态（队列占用 + 丢弃/落盘计数）。
type ErrLogWorkerStats struct {
	Queued         int   `json:"queued"`           // 两队列积压总条数（恒 ≤ 容量和）
	QueueCap       int   `json:"queue_cap"`        // 拒绝队列容量（风暴采样阈值）
	ExemptQueueCap int   `json:"exempt_queue_cap"` // 豁免队列容量（双轨行）
	DroppedReject  int64 `json:"dropped_reject"`   // 拒绝行采样丢弃累计
	DroppedExempt  int64 `json:"dropped_exempt"`   // 双轨行丢弃累计（>0 即异常态）
	Inserted       int64 `json:"inserted"`         // 成功落盘累计
	WarnedReject   bool  `json:"warned_reject"`    // 拒绝丢弃告警边沿
	WarnedExempt   bool  `json:"warned_exempt"`    // 双轨丢弃告警边沿
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (w *ErrLogWorker) Stats() any {
	return ErrLogWorkerStats{
		Queued:         w.Queued(),
		QueueCap:       w.cfg.QueueSize,
		ExemptQueueCap: w.cfg.ExemptQueueSize,
		DroppedReject:  w.DroppedReject(),
		DroppedExempt:  w.DroppedExempt(),
		Inserted:       w.Inserted(),
		WarnedReject:   w.warnReject.Load(),
		WarnedExempt:   w.warnExempt.Load(),
	}
}

// RetentionWorkerStats 分区保留 worker 状态（runOnce 收尾原子写，零新增 DB）。
type RetentionWorkerStats struct {
	LastPatrolUnixMs                 int64 `json:"last_patrol_unix_ms"`                     // 最近一次巡检完成时刻（0 = 尚未巡检）
	LastDroppedLogPartitions         int64 `json:"last_dropped_log_partitions"`             // 最近成功轮 usage_logs DROP 分区数（失败轮保留上轮值）
	LastDroppedErrLogPartitions      int64 `json:"last_dropped_errlog_partitions"`          // 最近成功轮 err_logs DROP 分区数（失败轮保留上轮值）
	LastDroppedStatsPartitions       int64 `json:"last_dropped_stats_partitions"`           // 最近成功轮 usage_stats DROP 分区数（失败轮保留上轮值）
	LastDroppedEntityStatsPartitions int64 `json:"last_dropped_entity_stats_partitions"`    // 最近成功轮 usage_entity_stats DROP 分区数（与 stats 同 StatsRetentionDays，失败轮保留上轮值）
	LastDroppedRoutingQualityParts   int64 `json:"last_dropped_routing_quality_partitions"` // 最近成功轮 routing_quality_fact DROP 分区数（失败轮保留上轮值；0 也可能=无过期分区，是否失败看 Warn 日志）
	LastDroppedRoutingFlowParts      int64 `json:"last_dropped_routing_flow_partitions"`    // 最近成功轮 routing_flow_fact DROP 分区数（失败轮保留上轮值；口径同上）
	OldestPartitionUnixMs            int64 `json:"oldest_partition_unix_ms"`                // 路由两张事实表最老分区下界（UnixMilli；0 = 无分区）。早于观测保留 cutoff ⇒ Warn：保留 worker 停摆的兜底观测
	PartitionCount                   int64 `json:"partition_count"`                         // 路由两张事实表的分区总数（保留 worker 在跑即有界）
	QualityPartitionCount            int64 `json:"quality_partition_count"`                 // routing_quality_fact 分区数（一表 DROP 失败时聚合只显漂移，此值定位故障表）
	QualityOldestPartitionUnixMs     int64 `json:"quality_oldest_partition_unix_ms"`        // routing_quality_fact 最老分区下界（UnixMilli；0 = 该表无可解析分区）
	FlowPartitionCount               int64 `json:"flow_partition_count"`                    // routing_flow_fact 分区数（口径同 quality）
	FlowOldestPartitionUnixMs        int64 `json:"flow_oldest_partition_unix_ms"`           // routing_flow_fact 最老分区下界（UnixMilli；0 = 该表无可解析分区）
	SnapshotStateRows                int64 `json:"snapshot_state_rows"`                     // routing_flow_snapshot_state 当前总行数（普通表无分区可 DROP，DELETE 失败即无界增长）
	UndatedPartitionCount            int64 `json:"undated_partition_count"`                 // 名解析失败的分区数（既不计数也不 DROP，只能人工介入）
	PartitionStatsStale              bool  `json:"partition_stats_stale"`                   // true = 分区统计查询失败，当前呈现的是上轮过期值（lastPatrol 仍推进）
	LogRetentionDays                 int   `json:"log_retention_days"`
	ErrLogRetentionDays              int   `json:"errlog_retention_days"`
	StatsRetentionDays               int   `json:"stats_retention_days"`
	RoutingObservationRetentionDays  int   `json:"routing_observation_retention_days"` // 路由观测保留天数（oldest_partition_unix_ms 的 cutoff 解释口径：cutoff = now - 本值）
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (w *RetentionWorker) Stats() any {
	return RetentionWorkerStats{
		LastPatrolUnixMs:                 w.lastPatrol.Load(),
		LastDroppedLogPartitions:         w.lastDropLogs.Load(),
		LastDroppedErrLogPartitions:      w.lastDropErrLogs.Load(),
		LastDroppedStatsPartitions:       w.lastDropStats.Load(),
		LastDroppedEntityStatsPartitions: w.lastDropEntityStats.Load(),
		LastDroppedRoutingQualityParts:   w.lastDropRoutingQuality.Load(),
		LastDroppedRoutingFlowParts:      w.lastDropRoutingFlow.Load(),
		OldestPartitionUnixMs:            w.oldestPartition.Load(),
		PartitionCount:                   w.partitionCount.Load(),
		QualityPartitionCount:            w.qualityPartitionCount.Load(),
		QualityOldestPartitionUnixMs:     w.qualityOldest.Load(),
		FlowPartitionCount:               w.flowPartitionCount.Load(),
		FlowOldestPartitionUnixMs:        w.flowOldest.Load(),
		SnapshotStateRows:                w.snapshotRows.Load(),
		UndatedPartitionCount:            w.undatedPartitions.Load(),
		PartitionStatsStale:              w.statsStale.Load(),
		LogRetentionDays:                 w.cfg.LogRetentionDays,
		ErrLogRetentionDays:              w.cfg.ErrLogRetentionDays,
		StatsRetentionDays:               w.cfg.StatsRetentionDays,
		RoutingObservationRetentionDays:  w.cfg.RoutingObservationRetentionDays,
	}
}
