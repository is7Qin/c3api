package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// AccountExt 账号类型化鉴权扩展（1:1 边缘表；credential_type ∈
// {codex-oauth, codex-pat}——账号只两种 codex 类型）。列组按类型约束由
// service 校验：oauth 只允许 oauth_* 列组；pat 只允许 pat_key。
// 身份四元组（对齐真实 codex 客户端语义，导入时 service NewCodexIdentity()
// 自动生成并持久化、账号存在期间稳定）：installation_id 必存（UUIDv4 安装级
// 永久）；session_id/thread_id UUIDv7 会话级（主线程 thread_id==session_id）；
// window_id = {thread_id}:0（导入时生成后恒定不变——零递增零状态）。扩展 = 加列。
type AccountExt struct{ ent.Schema }

func (AccountExt) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("account_id"),
		field.String("credential_type"),
		field.String("codex_account_id").Optional().Nillable(),
		// —— 身份四元组连续块（对齐真实 codex 客户端语义；导入时 service
		// NewCodexIdentity() 自动生成、账号存在期间稳定）——
		// 账号级唯一身份（真实客户端 ~/.codex/installation_id UUIDv4 永久复用；
		// 伪装下每账号恒定一个值，service 校验非空）
		field.String("installation_id"),
		// 会话级身份（UUIDv7；主线程 thread_id==session_id；导入时生成、持久复用）
		field.String("session_id").Optional().Nillable(),
		field.String("thread_id").Optional().Nillable(),
		field.String("window_id").Optional().Nillable(), // {thread_id}:{n}（起始 :0）
		// —— 凭据列组（按 credential_type 约束：oauth 只允许 oauth_*；pat 只允许 pat_key）——
		field.String("oauth_token").Optional().Nillable(),
		field.String("oauth_refresh_token").Optional().Nillable(),
		field.Time("oauth_expires_at").Optional().Nillable(),
		// pat 组（Codex PAT 账号池）
		field.String("pat_key").Optional().Nillable(),
		// —— 管理标识 ——
		// codex 账号登录邮箱（管理面标识；导入时由人工/上游提供，非自动生成——
		// NewCodexIdentity 不生成 email，只生成身份四元组；可空）
		field.String("email").Optional().Nillable(),
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
		index.Fields("account_id").Unique(),       // 1:1（upsert 冲突列）
		index.Fields("codex_account_id").Unique(), // 外部 account_id 去重；NULL 可多行
	}
}
