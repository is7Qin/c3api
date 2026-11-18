// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// BPSNoWindowQuotaResolver is the initial independent BPS quota resolver.
// BPS currently exposes no trusted quota-window/reset API, so it deliberately
// returns no deadline instead of fabricating one or reusing Codex quota logic.
type BPSNoWindowQuotaResolver struct{}

func (BPSNoWindowQuotaResolver) ResolveThrottleDeadline(context.Context, *domain.Account) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
