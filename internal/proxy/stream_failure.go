// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/pkg/sserelay"
)

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

// streamFailureOf 已提交 / 写失败出口的终态归因入口（spec §3 序 0b–0c + 1–5）。
//
// classifyStreamFailure 只吃 cause（序 1–5），而 streamExitWriteFailed 同时覆盖
// 两类完全不同的事实：**下游写不进去**（客户端侧结束 ⇒ 序 0c，abort）与**网关
// 自己的 UpstreamStreamTimeout 到期**（网关侧失败 ⇒ 序 0b，network）。把后者记
// abort 是误标——这一分叉只能看 ctx，因此单列在此，各出口不再自建第二套判定。
//
// 序 0a（客户端先取消、非 Output 自取消）不经过本函数：调用方直接记 abort 且不
// 投递健康（I3/I8）。
func streamFailureOf(ctx context.Context, kind streamExitKind, cause error) streamFailure {
	switch kind {
	case streamExitClientCancel:
		// 序 0a：客户端侧结束。调用方一般不走到这里（0a 直接记 abort），此处给出
		// 同一结论，保证任何入口都不会把它误判成上游失败。
		return streamFailure{ET: domain.ErrAbort, Message: failureMessage(cause)}
	case streamExitWriteFailed:
		// 下面按 ctx 分 0b / 0c。
	default:
		return classifyStreamFailure(cause)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// 序 0b：网关侧超时到期（写 deadline 已失效），上游失败而非客户端结束。
		return streamFailure{ET: domain.ErrNetwork, Kind: rule.KindNetwork, Message: failureMessage(cause)}
	}
	// 序 0c：客户端侧结束（SelfCanceled / 下游连接断裂），Kind 留零 ⇒ 不投递健康。
	return streamFailure{ET: domain.ErrAbort, Message: failureMessage(cause)}
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
