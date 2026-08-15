package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Key 客户端 API key：独立表（重建 group 内嵌 key 语义，不向后兼容）。
// 多 key 选组、key 级轮换/禁用/并发上限/额度（quota/quota_used 后扣模型）。
type Key struct{ ent.Schema }

func (Key) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"),
		field.Int64("group_id"),
		field.String("name"),
		field.String("key_raw").Unique(), // 明文常驻（长期可查看/复制；DB 泄露即明文暴露——自托管权衡）
		field.Enum("status").Values("active", "disabled").Default("active"),
		field.Int("max_concurrency").Default(0), // 0 = 不限
		field.Int64("quota").Default(0),         // 累计 token 上限；0 = 不限（HasQuota 短路）
		field.Int64("quota_used").Default(0),    // 已消耗（后扣；无额度 key 恒 0）
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）；null 语义 = 未删除
		field.Time("created_at").Default(time.Now),
	}
}

func (Key) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("keys").
			Field("user_id").
			Unique().
			Required(),
		edge.From("group", Group.Type).
			Ref("keys").
			Field("group_id").
			Unique().
			Required(),
	}
}

// Indexes 管理面两路径的索引载体：ListKeysByUser（user_id EQ + deleted_at
// IS NULL，先 Count 全扫再取页）与 DeleteKeysByGroup（group_id EQ ± 软删
// 过滤）。写路径低频（CreateKey/UpdateKey/软删），AddQuotaUsed 按 id 批量
// 不触碰新索引列——零额外写放大。
func (Key) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "deleted_at"),
		index.Fields("group_id", "deleted_at"),
	}
}
