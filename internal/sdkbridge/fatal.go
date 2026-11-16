// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"errors"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// 帧与错误的**判据出处**集中在这里。
//
// 归属理由：哪些码算账号死亡、哪些错误算"请求体不能整形"、哪一个是"上游读空闲
// 超时"——这三条都是 SDK 与上游协议之间的事实，不是网关的策略。网关只消费结论，
// 不维护码集副本，也不按状态码反推（4xx ≠ 凭据死，只有 SDK 说了才算）。

// ClassifyFatal 判定一帧上游数据是否在声明**账号级终止**（鉴权码失效 / 账号停用）。
//
// 调用方负责零分配预筛（本进程的成本决定，不是判据）：没有 `"error"` 字节的帧不
// 必进来。返回 nil 表示这一帧只是普通业务流。
func ClassifyFatal(frame []byte) error { return codexsdk.ClassifyFatalUpstream(frame) }

// IsInvalidBody 判定错误是否为 SDK 本地拒绝（请求体不能整形为上游可接受的形状）。
// 穿透 EnvelopeError 与 DialError 的 Unwrap 链——SDK 在**出网前**就拒绝，
// 所以这是"未发送"的确定事实，不是网络失败。
func IsInvalidBody(err error) bool {
	return err != nil && errors.Is(err, codexsdk.ErrInvalidRequestBody)
}

// IsWSIdleTimeout 判定错误是否为 SDK WS 通道的读空闲超时（一次取帧等待里没有任何
// 可交付帧；Ping/Pong 不续命）。归上游侧超时，不是网关自身预算到期。
func IsWSIdleTimeout(err error) bool { return errors.Is(err, codexsdk.ErrWSIdleTimeout) }
