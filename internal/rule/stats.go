// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：原子读 + len(channel)（零锁零分配，O(1)）。

// RuleEngineStats 规则引擎状态（两阶段有界队列可观测面，Task2）。
type RuleEngineStats struct {
	Queued            int   `json:"queued"`               // 准入队列积压
	QueueCap          int   `json:"queue_cap"`            // 准入容量
	Dropped           int64 `json:"dropped"`              // alias admission_dropped
	DropWarnThreshold int64 `json:"drop_warn_threshold"`  // 告警阈值
	AdmissionDropped  int64 `json:"admission_dropped"`    // 准入满丢弃
	MatchedActions    int64 `json:"matched_actions"`      // 命中计数
	PersistQueued     int   `json:"persist_queued"`       // 持久化队列积压
	PersistCap        int   `json:"persist_cap"`          // 持久化容量
	PersistDropped    int64 `json:"persist_dropped"`      // 持久化满丢弃
	PersistFailures   int64 `json:"persist_failures"`     // 持久化执行失败
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (e *RuleEngine) Stats() any {
	return RuleEngineStats{
		Queued:            len(e.ch),
		QueueCap:          cap(e.ch),
		Dropped:           int64(e.dropped.Load()),
		DropWarnThreshold: ruleDropWarnThreshold,
		AdmissionDropped:  int64(e.dropped.Load()),
		MatchedActions:    int64(e.matched.Load()),
		PersistQueued:     int(e.persistPending.Load()),
		PersistCap:        cap(e.persistCh),
		PersistDropped:    int64(e.persistDropped.Load()),
		PersistFailures:   int64(e.persistFailures.Load()),
	}
}
