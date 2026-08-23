package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// UsageEntityStat 实体小时卷积桶（usage_entity_stats 行；entity OLAP）。
// entity_type ∈ {'account','user','key'}；entity_id 0 不存在（IS NOT NULL 丢弃语义，见 spec §3.2）。
// 全字段进 ent，无 carve-out（无数组列）；DDL 事实源 partition.go usageEntityStatsColumnDefs。
type UsageEntityStat struct{ ent.Schema }

func (UsageEntityStat) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Time("bucket_time"),
		field.String("entity_type"), // 'account' | 'user' | 'key'
		field.Int64("entity_id"),
		field.String("model").Default(""),
		field.Int64("request_count").Default(0),
		field.Int64("error_count").Default(0),
		field.Int64("call_count").Default(0),
		field.Int64("input_tokens").Default(0),
		field.Int64("output_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Int64("cache_read_tokens").Default(0),
		field.Int64("cache_creation_tokens").Default(0),
		field.Int64("cost").Default(0),     // 毫分（1 USD = 100,000 毫分）
		field.Int64("raw_cost").Default(0), // 毫分（乘倍率前原始成本）
		field.Int64("ttft_total_ms").Default(0),
		field.Int64("ttft_count").Default(0),
		field.Int64("ttft_max_ms").Default(0),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (UsageEntityStat) Indexes() []ent.Index {
	return []ent.Index{
		// 唯一：ON CONFLICT 目标列序（batched merge 依赖；分区表侧 DDL 见 repository/partition.go）。
		index.Fields("bucket_time", "entity_type", "entity_id", "model").Unique(),
		// 实体钻取扫描：entity_type EQ + entity_id EQ + bucket_time 范围。
		index.Fields("entity_type", "entity_id", "bucket_time"),
	}
}
