package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// RedemptionCode 兑换码：资源发放的通用载体（Phase 5 计费前基础设施）。
// 任何"给用户发资源"的场景（充值/试用/扩容/赠品）统一走兑换码，后续功能不再各自实现发放逻辑。
// 三类型：balance（users.balance += value）、concurrency（users.max_concurrency，
// 0=不限语义特判）、temp_balance（插入 temp_balances 行，resource_expires_at 必填）。
type RedemptionCode struct{ ent.Schema }

func (RedemptionCode) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("code").Unique(), // XXXX-XXXX-XXXX-XXXX，16 字符，字符集 32（熵 ~80bit）
		field.Enum("type").Values("balance", "concurrency", "temp_balance"),
		field.Int64("value"), // 最小单位（分/并发数）
		field.String("remark").Optional().Nillable(),
		field.Time("expires_at").Optional().Nillable(),          // 码未兑换即过期；nil = 永久
		field.Time("resource_expires_at").Optional().Nillable(), // 兑换后资源到期；temp_balance 必填（service 校验）
		field.Int("max_uses").Default(1),                        // 1 = 单次码；>1 = 多人码
		field.Int("used_count").Default(0),
		field.Enum("status").Values("active", "disabled").Default("active"),
		field.Int64("created_by").Default(0), // 0 = 系统（静态 admin token）；>0 = platform_admin user_id
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (RedemptionCode) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("uses", RedemptionUse.Type),
	}
}
