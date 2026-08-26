// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// EmailTemplate 邮件模板（purpose 唯一：register_code/reset_code），缺行时走
// domain.DefaultEmailTemplate 回退（镜像 Setting 注册表无行=默认模式）。
type EmailTemplate struct{ ent.Schema }

func (EmailTemplate) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Enum("purpose").Values("register_code", "reset_code"),
		field.String("subject"),
		field.String("body_text"),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (EmailTemplate) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("purpose").Unique(),
	}
}
