// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

// AttemptPlan 是数据面对计划的**窄消费面**：恰好两个方法（F1 的姊妹断言 ——
// 多一个都算冻结面白扩）。
//
// 为什么只有这两个：
//   - ContinuationIncompatible 是构造期定型的只读政策位（"本次派发必须先解析
//     previous_response_id"），数据面每续接请求读一次；
//   - AbandonLastAttempt 是"跳过了一个从未派发的候选"的唯一退款动作。
//
// 为什么 ReserveAttempt **不在**这里：它是控制面编排（Scheduler 的方法），
// 数据面只经 p.sched.ReserveAttempt 推进计划，把预留塞进计划接口会把
// 冻结面从 2 个签名扩到整个预留机制（momus §3 / oracle §7）。
//
// typed-nil 合同：实现必须容忍 nil 动态值。控制面今天在 nil / 未绑定计划的
// 两条路径上分别返回保守值（true）与 no-op；数据面可能把一个 typed-nil 的
// *scheduler.AttemptPlan 装进本接口（`var p api.AttemptPlan = (*T)(nil)`），
// 调用不得 panic（用例见 internal/scheduler 的 TestTypedNilAttemptPlan）。
type AttemptPlan interface {
	// ContinuationIncompatible 报告本次派发是否必须解析续接。nil / 未绑定路由
	// ⇒ true（fail-closed：没有候选信息时不能断言"全都原生支持"）。
	ContinuationIncompatible() bool
	// AbandonLastAttempt 退回最近一次从未派发的预留（ordinal/attempt 计数回卷，
	// 游标与去重不重扫）。nil / 无预留 ⇒ no-op。
	AbandonLastAttempt()
}
