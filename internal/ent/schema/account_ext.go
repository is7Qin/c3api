package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/is7qin/c3api/internal/domain"
)

// AccountExt 账号类型化鉴权扩展（1:1 边缘表；credential_type ∈
// {codex-oauth, codex-pat}——账号只两种 codex 类型）。列组按类型约束由
// service 校验：oauth 只允许 codex_oauth_* 列组；pat 只允许 codex_pat_key。
// 身份四元组（对齐真实 codex 客户端语义，导入时 service NewCodexIdentity()
// 自动生成并持久化、账号存在期间稳定）合并为 codex_identity jsonb 单列：
// installation_id 必存（UUIDv4 安装级永久）；session_id/thread_id UUIDv7
// 会话级（主线程 thread_id==session_id）；window_id = {thread_id}:0（导入时
// 生成后恒定不变——零递增零状态）。除 id/account_id/credential_type 外全
// nullable（用户裁决——未来其他账号类型复用表加自己的列组，零约束冲突）。
type AccountExt struct{ ent.Schema }

func (AccountExt) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("account_id"),
		field.String("credential_type"),
		// —— 身份四元组（jsonb 单列；导入时 service NewCodexIdentity() 自动
		// 生成、账号存在期间稳定；nil = 未配置/旧数据异常）——
		field.JSON("codex_identity", &domain.CodexIdentity{}).Optional(),
		// —— 凭据列组（按 credential_type 约束：oauth 只允许 codex_oauth_*；pat 只允许 codex_pat_key）——
		field.String("codex_oauth_token").Optional().Nillable(),
		field.String("codex_oauth_refresh_token").Optional().Nillable(),
		field.Time("codex_oauth_expires_at").Optional().Nillable(),
		// pat 组（Codex PAT 账号池）
		field.String("codex_pat_key").Optional().Nillable(),
		// —— 管理标识 ——
		// codex 账号登录邮箱（管理面标识；导入时由人工/上游提供，非自动生成——
		// NewCodexIdentity 不生成 codex_email，只生成身份四元组；可空）
		field.String("codex_email").Optional().Nillable(),
		// 上游账号/空间标识（Task B 批量导入必填；本 task 仅建列 + 组合唯一——
		// NULL 不参与唯一）
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
		// 幂等组合键（Task B 批量导入按 (codex_email, codex_account_id) 定位）：
		// NULL 不参与唯一（PG 语义）——两行同 email 但 codex_account_id 全 NULL
		// 可共存（存量管理面写入形态）；导入必填由 Task B service 校验保证。
		index.Fields("codex_email", "codex_account_id").Unique(),
	}
}
