// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// --- 可读失败文案（spec-commit-state-and-round-billing §3.2） ---
//
// abort / timeout 的 error_message **禁止**落库内部文案（用户实证：中止行的
// 错误信息列出现 `failed to get reader: failed to read frame header: EOF`）。
// 这两类是「谁先结束」与「等不到产出」的网关侧结论，落可读结论即可；原始
// error 仍进进程日志（debug 面），但不进用户可见字段。
const (
	// msgClientAbortedBeforeDone 客户端在响应完成前结束（干净关闭或裸断）。
	msgClientAbortedBeforeDone = "客户端在响应完成前断开"
	// msgUpstreamTimeoutNoOutput 网关等上游产出的预算内零产出。
	msgUpstreamTimeoutNoOutput = "上游在预算内未产出"
)

// errGatewayUpstreamTimeout 是网关流预算（p.cfg.UpstreamStreamTimeout /
// p.cfg.UpstreamTimeout）到期的**可识别 cause**（spec §3.1）。
//
// 为什么必须存在：各 caller 用 `context.WithTimeoutCause(ctx, budget, 本哨兵)`
// 从**客户端请求 ctx** 派生网关 ctx；父 ctx 到期（客户端自己的 deadline）与
// 网关预算到期都会让 `ctx.Err() == context.DeadlineExceeded`。旧实现只看这一点，
// 于是客户端 deadline 被错记成上游问题（network）。用 `context.Cause(ctx)` 比对
// 哨兵才能把两者分开：命中哨兵 ⇒ timeout（上游侧），否则 ⇒ abort（客户端侧）。
var errGatewayUpstreamTimeout = errors.New("proxy: gateway upstream stream budget exhausted")

// gatewayBudgetError 给「网关预算到期」的传输错误**打标记**（不改文案、不改错误链）：
// 非流式/未提交路径的 ctx 在 helper 内派生并丢弃，归类点（pipeline 的
// classifyFailoverAttempt / writeFailoverExhausted）拿不到它，只能靠错误本身携带
// 这一事实。`Is` 只对哨兵成立，故 `statusOf`/`upstreamBody`/`errors.As` 行为不变。
type gatewayBudgetError struct{ err error }

func (e gatewayBudgetError) Error() string { return e.err.Error() }
func (e gatewayBudgetError) Unwrap() error { return e.err }
func (e gatewayBudgetError) Is(target error) bool {
	return target == errGatewayUpstreamTimeout
}

// markGatewayBudget 网关预算到期时给 err 打标记（其余情形原样返回，含 nil）。
// 调用点全在错误路径（请求失败后一次），不进热路径。
func markGatewayBudget(ctx context.Context, err error) error {
	if err == nil || !errors.Is(context.Cause(ctx), errGatewayUpstreamTimeout) {
		return err
	}
	return gatewayBudgetError{err: err}
}

// zeroStatusFailureClass 零状态（code == 0）失败的类别（§3 归因表）：网关预算
// 到期 = 上游在预算内未产出 ⇒ timeout（seed-timeout 10m）；其余零状态失败仍是
// 传输层断开 ⇒ network（seed-network 5s）。非流式零帧与 SSE 未提交零帧共用这一
// 单一判定——它们都归 pipeline，不在 caller 内归因。
func zeroStatusFailureClass(cause error) (domain.ErrorType, rule.Kind) {
	if errors.Is(cause, errGatewayUpstreamTimeout) {
		return domain.ErrTimeout, rule.KindTimeout
	}
	return domain.ErrNetwork, rule.KindNetwork
}

// --- 流失败归因（spec-stream-failure-truth §3，唯一派生） ---
//
// I1：一次 dispatched attempt 只有一个终态归因，由**单一函数**产生；各出口只提供
// 事实（cause / exit kind / usage / 交付事实），不得自行拼 error_type / kind /
// status —— 旧实现每个出口各拼一套（codex HTTP 恒 network、chat/WS 恒 abort），
// 同一次故障在三条路径上得出三种结论。
//
// 热路径纪律（I6）：本文件全部函数只在 relay 返回后的收尾路径调用**一次**，不得
// 进 OnEvent / 逐帧循环（statusOf 含反射兜底，逐帧调用是明确禁止的）。

// streamFailure 一次已提交尝试的终态归因（四个字段就是各出口要写的全部结论）。
type streamFailure struct {
	// Status 归因状态：写日志 status_code 与规则事件 HTTPStatus。0 = 上游没给过
	// 状态码（序 1 PayloadError / 序 5 断流）——**不是**缺失值，而是「网关不合成
	// 上游没给过的码」这一裁决的表示（§3.1）。
	Status int
	ET     domain.ErrorType
	Kind   rule.Kind
	// Message 可读原因（上游信封 message 优先，退 err.Error()）：日志
	// error_message 与健康事件 message 同源，空 = 无原因（落库保持 NULL）。
	Message string
}

// classifyStreamFailure 唯一归因函数（spec §3 序 1–5）。序 0a–0c（客户端侧结束）
// 由 classifyStreamExit 承担，不进入本函数——本函数返回的 ET 恒为失败类别。
//
// 序 1 命中时**不调用 statusOf**：PayloadError 只承载 type/message（不含 status），
// 问它只会走一遍反射兜底拿到 0，白付一次反射。
func classifyStreamFailure(cause error) streamFailure {
	// 序 0b'：SDK WS 读空闲超时——上游连接活着但预算内零产出（§5）。必须排在
	// statusOf 之前：该哨兵**不**包装 any 状态（statusOf 会返 0，落到序 5 被记成
	// network）。文案落可读结论：SDK 自述文本属于库内部文案（§3.2）。
	if errors.Is(cause, codexsdk.ErrWSIdleTimeout) {
		return streamFailure{ET: domain.ErrTimeout, Kind: rule.KindTimeout, Message: msgUpstreamTimeoutNoOutput}
	}
	// 序 0b'：网关预算到期（非流式/未提交路径把该事实挂在错误链上，见
	// markGatewayBudget）——同一条判定，避免两条路径各写一套。
	if errors.Is(cause, errGatewayUpstreamTimeout) {
		return streamFailure{ET: domain.ErrTimeout, Kind: rule.KindTimeout, Message: msgUpstreamTimeoutNoOutput}
	}
	msg := failureMessage(cause)
	// 序 1：流内错误载荷 —— 上游已明确表态。优先于客户端取消（序 1 竞态）：
	// 上游语义权威高于客户端侧信号，归因不得被取消覆盖。
	if _, ok := errors.AsType[*sserelay.PayloadError](cause); ok {
		return streamFailure{ET: domain.Err5xx, Kind: rule.Kind5xx, Message: msg}
	}
	// 序 2–5：状态驱动。PayloadError.Type 不可靠（`{"type":"error","code":
	// "invalid_request_error"}` 会退化成字面量 "error"，真类别在未捕获的 code
	// 里）⇒ 按 error.type 分档不可行，只能按 status。
	switch st := statusOf(cause); {
	case st == 429: // 序 2
		return streamFailure{Status: st, ET: domain.Err429, Kind: rule.Kind429, Message: msg}
	case st >= 400 && st <= 499: // 序 3
		return streamFailure{Status: st, ET: domain.Err4xx, Kind: rule.Kind4xx, Message: msg}
	case st >= 500 && st <= 599: // 序 4
		return streamFailure{Status: st, ET: domain.Err5xx, Kind: rule.Kind5xx, Message: msg}
	default: // 序 5：关闭帧 / EOF / 读失败（statusOf == 0）
		return streamFailure{ET: domain.ErrNetwork, Kind: rule.KindNetwork, Message: msg}
	}
}

// streamFailureHealth 归因 → 健康事件（I4：chat / 入站 WS / codex HTTP 三条路径
// 必须给出同一个 kind/status/message）。Kind == 0 表示本次归因不投递健康（序 0c
// 客户端侧结束）⇒ nil，调用方直接把 nil 交给观测，不必各自判断。
func streamFailureHealth(f streamFailure) *AttemptHealthEvent {
	if f.Kind == 0 {
		return nil
	}
	return &AttemptHealthEvent{Kind: f.Kind, Status: f.Status, ErrorMessage: f.Message}
}

// timeoutHealth 冷却条件（§5）：timeout **仅在本轮零业务帧**时携带健康事件。
// 已发业务帧的 timeout 只记日志不冷却（与 abort 同档）——上游已产出真实业务帧，
// 把它当「一点都没产出」罚 10 分钟等于把客户端侧或长尾成本算到上游头上。
//
// businessFramesSent 由各出口用**现成**的交付事实提供（sserelay.Output 的
// BusinessFrameSent() / AttemptOutcome.BusinessFrameSent），不新造计数器。
func timeoutHealth(f streamFailure, businessFramesSent bool) *AttemptHealthEvent {
	h := streamFailureHealth(f)
	if h != nil && h.Kind == rule.KindTimeout && businessFramesSent {
		return nil
	}
	return h
}

// streamFailureOf 已提交 / 写失败出口的终态归因入口（spec §3 序 0b–0c + 1–5）。
//
// classifyStreamFailure 只吃 cause（序 1–5），而 streamExitWriteFailed 同时覆盖
// 三类完全不同的事实：**下游写不进去**（客户端侧结束 ⇒ 序 0c，abort）、**请求
// 父 ctx 自己的 deadline 到期**（同样是客户端侧结束 ⇒ abort）与**网关自己的
// UpstreamStreamTimeout 到期**（网关侧失败 ⇒ 序 0b，timeout）。
//
// 三分只能看 ctx，因此单列在此（`context.Cause` 比对网关哨兵），各出口不再自建
// 第二套判定：
//
//  1. `errors.Is(context.Cause(ctx), errGatewayUpstreamTimeout)` ⇒ timeout（上游
//     在预算内未产出，冷却策略见 seed-timeout）；
//  2. 其余任何 `ctx.Err() == context.DeadlineExceeded`（= 请求 parent 自己的
//     deadline 到期）⇒ abort（**不再记 network**：客户端自己的期限不是上游问题）；
//  3. 其余（下游连接断裂 / SelfCanceled）⇒ abort。
//
// 序 0a（客户端先取消、非 Output 自取消）不经过本函数：调用方直接记 abort 且不
// 投递健康（I3/I8）。
func streamFailureOf(ctx context.Context, kind streamExitKind, cause error) streamFailure {
	switch kind {
	case streamExitClientCancel:
		// 序 0a：客户端侧结束。调用方一般不走到这里（0a 直接记 abort），此处给出
		// 同一结论，保证任何入口都不会把它误判成上游失败。
		return streamFailure{ET: domain.ErrAbort, Message: msgClientAbortedBeforeDone}
	case streamExitWriteFailed:
		// 下面按 context.Cause 分 0b / 0c。
	default:
		return classifyStreamFailure(cause)
	}
	if errors.Is(context.Cause(ctx), errGatewayUpstreamTimeout) {
		// 序 0b：网关自己的流预算到期（上游不产出），不是客户端结束。
		return streamFailure{ET: domain.ErrTimeout, Kind: rule.KindTimeout, Message: msgUpstreamTimeoutNoOutput}
	}
	// 序 0c：客户端侧结束——父 ctx 自己的 deadline 到期、SelfCanceled、下游连接
	// 断裂都在此列。旧实现在这里判 `ctx.Err() == DeadlineExceeded` 并记 network，
	// 把客户端期限错当成上游故障（§3.1 P0）。
	return streamFailure{ET: domain.ErrAbort, Message: msgClientAbortedBeforeDone}
}

// userFacingFailureMessage 落库 error_message 的统一出口：abort / timeout 落
// 可读文案（§3.2 禁止库内部文本），其余类别沿用 causeMessage（上游信封原文优先）。
// 空 = 无原因（落库保持 NULL 语义）。
func userFacingFailureMessage(et domain.ErrorType, cause error) *string {
	switch et {
	case domain.ErrAbort:
		m := msgClientAbortedBeforeDone
		return &m
	case domain.ErrTimeout:
		m := msgUpstreamTimeoutNoOutput
		return &m
	}
	return causeMessage(cause)
}

// failureMessage 归因文本（与日志 error_message 同一口径，复用 causeMessage）：
// 上游信封 message 优先，退 err.Error()，两者都空 ⇒ 空串（落库保持 NULL 语义）。
func failureMessage(cause error) string {
	if m := causeMessage(cause); m != nil {
		return *m
	}
	return ""
}

// message 归因文本的落盘形式（空 ⇒ nil，保持 NULL 语义：不留空串）。
func (f streamFailure) message() *string {
	if f.Message == "" {
		return nil
	}
	m := f.Message
	return &m
}
