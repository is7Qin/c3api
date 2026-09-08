// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。

// ListenerStats NOTIFY 监听 worker 状态（存活最小集）。
type ListenerStats struct {
	Running bool `json:"running"` // 监听循环存活（Start 置位、run 退出复位；原子读零锁）
	Ready   bool `json:"ready"`   // LISTEN + FullRefresh 已完成，可作为不漏增量的启动屏障
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (l *Listener) Stats() any {
	return ListenerStats{Running: l.running.Load(), Ready: l.ready.Load()}
}
