package schema

import (
	"encoding/json"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PaymentAccount 支付账户（spec 支付网关 v2 §3-1）：一行 = 一家上游的一个商户号。
//
// 「账户是数据」：新增一家上游 = 插一行，**零代码改动**——协议只回答怎么签/怎么验/
// 怎么查，凭据形状与哪些字段是秘密由协议自述（`ParseConfig`/`SecretFields`）。
//
// 三条只读契约（DB 层只保住唯一性，语义校验在 service）：
//   - `key` 是网关后台填过的回调 URL 段 + 订单快照，**永不复用**、不可改
//     （改 = 新建账户 + 停用旧账户）。
//   - `provider` 创建后只读：改协议会让该账户所有 pending 单的回调按新协议验签而
//     永久 fail。
//   - `merchant_ref` 创建后只读：同一协议下同一商户只能有一个账户，否则网关只回填
//     一个 notify URL → 回调打到非订单账户 → 静默资损。它独立成列而不是留在
//     `config` jsonb 里，就是为了**能建唯一索引**（jsonb 内字段索引不了）。
//
// 账户永不物理删除（N30）：DELETE ≡ enabled=false。但渠道行是它的子资源，
// `ON DELETE CASCADE` 仍按 §3-2 挂在边上一一DBA 手工清理账户行时不留孤儿渠道。
type PaymentAccount struct{ ent.Schema }

func (PaymentAccount) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		// `^[a-z][a-z0-9_]{2,31}$`（service 校验；URL 段直接取用）。
		field.String("key").Unique(),
		// 协议标识（= 注册表键，如 "epay_v2"）；未注册的 provider 在管理面红标、
		// 选路跳过（不是删除账户的理由）。
		field.String("provider"),
		// 协议自述的商户唯一标识（epay = pid），由 ParseConfig 推导。
		field.String("merchant_ref"),
		field.String("display_name"),
		// 可售开关：**不影响回调处理**（钱可能已收，停用账户照常入账）。
		field.Bool("enabled").Default(true),
		// 升序选路（0 = 主）；同 priority 按 id 升序（P38）。
		field.Int("priority").Default(0),
		// 协议专属凭据：写入由 ParseConfig 校验，读出由 SecretFields 掩码。
		// jsonb 而非 text：协议演进时旧行解析失败 → 该账户被跳过 + 列表
		// config_error + 指标（不静默），无需列迁移。
		field.JSON("config", json.RawMessage{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		// 每次 config 变更 +1；与 account_key 一起构成 service 配置缓存的键。
		// 创建即 1（0 保留给"未初始化"，避免与零值混淆）。
		field.Int64("config_revision").Default(1),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (PaymentAccount) Edges() []ent.Edge {
	return []ent.Edge{
		// 一账户多渠道。`ON DELETE CASCADE`（§3-2）：渠道是账户的从属子资源，
		// 账户行被 DBA 手工清理时渠道行随之消失，不留孤儿（正常路径只有软停，
		// 见上方类型注释）。
		edge.To("channels", PaymentChannel.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}

// Indexes (provider, merchant_ref) UNIQUE：同一协议下同一商户只能录一次
// （N5/N28——重复录入会让网关只回填一个 notify URL，回调打到非订单账户）。
// key 的唯一索引由 field.String("key").Unique() 给出。
func (PaymentAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("provider", "merchant_ref").Unique(),
	}
}
