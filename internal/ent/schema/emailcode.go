// SPDX-License-Identifier: AGPL-3.0-or-later
package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// EmailCode 邮箱验证码（email + purpose 唯一，重复发送覆盖旧码天然失效）。
type EmailCode struct{ ent.Schema }

func (EmailCode) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("email"),
		field.Enum("purpose").Values("register", "reset"),
		field.String("code_sha256"),
		field.Time("expires_at"),
		field.Int("attempts").Default(0),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (EmailCode) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("email", "purpose").Unique(),
	}
}
