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
// “把 doc 传到别处/新建第二个构造器/包级存一份”，抓不住同构代码的语义绕行。
// v21（momus-r2 I-8）：补上包级 `var d schemaDoc`、`map[string]schemaDoc`、
// 以及 `.raw`/`.memo` 字段读写的位置限制。
var schemaDocAllowedFuncs = map[string]struct{}{
	"newSchemaDoc":           {},
	"resolve":                {},
	"resolveUncached":        {},
	"flattenMessSchema":      {},
	"flattenMessSchemaRoot":  {},
	"flattenMessSchemaDepth": {},
	// 只读辅助：只把 doc 当"解析器"用，不新建、不外传
	"derefChain":    {},
	"flattenNested": {},
}

// schemaDocFieldAllowedFuncs raw/memo 的读写只允许在这几个函数里（
// resolveUncached 是 resolve 的无缓存体，只能被 resolve 调用）。
var schemaDocFieldAllowedFuncs = map[string]struct{}{
	"newSchemaDoc":    {},
	"resolve":         {},
	"resolveUncached": {},
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
				// 顶层声明里只允许 `type schemaDoc struct{…}` 这一处出现：
				// `var d schemaDoc` / `var table map[string]schemaDoc` 一律红。
				var typeName *ast.Ident
				if decl.Tok == token.TYPE && len(decl.Specs) == 1 {
					if ts, ok := decl.Specs[0].(*ast.TypeSpec); ok && ts.Name.Name == "schemaDoc" {
						_, isStruct := ts.Type.(*ast.StructType)
						require.True(t, isStruct, "schemaDoc 必须是 struct")
						typeCount++
						typeName = ts.Name
					}
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if !ok || id.Name != "schemaDoc" {
						return true
					}
					occurrences++
					require.True(t, typeName != nil && id == typeName,
						"包级声明里出现了 schemaDoc 的非类型用法（文件 %s）", name)
					return true
				})
			case *ast.FuncDecl:
				_, allowed := schemaDocAllowedFuncs[decl.Name.Name]
				_, fieldsAllowed := schemaDocFieldAllowedFuncs[decl.Name.Name]
				ast.Inspect(decl, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if ok && id.Name == "schemaDoc" {
						occurrences++
						require.True(t, allowed, "schemaDoc 出现在不允许的函数 %s（文件 %s）", decl.Name.Name, name)
					}
					// (s)⑤ 字段读写白名单：d.raw / d.memo 只允许在 newSchemaDoc 与 resolve 内
					if sel, ok := n.(*ast.SelectorExpr); ok {
						if sel.Sel.Name == "raw" || sel.Sel.Name == "memo" {
							if x, ok := sel.X.(*ast.Ident); ok && x.Name == "d" {
								require.True(t, fieldsAllowed,
									"schemaDoc 字段 %s 在 %s 里被读写（只允许 newSchemaDoc/resolve）",
									sel.Sel.Name, decl.Name.Name)
							}
						}
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

// (s)② 的反向断言：`&schemaDoc{…}` / `new(schemaDoc)` 只允许出现在 newSchemaDoc 体内。
func TestSchemaDocConstructedOnce(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "toolschema.go", nil, 0)
	require.NoError(t, err)
	var ctor *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == "newSchemaDoc" {
			ctor = fd
		}
		return true
	})
	require.NotNil(t, ctor, "找不到 newSchemaDoc")
	built := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if id, ok := x.Type.(*ast.Ident); ok && id.Name == "schemaDoc" {
				built++
				require.True(t, ctor.Pos() <= x.Pos() && x.End() <= ctor.End(),
					"&schemaDoc{…} 只允许出现在 newSchemaDoc 体内")
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
				if aid, ok := x.Args[0].(*ast.Ident); ok && aid.Name == "schemaDoc" {
					require.Fail(t, "禁止 new(schemaDoc)：构造点必须唯一")
				}
			}
		}
		return true
	})
	require.Equal(t, 1, built, "schemaDoc 的复合字面量必须恰好 1 处")
}
