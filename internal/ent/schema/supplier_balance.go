package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SupplierBalance 供应商收益余额（一供应商一行；spec 2026-10-09 §3.4）。
// 独立于 users：供应商同时是消费者时用户余额行被两条链路争抢，且用户余额
// 快照预检会把平台应付账款误当可消费额度（§3.4 三条理由）。无外键
// （supplier_user_id 逻辑指向 users.id，§10）——分区表外键会串行化 DROP，
// 靠 §2.5 值域校验 + §2.7 生命周期兜底。
//
// ent 无自定义主键名/复合主键能力：自然键 supplier_user_id 以 UNIQUE 约束
// 承载（ON CONFLICT 目标与查找等价），物理主键为 ent 默认代理 id。DB CHECK
// 由 ent postgres 迁移的 checks 注解生成（M2）。
type SupplierBalance struct{ ent.Schema }

func (SupplierBalance) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("supplier_user_id"),                 // 逻辑指向 users.id（唯一自然键）
		field.Int64("available").Default(0),             // 可提领（结算申请条件扣）
		field.Int64("lifetime_credited").Default(0),     // 累计已入账（只增，永久稳定）
		field.Int64("lifetime_paid").Default(0),         // 累计已付（只增，永久稳定）
		field.Int("share_bp").Optional().Nillable(),     // NULL = 继承 config.share_bp_default
		field.Int("freeze_hours").Optional().Nillable(), // NULL = 继承；0 = 不冻结；>0 = 覆盖
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// Indexes supplier_user_id 唯一自然键（ON CONFLICT (supplier_user_id) 目标）。
func (SupplierBalance) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("supplier_user_id").Unique(),
	}
}

// Annotations DB CHECK 约束（M2；ent postgres 默认迁移经 checks 注解生成）。
func (SupplierBalance) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Checks: map[string]string{
			"supplier_balances_available_non_negative":         "available >= 0",
			"supplier_balances_lifetime_credited_non_negative": "lifetime_credited >= 0",
			"supplier_balances_lifetime_paid_non_negative":     "lifetime_paid >= 0",
			"supplier_balances_share_bp_range":                 "share_bp IS NULL OR (share_bp >= 0 AND share_bp <= 10000)",
			"supplier_balances_freeze_hours_non_negative":      "freeze_hours IS NULL OR freeze_hours >= 0",
		}},
	}
}
