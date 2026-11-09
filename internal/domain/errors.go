// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "errors"

var ErrPartitionUnavailable = errors.New("partition unavailable")

// ErrUpstreamBackpressure 上游 WS 出站背压（codex-sdk 的 ErrWSBackpressure 经
// sdkbridge 映射而来）：语义是"上游没坏、下游消费太慢"，与网络失败/鉴权失败
// 必须分开，否则重试与账号冷却会误判。sentinel 换 sentinel，不按字符串匹配。
var ErrUpstreamBackpressure = errors.New("upstream backpressure")
