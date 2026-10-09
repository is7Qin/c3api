// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// AST 构造门禁：snapshotStatic 的构造/克隆入口必须**唯一**（newSnapshotStatic）。
// 独立扫描（**含 `_test.go`**，不复用只扫非测试的 gateSources）匹配全部
// `snapshotStatic{...}` CompositeLit（非仅 `&snapshotStatic{}`），以及全部
// `*root` 解引用拷贝（root = `.static.Load()` / `&snapshotStatic{}` 绑定的
// *snapshotStatic 根）。二者只允许出现在构造函数 newSnapshotStatic 内——任何
// 就地复制-改字段的写法都会绕过 key 缓存（planKey 在构造期一次派生并持有），
// 必须显式改这里而非静默遗漏。
func TestSnapshotStaticConstructedOnlyByConstructor(t *testing.T) {
	root := gateRoot(t)
	dir := filepath.Join(root, "internal", "scheduler")
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)

	var offending []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		require.NoError(t, perr, "解析失败：%s", n)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			roots := snapRoots(fn)
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch v := node.(type) {
				case *ast.CompositeLit:
					if isSnapshotStaticType(v.Type) && fn.Name.Name != "newSnapshotStatic" {
						offending = append(offending, n+"#"+fn.Name.Name+" composite-literal")
					}
				case *ast.StarExpr:
					if id, ok := v.X.(*ast.Ident); ok && roots[id.Name] && fn.Name.Name != "newSnapshotStatic" {
						offending = append(offending, n+"#"+fn.Name.Name+" deref-copy *"+id.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(offending)
	require.Empty(t, offending,
		"snapshotStatic 只能经 newSnapshotStatic 构造/克隆（含测试）；就地复制-改字段会绕过 key 缓存。若确有豁免，必须显式登记单条 test-only 豁免并写明理由：%v", offending)
}

// isSnapshotStaticType 报告 e 是否裸 `snapshotStatic`（CompositeLit 的 Type）。
func isSnapshotStaticType(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "snapshotStatic"
}
