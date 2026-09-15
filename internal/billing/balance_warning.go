// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import "github.com/is7qin/c3api/internal/domain"

// BalanceWarningSink accepts committed warning events without waiting for downstream work.
type BalanceWarningSink interface {
	TrySubmit(domain.BalanceWarningEvent) bool
}

// BalanceWarningSink 由 NewFlusher 构造期一次注入（ctor 参数，禁止事后回填）。
// TrySubmit 必须非阻塞；nil sink = 事件丢弃（R10，见 drain.go applySettlement 守卫）。
