// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A11（G5：Redis 不是 SoR）——观测写面不得依赖 Redis 持久性。
//
// 三件事各断言一次：
//  1. **写面构造器/写动词签名无 redis 参数**（AST 断言）：仓库写面（构造器 +
//     routing 写动词）的形参/结果类型里不得出现任何 redis 类型。Redis 挂掉时
//     观测写面必须照常落 PG——这是"Redis 只存可丢可重建状态"的结构性保证。
//  2. **路由存储面无任何 redis 引用**（包内 `git grep redis` 门禁形式）：新增
//     routing 观测 Redis key（`c3api:routing:` 字面量）或 import redis 客户端
//     即失败。观测读/写面钉死 rollup 表，不经 Redis。
//  3. **路由 PG 套件无 Redis 全过**：本套件不读任何 Redis 环境变量、不建连接
//     （`go test ./internal/repository/` 在无 Redis 的机器上全绿即为证据）。
func TestRoutingStorageSurfaceHasNoRedisDependency(t *testing.T) {
	dir := repoPackageDir(t)
	files := parseGoFiles(t, dir)

	// 1) 签名断言：写面构造器与 routing 写动词。
	for _, name := range []string{
		"NewWithPG",                            // 仓库构造器（生产写面入口）
		"UpsertFlowSnapshot",                   // 合并层快照写
		"UpsertQualityAndMarkDirty",            // quality 暂存写
		"RollupQuality",                        // quality 汇总写
		"DeleteRoutingFlowSnapshotStateBefore", // 交接状态有界清理
	} {
		types := funcTypeNames(files, name)
		require.NotEmpty(t, types, "%s must exist (write-surface signature assertion)", name)
		require.NotContains(t, strings.ToLower(strings.Join(types, " ")), "redis",
			"%s must not take or return a redis-typed value", name)
	}

	// PartitionRepo 结构体字段同样不得有 redis 类型。
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "PartitionRepo" {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				require.NotContains(t, strings.ToLower(exprTypeName(field.Type)), "redis",
					"PartitionRepo must not hold a redis-typed field")
			}
			return false
		})
	}

	// 2) 源文本门禁：路由存储面（routing*.go 非测试）不得出现 redis 或观测 Redis key。
	entries, err := filepath.Glob(filepath.Join(dir, "routing*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		lower := strings.ToLower(string(raw))
		require.NotContains(t, lower, "redis", "%s must not depend on Redis (Redis is not a source of truth)", filepath.Base(path))
		require.NotContains(t, string(raw), "c3api:routing:",
			"%s must not declare a routing observation Redis key", filepath.Base(path))
	}
}

// funcTypeNames 返回指定函数声明的形参与结果类型名（去指针，选择子取末段）。
func funcTypeNames(files []*ast.File, name string) []string {
	var out []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != name || fn.Type == nil {
				return true
			}
			for _, list := range []*ast.FieldList{fn.Type.Params, fn.Type.Results} {
				if list == nil {
					continue
				}
				for _, field := range list.List {
					out = append(out, exprTypeName(field.Type))
				}
			}
			return false
		})
	}
	return out
}

// exprTypeName 取类型表达式的可读名字（*T → T，pkg.T → pkg.T，其余用 %T 兜底）。
func exprTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return exprTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprTypeName(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprTypeName(t.Elt)
	case *ast.MapType:
		return "map[" + exprTypeName(t.Key) + "]" + exprTypeName(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	default:
		return "expr"
	}
}
