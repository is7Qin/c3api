// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：原子读既有计数器（零锁零分配，O(1)）；不新增热路径埋点。
//
// F2 表面稳定裁决（spec-f2-ledger-cursor T3）：FlusherStats 结构体与 Stats()
// 签名原样保留（下游 main.go BillingAlerts / overview / openapi 契约归 T5 改），
// 仅字段语义重映射——内存 pending 队列已删除，字段真值换游标观测原子。

// FlusherStats billing flusher 状态（原子读组装，不锁模块内部）。
type FlusherStats struct {
	// PendingLogs 语义重映射（F2）：当前 Unbilled 行数（每消费周期经
	// UnbilledLag 刷新；旧语义"内存 pending 日志条数"随队列删除消亡）。
	PendingLogs int64 `json:"pending_logs"`
	// PendingWaterline 恒 0：水线机制随内存队列删除。TODO(T5)：openapi 契约
	// pending_waterline 必填族 → lag 族字段替换后本字段随响应面一并退役。
	PendingWaterline int64 `json:"pending_waterline"`
	// Warned 恒 false：水线告警边沿退役。TODO(T5)：lag 护栏告警态转正
	// （真值见非导出 lagWarned 原子）。
	Warned bool `json:"warned"`
	// LastFlushUnixMs 最近一次成功消费周期时刻（0 = 尚未成功消费；空周期/
	// 全失败不推进——语义沿袭 G2-4"成功落库时刻"）。
	LastFlushUnixMs int64 `json:"last_flush_unix_ms"`

	// —— F2 新指标真值（非导出：不进 JSON 契约；T5 启用 lag/unbilled/
	// quarantine 指标族时转导出）——
	unbilledRows    int64 // 当前 Unbilled 行数（= PendingLogs 真值源）
	quarantinedRows int64 // 累计隔离行数（用户缺失组 + 毒行终极隔离）
	lagOldestUnixMs int64 // 最老 unbilled 行 created_at（0 = 游标空/未探测）
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (f *Flusher) Stats() any {
	return FlusherStats{
		PendingLogs:      f.unbilledN.Load(),
		PendingWaterline: 0, // TODO(T5)：契约换 lag 族后删（水线机制已退役）
		Warned:           false,
		LastFlushUnixMs:  f.lastFlush.Load(),

		unbilledRows:    f.unbilledN.Load(),
		quarantinedRows: f.quarantined.Load(),
		lagOldestUnixMs: f.lagOldestUnixMs.Load(),
	}
}
