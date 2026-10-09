// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
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
// 独立扫描（**含 `_test.go`**，不复用只扫非测试的 gateSources）遍历**整个
// ast.File**——含包级 GenDecl（`var x = snapshotStatic{...}` 也在扫）——匹配
// **全部** `snapshotStatic{...}` CompositeLit（非仅 `&snapshotStatic{}`）以及
// 全部 `*root` 解引用拷贝（root = `.static.Load()` / `&snapshotStatic{}` 绑定的
// *snapshotStatic 根；`StarExpr` 先解 `ParenExpr`/索引）。二者只允许出现在
// 构造函数 newSnapshotStatic 内（且该函数内只允许**恰好一处**
// `&snapshotStatic{...}`）——任何就地复制-改字段的写法都会绕过 key 缓存
// （planKey 在构造期一次派生并持有），必须显式改这里而非静默遗漏。
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
		offending = append(offending, scanSnapshotStaticConstructs(n, f)...)
	}
	sort.Strings(offending)
	require.Empty(t, offending,
		"snapshotStatic 只能经 newSnapshotStatic 构造/克隆（含测试）；就地复制-改字段会绕过 key 缓存。若确有豁免，必须显式登记单条 test-only 豁免并写明理由：%v", offending)
}

// TestSnapshotStaticConstructorGateRejectsBypasses 是门禁的负例自检：用字符串
// 夹具证明裸字面量（包级 / 函数内）、取址字面量、`*st`/`*(st)`/`*m[i]` 解引用
// 拷贝均被拒绝，而函数名恰为 newSnapshotStatic 且只有一处字面量时被豁免、
// 出现两处时应报错。没有这段自检，门禁路径一旦写歪就可能静默放行。
func TestSnapshotStaticConstructorGateRejectsBypasses(t *testing.T) {
	const bypasses = `package scheduler

import "github.com/is7qin/c3api/internal/domain"

var pkgLevel = snapshotStatic{acc: domain.Account{ID: 1}}
var pkgAddr = &snapshotStatic{acc: domain.Account{ID: 2}}

func bypassBare(acc domain.Account) {
	_ = snapshotStatic{acc: acc}
}

func bypassAddr(acc domain.Account) {
	_ = &snapshotStatic{acc: acc}
}

func bypassDeref(leaf *accountSnapshot) {
	st := leaf.static.Load()
	x := *st
	_ = x
}

func bypassParen(leaf *accountSnapshot) {
	st := leaf.static.Load()
	x := *(st)
	_ = x
}

func bypassIndex(m []*snapshotStatic, i int) {
	x := *m[i]
	_ = x
}

func newSnapshotStatic(acc domain.Account) *snapshotStatic {
	return &snapshotStatic{acc: acc}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", bypasses, parser.SkipObjectResolution)
	require.NoError(t, err)
	findings := scanSnapshotStaticConstructs("fixture.go", f)
	for _, want := range []string{
		"composite-literal", // pkgLevel / pkgAddr / bypassBare / bypassAddr
		"deref-copy *st",    // bypassDeref / bypassParen
		"deref-copy *m",     // bypassIndex (index base)
	} {
		require.Contains(t, strings.Join(findings, "\n"), want, "bypass %q must be flagged: %v", want, findings)
	}
	for _, fnd := range findings {
		require.NotContains(t, fnd, "newSnapshotStatic",
			"the single-literal constructor must be exempt: %v", findings)
	}
	require.Len(t, findings, 7, "exactly the seven bypass literals/derefs: %v", findings)

	const duplicateCtor = `package scheduler

import "github.com/is7qin/c3api/internal/domain"

func newSnapshotStatic(acc domain.Account) *snapshotStatic {
	a := &snapshotStatic{acc: acc}
	b := &snapshotStatic{acc: acc}
	_ = b
	return a
}
`
	fset2 := token.NewFileSet()
	f2, err := parser.ParseFile(fset2, "dup.go", duplicateCtor, parser.SkipObjectResolution)
	require.NoError(t, err)
	dupFindings := scanSnapshotStaticConstructs("dup.go", f2)
	require.Len(t, dupFindings, 1, "two literals in the constructor must be rejected: %v", dupFindings)
	require.Contains(t, dupFindings[0], "constructor-literals=2")
}

// scanSnapshotStaticConstructs returns one finding string per offending
// snapshotStatic construct in f (filename is used only for the message). It
// walks the WHOLE file: each *ast.FuncDecl body (with its tracked roots) and
// each top-level *ast.GenDecl (package scope, no roots). The unique
// constructor newSnapshotStatic is exempt only for a single `&snapshotStatic{}`.
func scanSnapshotStaticConstructs(filename string, f *ast.File) []string {
	var out []string

	inspect := func(body ast.Node, fnName string, roots map[string]bool) {
		ctorLits := 0
		ast.Inspect(body, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.CompositeLit:
				if !isSnapshotStaticType(v.Type) {
					return true
				}
				if fnName == "newSnapshotStatic" {
					ctorLits++
				} else {
					out = append(out, filename+"#"+fnName+" composite-literal")
				}
			case *ast.StarExpr:
				if fnName == "newSnapshotStatic" {
					return true
				}
				if name, ok := rootIdent(v.X); ok && roots[name] {
					out = append(out, filename+"#"+fnName+" deref-copy *"+name)
					return true
				}
				// `*m[i]` — deref of an indexed element: the base ident is not
				// a tracked *snapshotStatic root, but this is the bypass shape
				// the gate must catch (no legitimate occurrence in-package).
				if _, ok := unwrapParen(v.X).(*ast.IndexExpr); ok {
					if name, ok := rootIdent(v.X); ok {
						out = append(out, filename+"#"+fnName+" deref-copy *"+name)
					}
				}
			}
			return true
		})
		if fnName == "newSnapshotStatic" && ctorLits != 1 {
			out = append(out, fmt.Sprintf("%s#newSnapshotStatic constructor-literals=%d (want exactly 1)", filename, ctorLits))
		}
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			inspect(d.Body, d.Name.Name, snapRoots(d))
		case *ast.GenDecl:
			inspect(d, "", nil)
		}
	}
	return out
}

// isSnapshotStaticType 报告 e（先解括号）是否裸 `snapshotStatic`（CompositeLit 的 Type）。
func isSnapshotStaticType(e ast.Expr) bool {
	id, ok := unwrapParen(e).(*ast.Ident)
	return ok && id.Name == "snapshotStatic"
}

// unwrapParen 去掉表达式的括号包装。
func unwrapParen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// rootIdent 把 `name`、`(name)`、`name[i]` 解析为根标识符名字（先解括号/索引）。
func rootIdent(e ast.Expr) (string, bool) {
	for {
		switch v := unwrapParen(e).(type) {
		case *ast.IndexExpr:
			e = v.X
		case *ast.Ident:
			return v.Name, true
		default:
			return "", false
		}
	}
}
