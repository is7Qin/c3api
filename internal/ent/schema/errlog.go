package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ErrLog 错误明细审计表（用户裁决分表设计：err_logs 独立于 usage_logs，不混表）：
// 拒绝/异常路径的错误审计字段（status_code/error_message 等 usage_logs 瘦身去掉
// 的排障列）由本表承载——瘦表，无 token/价格列。写入来源：recordRejected（本地
// 预用量拒绝 + 401 鉴权失败）+ record/finish 的错误行（abort/failover/4xx/5xx/
// network 半异常双轨，request_id 关联 usage_logs 同请求）。
// 分区表 DDL 由 bootstrap 独占管理（migrate 钩子排除，与 usage_logs 同路线），
// 保留期独立（usage.err_log_retention_days，默认 7 天短保留）。
type ErrLog struct{ ent.Schema }

func (ErrLog) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("request_id"),
		// 用户裁决（2026-08-17，S-E）：client_ip 审计列——供应商头识别 + RemoteAddr
		// 兜底（proxy.behind_cdn 开关门控；与 usage_logs 同列同步加）；拒绝行
		// （401 鉴权失败等）也带（guardPipeline 入口鉴权前提取）。NULL = Optional
		// 未 Set。
		field.String("client_ip").Optional().Nillable(),
		field.Int64("group_id").Optional().Nillable(),
		field.Int64("account_id").Optional().Nillable(),
		field.Int64("template_id").Optional().Nillable(),
		// 归属字段（context 传递，0 = 无）：401 鉴权失败行无 KeyMeta → 全部
		// 0/NULL（无效 key 本质不可归因，可接受——架构审查 A1；仅 request_id +
		// format + status 可关联）。
		field.Int64("user_id").Optional().Nillable(),
		field.Int64("key_id").Optional().Nillable(),
		field.String("model").Default(""),
		field.Enum("format").
			Values("openai-chat", "openai-responses", "openai-responses-ws", "openai-images", "anthropic"),
		field.Int("status_code").Default(0),
		field.String("error_type").Default("none"),
		// 错误文本：连接级 err.Error() / 4xx+ 上游 body / 拒绝文案，域内截断
		// 500 字符（domain.TruncateErrMsg）；NULL = 无错误文本。
		field.String("error_message").Optional().Nillable(),
		field.Int64("latency_ms").Default(0),
		// 计费档位审计（评审 I-3）：service_tier 归一化值（priority/flex/fast/auto）
		// ——tier reject 的 tier 维度审计保留；NULL = 未计费路径。
		field.String("billing_tier").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
	}
}

func (ErrLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		// 查询面索引（架构审查 S1）：/err_logs 按用户/组过滤（/user/err_logs
		// 强制 user_id）——风暴表无 (user_id/group_id, created_at) 索引将逐
		// 分区 seq scan。
		index.Fields("group_id", "created_at"),
		index.Fields("user_id", "created_at"),
	}
}
