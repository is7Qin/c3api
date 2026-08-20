// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "time"

// TempBalance 临时额度行（只读查询面：/api/user/temp-balances + /api/admin/temp-balances）。
// 写面仅 CreateTempBalance（标量参数）；扣费走 FEFO 条件更新不经过本类型。
type TempBalance struct {
	ID        int64
	UserID    int64
	Amount    int64      // 毫分（1 USD = 100,000 毫分；handler 边界 /1e5 → USD）
	ExpiresAt *time.Time // nil = 永久额度
	Note      *string    // 固定系统备注（signup bonus / redemption code），无敏感信息
	CreatedAt time.Time
}
