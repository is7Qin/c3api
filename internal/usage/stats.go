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
	LastPatrolUnixMs                 int64 `json:"last_patrol_unix_ms"`                  // 最近一次巡检完成时刻（0 = 尚未巡检）
	LastDroppedLogPartitions         int64 `json:"last_dropped_log_partitions"`          // 最近成功轮 usage_logs DROP 分区数（失败轮保留上轮值）
	LastDroppedErrLogPartitions      int64 `json:"last_dropped_errlog_partitions"`       // 最近成功轮 err_logs DROP 分区数（失败轮保留上轮值）
	LastDroppedStatsPartitions       int64 `json:"last_dropped_stats_partitions"`        // 最近成功轮 usage_stats DROP 分区数（失败轮保留上轮值）
	LastDroppedEntityStatsPartitions int64 `json:"last_dropped_entity_stats_partitions"` // 最近成功轮 usage_entity_stats DROP 分区数（与 stats 同 StatsRetentionDays，失败轮保留上轮值）
	LogRetentionDays                 int   `json:"log_retention_days"`
	ErrLogRetentionDays              int   `json:"errlog_retention_days"`
	StatsRetentionDays               int   `json:"stats_retention_days"`
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (w *RetentionWorker) Stats() any {
	return RetentionWorkerStats{
		LastPatrolUnixMs:                 w.lastPatrol.Load(),
		LastDroppedLogPartitions:         w.lastDropLogs.Load(),
		LastDroppedErrLogPartitions:      w.lastDropErrLogs.Load(),
		LastDroppedStatsPartitions:       w.lastDropStats.Load(),
		LastDroppedEntityStatsPartitions: w.lastDropEntityStats.Load(),
		LogRetentionDays:                 w.cfg.LogRetentionDays,
		ErrLogRetentionDays:              w.cfg.ErrLogRetentionDays,
		StatsRetentionDays:               w.cfg.StatsRetentionDays,
	}
}
