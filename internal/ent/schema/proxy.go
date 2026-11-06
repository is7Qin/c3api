package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Proxy 出口代理条目（spec-proxy-pool §3.1）。一张扁平表：连接规格 + 探测结果
// 快照。**不建 edge、不建外键**：绑定关系是 account.proxy_id 的单向裸列，
// 删除全是软删（deleted_at），DB 级联永不触发；解绑由 service 在同事务内显式
// 完成（§6.1），加载期再把「已软删的代理」视为不可用（§4.1）。
type Proxy struct{ ent.Schema }

func (Proxy) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		// name 的唯一域是**每归属**，不是全局（spec-proxy-pool §2.5「名字唯一域」）：
		// 唯一的载体见下方 Indexes 的两个**部分唯一索引**，此处不再 Unique()。
		// 理由（两条，都是多租户语境下的硬伤）：
		//   1. 全局唯一 ⇒ 供应商 A 建了 `office`，供应商 B 就建不了 `office`——B 得到一个
		//      与自己无关的失败（跨租户命名冲突）；
		//   2. 更要紧的是**探测面**：B 只要试一次就能确认「A 有一条叫 office 的代理」，
		//      与「越域一律 404、不泄露存在性」的纪律直接矛盾。
		field.String("name"),
		// owner_uid 归属（spec-proxy-pool §3.6，D7 = C 模式）：NULL = 平台代理（只由
		// 管理员维护），非 NULL = 该供应商自建（私池）。它决定**可见性与可绑定范围**
		// ——平台池不是公共池：供应商既看不到也绑不上。**不建 edge/FK**（与整张表的
		// 扁平纪律一致）；也**不进任何键**：调度只看 enabled + 绑定是否可用，
		// proxyDigest 只是连接规格摘要（§3.6 末条）。
		field.Int64("owner_uid").Optional().Nillable(),
		// scheme ∈ {socks5, socks5h, http, https}：白名单校验在 service 层
		// （domain.ProxyScheme.Valid），列只落小写原值——socks5h 不做归一改写。
		field.String("scheme"),
		// host 不带 IPv6 方括号；port ∈ 1..65535（校验同 service 层）。
		field.String("host"),
		field.Int("port"),
		// username/password 可选：空 = 无认证。**明文列**（§3.3：整个凭据面已是
		// 明文，DB 是信任边界；读面只回显 has_password，密码恒不出）。
		field.String("username").Optional().Nillable(),
		field.String("password").Optional().Nillable(),
		// enabled 是**唯一**可用性开关（§6.3：探测结果不影响 Routable）。
		field.Bool("enabled").Default(true),
		field.String("remark").Optional().Nillable(),
		// 探测结果快照（last_probe_*）：只用于回显/排障，不参与路由判定。
		field.Time("last_probe_at").Optional().Nillable(),
		field.Bool("last_probe_ok").Default(false),
		field.String("last_probe_error").Default(""), // 错误文本已脱敏（§5.2）
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）；null 语义 = 未删除
		field.Time("created_at").Default(time.Now),
	}
}

// Indexes name 的唯一域 = **每归属**（spec-proxy-pool §2.5）：
//   - 平台池：`(name) WHERE owner_uid IS NULL` —— 平台代理之间仍互不同名（管理员面
//     的既有语义不变）；
//   - 私池：`(owner_uid, name) WHERE owner_uid IS NOT NULL` —— 同一供应商内唯一，
//     不同供应商可以同名（互不干扰，也不构成「猜别人有没有这条代理」的探测面）。
//
// 两个都是**部分唯一索引**：`(owner_uid, name)` 整体唯一在本列可空时并不成立
// （PG 里 NULL 互不相等，平台池会退化成「随便重名」），故必须按 owner_uid 是否为空
// 分成两条带谓词的索引来表达。
//
// **软删语义保持不变**：索引谓词里没有 deleted_at ⇒ 删条目不释放名字（既有行为；
// 仓储把唯一冲突映射成 ErrConflict → 409）。
func (Proxy) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("name").
			Unique().
			Annotations(entsql.IndexWhere("owner_uid IS NULL")),
		index.Fields("owner_uid", "name").
			Unique().
			Annotations(entsql.IndexWhere("owner_uid IS NOT NULL")),
	}
}
