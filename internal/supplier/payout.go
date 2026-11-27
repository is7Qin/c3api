// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import "github.com/is7qin/c3api/internal/domain"

// payout.go 付款风控门的**别名**：canonical 实现已下沉 internal/domain
// 的 supplier_rules.go（repository 在 claim 事务里要用它判定，不该依赖 worker 编排包）。

type (
	// PayoutGateConfig 付款风控门阈值。
	PayoutGateConfig = domain.PayoutGateConfig
	// BillingProbe 扣费链健康探针结果。
	BillingProbe = domain.BillingProbe
)

var (
	// ErrPayoutGate 付款风控门拒绝。
	ErrPayoutGate = domain.ErrPayoutGate
)

// EvaluatePayoutGate 放行谓词。
var EvaluatePayoutGate = domain.EvaluatePayoutGate
