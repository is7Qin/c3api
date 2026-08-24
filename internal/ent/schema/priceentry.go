// SPDX-License-Identifier: AGPL-3.0-or-later
package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// PriceEntry 统一价格条目：每模型一行，mode 声明主计费方式但价格分量跨模式可选配
// （token 行可携带 image/call 分量治视觉旁路归零盲区，见 spec D-P1）。
type PriceEntry struct{ ent.Schema }

func (PriceEntry) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("model").Unique(),
		field.Enum("mode").Values("token", "call", "image"),
		field.Int64("input_per_m").Optional().Nillable(),
		field.Int64("output_per_m").Optional().Nillable(),
		field.Int64("cache_read_per_m").Optional().Nillable(),
		field.Int64("cache_write_per_m").Optional().Nillable(),
		field.Int64("price_per_call").Optional().Nillable(),
		field.Int64("img_in_tok_per_m").Optional().Nillable(),
		field.Int64("img_out_tok_per_m").Optional().Nillable(),
		field.Int64("price_per_image").Optional().Nillable(),
		field.String("provider").Optional().Nillable(),
		field.Int64("max_input_tokens").Optional().Nillable(),
		field.Int64("max_output_tokens").Optional().Nillable(),
		field.Bool("supports_prompt_caching").Optional().Nillable(),
		field.JSON("raw", json.RawMessage{}).Optional(),
		field.Enum("source").Values("litellm", "manual").Default("manual"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}
