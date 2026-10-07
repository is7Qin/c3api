// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// BalanceLog 永久余额（users.balance）的非 usage 变动记录表。
// 逐笔落库：注册默认余额 / 管理面建用户初始余额 / 管理面改余额 / 兑换码
// balance 型，与余额变更同事务提交。user_id 为普通列无 FK 边（对齐 usage_logs/
// err_logs 日志面：不做外键，避免删除耦合）。临时额度不入本表（temp_balances
// 自有行），usage 按次消费不入本表（已在 usage_logs）。普通表（非分区）：
// 低频（每用户个位数~百级行），无保留期/不 DROP，随 ent migrate 自动建表。
// 不回溯：存量用户无历史，表从空开始（fresh-setup）。
type BalanceLog struct{ ent.Schema }

func (BalanceLog) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"),       // 归属用户（普通列，无 FK 边）
		field.Int64("amount"),        // 有符号毫分：+ 增加 / - 减少（1 USD = 100,000 毫分）
		field.Int64("balance_after"), // 变更后 users.balance 快照（毫分）
		field.Enum("source").Values("signup_default", "admin_create", "admin_adjust", "redemption"),
		field.Int64("operator_id").Default(0),      // 0 = 系统/用户自助；>0 = platform_admin 用户 id（静态 admin token 路径 → 0）
		field.String("note").Optional().Nillable(), // 附加说明（兑换→兑换码文本；其余可空）
		field.Time("created_at").Default(time.Now),
	}
}

// Indexes 按用户分页（WHERE user_id EQ + ORDER BY id DESC）的索引载体。
func (BalanceLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "id"),
	}
}
