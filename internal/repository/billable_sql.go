// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billable_sql.go 计费/统计谓词的唯一事实源（spec-commit-state-and-round-billing
// §0 P0-A）。两常量与写侧 proxy.routeLog 的分支逐字对应（forward.go:462-473）：
//
//	if l.ErrorType == domain.ErrNone || commitStateBillable(l.CommitState) {
//	    ... p.rec.Record(l)          // → usage_logs（可计费行）
//	}
//	if l.ErrorType != domain.ErrNone {
//	    ... p.enqueueErrLog(l)       // → err_logs（错误明细，双轨）
//	}
//
// 其中 commitStateBillable（forward.go:483-493）= commit_state ∈ {sent_ambiguous,
// response_started}；nil / not_sent / upstream_responded 一律
// 不计费。写侧是「按交付事实计费」：已写帧的失败行（如 error_type=5xx +
// commit_state=response_started）同样进 usage_logs 且 billed=false，读侧账本与
// 统计必须用同一判据消费，否则这些行会永久滞留在 usagelog_unbilled_id 部分索引。

// billableSQL usage_logs 侧可计费谓词：与 forward.go:463 的「进 usage_logs」
// 分支逐字对应（error_type='none' 或 commit_state 已交付）。账本五条 SQL
// （取批/队头/两车道结算/提现探针）与统计 usage 源共用本常量。
//
// COALESCE 把 commit_state 的 NULL 补成空串，不可省：SQL 三值逻辑下 NULL NOT IN (...) 求值为
// NULL（不是 TRUE），会让 NULL 行从补集谓词里静默漏掉；COALESCE 先把未知落成
// 空串，稳定落在集合外。空串是 SQL 侧的「未写/未知」，不是写侧的 not_sent
// ——NULL 的归属由写侧路由决定（不进 usage_logs、但进 err_logs），这里只做
// 与写侧逐字互补的判定，不解释成任何具体状态。
const billableSQL = `(error_type = 'none' OR COALESCE(commit_state, '') IN ('sent_ambiguous','response_started'))`

// notBillableErrSQL err_logs 侧互补谓词：只计「不可计费」的错误行，防止
// 双轨写（同一 attempt 既进 err_logs、又可计费进 usage_logs）被统计双计。
// 与 billableSQL 严格互补：error_type='none' 的成功行永不属错误源；错误行仅当
// commit_state 不在可计费集合（含 NULL 经 COALESCE 落成的空串）时归 err 源。
// 统计 err 源共用本常量。
const notBillableErrSQL = `(error_type <> 'none' AND COALESCE(commit_state, '') NOT IN ('sent_ambiguous','response_started'))`
