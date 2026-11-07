package protoconv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// (s) 文档基准唯一性（包级，AST 版）。
// 说明：spec 要求 packages.Load + go/types；本仓库未引入 golang.org/x/tools，
// 这里用 go/ast 做**位置白名单**（弱一层，L15 已登记“启发式”）：抓得住
// “把 doc 传到别处/新建第二个构造器”，抓不住同构代码的语义绕行。
var schemaDocAllowedFuncs = map[string]struct{}{
	"newSchemaDoc":      {},
	"resolve":           {},
	"resolveUncached":   {},
	"flattenMessSchema": {},
	// 只读辅助：只把 doc 当"解析器"用，不新建、不外传
	"derefChain": {},
}

func TestSchemaDocUniqueness(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	typeCount, occurrences := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.GenDecl:
				// 顶层（类型定义/变量）允许出现
				ast.Inspect(decl, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if ok && ts.Name.Name == "schemaDoc" {
						_, isStruct := ts.Type.(*ast.StructType)
						require.True(t, isStruct, "schemaDoc 必须是 struct")
						typeCount++
					}
					if id, ok := n.(*ast.Ident); ok && id.Name == "schemaDoc" {
						occurrences++
					}
					return true
				})
			case *ast.FuncDecl:
				_, allowed := schemaDocAllowedFuncs[decl.Name.Name]
				ast.Inspect(decl, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if ok && id.Name == "schemaDoc" {
						occurrences++
						require.True(t, allowed, "schemaDoc 出现在不允许的函数 %s（文件 %s）", decl.Name.Name, name)
					}
					return true
				})
				// ④ 只有 newSchemaDoc 可以返回 *schemaDoc
				if decl.Type.Results != nil {
					for _, res := range decl.Type.Results.List {
						if star, ok := res.Type.(*ast.StarExpr); ok {
							if id, ok := star.X.(*ast.Ident); ok && id.Name == "schemaDoc" {
								require.Equal(t, "newSchemaDoc", decl.Name.Name,
									"只有 newSchemaDoc 可以返回 *schemaDoc（%s 违反了）", decl.Name.Name)
							}
						}
					}
				}
			}
		}
	}
	require.Equal(t, 1, typeCount, "type schemaDoc 全包必须恰好 1 处")
	require.Greater(t, occurrences, 2, "至少要扫到几处出现，否则遍历失效")
}
