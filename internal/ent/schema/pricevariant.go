// SPDX-License-Identifier: AGPL-3.0-or-later
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PriceVariant 条件变体：模型+seq 唯一，条件全可空=通配 AND 组合，效果至少其一非空。
type PriceVariant struct{ ent.Schema }

func (PriceVariant) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("model"),
		field.Int("seq"),
		field.String("service_tier").Optional().Nillable(),
		field.Int64("ctx_min").Optional().Nillable(),
		field.Int64("ctx_max").Optional().Nillable(),
		field.String("time_start").Optional().Nillable(),
		field.String("time_end").Optional().Nillable(),
		field.Int("dow_mask").Optional().Nillable(),
		field.Int("mult_bp").Optional().Nillable(),
		field.Int64("set_input_per_m").Optional().Nillable(),
		field.Int64("set_output_per_m").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (PriceVariant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("model", "seq").Unique(),
	}
}
