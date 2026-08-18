package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type UsageLog struct{ ent.Schema }

func (UsageLog) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("request_id"),
		// 用户裁决（2026-08-17，S-E）：client_ip 审计列——供应商头识别
		// （CF-Connecting-IP / True-Client-IP / X-Real-IP 按序）+ RemoteAddr 兜底，
		// proxy.behind_cdn 开关门控；审计/排障的尽力而为标识，非安全边界。
		// NULL = Optional 未 Set（空 = 无）。
		field.String("client_ip").Optional().Nillable(),
		field.Int64("group_id").Optional().Nillable(),
		field.Int64("account_id").Optional().Nillable(),
		field.Int64("template_id").Optional().Nillable(),
		field.Int64("user_id").Optional().Nillable(), // 鉴权 key 归属用户（context 传递，0 = 无）
		field.Int64("key_id").Optional().Nillable(),  // 鉴权 key（context 传递，0 = 无）
		field.String("model").Default(""),
		field.String("mapped_model").Optional().Nillable(),
		field.Enum("format").
			// 图片生成（spec §4.3）：openai-images——/v1/images/generations|edits
			// 落库 format；分区表 format 列为 varchar（无 DB enum），本枚举为
			// 客户端面校验（ent FormatValidator / COPY 逐行 FormatValidator）。
			// openai-search（统一计费模型 spec 2026-08-13）：codex search 端点
			// 落库 format——本 task 只扩枚举（search 端点接入为独立 task，消费
			// 本枚举 + call_count 落账）。
			Values("openai-chat", "openai-responses", "openai-responses-ws", "anthropic", "openai-images", "openai-search"),
		// 用户裁决（err_logs 分表设计 + 升级原则）：usage_logs 瘦身去 2 留 1——
		// status_code（成功行恒 200 无信息量）与 error_message（纯排障文本列）
		// 移入 err_logs 独立审计表；error_type 保留（半异常计费行标记；分表后
		// usage_logs = 纯计费明细（仅 cost>0），值域收敛为 none/abort 两值——
		// 4xx/5xx/network 等 cost=0 错误行不写 usage_logs（不计费不入明细）。
		// 错误审计字段（status_code/error_message 等）由 err_logs 承载（见
		// errlog.go）。
		field.String("error_type").Default("none"),
		field.Int64("latency_ms").Default(0),
		// TTFT（首 token 时间毫秒）：caller 流式首 chunk 采集（ctx 传递）；
		// 非流式/失败/无首 token 路径 = NULL。
		field.Int64("ttft_ms").Optional().Nillable(),
		field.Int64("input_tokens").Default(0),
		// 价格快照（每 M token 毫分，1 USD = 100,000 毫分；pricing 同款单位）：
		// 请求时点生效基础单价，applyBilling 填充（GetPrice 已取价，零额外查找）；
		// 各价格列紧邻其 tokens 列（单价 × tokens = cost 可读性）。null =
		// 未计费路径（no_price 防御）；缓存价 null = 该请求无缓存读或无缓存价。
		field.Int64("price_input_millis").Optional().Nillable(),
		field.Int64("output_tokens").Default(0),
		field.Int64("price_output_millis").Optional().Nillable(),
		field.Int64("total_tokens").Default(0),
		field.Int64("cache_read_tokens").Default(0),
		field.Int64("price_cache_read_millis").Optional().Nillable(),
		field.Int64("cache_creation_tokens").Default(0),
		field.Int64("price_cache_creation_millis").Optional().Nillable(),
		// 统一计费模型功能调用分量（spec 2026-08-13；search 端点 task 消费）：
		// call_count 功能调用计数——图片生成 = 张数（data 长/completed 事件数）、
		// search = 1；**不入 TotalTokens**（功能调用非 token——对齐原 image_count
		// 语义）；统计 sum(call_count) = 功能调用量。price_per_call_millis 按单元
		// 价快照（**毫分/单元**——search 每次 / 图片每张；例外于本表其余
		// price_*_millis 列的"毫分/1M tokens"口径——per-call 计费不走 /1e6 除法，
		// spec §4.2 例外说明）；nil = 无按单元分量。原图片 6 专列（image
		// input/output_tokens、image_count + 3 价格快照）已删除——image token
		// 并入 input/output_tokens（TotalTokens 口径不变），per-image 价迁移为
		// price_per_call_millis。
		field.Int64("call_count").Default(0),
		field.Int64("price_per_call_millis").Optional().Nillable(),
		// 计费列（Phase 5）：cost 毫分（1 USD = 100,000 毫分）；billing_tier
		// 请求 service_tier 归一化值（priority/flex/fast/auto；nil = 未计费路径）；
		// above_hit 任一分量超 above_threshold 命中分段；overdraft 本次扣费透支
		// （负余额）。错误请求（402/4xx）cost = 0。
		// raw_cost（spec 2026-08-18）：乘倍率前的原始成本（毫分）——免费组
		// cost=0 但 raw 有值（"实际消耗"可见）；历史行/缺省 = 0（fresh setup
		// 不迁移）。恒落（对齐 cost 恒落语义）。
		field.Int64("cost").Default(0),
		field.Int64("raw_cost").Default(0),
		field.String("billing_tier").Optional().Nillable(),
		field.Bool("above_hit").Default(false),
		field.Bool("overdraft").Default(false),
		field.Time("created_at").Default(time.Now),
	}
}

func (UsageLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		index.Fields("group_id", "created_at"),
		index.Fields("account_id", "created_at"),
		index.Fields("user_id", "created_at"),
		index.Fields("key_id", "created_at"),
		// 幂等键（方向 A 批次 1a，A-P2-3；双轨声明——分区表 DDL 见
		// repository/partition.go usageLogIndexDDLs，migrate 经钩子跳过本表）：
		// request_id 等值 + 分区键 created_at 收尾（分区表唯一索引必须含分区键）。
		// COMMIT 歧义窗口重试撞 23505 → flusher 按成功处理（防双扣，见
		// internal/billing/flusher.go isUniqueLogConflict）。
		index.Fields("request_id", "created_at").Unique(),
	}
}
