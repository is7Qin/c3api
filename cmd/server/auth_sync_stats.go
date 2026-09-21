// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。

// authSyncStats auth-sync 周期刷新 worker 状态（存活/状态最小集）。
type authSyncStats struct {
	Running           bool  `json:"running"`              // 周期循环存活（Start 置位、退出复位）
	LastReloadUnixMs  int64 `json:"last_reload_unix_ms"`  // 最近一次 Reload 成功完成时刻（0 = 尚未成功；语义修正——失败不前移）
	Failures          int64 `json:"failures"`             // Reload 失败累计次数（新增：快照陈旧可观测）
	LastFailureUnixMs int64 `json:"last_failure_unix_ms"` // 最近一次失败时刻（0 = 从未失败；新增）
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；
// 装配链路见 internal/handler/ops.go 文件头）。
func (w *authSync) Stats() any {
	return authSyncStats{
		Running:           w.running.Load(),
		LastReloadUnixMs:  w.lastReload.Load(),
		Failures:          w.failures.Load(),
		LastFailureUnixMs: w.lastFailure.Load(),
	}
}
