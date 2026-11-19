package repository

import (
	"strings"

	"entgo.io/ent/dialect/sql/schema"
)

// MigrateHookExcludesPartitioned 导出 migrateHookExcludesPartitioned，仅供
// 0001 baseline 生成器（internal/db/migrate/gen）使用：分区父表由
// partitionedCreateDDL 建表，ent 不得 diff 它们（见 migrateHookExcludesPartitioned
// 注释）。生产启动路径不得调用本函数——M0b 之后启动不再跑 ent 自动迁移。
func MigrateHookExcludesPartitioned() schema.MigrateOption {
	return migrateHookExcludesPartitioned()
}

// BaselineTable 是 baseline 迁移里的一张「不由 ent 生成」的表。
type BaselineTable struct {
	// Name 是表名（不带 schema 限定）。
	Name string
	// Partitioned 表示它是分区父表（既有 6 张：usage_logs / err_logs /
	// usage_stats / usage_entity_stats / routing_quality_fact / routing_flow_fact）。
	Partitioned bool
	// Sequence 是 id 依赖的独立序列名（分区父表 id 走 nextval 而非 identity）；
	// 空表示无独立序列。
	Sequence string
	// Create 是建表 DDL（不带 schema 限定，且不含 IF NOT EXISTS——baseline 只允许
	// 在空库上跑，重复建表必须报错）。
	Create string
	// Indexes 是紧随建表的索引 DDL（不带 schema 限定）。
	Indexes []string
}

// MigrationBaselineDDLs 返回 0001 baseline 里「不由 ent 生成」的 8 张表：
// 6 张分区父表 + 2 张代码内静态表（stats_agg_watermark / routing_flow_snapshot_state）。
//
// **attempt_logs 不在这里**：它属 0003_attempt_logs（手写迁移），事实源是
// 同包的 attemptLogColumnDefs / MigrationAttemptLogDDLs。把它加进本函数会让
// 生成器重写已冻结的 0001（checksum 漂移 ⇒ 启动门 NotReady），这不是「补齐」，
// 是破坏。
//
// 顺序是 0001 的输出顺序：先分区父表（与 wiring 的 bootstrap 顺序一致），再静态表。
// DDL 全部来自既有事实源（partitionedCreateDDL / 各自的 *ColumnDefs 与 *IndexDDLs /
// statsAggWatermarkDDL / routingFlowSnapshotStateDDL）——本函数不复制任何列定义，
// 否则就是第二个建表真相。
//
// 仅供生成器使用（internal/db/migrate/gen 经 go:generate 调用）。
func MigrationBaselineDDLs() []BaselineTable {
	partitioned := []struct {
		table   string
		keyCol  string
		columns []string
		indexes []string
	}{
		{"usage_logs", "created_at", usageLogColumnDefs, usageLogIndexDDLs},
		{"err_logs", "created_at", errLogColumnDefs, errLogIndexDDLs},
		{"usage_stats", "bucket_time", usageStatsColumnDefs, usageStatsIndexDDLs},
		{"usage_entity_stats", "bucket_time", usageEntityStatsColumnDefs, usageEntityStatsIndexDDLs},
		{"routing_quality_fact", "bucket_minute", routingQualityFactColumnDefs, routingQualityFactIndexDDLs},
		{"routing_flow_fact", "terminal_minute", routingFlowFactColumnDefs, routingFlowFactIndexDDLs},
	}
	out := make([]BaselineTable, 0, len(partitioned)+2)
	for _, p := range partitioned {
		out = append(out, BaselineTable{
			Name:        p.table,
			Partitioned: true,
			Sequence:    p.table + "_id_seq",
			Create:      partitionedCreateDDL(p.table, p.keyCol, p.columns),
			Indexes:     p.indexes,
		})
	}
	// 静态表的建表常量带 IF NOT EXISTS（运行时幂等 bootstrap 语义）；baseline 里
	// 必须去掉——空库上重复建表要报错而不是静默通过。
	for _, static := range []struct {
		table string
		ddl   string
	}{
		{"stats_agg_watermark", statsAggWatermarkDDL},
		{"routing_flow_snapshot_state", routingFlowSnapshotStateDDL},
	} {
		out = append(out, BaselineTable{
			Name:   static.table,
			Create: strings.Replace(static.ddl, "IF NOT EXISTS ", "", 1),
		})
	}
	return out
}
