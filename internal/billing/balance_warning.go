// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import "github.com/is7qin/c3api/internal/domain"

// BalanceWarningSink accepts committed warning events without waiting for downstream work.
type BalanceWarningSink interface {
	TrySubmit(domain.BalanceWarningEvent) bool
}

// SetBalanceWarningSink injects the composition-time warning sink. TrySubmit must not block.
func (f *Flusher) SetBalanceWarningSink(sink BalanceWarningSink) { f.warningSink = sink }
