// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// BlockedPartition 描述被 §3.11 分区退休屏障挡住、拒绝 DROP 的 usage_logs 分区。
// usage_logs 是**唯一未确认债权凭证**：未确认资金事件（未记账正收益行 /
// 未扣费事件）不得随 retention 删除。被挡分区进入独立持久告警（blocked 列表由
// retention worker 呈现）。
type BlockedPartition struct {
	Name               string // 分区名（usage_logs_YYYYMMDD）
	UncreditedEarnRows int64  // NOT supplier_credited AND supplier_earn_millis > 0 行数
	UnbilledRows       int64  // NOT billed 行数（既有未扣费事件同受保护）
	OldestUncredited   *int64 // 最老未确认债权行 created_at（UnixMilli；无 = nil）

	// 源日封账核对证据（§3.10/§3.11）：源行（earn>0）按 (uid, UTC 源日) 聚合与
	// supplier_reconciliation 的核对结果。全 0 = 未进入封账步（被前序未确认屏障挡）。
	ReconSourceKeys int64 // 分区内 earn>0 源行聚合出的 (uid, source_day) 键数
	ReconMismatch   int64 // 与 supplier_reconciliation 键集合/数值不一致的键数（缺/多/偏差）
	ReconOpenKeys   int64 // 数值一致但仍 open 且未能在本事务原子封账的键数
}

// UsageLogRetireResult 单个 usage_logs 分区退休轮结果（§3.11）：Dropped =
// 通过屏障成功 DROP 的分区数；Blocked = 被挡分区（禁止 DROP，进入告警）。
// ETA 由调用方按磁盘消耗速率估算（本结果只给阻断证据）。
type UsageLogRetireResult struct {
	Dropped int
	Blocked []BlockedPartition
}
