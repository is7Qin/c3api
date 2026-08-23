package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// UsageStat 平台小时卷积桶（usage_stats 行；cube OLAP）。
// v2 列定义：三键 (bucket_time, group_id, model) 唯一；删 account_id/template_id/user_id/is_error（见 spec §1.1）。
// ttft_hist bigint[] carve-out 不进 ent（ent 无 PG 数组类型；field.Ints 是 JSON 语义）——唯一事实源 partition.go usageStatsColumnDefs。
type UsageStat struct{ ent.Schema }

func (UsageStat) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.Time("bucket_time"),
		field.Int64("group_id").Default(0), // 0 = 无（唯一索引需要非 NULL）
		field.String("model").Default(""),
		field.Int64("request_count").Default(0),
		field.Int64("error_count").Default(0),
		field.Int64("input_tokens").Default(0),
		field.Int64("output_tokens").Default(0),
		field.Int64("total_tokens").Default(0),
		field.Int64("cache_read_tokens").Default(0),
		field.Int64("cache_creation_tokens").Default(0),
		field.Int64("cost").Default(0), // 毫分（1 USD = 100,000 毫分）；计费预聚合
		field.Int64("raw_cost").Default(0),
		// 按次调用：图片生成 = 张数、search = 1
		field.Int64("call_count").Default(0),
		// TTFT 三标量列：avg 查询侧 Go 除（ttft_total/ttft_count）
		field.Int64("ttft_total_ms").Default(0),
		field.Int64("ttft_count").Default(0),
		field.Int64("ttft_max_ms").Default(0),
		// 注：ttft_hist bigint[10] 数组列 carve-out 不进 ent schema（ent 无 PG
		// 数组类型；field.Ints 等是 JSON 语义）——列定义唯一事实源在
		// repository/partition.go usageStatsColumnDefs，ScanStats 改 pgx 直查。
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (UsageStat) Indexes() []ent.Index {
	return []ent.Index{
		// v2 三键唯一（列序 = ON CONFLICT 目标列序，batched merge 依赖；分区表侧 DDL 见 repository/partition.go usageStatsIndexDDLs）。
		index.Fields("bucket_time", "group_id", "model").Unique(),
	}
}
