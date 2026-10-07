package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SupplierFrozenChunk 供应商冻结桶（一供应商 × 活跃桶一行；spec 2026-10-09 §3.5）。
// 桶时刻 available_at 写时算定，永不回改（G3）。amount 只增（ON CONFLICT DO
// UPDATE amount + EXCLUDED.amount）或随整行删除，永不归零 ⇒ CHECK (amount > 0)。
// 自然键 (supplier_user_id, available_at)；ent 无复合主键 ⇒ 以 UNIQUE 约束承载
// （ON CONFLICT / 查找等价），物理主键为 ent 默认代理 id。
//
// ctid 禁令：不得用 ctid 作行标识（PG 行版本移动 ⇒ UPDATE 后失效）——解冻链用
// 自然键 + FOR UPDATE SKIP LOCKED + RETURNING（§5.3）。
type SupplierFrozenChunk struct{ ent.Schema }

func (SupplierFrozenChunk) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("supplier_user_id"),
		field.Time("available_at"), // 该桶解冻时刻（写时算定）
		field.Int64("amount"),      // 该桶待解冻额
	}
}

// Indexes：
//   - (supplier_user_id, available_at) UNIQUE —— 自然键（ON CONFLICT 目标）；
//   - (available_at) —— 解冻取批专用（无谓词）：PK/自然键前导列是 supplier_user_id，
//     解冻按 available_at 全局过滤用不上该前缀（§3.5）。
func (SupplierFrozenChunk) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("supplier_user_id", "available_at").Unique(),
		index.Fields("available_at"),
	}
}

// Annotations DB CHECK（M2）：amount 只增/整行删除 ⇒ 恒正。
func (SupplierFrozenChunk) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Checks: map[string]string{
			"supplier_frozen_chunks_amount_positive": "amount > 0",
		}},
	}
}
