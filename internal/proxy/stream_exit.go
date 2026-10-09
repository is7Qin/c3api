// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"

	"github.com/is7qin/c3api/pkg/sserelay"
)

// streamExitKind 是 relay 读循环退出后的统一出口判定（阶段② §3.7，全 SSE
// 路径共用单一判定；各 caller 不复制分支）。判定前调用方须已 StopTimer/汇合
// 在途心跳以冻结提交态（Relay 返回时已做）。
type streamExitKind int

const (
	// streamExitClientCancel 客户端取消：不补写。
	streamExitClientCancel streamExitKind = iota
	// streamExitWriteFailed 真实下行写失败：不补写。
	streamExitWriteFailed
	// streamExitUncommitted 未提交读取失败：交 pipeline（handled=false，可
	// failover/写 JSON）；Output 缓冲残余业务字节直接丢弃。
	streamExitUncommitted
	// streamExitCommitted 已提交且写侧可用：客户端协议 SSE error。
	streamExitCommitted
)

// classifyStreamExit 冻结提交态后的单一出口判定（顺序见 §3.7）。
func classifyStreamExit(out *sserelay.Output, err error) streamExitKind {
	switch {
	case errors.Is(err, context.Canceled):
		return streamExitClientCancel
	case out.WriteFailed():
		return streamExitWriteFailed
	case !out.Committed():
		return streamExitUncommitted
	default:
		return streamExitCommitted
	}
}

// writeClientStreamError 写客户端协议 SSE error 帧（已提交且写侧可用时）；
// WriteFailed 由 Output.WriteError 内部短路（不补写）。
func writeClientStreamError(out *sserelay.Output, err error) {
	_ = out.WriteError(buildErrorFrame(streamErrMessage(err)))
}
