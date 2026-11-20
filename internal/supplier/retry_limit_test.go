// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// constBasicLit 取 ValueSpec 第 i 个值：是基本字面量则返回其文本，否则 ok=false
// （隐式值重复 / 非字面量表达式都算不可判，由调用点按失败处理）。
func constBasicLit(vs *ast.ValueSpec, i int) (value string, ok bool) {
	if i >= len(vs.Values) {
		return "", false
	}
	lit, ok := vs.Values[i].(*ast.BasicLit)
	if !ok {
		return "", false
	}
	return lit.Value, true
}

// TestRetryLimitSingleSource：40P01/55P03 的重试上界只有一个来源——包级
// retryMaxAttempts（=5）；thaw 与 credit 两个 worker 各引用它至少一次，且包内
// （函数内或包级）不许再出现名为 maxAttempts 的常量副本。
//
// AST 扫包内非测试源；扫面为空同样失败——否则门禁会在扫不到东西时静默变绿。
func TestRetryLimitSingleSource(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	srcs := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "解析 %s", name)
		srcs[name] = f
	}
	require.NotEmpty(t, srcs, "扫面为空：包内没有解析到任何非测试源")

	// 包级 retryMaxAttempts 恰好 1 个，值 = 5。
	declNames := map[*ast.Ident]bool{}
	declSites := make([]string, 0, 1)
	declValue := ""
	for name, f := range srcs {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if id.Name != "retryMaxAttempts" {
						continue
					}
					declNames[id] = true
					declSites = append(declSites, fmt.Sprintf("%s:%d", name, fset.Position(id.Pos()).Line))
					declValue, _ = constBasicLit(vs, i)
				}
			}
		}
	}
	require.Len(t, declSites, 1, "包级 retryMaxAttempts 必须恰好声明一次（实际 %v）", declSites)
	require.Equal(t, "5", declValue, "retryMaxAttempts 必须仍是 5（声明于 %v）", declSites)

	// 包内不许存在任何名为 maxAttempts 的常量（函数内副本也算）。
	var stray []string
	for name, f := range srcs {
		ast.Inspect(f, func(n ast.Node) bool {
			gd, ok := n.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					if id.Name == "maxAttempts" {
						stray = append(stray, fmt.Sprintf("%s:%d", name, fset.Position(id.Pos()).Line))
					}
				}
			}
			return true
		})
	}
	sort.Strings(stray)
	require.Empty(t, stray, "不许存在名为 maxAttempts 的常量副本（应引用包级 retryMaxAttempts）：%v", stray)

	// 两个 worker 的重试循环各引用同一个符号（声明处本身不算引用）。
	refs := map[string]int{}
	for name, f := range srcs {
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != "retryMaxAttempts" || declNames[id] {
				return true
			}
			refs[name]++
			return true
		})
	}
	for _, name := range []string{"thaw.go", "credit.go"} {
		require.Positive(t, refs[name], "%s 必须引用包级 retryMaxAttempts（两处重试循环共用一个上界）", name)
	}
}
