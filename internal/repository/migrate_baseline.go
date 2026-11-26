package repository

import (
	"context"
	"fmt"
	"strings"

	"entgo.io/ent/dialect/sql/schema"

	"github.com/is7qin/c3api/internal/ent/templateext"
)

// PostBaselineColumn 是「ent schema 里有、但 0001 baseline 里**没有**」的一列：
// baseline 生成时必须从 ent 的建表 DDL 里裁掉，由后续 NNNN 迁移 ADD COLUMN 补上。
type PostBaselineColumn struct {
	// Table 是不带 schema 限定的 ent 表名（schema.Table.Name）。
	Table string
	// Column 是不带引号的列名。
	Column string
}

// PostBaselineColumns 是 baseline 生成器的**显式**删列清单（spec §4.2 的唯一清单，
// 当前只有 template_exts.request_patch）。规则：
//
//   - 0001_baseline 一旦发布即不可变（checksum 漂移 ⇒ 启动门 ModeReadonly）。ent schema
//     继续加列时，不能让 gen 重写 0001；把「baseline 之后的列」显式登记在这里，
//     gen 的 MigrateHookForBaseline 会从 ent 的建表输出里裁掉它们；物理列由后续
//     迁移（0004_custom_request_patch）ADD COLUMN 建立。
//   - 这是 **baseline 生成关注点，不是 runtime schema 关注点**：runtime automigrate
//     （repository.NewWithPG(migrate=true)）仍然建**全列**（request_patch 在内），
//     测试/工具/demo/seed 路径不会 42703。看到本清单不要顺手把它加进 runtime hook。
//   - 清单项不得被 ent 表的 Indexes / ForeignKeys / PrimaryKey 引用：atlas 的列来自
//     Columns，索引/FK 来自各自的 Columns 字段——只删列会让索引 DDL 引用缺失列。
//     该结构约束由 TestTemplateExtPostBaselineShape 用 entmigrate.Tables 断言。
//   - 新增项必须同时新增一条 ADD COLUMN 迁移；反向互锁（0004 up 的 ADD COLUMN 集
//     == 本清单）由 internal/db/migrate/gen 的测试双向断言（多/少都红）。
func PostBaselineColumns() []PostBaselineColumn {
	return []PostBaselineColumn{
		{Table: templateext.Table, Column: "request_patch"},
	}
}

// MigrateHookForBaseline = migrateHookExcludesPartitioned ∘ PostBaselineColumns 删列，
// **仅供 0001 baseline 生成器**使用（gen.go 的 entDDL）。
//
// 为什么必须是两个 hook 而不是直接扩私有 hook（评审 momus C3 / oracle I1）：私有
// hook 有两个消费者——runtime automigrate 与 gen。把删列塞进私有 hook 会让
// automigrate 建出的库缺 request_patch 列（demo/seed 的 UpsertTemplateExt 直接
// 42703），而 baseline 生成又必须缺这一列。所以导出的是组合版，私有 hook 语义不变。
//
// 幂等/不污染全局表：ent 的 Tables 是进程级全局对象，hook 收到的 *schema.Table 就是
// 它们。实现用 ent 自带的 schema.CopyTables 先整批复制、再在**副本**上重建
// Columns slice，绝不原地改全局表——同进程连续两次 Create/WriteTo 输出逐字节一致，
// 全局表列集合不变（TestGenerateIsDeterministic 断言）。
//
// 为什么不是「复制 table 结构体」的浅拷贝：schema.Table 内嵌 sync.Mutex 且带未导出
// 的关系索引 map，按值复制既是 go vet 的 copylocks 违规，也会让副本与全局表共享
// 索引/FK 关系；schema.CopyTables 是 ent 官方为此场景（「迁移前复制 Tables」）提供
// 的复制入口，语义更强。本路径只在 gen 里跑一次，复制成本不在热路径。
func MigrateHookForBaseline() schema.MigrateOption {
	index := make(map[string]map[string]bool, 1)
	for _, column := range PostBaselineColumns() {
		if index[column.Table] == nil {
			index[column.Table] = make(map[string]bool, 1)
		}
		index[column.Table][column.Column] = true
	}
	return schema.WithHooks(
		hookExcludesPartitionedTables(),
		hookExcludesColumns(index),
	)
}

// hookExcludesColumns 返回「裁掉 index 里声明列」的 schema.Hook；index 在 hook
// 构造时固化（同一迁移清单内的两次 Create 不重新解析）。
func hookExcludesColumns(index map[string]map[string]bool) schema.Hook {
	return func(next schema.Creator) schema.Creator {
		return schema.CreateFunc(func(ctx context.Context, tables ...*schema.Table) error {
			kept, err := withoutColumns(tables, index)
			if err != nil {
				return err
			}
			return next.Create(ctx, kept...)
		})
	}
}

// withoutColumns 返回「已裁掉 index 声明列」的表集合。没有任何表命中时原样返回
// （不复制）；命中时用 schema.CopyTables 整批复制后在副本上重建 Columns slice，
// 全局表对象一个字节都不改。错误只可能来自 ent 的复制校验（FK 引用表不在本次
// Create 集合里）——那种情况必须 fail-fast，静默跳过等于生成坏 baseline。
func withoutColumns(tables []*schema.Table, index map[string]map[string]bool) ([]*schema.Table, error) {
	hit := false
	for _, table := range tables {
		if len(index[table.Name]) > 0 {
			hit = true
			break
		}
	}
	if !hit {
		return tables, nil
	}
	// 深拷贝：schema.Table 含 sync.Mutex 与未导出关系 map，按值复制是 copylocks
	// 违规；CopyTables 是 ent 官方「迁移前复制 Tables」入口。
	clones, err := schema.CopyTables(tables)
	if err != nil {
		return nil, fmt.Errorf("repository: copy ent tables for baseline hook: %w", err)
	}
	out := make([]*schema.Table, 0, len(clones))
	for _, table := range clones {
		declared := index[table.Name]
		if len(declared) == 0 {
			out = append(out, table)
			continue
		}
		columns := make([]*schema.Column, 0, len(table.Columns))
		for _, column := range table.Columns {
			if declared[column.Name] {
				continue
			}
			columns = append(columns, column)
		}
		table.Columns = columns
		out = append(out, table)
	}
	return out, nil
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
