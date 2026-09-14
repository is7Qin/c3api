// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package scheduler

// /ops/workers 可观测面（spec 2026-08-11）：独立 Stats 契约不改 worker.Worker
// 接口——装配侧类型断言聚合（main.go），响应 typed struct 非 map。
// 采集纪律：len(channel) 零成本；快照状态走 snapshot registry Status，不重复。

// SchedulerStats 调度器状态（编译道观测；快照/路由
// 状态经注册表 Status 直出，此处不重复采集。legacy 状态回写队列已随
// cutover 删除——持久状态列不存在，无回写可言）。
type SchedulerStats struct {
	// 编译道（Task18 wiring）：pending=待触发编译信号（cap 1，trailing-edge
	// 合并是设计语义非丢弃）；last_compile_*_unix_ms=最近一次成功/失败编译
	// 时刻（0=从未，失败保留旧视图是契约）；decision_generation/decision_routes
	// = 当前发布 DecisionView 的世代与路由数（0/0 = 编译道未发布过）。
	CompilePending       int    `json:"compile_pending"`
	CompileCap           int    `json:"compile_cap"`
	LastCompileOKUnixMs  int64  `json:"last_compile_ok_unix_ms"`
	LastCompileErrUnixMs int64  `json:"last_compile_err_unix_ms"`
	DecisionGeneration   uint64 `json:"decision_generation"`
	DecisionRoutes       int    `json:"decision_routes"`
	// Incident lane (expose-only): active_incidents = routes with an active
	// mark at the last successful fire; last_incident_eval_unix_ms = last
	// fire that evaluated incidents (0 = 从未发生）.
	ActiveIncidents        int   `json:"active_incidents"`
	LastIncidentEvalUnixMs int64 `json:"last_incident_eval_unix_ms"`
}

// Stats 满足 handler.StatsProvider（独立于 worker.Worker 契约；装配链路见 internal/handler/ops.go 文件头）。
func (s *Scheduler) Stats() any {
	st := SchedulerStats{
		CompilePending:         len(s.compileCh),
		CompileCap:             cap(s.compileCh),
		LastCompileOKUnixMs:    s.compileOKMs.Load(),
		LastCompileErrUnixMs:   s.compileErrMs.Load(),
		ActiveIncidents:        int(s.incidentActive.Load()),
		LastIncidentEvalUnixMs: s.incidentEvalMs.Load(),
	}
	if v := s.view.Load(); v != nil && v.decision != nil {
		st.DecisionGeneration = v.decision.generation
		st.DecisionRoutes = len(v.decision.routes)
	}
	return st
}

// RuntimeHealthStats 健康投影道观测（Task18 ops 可见性）：records=当前视图
// 条目数（按态分桶）；generation=已同步的全局健康世代；last_sync_ok_unix_ms=
// 最近一次成功 Sync 时刻（0=从未，freshness 锚）；sync_errors=Sync 失败累计
// （视图冻结的可见痕迹）；last_tick_ok=最近 tick 成败。
type RuntimeHealthStats struct {
	Records          int   `json:"records"`
	Open             int   `json:"open"`
	RetryAfter       int   `json:"retry_after"`
	Probing          int   `json:"probing"`
	Ready            int   `json:"ready"`
	Generation       int64 `json:"generation"`
	LastSyncOKUnixMs int64 `json:"last_sync_ok_unix_ms"`
	SyncErrors       int64 `json:"sync_errors"`
	LastTickOK       bool  `json:"last_tick_ok"`
}

// Stats 满足 handler.StatsProvider：视图原子读 + 状态计数（冷路径，O(视图)）。
func (h *RuntimeHealth) Stats() any {
	st := RuntimeHealthStats{
		Generation:       h.curGen.Load(),
		LastSyncOKUnixMs: h.lastSyncOkMs.Load(),
		SyncErrors:       h.syncErrors.Load(),
		LastTickOK:       h.lastTickOk.Load(),
	}
	if v := h.view.Load(); v != nil {
		st.Records = len(v.entries)
		for _, e := range v.entries {
			switch e.State {
			case StateOPEN:
				st.Open++
			case StateRetryAfter:
				st.RetryAfter++
			case StateProbing:
				st.Probing++
			case StateReady:
				st.Ready++
			}
		}
	}
	return st
}
