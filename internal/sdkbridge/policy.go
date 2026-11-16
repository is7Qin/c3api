// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import codexsdk "github.com/is7Qin/codex-sdk"

// DefaultRotatePolicy 默认线程轮换策略（每线程最多活 16–48 个窗口）。
//
// 存在的意义只有一个：让装配根不必为了拿一个默认值而 import SDK。轮换策略是
// **协议侧**参数（窗口跨度与退休阈值都由 SDK 定义），T3 会把它的宿主形状收敛到
// 本包（`sdkbridge.RotatePolicy`），届时本函数不再返回 SDK 类型。
func DefaultRotatePolicy() codexsdk.RotatePolicy { return codexsdk.DefaultRotatePolicy() }
