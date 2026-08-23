package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Template struct{ ent.Schema }

func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name").Unique(),
		field.String("base_url"),
		// credential_type 模板级：一个模板 = 一种号池（默认 api_key；号池生态类型后续追加）。
		field.String("credential_type").Default("api_key"),
		field.JSON("supported_formats", []string{}), // 格式数组
		field.JSON("models", []string{}),
		field.JSON("format_models", map[string][]string{}), // format -> model 列表
		field.JSON("model_mapping", map[string]string{}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）；null 语义 = 未删除
		field.Time("created_at").Default(time.Now),
	}
}

func (Template) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("accounts", Account.Type),
		// ext 模板类型化扩展配置（1:1；api_key 类型无 ext 行）
		edge.To("ext", TemplateExt.Type),
	}
}
