// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package schema

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// 本文件是 U3 的双门禁：status 枚举只允许有一个权威（internal/domain），
// schema 枚举与 DDL 默认值都只是它的引用方。
//
// 为什么需要两道：
//   - (a) 抓 schema 再写 `"active"`/`"disabled"` 字面量。否则改 domain 常量值
//     后非 PG 测试全绿，运行期才炸；
//   - (b) 抓 DDL 默认值漂移。ent 生成物不读 0001_baseline.up.sql，只改 domain
//     或只改迁移都不会被 (a) 发现。
//
// 门禁 (b) 的期望值从 domain 常量现算；本文件不保存第三份值副本。

// gateSchemaDir 返回本包源码目录。
func gateSchemaDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must locate this test file")
	return filepath.Dir(file)
}

// gateForbiddenStatusLiterals 是门禁 (a) 的拼写黑名单：schema 里这些枚举取值
// 只许经 domain 常量引用。它是“禁止出现的拼写”清单，不是值的权威副本；值等价
// （DDL 默认值 vs domain 常量）由门禁 (b) 现算比对。
//
// 不用“从 domain 取值现算”的黑名单：domain 值一旦改成 enabled 这类词，会与表里
// 已有的列名（field.Bool("enabled") 等）撞车，现算集合会把无关列名误判为红。
var gateForbiddenStatusLiterals = []string{"active", "disabled"}

// TestSchemaStatusValuesLiveInDomain 门禁 (a)：internal/ent/schema 的非测试源
// 不许出现 4 组 status 枚举的取值字面量，枚举值只能来自 domain 常量。
// 命中按 file:line 全量列出（不做首个短路）；注释不算字面量（AST 里注释不是
// BasicLit），故 management_key.go 的 status 说明注释不触发本门。
func TestSchemaStatusValuesLiveInDomain(t *testing.T) {
	forbidden := make(map[string]struct{}, len(gateForbiddenStatusLiterals))
	for _, value := range gateForbiddenStatusLiterals {
		forbidden[value] = struct{}{}
	}
	dir := gateSchemaDir(t)
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "schema package has no Go sources under %s", dir)

	fset := token.NewFileSet()
	var hits []string
	scanned := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "parse %s", path)
		scanned++
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			if _, banned := forbidden[value]; banned {
				position := fset.Position(literal.Pos())
				hits = append(hits, fmt.Sprintf("%s:%d", filepath.Base(position.Filename), position.Line))
			}
			return true
		})
	}
	require.NotZero(t, scanned, "gate must parse at least one non-test schema source")
	sort.Strings(hits)
	require.Empty(t, hits, "status enum values must come from internal/domain constants; literal copies at %s", strings.Join(hits, ", "))
}

// gateStatusDefaultRe 匹配 CREATE TABLE 块内 status 列的默认值。`[^,]*?` 把
// 搜索限制在同一列内（列以逗号分隔），故不会把下一列的 DEFAULT 认成 status 的。
var gateStatusDefaultRe = regexp.MustCompile(`"status"[^,]*?DEFAULT\s+'([^']*)'`)

// gateSQLTable 是一个 CREATE TABLE 块：表名、列定义体（已去掉外层括号）与
// 建表语句起始行（报错定位用）。
type gateSQLTable struct {
	table string
	body  string
	file  string
	line  int
}

// gateCreateTables 从 SQL 里切出每个 `CREATE TABLE public.<name> (...)` 块。
// 括号按配平扫描并跳过单/双引号内的字符（CHECK 约束与字符串默认值里都可能
// 出现括号或逗号）；迁移里单行建表与多行建表两种形态都必须能切。
func gateCreateTables(src string) []gateSQLTable {
	const prefix = "CREATE TABLE public."
	var out []gateSQLTable
	for offset := 0; offset < len(src); {
		rel := strings.Index(src[offset:], prefix)
		if rel < 0 {
			break
		}
		start := offset + rel
		after := start + len(prefix)
		name, width := gateSQLIdent(src[after:])
		if name == "" {
			offset = after
			continue
		}
		openRel := strings.IndexByte(src[after+width:], '(')
		if openRel < 0 {
			break
		}
		open := after + width + openRel
		body, end := gateSQLParens(src, open)
		if end < 0 {
			break
		}
		out = append(out, gateSQLTable{
			table: name,
			body:  body,
			line:  strings.Count(src[:start], "\n") + 1,
		})
		offset = end + 1
	}
	return out
}

// gateSQLIdent 读取 CREATE TABLE 后的表名，返回表名与消耗的字节数。
// 同时支持 public."users" 的引号形式与 public.usage_logs 的裸标识符形式。
func gateSQLIdent(s string) (string, int) {
	if s == "" {
		return "", 0
	}
	if s[0] == '"' {
		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return "", 0
		}
		return s[1 : 1+end], end + 2
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			i++
			continue
		}
		break
	}
	return s[:i], i
}

// gateSQLParens 从 open（开括号下标）起配平扫描，返回括号内的文本与闭括号
// 下标；引号内的括号不计数，SQL 字符串里成对单引号的转义按双字符跳过。
// 未配平返回 end=-1。
func gateSQLParens(src string, open int) (string, int) {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '\'', '"':
			quote := src[i]
			for i++; i < len(src); i++ {
				if src[i] != quote {
					continue
				}
				if i+1 < len(src) && src[i+1] == quote {
					i++
					continue
				}
				break
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return src[open+1 : i], i
			}
		}
	}
	return "", -1
}

// gateStatusEntity 把一张实体表映射到它 status 列应当取的 domain 常量值。
// 这是"哪张表存哪个实体"的映射，不是值副本：value 永远从 domain 常量现算。
type gateStatusEntity struct {
	table string
	value string
}

// gateStatusEntities 是 4 组 status 枚举对应的实体表与权威值。CREATE TABLE 的
// DEFAULT 与 ALTER ... SET DEFAULT 两条写入路径共用这份映射，避免两处各写一份。
var gateStatusEntities = []gateStatusEntity{
	{"users", string(domain.UserStatusActive)},
	{"keys", string(domain.KeyStatusActive)},
	{"management_keys", string(domain.ManagementKeyStatusActive)},
	{"redemption_codes", string(domain.RedemptionStatusActive)},
}

// TestMigrationStatusDefaultsMatchDomain 门禁 (b)：基线 DDL 里 4 张实体表的
// status 默认值必须等于对应 domain 常量的当前取值。
//
// 按表判定而不是"扫全库所有 status 默认值"：同一迁移里 orders 与
// supplier_settlements 的 status 默认值是 'pending'，不属于这 4 组状态枚举，
// 把它们拉进判据只会假红。表名 → 常量 的映射是"哪张表存哪个实体"，不是值副本。
func TestMigrationStatusDefaultsMatchDomain(t *testing.T) {
	migrationsDir := filepath.Join(gateSchemaDir(t), "..", "..", "db", "migrate", "migrations")
	paths, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no up migrations under %s", migrationsDir)

	byTable := make(map[string][]gateSQLTable)
	for _, path := range paths {
		src, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", path)
		for _, block := range gateCreateTables(string(src)) {
			block.file = filepath.Base(path)
			byTable[block.table] = append(byTable[block.table], block)
		}
	}

	for _, entity := range gateStatusEntities {
		blocks := byTable[entity.table]
		require.NotEmptyf(t, blocks, "CREATE TABLE public.%q not found in %v; a missing table would make this gate vacuous", entity.table, paths)
		for _, block := range blocks {
			matches := gateStatusDefaultRe.FindStringSubmatch(block.body)
			require.NotEmptyf(t, matches, "%s:%d: CREATE TABLE public.%q has no single-quoted status DEFAULT; the gate cannot verify it", block.file, block.line, entity.table)
			require.Equalf(t, entity.value, matches[1], "%s:%d: public.%q.status DDL default drifted from the domain constant: got %q, want %q", block.file, block.line, entity.table, matches[1], entity.value)
		}
	}
}

// gateAlterStatusDefaultRe 匹配 `ALTER TABLE [public.]"<t>" ALTER COLUMN "status"
// SET DEFAULT '<v>'`：表名可带/不带双引号、可带/不带 public. 前缀，空白（含换行）
// 一律按 \s+ 吃。CREATE TABLE 的 DEFAULT 与 ALTER ... SET DEFAULT 是"status 默认
// 值"这同一个事实的两条写入路径；只切 CREATE TABLE，ALTER 这条路径就完全隐形。
var gateAlterStatusDefaultRe = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(?:public\s*\.\s*)?"?([A-Za-z_][A-Za-z0-9_]*)"?\s+ALTER\s+COLUMN\s+"?status"?\s+SET\s+DEFAULT\s+'([^']*)'`)

// TestMigrationAlterStatusDefaultsMatchDomain 门禁 (b) 的第二条写入路径：迁移里
// 一旦出现 `ALTER TABLE ... ALTER COLUMN "status" SET DEFAULT '<v>'`，其取值必须
// 等于对应 domain 常量现算值（与 CREATE TABLE 门禁同一份表→常量映射）。
//
// 今天这四张表没有这样的 ALTER，故不要求它存在；但一旦出现就必须取值正确——
// 否则 `CREATE TABLE` 改成 'active' 后、运行期又被 ALTER 改回漂移值，门禁全绿而
// 线上默认值已错。别的表（如 orders）的 ALTER 不纳判据，与门禁 (b) 同口径。
func TestMigrationAlterStatusDefaultsMatchDomain(t *testing.T) {
	migrationsDir := filepath.Join(gateSchemaDir(t), "..", "..", "db", "migrate", "migrations")
	paths, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no up migrations under %s", migrationsDir)

	want := make(map[string]string, len(gateStatusEntities))
	for _, entity := range gateStatusEntities {
		want[entity.table] = entity.value
	}

	scanned := 0
	for _, path := range paths {
		src, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", path)
		scanned++
		for _, m := range gateAlterStatusDefaultRe.FindAllStringSubmatch(string(src), -1) {
			table, value := m[1], m[2]
			wantValue, tracked := want[table]
			if !tracked {
				continue // 别的表的 ALTER 不纳判据
			}
			require.Equalf(t, wantValue, value, "%s: ALTER TABLE public.%q ALTER COLUMN status SET DEFAULT drifted from the domain constant: got %q, want %q", filepath.Base(path), table, value, wantValue)
		}
	}
	require.NotZero(t, scanned, "gate must scan at least one up migration")
}
