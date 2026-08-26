package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// User 用户即顶层（无租户实体）。标识 = 邮箱（无 username 字段，与 sub2api
// 数据可迁移）；密码哈希 = bcrypt DefaultCost(10)，sub2api 同参数存量 hash
// 直接可验证。balance（最小单位）本轮只建模型（管理面可读写），扣费逻辑
// Phase 5 计费。
type User struct{ ent.Schema }

func (User) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("email").Unique(),
		field.String("password_hash"),
		field.Enum("role").Values("platform_admin", "user").Default("user"),
		field.Enum("status").Values("active", "disabled").Default("active"),
		field.Int("max_concurrency").Default(0), // 0 = 不限
		field.Int64("balance").Default(0),       // 最小单位；Phase 5 扣费
		// token_version JWT 撤销版本（spec 2026-08-25-jwt-password-revocation）：
		// 改密/重置密码单语句原子递增；Claims.Ver 签发时快照，RequireJWT/adminAuth
		// 与内存快照比对，不匹配 → 401（撤销该用户全部既有 JWT）。存量行默认 0
		// ⇒ 升级平滑（ver 缺失解码 0 = 旧票仍有效，首次改密才全灭）。列 ADD 由
		// 启动迁移自动应用（fresh-setup 哲学，无迁移路径）。
		field.Int64("token_version").Default(0),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("keys", Key.Type),
		edge.To("temp_balances", TempBalance.Type),
		edge.To("group_assignments", GroupAssignment.Type),
	}
}
