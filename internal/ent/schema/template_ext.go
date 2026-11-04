package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// TemplateExt 模板类型化扩展配置（1:1 边缘表；credential_type ∈
// {responses-special, codex-oauth, codex-pat} 的模板才有 ext 行——api_key 主列
// 类型无 ext 行）。模板是共享配置面：只承载类型声明 + strip_image_tools 公共
// 能力开关（三类型通用，NULL = 未配置 = 关闭）——凭据列组（oauth/pat）一律
// 只在账号级 account_ext（账号私有，见 account_ext.go）。扩展 = 加列。
type TemplateExt struct{ ent.Schema }

func (TemplateExt) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("template_id"),
		field.String("credential_type"),
		// 三类型公共能力开关：模板级图像 tool 剥离（NULL = 未配置 = 关闭）
		field.Bool("strip_image_tools").Optional().Nillable(),
		// custom 专用：逐格式自定义上游路径（NULL/空 = 全回落官方默认）。
		// 键集合与校验见 spec §4.3/§4.4；仅 credential_type=custom 可写。
		field.JSON("upstream_paths", map[string]string{}).Optional(),
	}
}

func (TemplateExt) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("template", Template.Type).
			Ref("ext").
			Field("template_id").
			Unique().
			Required(),
	}
}

func (TemplateExt) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("template_id").Unique(), // 1:1（upsert 冲突列）
	}
}
