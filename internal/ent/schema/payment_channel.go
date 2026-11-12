package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PaymentChannel 支付渠道（spec 支付网关 v2 §3-2）：一行 = 该账户下可售卖的一种
// 支付方式，带自己的币种、汇率与单笔限额。
//
// 与 v1 的「全局渠道」不同：同一 pay_type 可出现在多个账户（这正是多上游），
// 由选路按 `priority ASC, id ASC` 决定这一次走谁。渠道是**按 pay_type 寻址的子资源**
// （upsert/delete 单条），不做整体替换——否则改个展示名会删光渠道。
//
// `pay_type ∈ capabilities(provider)` 的校验在 service 层（repo 不引 provider），
// 本表只保证 `(account_id, pay_type)` 唯一。
type PaymentChannel struct{ ent.Schema }

func (PaymentChannel) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Int64("account_id"),
		// 协议声明的 pay_type（epay = alipay/wxpay）；写入前由 service 对照
		// provider.Capabilities() 校验。
		field.String("pay_type"),
		field.Bool("enabled").Default(true),
		// ISO-4217 两位小数白名单币种（domain.PaymentCurrencySupported）。
		field.String("currency"),
		// 1 USD = N 货币 × 1e6（整数；禁 float；> 0 才可售）。
		field.Int64("rate_micro"),
		// 单笔限额（整数 USD）：min ≥ 1，max ≥ min；下单时再按汇率收敛生效上限。
		field.Int64("min_usd").Default(1),
		field.Int64("max_usd"),
		field.Time("created_at").Default(time.Now),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (PaymentChannel) Edges() []ent.Edge {
	return []ent.Edge{
		// 反向边：FK 列 account_id + ON DELETE CASCADE 由 PaymentAccount 侧的
		// edge.To 注解声明（见 payment_account.go）。
		edge.From("account", PaymentAccount.Type).
			Ref("channels").
			Field("account_id").
			Unique().
			Required(),
	}
}

// Indexes (account_id, pay_type) UNIQUE：同一账户下同一支付方式只能有一行
// （upsert 的冲突键）；跨账户同名 pay_type 允许——多上游的全部意义。
func (PaymentChannel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id", "pay_type").Unique(),
	}
}
