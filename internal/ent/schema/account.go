package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Account struct{ ent.Schema }

func (Account) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name"),
		field.Int64("template_id"),
		// base_url 账号级覆盖（路由属性，用户裁决 2026-08-14 置于静态凭据面
		// upstream_key 之前）：nil = 继承模板 base_url；非空 = 覆盖模板值。
		// 无迁移逻辑——ent auto-migrate 自动加列（accounts 非分区表），无默认值/
		// 回填：旧行 NULL = 继承模板（旧行为天然成立）。
		field.String("base_url").Optional().Nillable(),
		field.String("upstream_key"),
		field.Int("max_concurrency").Default(8),
		field.String("last_error").Optional().Nillable(),
		field.Time("last_used_at").Optional().Nillable(),
		// failed_at SDK 上报的运行时失效时刻（SDK 接入 T1——用户裁决 2026-08-13：
		// 仅此一列；失效原因复用既有 last_error，两原因字段并存会漂移）。nil =
		// 未失效；与 enabled=false（管理面手动禁用）语义分离，两者可并存。
		field.Time("failed_at").Optional().Nillable(),
		field.String("failure_source").Optional().Nillable(),
		field.Bool("enabled").Default(true),
		field.Int64("lifecycle_revision").Default(1),
		field.Int("upstream_cost_multiplier_bp").Default(10000),
		field.String("cache_domain").Optional().Nillable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）；null 语义 = 未删除
		field.Time("created_at").Default(time.Now),
	}
}

func (Account) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("template", Template.Type).
			Ref("accounts").
			Field("template_id").
			Unique().
			Required(),
		edge.To("groups", Group.Type),
		// ext 账号类型化鉴权扩展（1:1；api_key 类型无 ext 行）
		edge.To("ext", AccountExt.Type),
	}
}
