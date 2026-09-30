// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// logQueryLimit 明细 keyset 查询 limit 归一：≤0 → 20（UsageQuery/ErrLogQuery
// 两查询共用——原两处各写一份 if q.Limit <= 0）。
func logQueryLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	return limit
}

// logValueOrZero 行可选列指针 → 值（nil → 零值）。usage_logs/err_logs 行映射
// 共用，替代逐字段 `if row.X != nil { l.X = *row.X }` 的重复 nil 检查。
func logValueOrZero[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
