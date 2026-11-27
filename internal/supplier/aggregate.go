// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"github.com/is7qin/c3api/internal/domain"
)

// 记账聚合的 canonical 实现已下沉 internal/domain 的 supplier_rules.go
// （持久化实现要用它做守恒校验，不该反向依赖 worker 编排包）。
// 行值的别名见下方 type 块。

// AggregateBatch 按 uid 聚合与源日封账增量（含金额守恒与溢出检查）。
var AggregateBatch = domain.AggregateBatch

// AddChecked int64 加法溢出检查（失败闭合，不静默钳制）。
var AddChecked = domain.AddChecked

type (
	// BatchRow credit 取批单行。
	BatchRow = domain.BatchRow
	// UIDCredit 单供应商本批记账增量。
	UIDCredit = domain.UIDCredit
	// DayRecon 源日封账增量。
	DayRecon = domain.DayRecon
)
