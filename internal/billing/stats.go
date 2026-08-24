// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：原子读既有计数器（零锁零分配，O(1)）；不新增热路径埋点。
//
// F2 ABI-4 终态（spec-f2-ledger-cursor T5）：ops 观测为 lag 族——lag/unbilled/
// quarantine 三真值由 worker 每周期收尾 refreshLag 原子写（spec §一 lag 度量源
// 点名：部分索引最小 unbilled 行 created_at vs now）。

// FlusherStats billing flusher 状态（原子读组装，不锁模块内部）。
type FlusherStats struct {
	// LagMs 游标积压时滞（毫秒）= 最近周期探测的 now − 最老 unbilled 行
	// created_at；0 = 游标空/未探测。消费停摆时此值持续增长——护栏告警同源。
	LagMs int64 `json:"lag_ms"`
	// UnbilledRows 当前未扣费账本行数（部分索引 WHERE NOT billed 行数）。
	UnbilledRows int64 `json:"unbilled_rows"`
	// QuarantinedRows 累计隔离行数（用户缺失组 + 毒行终极隔离——未扣费写销）。
	QuarantinedRows int64 `json:"quarantined_rows"`
	// LastCycleUnixMs 最近一次成功消费周期时刻（0 = 尚未成功消费；空周期/
	// 全失败不推进——语义沿袭 G2-4"成功落库时刻"）。
	LastCycleUnixMs int64 `json:"last_cycle_unix_ms"`
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (f *Flusher) Stats() any {
	return FlusherStats{
		LagMs:           f.lagMs.Load(),
		UnbilledRows:    f.unbilledN.Load(),
		QuarantinedRows: f.quarantined.Load(),
		LastCycleUnixMs: f.lastFlush.Load(),
	}
}
