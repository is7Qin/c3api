// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "time"

// AttemptLog 是一次上游尝试的审计行（attempt_logs，按日分区；spec 2026-11-18
// retry-observability §3）。
//
// 语义边界（诚实版）：只在 failover 循环**可见**的失败上落行——handled=true 的
// 已交付失败与本地拒绝（401/额度/402 预检）不产生行，它们的真值仍在
// usage_logs / err_logs。一次 dispatch 最多一行/一次失败尝试。
//
// 本表只写不读：链式查询（S3）走 SQL，故没有 ent 实体，本类型纯值语义。
type AttemptLog struct {
	RequestID string
	// Ordinal 是 scheduler.Attempt.Ordinal。**允许空洞**（本地拒绝、402 预检不
	// 产生行）：不得假设连续，也不得用 max(ordinal) 反推尝试次数。
	Ordinal int16
	// AccountID 是被尝试的上游账号（>0；本地拒绝不落本表，故无 0 = 无 的语义）。
	AccountID int64
	// Lane 是尝试所在的调度 lane（scheduler 的通道/优先级分道）。
	Lane string
	// Format 是请求协议（与 usage_logs.format 同词表）。
	Format string
	// StatusCode 是上游/中间层返回的 HTTP 状态（0 = 未拿到响应，如网络错误）。
	StatusCode int
	// Outcome 与 flowOutcomeToken 同一投影（含 client_cancel）——不得自造枚举，
	// 否则会重新引入「客户端取消被误记为上游错误」。
	Outcome string
	// ErrorType 与 dispatchFailureOutcome 的 domain.Err* 词表同一投影。
	ErrorType string
	// CommitState 交付事实（nil = 未知，与 usage_logs/err_logs 同义）。
	CommitState *string
	// ErrorMessage 已按写点纪律截断（TruncateErrMsg 500）；nil = 无文本。
	ErrorMessage *string
	LatencyMS    int64
	CreatedAt    time.Time
}
