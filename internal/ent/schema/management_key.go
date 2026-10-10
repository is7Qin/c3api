package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ManagementKey 管理 API key（前缀 mk-，spec 2026-10-09）：管理面（/api/user、
// /api/user/supplier、/api/admin）鉴权，等价于「以 owner 身份登录」——可达 API =
// owner 角色闭包。明文常驻 key_raw（自托管权衡，与客户端 key 一致，列表页可见）。
// status 可写：disabled = 软禁用（快照即时 401，可再启用）；删除 = 软删（不可逆）。
type ManagementKey struct{ ent.Schema }

func (ManagementKey) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("user_id"), // owner
		field.String("name"),
		field.String("key_raw").Unique(), // 明文，前缀 mk-
		field.Enum("status").Values("active", "disabled").Default("active"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）
	}
}

func (ManagementKey) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("management_keys").
			Field("user_id").
			Unique().
			Required(),
	}
}

// Indexes 管理面列表路径的索引载体：ListManagementKeysByUser（user_id EQ +
// deleted_at IS NULL）。写路径低频（create/update/软删）。
func (ManagementKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "deleted_at"),
	}
}
