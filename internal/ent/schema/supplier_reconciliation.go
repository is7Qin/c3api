package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SupplierReconciliation 供应商永久封账（源日级收入证据；spec 2026-10-09 §3.10）。
// usage_logs 仅 30 天，仅靠可变累计数无法独立证明累计应付从哪些服务事实产生；
// 本表提供永久、紧凑的源日级证据。维度 = 源行 created_at 的 UTC 日（与 usage_logs
// 分区同维），不是处理日。范围 = 只累计进入 credit 链的正收益行（earn > 0）；
// gross_cost 是正收益债权的计价基底，不是全部供应商流量收入。
//
// ent 无复合主键 ⇒ 自然键 (supplier_user_id, source_day) 以 UNIQUE 约束承载，
// 物理主键为 ent 默认代理 id。
type SupplierReconciliation struct{ ent.Schema }

func (SupplierReconciliation) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("supplier_user_id"),
		// source_day 为 DATE（UTC 日），非 timestamptz——与 usage_logs 分区同维，
		// 跨日/停摆追赶不并日（§3.10）。ent Time 默认 timestamptz，此处显式
		// SchemaType 落 date。
		field.Time("source_day").SchemaType(map[string]string{dialect.Postgres: "date"}),
		field.Int64("gross_cost").Default(0), // 该源日正收益债权行的 Σ cost（毫分）
		field.Int64("earned").Default(0),     // 该源日行的 Σ earn（毫分）
		field.Int64("row_count").Default(0),  // 该源日行数
		field.Enum("state").Values("open", "closed").Default("open"),
		field.Int64("closed_revision").Optional().Nillable(), // 封账版本/批号（open 为 NULL）
		field.Time("closed_at").Optional().Nillable(),
	}
}

// Indexes 自然键 (supplier_user_id, source_day) UNIQUE（ON CONFLICT 目标）。
func (SupplierReconciliation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("supplier_user_id", "source_day").Unique(),
	}
}

// Annotations 表名 override（ent 复数化为 supplier_reconciliations，spec §3.10
// 定名 supplier_reconciliation）+ DB CHECK（M2）。
func (SupplierReconciliation) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "supplier_reconciliation", Checks: map[string]string{
			"supplier_reconciliation_gross_cost_non_negative": "gross_cost >= 0",
			"supplier_reconciliation_earned_non_negative":     "earned >= 0",
			"supplier_reconciliation_row_count_non_negative":  "row_count >= 0",
		}},
	}
}
