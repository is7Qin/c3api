// Package catalog 把 PostgreSQL 的「结构事实」导出成规范化文本（每个对象一行、
// 稳定排序、不含 oid/relfilenode/统计信息），sha256 即结构指纹。
//
// 为什么不用 information_schema：它拿不到索引谓词、convalidated、attnum、
// indnkeyatts 等本规格必须逐字段比对的语义（本仓 PG18 实测 69 个视图里名字含
// index 的 0 个）。故全部数据源来自 pg_catalog。
//
// 两个视图（见 spec 2026-11-18-schema-migrations §2.8）：
//   - Manifest.Static：静态指纹，参与 hash 与相等比较；排除全部分区实例与
//     runner 自建的 schema_migrations。
//   - Manifest.Dynamic：分区实例视图，**不参与 hash**，只做三条合法性校验
//     （命名 YYYYMMDD / 父归属 / 精确 bounds）。
package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ControlTable 是迁移账本表名：由 runner 幂等创建，不属于任何迁移版本，故不在
// 静态指纹里（否则「升到任意版本」的指纹都会漂移）。
const ControlTable = "schema_migrations"

// BaseExtensions 是默认 PostgreSQL 安装自带的 extension 白名单：本仓业务结构
// 不引入 extension（由数据库基础设施预装），多出即拒（见 UnexpectedExtensions）。
var BaseExtensions = []string{"plpgsql"}

// partitionInstanceRE 匹配分区实例表名（父表名 + '_' + YYYYMMDD）。
var partitionInstanceRE = regexp.MustCompile(`^([a-z0-9_]+)_([0-9]{8})$`)

// Manifest 是一次 catalog 导出的结果。
type Manifest struct {
	// Static 是静态指纹原文：每对象一行、`\n` 分隔、按行排序（末尾带换行）。
	Static string
	// Dynamic 是分区实例视图（不参与指纹）。
	Dynamic Dynamic
	// Extensions 是库内 extension 名（排序）。是否允许由调用方判定
	// （见 UnexpectedExtensions）。
	Extensions []string
	// Violations 记录「本仓当前必须为空」的对象类别里发现的对象
	// （view / trigger / policy / function）。
	Violations []string
}

// Fingerprint 返回静态指纹的 sha256（hex）。
func (m Manifest) Fingerprint() string {
	sum := sha256.Sum256([]byte(m.Static))
	return hex.EncodeToString(sum[:])
}

// Lines 返回静态指纹的行切片（不含空行）。
func (m Manifest) Lines() []string {
	if m.Static == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(m.Static, "\n"), "\n")
}

// Dynamic 是分区实例视图（父表 → 子分区），只做合法性校验。
type Dynamic struct {
	// Partitions 是全部 (父表, 子分区) 对，按子分区名排序。
	Partitions []Partition
	// Violations 是三条动态规则的违反项：命名 YYYYMMDD、父归属、精确 bounds。
	Violations []string
}

// Partition 是一个分区实例。
type Partition struct {
	// Parent 是父表名（public.<name>）。
	Parent string
	// Child 是子分区名（public.<name>_YYYYMMDD）。
	Child string
	// Day 是子分区名里的日期（UTC 当日）。
	Day time.Time
	// From/To 是分区边界（解析自 relpartbound，UTC）。
	From time.Time
	To   time.Time
}

// Valid 表示该分区实例是否满足三条动态规则（命名/父归属/精确 bounds）。
func (p Partition) Valid() bool { return p.Day.Equal(p.From) && p.To.Equal(p.Day.AddDate(0, 0, 1)) }

// Dump 导出当前库（public schema）的结构事实。
//
// 全部查询在**一个只读事务**里执行，并在事务内 `SET LOCAL search_path = public`
// ——这样 pg_get_constraintdef/pg_get_indexdef 的限定形式与调用方会话设置无关，
// 输出才可逐字节比较。
func Dump(ctx context.Context, db *sql.DB) (Manifest, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return Manifest{}, fmt.Errorf("catalog: begin read-only tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path = public`); err != nil {
		return Manifest{}, fmt.Errorf("catalog: set search_path: %w", err)
	}

	var lines []string
	for _, q := range []struct {
		kind  string
		query string
	}{
		{"table", qTables},
		{"column", qColumns},
		{"index", qIndexes},
		{"constraint", qConstraints},
		{"sequence", qSequences},
		{"enum", qEnums},
		{"partitioned", qPartitioned},
	} {
		got, err := queryLines(ctx, tx, q.kind, q.query)
		if err != nil {
			return Manifest{}, err
		}
		lines = append(lines, got...)
	}

	m := Manifest{}
	sort.Strings(lines)
	if len(lines) > 0 {
		m.Static = strings.Join(lines, "\n") + "\n"
	}
	if m.Dynamic, err = loadDynamic(ctx, tx); err != nil {
		return Manifest{}, err
	}
	if m.Extensions, err = loadStrings(ctx, tx, qExtensions); err != nil {
		return Manifest{}, err
	}
	if m.Violations, err = loadStrings(ctx, tx, qMustBeEmpty); err != nil {
		return Manifest{}, err
	}
	if err := tx.Commit(); err != nil {
		return Manifest{}, fmt.Errorf("catalog: commit read-only tx: %w", err)
	}
	return m, nil
}

// queryLines 跑一条「首列是 kind、其余列用 '|' 拼好」的查询并把 kind 前缀加上。
func queryLines(ctx context.Context, tx *sql.Tx, kind, query string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("catalog: query %s: %w", kind, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("catalog: scan %s: %w", kind, err)
		}
		out = append(out, kind+"|"+line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate %s: %w", kind, err)
	}
	return out, nil
}

func loadStrings(ctx context.Context, tx *sql.Tx, query string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("catalog: query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("catalog: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate: %w", err)
	}
	return out, nil
}

// PartitionParents 是分区父表 allowlist：只有这些父表的按日分区才从静态指纹里
// 排除（它们走 Dynamic 视图，数量随保留策略变化）。新增分区父表（例如 M0b 的
// attempt_logs）必须在这里登记——漏登记不会静默吞对象，而是让分区实例出现在
// 静态指纹里（漂移可见），这是刻意的失败方向。
var PartitionParents = []string{
	"usage_logs",
	"err_logs",
	"usage_stats",
	"usage_entity_stats",
	"routing_quality_fact",
	"routing_flow_fact",
}

// partitionInstanceSQL 构造「该关系是已知父表的按日分区」判定。父归属取自
// pg_inherits（真实继承边），**不是**只看名字后缀——否则一张名叫 foo_20240101
// 的普通业务表会从指纹里消失（oracle R1 §3）。
func partitionInstanceSQL(oidExpr, nameExpr string) string {
	quoted := make([]string, len(PartitionParents))
	for i, parent := range PartitionParents {
		quoted[i] = "'" + parent + "'"
	}
	return nameExpr + ` ~ '_[0-9]{8}$' AND EXISTS (
		SELECT 1 FROM pg_inherits inh
		JOIN pg_class parent ON parent.oid = inh.inhparent
		WHERE inh.inhrelid = ` + oidExpr + `
			AND parent.relname = ANY (ARRAY[` + strings.Join(quoted, ", ") + `])
	)`
}

// excludedRel 是「表/列/约束」口径的排除片段：runner 账本表 + 已知父表的分区实例。
var excludedRel = `AND c.relname <> '` + ControlTable + `' AND NOT (` + partitionInstanceSQL("c.oid", "c.relname") + `)`

// excludedIndexRel 是「索引」口径的排除片段（索引挂在被索引表 tc 上）。
var excludedIndexRel = `AND tc.relname <> '` + ControlTable + `' AND NOT (` + partitionInstanceSQL("tc.oid", "tc.relname") + `)`

// 这四条查询是 var（不是 const）：排除片段要按 PartitionParents 在运行时拼出来。
var qTables = `
SELECT c.relname
	|| '|relkind=' || c.relkind::text
	|| '|persistence=' || c.relpersistence::text
	|| '|tablespace=' || COALESCE(ts.spcname, '')
	|| '|options=' || COALESCE((SELECT string_agg(o, ',' ORDER BY o) FROM unnest(c.reloptions) AS o), '')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_tablespace ts ON ts.oid = c.reltablespace
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ` + excludedRel + `
ORDER BY 1`

var qColumns = `
SELECT c.relname
	|| '|attnum=' || a.attnum
	|| '|name=' || a.attname
	|| '|type=' || format_type(a.atttypid, a.atttypmod)
	|| '|notnull=' || a.attnotnull::text
	|| '|identity=' || a.attidentity::text
	|| '|generated=' || a.attgenerated::text
	|| '|collation=' || CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation
		THEN quote_ident(coll.collname) ELSE '' END
	|| '|default=' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_collation coll ON coll.oid = a.attcollation
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
	AND a.attnum > 0 AND NOT a.attisdropped ` + excludedRel + `
ORDER BY 1, a.attnum`

var qIndexes = `
SELECT ic.relname
	|| '|table=' || tc.relname
	|| '|unique=' || i.indisunique::text
	|| '|primary=' || i.indisprimary::text
	|| '|nkeyatts=' || i.indnkeyatts
	|| '|indkey=' || i.indkey::text
	|| '|predicate=' || COALESCE(pg_get_expr(i.indpred, i.indrelid), '')
	|| '|options=' || COALESCE((SELECT string_agg(o, ',' ORDER BY o) FROM unnest(ic.reloptions) AS o), '')
	|| '|def=' || pg_get_indexdef(i.indexrelid)
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_class tc ON tc.oid = i.indrelid
JOIN pg_namespace n ON n.oid = ic.relnamespace
WHERE n.nspname = 'public' ` + excludedIndexRel + `
ORDER BY 1`

// qConstraints 排除 contype='n'（PG15+ 把 NOT NULL 也建成约束行）：NOT NULL 由
// column 行的 attnotnull 承载，重复一份只会放大噪音并制造两条真相。
var qConstraints = `
SELECT c.relname
	|| '|name=' || con.conname
	|| '|type=' || con.contype::text
	|| '|validated=' || con.convalidated::text
	|| '|def=' || pg_get_constraintdef(con.oid)
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND con.contype <> 'n' ` + excludedRel + `
ORDER BY 1`

const qSequences = `
SELECT c.relname
	|| '|type=' || format_type(s.seqtypid, NULL)
	|| '|start=' || s.seqstart
	|| '|increment=' || s.seqincrement
	|| '|min=' || s.seqmin
	|| '|max=' || s.seqmax
	|| '|cache=' || s.seqcache
	|| '|cycle=' || s.seqcycle::text
	|| '|owned=' || COALESCE(oc.relname || '.' || oa.attname, '')
FROM pg_sequence s
JOIN pg_class c ON c.oid = s.seqrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('a', 'i')
LEFT JOIN pg_class oc ON oc.oid = d.refobjid
LEFT JOIN pg_attribute oa ON oa.attrelid = d.refobjid AND oa.attnum = d.refobjsubid
WHERE n.nspname = 'public'
ORDER BY 1`

const qEnums = `
SELECT t.typname
	|| '|sortorder=' || e.enumsortorder
	|| '|label=' || e.enumlabel
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
JOIN pg_enum e ON e.enumtypid = t.oid
WHERE n.nspname = 'public'
ORDER BY 1`

const qPartitioned = `
SELECT c.relname
	|| '|strategy=' || pt.partstrat::text
	|| '|key=' || COALESCE((
		SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		FROM unnest(pt.partattrs::int2[]) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
	), '')
FROM pg_partitioned_table pt
JOIN pg_class c ON c.oid = pt.partrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
ORDER BY 1`

// qExtensions 记录库内 extension；纳入指纹的是「白名单校验」而不是 extension 本体
// （extension 由数据库基础设施预装，不属于业务结构）。
const qExtensions = `SELECT extname FROM pg_extension ORDER BY 1`

// qMustBeEmpty 枚举本仓当前必须为空的对象类别（view/matview/trigger/policy/function）。
// 出现任何一个都视为未知结构，静态指纹与它比对即失败。
const qMustBeEmpty = `
SELECT kind || '|' || name FROM (
	SELECT CASE c.relkind WHEN 'v' THEN 'view' ELSE 'matview' END AS kind, c.relname AS name
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public' AND c.relkind IN ('v', 'm')
	UNION ALL
	SELECT 'trigger', c.relname || '.' || t.tgname
	FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public' AND NOT t.tgisinternal
	UNION ALL
	SELECT 'policy', c.relname || '.' || p.polname
	FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public'
	UNION ALL
	SELECT 'function', p.proname
	FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	WHERE n.nspname = 'public'
) s
ORDER BY 1`

// partitionBoundsRE 解析 `FOR VALUES FROM ('…') TO ('…')`（PG 对 timestamptz 分区
// 输出的规范形式）。
var partitionBoundsRE = regexp.MustCompile(`^FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)$`)

// loadDynamic 读分区实例并做三条动态规则校验（不参与静态指纹）。
func loadDynamic(ctx context.Context, tx *sql.Tx) (Dynamic, error) {
	rows, err := tx.QueryContext(ctx, qPartitions)
	if err != nil {
		return Dynamic{}, fmt.Errorf("catalog: query partitions: %w", err)
	}
	defer rows.Close()
	var d Dynamic
	for rows.Next() {
		var parent, child, bound string
		if err := rows.Scan(&parent, &child, &bound); err != nil {
			return Dynamic{}, fmt.Errorf("catalog: scan partitions: %w", err)
		}
		p := Partition{Parent: "public." + parent, Child: "public." + child}
		match := partitionInstanceRE.FindStringSubmatch(child)
		switch {
		case match == nil || match[1] != parent:
			// 规则①②：命名必须是 <父表>_YYYYMMDD，且父归属与名字前缀一致
			// （同名子表挂错父由 pg_inherits 的实际父表暴露）。
			d.Violations = append(d.Violations, fmt.Sprintf("partition %s: name does not match parent %s with YYYYMMDD suffix", child, parent))
		default:
			day, err := time.ParseInLocation("20060102", match[2], time.UTC)
			if err != nil {
				d.Violations = append(d.Violations, fmt.Sprintf("partition %s: date suffix is not a valid YYYYMMDD: %v", child, err))
				break
			}
			p.Day = day
			from, to, err := parseBounds(bound)
			if err != nil {
				d.Violations = append(d.Violations, fmt.Sprintf("partition %s: bounds %q: %v", child, bound, err))
				break
			}
			p.From, p.To = from, to
			// 规则③：区间必须精确等于 [当日 00:00Z, 次日 00:00Z)，不接受更粗的区间。
			if !p.Valid() {
				d.Violations = append(d.Violations, fmt.Sprintf("partition %s: bounds [%s, %s) are not exactly [%s, %s)",
					child, from.Format(time.RFC3339), to.Format(time.RFC3339), day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339)))
			}
		}
		d.Partitions = append(d.Partitions, p)
	}
	if err := rows.Err(); err != nil {
		return Dynamic{}, fmt.Errorf("catalog: iterate partitions: %w", err)
	}
	sort.Slice(d.Partitions, func(i, j int) bool { return d.Partitions[i].Child < d.Partitions[j].Child })
	sort.Strings(d.Violations)
	return d, nil
}

const qPartitions = `
SELECT pc.relname, cc.relname, pg_get_expr(cc.relpartbound, cc.oid)
FROM pg_inherits inh
JOIN pg_class pc ON pc.oid = inh.inhparent
JOIN pg_class cc ON cc.oid = inh.inhrelid
JOIN pg_namespace pn ON pn.oid = pc.relnamespace
WHERE pn.nspname = 'public' AND pc.relkind = 'p'
ORDER BY 2`

func parseBounds(bound string) (time.Time, time.Time, error) {
	m := partitionBoundsRE.FindStringSubmatch(strings.TrimSpace(bound))
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported partition bound syntax")
	}
	from, err := parseBoundTime(m[1])
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseBoundTime(m[2])
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

func parseBoundTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05-07", "2006-01-02 15:04:05-07:00", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported bound timestamp %q", s)
}

// Missing 返回 want 里有、got 里没有的行（I2：迁移后的 catalog ⊇ 基线快照）。
func Missing(want, got Manifest) []string {
	return missingLines(want.Lines(), got)
}

func missingLines(want []string, got Manifest) []string {
	have := make(map[string]struct{}, len(want))
	for _, line := range got.Lines() {
		have[line] = struct{}{}
	}
	var out []string
	for _, line := range want {
		if _, ok := have[line]; !ok {
			out = append(out, line)
		}
	}
	return out
}

// ExpandOnly 实现 I3（expand-only 机械断言）：before → after 只允许「新增」，不允许
// 删除、改类型/改默认值/改约束、以及**收紧**（对已存在的表新增 UNIQUE 索引或
// UNIQUE/EXCLUDE 约束——旧二进制写出的合法行会被拒）。
//
// 返回违反项（空 = 通过）。不解析 SQL：只比对两份 catalog。
func ExpandOnly(before, after Manifest) []string {
	violations := missingLines(before.Lines(), after)
	// 已存在表的集合：用来区分「新表自带唯一索引」（合法）与「老表新增唯一约束」（收紧）。
	beforeTables := tables(before)
	beforeLines := make(map[string]struct{}, len(before.Lines()))
	for _, line := range before.Lines() {
		beforeLines[line] = struct{}{}
	}
	for _, line := range after.Lines() {
		if _, existed := beforeLines[line]; existed {
			continue // 原有对象原样保留，不算收紧
		}
		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "index":
			if !strings.Contains(line, "|unique=true") {
				continue
			}
			if table := fieldValue(fields, "table"); table != "" {
				if _, existed := beforeTables[table]; existed {
					violations = append(violations, "tightening: new unique index on existing table: "+line)
				}
			}
		case "constraint":
			contype := fieldValue(fields, "type")
			if contype != "u" && contype != "x" {
				continue
			}
			if _, existed := beforeTables[fields[1]]; existed {
				violations = append(violations, "tightening: new unique/exclusion constraint on existing table: "+line)
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// tables 返回 manifest 里的表名集合（不含 public. 前缀）。
func tables(m Manifest) map[string]struct{} {
	out := make(map[string]struct{})
	for _, line := range m.Lines() {
		fields := strings.Split(line, "|")
		if len(fields) > 1 && fields[0] == "table" {
			out[fields[1]] = struct{}{}
		}
	}
	return out
}

// fieldValue 取 `key=value` 形式字段的值。
func fieldValue(fields []string, key string) string {
	prefix := key + "="
	for _, f := range fields {
		if strings.HasPrefix(f, prefix) {
			return strings.TrimPrefix(f, prefix)
		}
	}
	return ""
}

// UnexpectedExtensions 返回不在 allowlist 里的 extension（白名单外多出即拒）。
func UnexpectedExtensions(m Manifest, allowlist []string) []string {
	allow := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		allow[name] = struct{}{}
	}
	var out []string
	for _, name := range m.Extensions {
		if _, ok := allow[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}
