// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"

	"github.com/is7qin/c3api/pkg/sserelay"
)

// streamExitKind 是 relay 读循环退出后的统一出口判定（§3.7，全 SSE 路径共用
// 单一判定；各 caller 不复制分支）。判定前调用方须已 StopTimer/汇合在途心跳以
// 冻结提交态（Relay 返回时已做）。
type streamExitKind int

const (
	// streamExitClientCancel 客户端取消：不补写。
	streamExitClientCancel streamExitKind = iota
	// streamExitWriteFailed 真实下行写失败（或写 deadline 已失效）：不补写。
	streamExitWriteFailed
	// streamExitUncommitted 未提交读取失败：交 pipeline（handled=false，可
	// failover/写 JSON）；Output 缓冲残余业务字节直接丢弃。
	streamExitUncommitted
	// streamExitCommitted 已提交且写侧可用：客户端协议 SSE error。
	streamExitCommitted
)

// classifyStreamExit 冻结提交态后的单一出口判定（§3.7）。取消/写失败/写 deadline
// 失效 → 不补写；未提交 → 交 pipeline（先把 caller 已采 usage/TTFT 写入 owner
// 观测 carryStreamUsage，使交 pipeline 的失败结束观测携带部分用量）；已提交且
// 写侧可用 → 客户端协议 SSE error。
//
// 区分「心跳/下行写失败自取消」（out.SelfCanceled → 写失败出口，不误记客户端
// 取消的健康惩罚）与「客户端先取消」（ctx 取消 → 客户端取消出口）。
func classifyStreamExit(ctx context.Context, out *sserelay.Output, err error, usage AttemptUsage, ttft *int64) streamExitKind {
	switch {
	case out.SelfCanceled():
		// 写失败触发 Output 取消上游 ctx：真实写失败出口（不补写）。
		return streamExitWriteFailed
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return streamExitClientCancel
	case out.WriteFailed():
		return streamExitWriteFailed
	case !out.Committed():
		carryStreamUsage(ctx, usage, ttft)
		return streamExitUncommitted
	case ctx.Err() != nil:
		// 已提交但写 deadline 已失效（取消/总超时后 relay 设立即过期写 deadline，
		// 不承诺 SSE error 必达）：按失败收口、不补写。
		return streamExitWriteFailed
	default:
		return streamExitCommitted
	}
}

// carryStreamUsage 将 caller 已采 usage/TTFT 写到 owner dispatch 观测（若存在），
// 供 observeDispatchFailure 合并进 handled=false 观测；无 owner 观测（直接调用
// caller、不排空 loop）时 no-op。
func carryStreamUsage(ctx context.Context, usage AttemptUsage, ttft *int64) {
	if d := dispatchFromContext(ctx); d != nil {
		d.carriedUsage = usage
		d.carriedTTFT = ttft
	}
}

// writeClientStreamError 写客户端协议 SSE error 帧（已提交且写侧可用时）；
// WriteFailed 由 Output.WriteError 内部短路（不补写）。
func writeClientStreamError(out *sserelay.Output, err error) {
	_ = out.WriteError(buildErrorFrame(streamErrMessage(err)))
}
