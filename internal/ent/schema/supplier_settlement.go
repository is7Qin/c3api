package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SupplierSettlement 供应商结算单（业务面五态；spec 2026-10-09 §3.6）。
// 不分区、不清理：行数 = 人工操作次数；它是唯一永久付款凭证（长期核验由
// supplier_reconciliation 承担）。kind 区分供应商自申请 / 管理员代申请
// （复用同一通道，仅 requested_operator 不同）。状态机：
//
//	pending ──approve──> approved ──claim──> paying ──paid──> paid（终态）
//	   │                    │                     └─confirm-failed─> approved（回退）
//	   └──────reject────────┴──────────────> rejected（终态；退还 available）
type SupplierSettlement struct{ ent.Schema }

func (SupplierSettlement) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("supplier_user_id"),
		field.Enum("kind").Values("supplier_request", "admin_request"),
		field.Int64("amount_millis"), // 本单金额（自持快照）
		field.Time("period_start"),   // 申请观察区间起点
		field.Time("period_end"),     // 申请观察区间终点（GREATEST(start, db now)）
		field.Enum("status").Values("pending", "approved", "paying", "paid", "rejected").Default("pending"),
		field.Int64("revision").Default(1), // CAS 令牌
		field.String("request_key"),        // 业务幂等键（§6.2）
		field.Time("requested_at"),         // 申请时刻
		field.Int64("requested_operator"),  // supplier_request = supplier uid；admin_request = 管理员 uid
		field.Time("reviewed_at").Optional().Nillable(),
		field.Int64("reviewer_user_id").Optional().Nillable(),
		field.Int64("payout_operator_user_id").Optional().Nillable(), // paying 认领人（执行打款者）
		field.Time("payout_started_at").Optional().Nillable(),
		field.String("payment_key").Optional().Nillable(),    // 首次 claim 服务端生成，全局唯一，永久固定
		field.String("external_ref").Optional().Nillable(),   // 银行/渠道回单号
		field.String("payee_snapshot").Optional().Nillable(), // 收款目标快照（副作用前固定，C2）
		field.String("payout_failure_reason").Optional().Nillable(),
		field.Time("payout_failed_at").Optional().Nillable(),
		field.String("risk_review").Optional().Nillable(), // 人工风险核对记录（严格 JSON，C1）
		field.Time("paid_at").Optional().Nillable(),
		field.Int64("paid_operator_user_id").Optional().Nillable(), // 付款确认人（未必 = 认领人）
		field.String("note").Optional().Nillable(),
		field.String("reject_reason").Optional().Nillable(),
	}
}

// Indexes：
//   - (requested_operator, request_key) UNIQUE —— 幂等键作用域（§6.2）；
//   - (payment_key) UNIQUE —— 付款键全局唯一（NULL 不冲突；§6.5）；
//   - (supplier_user_id, period_end) —— 期间链 MAX(period_end)（§6.2）；
//   - (status, requested_at) —— 工作台分页（M3）。
func (SupplierSettlement) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("requested_operator", "request_key").Unique(),
		index.Fields("payment_key").Unique(),
		index.Fields("supplier_user_id", "period_end"),
		index.Fields("status", "requested_at"),
	}
}

// Annotations DB CHECK（M2）：期间单调、金额为正。
func (SupplierSettlement) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Checks: map[string]string{
			"supplier_settlements_period_order":    "period_end >= period_start",
			"supplier_settlements_amount_positive": "amount_millis > 0",
		}},
	}
}
