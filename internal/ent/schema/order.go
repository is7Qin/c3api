package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Order 支付订单（spec 支付网关 v2 §3-3）：网关异步通知 → 单事务内四验 + 生成
// 一张 balance 兑换码并立即应用 → 订单置 paid。金额全整数（credit_milli 入账毫分、
// pay_minor 实付渠道最小单位、rate_micro 汇率 ×1e6），**禁 float**——下单时
// 快照账户/协议/渠道/金额四件套，之后改账户配置或汇率不影响既有订单。
//
// 幂等由数据库给（§2 判据 3）：out_trade_no UNIQUE + 行锁 + 条件迁移
// （paid 只从 pending 迁入）；redemption_code_id UNIQUE 是「同一订单只入账一次」
// 的第二道 DB 兜底。
type Order struct{ ent.Schema }

func (Order) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		// P{UTC yyyyMMddHHmmss}{10 位 base32}；单号冲突换号重试（≤3）。
		field.String("out_trade_no").Unique(),
		field.Int64("user_id"),
		// provider 协议快照（从账户取）：回调必须与**路径账户的** provider 一致
		// （v2 §6-2 第 9 步；跨协议回调 → fail + fail_reason=provider）。
		field.String("provider"),
		// account_key 上游账户快照（v2 §3-3 新增；v1 预留下的 `account` 已作废并
		// 删除）。**不建外键**：账户软停/改名后账本仍可读。回调第 8 步用它比对
		// 「路径账户 == 订单账户」（不符 → fail + fail_reason=account）。
		field.String("account_key"),
		field.Enum("status").Values("pending", "paid", "closed").Default("pending"),
		// credit_milli = usd_cents*1000（入账毫分；1 USD = 100,000 毫分）。
		field.Int64("credit_milli"),
		// pay_minor 实付渠道最小单位（分）= (credit_milli*rate_micro + 5e8)/1e9。
		field.Int64("pay_minor"),
		field.String("currency"),                                         // 2 位小数货币（service 白名单）
		field.Int64("rate_micro"),                                        // 汇率 ×1e6（D3：number setting 只收整数）
		field.String("pay_type"),                                         // 全局唯一渠道别名（决策 D4/D6）
		field.String("trade_no").Optional().Nillable(),                   // 网关流水
		field.Int64("redemption_code_id").Optional().Nillable().Unique(), // 自动到账凭据
		field.String("raw_notify").Optional().Nillable(),                 // 清洗后 ≤4 KiB JSON
		field.String("fail_reason").Optional().Nillable(),                // 见 domain.OrderFailReason*
		field.Int("fail_count").Default(0),                               // 尽力计数，不承担对账
		field.Time("last_notify_at").Optional().Nillable(),               // 最近拒绝时间
		field.Time("resolved_at").Optional().Nillable(),                  // 人工处置完成（防重复补账）
		field.Time("created_at").Default(time.Now),
		field.Time("paid_at").Optional().Nillable(),
		field.Time("expires_at"), // now + payment.order_ttl_minutes（过期只是派生视图）
	}
}

// Indexes 三条查询面：用户自己的订单列表（(user_id,id) 与既有 balance_logs
// 同形，倒序分页直接走索引）、管理面按状态的时间窗扫描、按上游对账
// （(account_key,id)——v2 §3-3 新增）。
func (Order) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "id"),
		index.Fields("status", "created_at"),
		index.Fields("account_key", "id"),
	}
}
