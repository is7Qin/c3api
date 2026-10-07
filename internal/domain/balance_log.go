// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "time"

// BalanceLogSource 余额变动来源枚举（永久余额 users.balance 的非 usage 变动）。
type BalanceLogSource string

const (
	// BalanceSourceSignupDefault 注册默认余额（default_user_balance > 0）。
	BalanceSourceSignupDefault BalanceLogSource = "signup_default"
	// BalanceSourceAdminCreate 管理面建用户初始余额。
	BalanceSourceAdminCreate BalanceLogSource = "admin_create"
	// BalanceSourceAdminAdjust 管理面改余额（显式且实际变动）。
	BalanceSourceAdminAdjust BalanceLogSource = "admin_adjust"
	// BalanceSourceRedemption 兑换码兑换 balance 型。
	BalanceSourceRedemption BalanceLogSource = "redemption"
)

// Valid 四值枚举校验。
func (s BalanceLogSource) Valid() bool {
	switch s {
	case BalanceSourceSignupDefault, BalanceSourceAdminCreate,
		BalanceSourceAdminAdjust, BalanceSourceRedemption:
		return true
	}
	return false
}

// BalanceLog 余额变动记录（只读查询面：管理面 /api/admin/users/{id}/balance-logs）。
// 写面仅 CreateBalanceLog（标量参数构造）；每笔与其对应的 users.balance 变更
// 同事务提交。
type BalanceLog struct {
	ID           int64
	UserID       int64
	Amount       int64 // 毫分，有符号：+ 增加 / - 减少
	BalanceAfter int64 // 变更后 users.balance 快照（毫分）
	Source       BalanceLogSource
	OperatorID   int64   // 0 = 系统/用户自助；>0 = platform_admin 用户 id
	Note         *string // 附加说明（兑换→兑换码文本；其余可空）
	CreatedAt    time.Time
}
