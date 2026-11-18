package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/is7qin/c3api/internal/domain"
)

// AccountExt 账号类型化鉴权扩展（1:1 边缘表）。credential_type 及列组约束
// 由 service/repository 校验；codex_identity 是可扩展 JSONB envelope：Codex
// installation_id、BPS profile subtree 和未知字段共存，任何 writer 必须 merge 保留
// 非本 writer 所有字段。
type AccountExt struct{ ent.Schema }

func (AccountExt) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("account_id"),
		field.String("credential_type"),
		// Provider identity envelope, preserving fields not owned by this writer.
		field.JSON("codex_identity", &domain.IdentityEnvelope{}).Optional(),
		// —— 凭据列组（按 credential_type 约束：oauth 只允许 codex_oauth_*；pat 只允许 codex_pat_key）——
		field.String("codex_oauth_token").Optional().Nillable(),
		field.String("codex_oauth_refresh_token").Optional().Nillable(),
		field.Time("codex_oauth_expires_at").Optional().Nillable(),
		// pat 组（Codex PAT 账号池）
		field.String("codex_pat_key").Optional().Nillable(),
		// —— 管理标识 ——
		// codex 账号登录邮箱（管理面标识；导入时由人工/上游提供，非自动生成——
		// NewCodexIdentity 不生成 codex_email，只生成 installation_id；可空）
		field.String("codex_email").Optional().Nillable(),
		// 上游账号/空间标识（可留空——导入/保存时自动识别：OAuth claims / PAT
		// whoami；组合唯一见 Indexes——NULL 不参与唯一）
		field.String("codex_account_id").Optional().Nillable(),
	}
}

func (AccountExt) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("account", Account.Type).
			Ref("ext").
			Field("account_id").
			Unique().
			Required(),
	}
}

func (AccountExt) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id").Unique(), // 1:1（upsert 冲突列）
		// 幂等组合键（批量导入按 (codex_email, codex_account_id) 定位）：
		// NULL 不参与唯一（PG 语义）——两行同 email 但 codex_account_id 全 NULL
		// 可共存；非空由 service 校验保证（缺省先自动识别，仍空才拒绝导入行）。
		index.Fields("codex_email", "codex_account_id").Unique(),
	}
}
