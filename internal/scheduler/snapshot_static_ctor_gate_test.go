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
// ast.File**——含包级 GenDecl（`var x = snapshotStatic{...}` 也在扫）——匹配：
//
//  1. **全部** `snapshotStatic{...}` CompositeLit（非仅 `&snapshotStatic{}`），
//     含**省略元素类型**的复合字面量：`[]snapshotStatic{{…}}`、
//     `map[K]snapshotStatic{k:{…}}`（内层 `CompositeLit.Type == nil`，类型来自
//     容器 `Elt`/`Value`）；
//  2. `new(snapshotStatic)`（裸构造绕过）；
//  3. `*root` 解引用拷贝（root = `.static.Load()` / `&snapshotStatic{}` 绑定的
//     `*snapshotStatic` 根；`StarExpr` 先解 `ParenExpr`/索引）——其中 `*m[i]`
//     **仅在 `m` 的元素类型确为 `*snapshotStatic` 时**才算（避免把合法
//     `*m[0]` on `[]*int` 误报）。
//
// 以上三类只允许出现在构造函数 newSnapshotStatic 内（且该函数内只允许**恰好
// 一处** `&snapshotStatic{...}`）——任何就地复制-改字段的写法都会绕过 key 缓存
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
// 夹具证明裸字面量（包级 / 函数内）、取址字面量、省略元素类型的复合字面量
// （slice/map）、`new(snapshotStatic)`、`*st`/`*(st)`/`*m[i]`（m 元素为
// `*snapshotStatic`）解引用拷贝均被拒绝；同时证明**合法**的 `*m[0]`（m 为
// `[]*int` 或 `map[K]*int`）与非目标 map 字面量**不**被误报；函数名恰为
// newSnapshotStatic 且只有一处字面量时被豁免、出现两处时应报错。没有这段
// 自检，门禁路径一旦写歪就可能静默放行或误伤。
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

func bypassElidedSlice(acc domain.Account) {
	_ = []snapshotStatic{{acc: acc}}
}

func bypassElidedMap(acc domain.Account) {
	_ = map[int64]snapshotStatic{1: {acc: acc}}
}

func bypassNew(acc domain.Account) {
	_ = new(snapshotStatic)
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

func okIndexPtr(m []*int, i int) {
	x := *m[i]
	_ = x
}

func okIndexMap(m map[int64]*int, i int64) {
	x := *m[i]
	_ = x
}

func okNonTargetMap(acc domain.Account) {
	_ = map[int64]int{1: 2}
}

func okSliceOfInts(acc domain.Account) {
	_ = []int{1, 2}
}

func newSnapshotStatic(acc domain.Account) *snapshotStatic {
	return &snapshotStatic{acc: acc}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", bypasses, parser.SkipObjectResolution)
	require.NoError(t, err)
	findings := scanSnapshotStaticConstructs("fixture.go", f)
	joined := strings.Join(findings, "\n")
	for _, want := range []string{
		"composite-literal",  // pkgLevel / pkgAddr / bypassBare / bypassAddr / elided
		"new-snapshotStatic", // bypassNew
		"deref-copy *st",     // bypassDeref / bypassParen
		"deref-copy *m",      // bypassIndex (index base)
	} {
		require.Contains(t, joined, want, "bypass %q must be flagged: %v", want, findings)
	}
	// Exactly the ten offending constructs: 4 bare/addr literals + 2 elided
	// literals + 1 new() + 2 deref roots + 1 deref index.
	require.Len(t, findings, 10, "exactly the ten bypass constructs: %v", findings)
	// The legal non-target index derefs and non-target literals must NOT appear.
	for _, bad := range []string{"okIndexPtr", "okIndexMap", "okNonTargetMap", "okSliceOfInts"} {
		require.NotContains(t, joined, bad, "non-target construct %q must not be flagged: %v", bad, findings)
	}
	for _, fnd := range findings {
		require.NotContains(t, fnd, "#newSnapshotStatic ",
			"the single-literal constructor must be exempt: %v", findings)
	}

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
// walks the WHOLE file: each *ast.FuncDecl body (with its tracked roots and
// local variable types for `*m[i]` resolution) and each top-level *ast.GenDecl
// (package scope, no roots). The unique constructor newSnapshotStatic is
// exempt only for a single `&snapshotStatic{}`.
func scanSnapshotStaticConstructs(filename string, f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			c := &scanCtx{filename: filename, fnName: d.Name.Name, roots: snapRoots(d), vars: collectVarTypes(d), out: &out}
			c.visit(d.Body, nil)
			if c.fnName == "newSnapshotStatic" && c.ctorLits != 1 {
				out = append(out, fmt.Sprintf("%s#newSnapshotStatic constructor-literals=%d (want exactly 1)", filename, c.ctorLits))
			}
		case *ast.GenDecl:
			c := &scanCtx{filename: filename, vars: collectPkgVarTypes(f), out: &out}
			c.visit(d, nil)
		}
	}
	return out
}

type scanCtx struct {
	filename string
	fnName   string // "" for package scope
	roots    map[string]bool
	vars     map[string]ast.Expr
	out      *[]string
	ctorLits int
}

func (c *scanCtx) report(kind string) {
	*c.out = append(*c.out, c.filename+"#"+c.fnName+" "+kind)
}

// visit descends n carrying `expected` — the type implied by the enclosing
// container for an elided composite literal (nil when none). Elements of an
// ArrayType/SliceType inherit Elt; MapType elements inherit Value (keys Key).
func (c *scanCtx) visit(n ast.Node, expected ast.Expr) {
	if n == nil {
		return
	}
	switch v := n.(type) {
	case *ast.CompositeLit:
		t := v.Type
		if t == nil {
			t = expected
		}
		eff := normalizeLitType(t)
		if isSnapshotStaticType(eff) {
			if c.fnName == "newSnapshotStatic" {
				c.ctorLits++
			} else {
				c.report("composite-literal")
			}
		}
		elt := litEltType(eff)
		key := litKeyType(eff)
		for _, e := range v.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				c.visit(kv.Key, key)
				c.visit(kv.Value, elt)
			} else {
				c.visit(e, elt)
			}
		}
		return
	case *ast.StarExpr:
		if c.fnName != "newSnapshotStatic" {
			if name, ok := c.derefTargetName(v.X); ok {
				c.report("deref-copy *" + name)
			}
		}
		c.visit(v.X, nil)
		return
	case *ast.CallExpr:
		// new(snapshotStatic) — a raw constructor bypass (reported even inside
		// the constructor: new() is never the sanctioned `&snapshotStatic{}`).
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 && isSnapshotStaticType(v.Args[0]) {
			c.report("new-snapshotStatic")
			return
		}
	}
	// Generic descent over the immediate children only (no expected type leaks).
	ast.Inspect(n, func(child ast.Node) bool {
		if child == nil {
			return false
		}
		if child == n {
			return true
		}
		c.visit(child, nil)
		return false
	})
}

// derefTargetName reports the identifier a `*x` deref targets when it is a
// known *snapshotStatic root, OR `x` is an index expression whose base element
// type is `*snapshotStatic` (`*m[i]`). A plain `*m[0]` on `[]*int` (or a
// non-target map) is NOT a bypass and must not be reported.
func (c *scanCtx) derefTargetName(x ast.Expr) (string, bool) {
	if _, ok := unwrapParen(x).(*ast.IndexExpr); ok {
		base, ok := rootIdent(x)
		if !ok {
			return "", false
		}
		if c.roots[base] || isSnapshotStaticPtrElem(c.vars, base) {
			return base, true
		}
		return "", false
	}
	if name, ok := rootIdent(x); ok && c.roots[name] {
		return name, true
	}
	return "", false
}

// normalizeLitType strips a pointer wrapper from a composite literal's type:
// an elided `{...}` in a `[]*T` element slot has effective type T, so
// `*snapshotStatic` normalizes to `snapshotStatic` for detection.
func normalizeLitType(t ast.Expr) ast.Expr {
	if st, ok := unwrapParen(t).(*ast.StarExpr); ok {
		return st.X
	}
	return t
}

// litEltType returns the element type implied by a container literal type
// (ArrayType/SliceType → Elt, MapType → Value), or nil.
func litEltType(t ast.Expr) ast.Expr {
	switch v := unwrapParen(t).(type) {
	case *ast.ArrayType:
		return v.Elt
	case *ast.MapType:
		return v.Value
	}
	return nil
}

// litKeyType returns the map key type of a MapType literal (nil otherwise).
func litKeyType(t ast.Expr) ast.Expr {
	if v, ok := unwrapParen(t).(*ast.MapType); ok {
		return v.Key
	}
	return nil
}

// isSnapshotStaticPtrElem reports whether `name`'s declared type is a slice of
// `*snapshotStatic` or a map whose value is `*snapshotStatic`.
func isSnapshotStaticPtrElem(vars map[string]ast.Expr, name string) bool {
	switch v := unwrapParen(vars[name]).(type) {
	case *ast.ArrayType:
		return isStarSnapshotStatic(v.Elt)
	case *ast.MapType:
		return isStarSnapshotStatic(v.Value)
	}
	return false
}

func isStarSnapshotStatic(e ast.Expr) bool {
	st, ok := unwrapParen(e).(*ast.StarExpr)
	return ok && isSnapshotStaticType(st.X)
}

// collectVarTypes maps identifier → declared type within a function (receiver,
// params, results, local var decls and typed short declarations). Only used to
// resolve the element type of `*m[i]`; an unknown identifier simply never
// matches.
func collectVarTypes(fn *ast.FuncDecl) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	collectFieldListTypes(fn.Recv, out)
	if fn.Type != nil {
		collectFieldListTypes(fn.Type.Params, out)
		collectFieldListTypes(fn.Type.Results, out)
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.DeclStmt:
			gd, ok := s.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					for _, nm := range vs.Names {
						out[nm.Name] = vs.Type
					}
					continue
				}
				for i, nm := range vs.Names {
					if i < len(vs.Values) {
						if t := typeFromValue(vs.Values[i]); t != nil {
							out[nm.Name] = t
						}
					}
				}
			}
		case *ast.AssignStmt:
			if s.Tok != token.DEFINE {
				return true
			}
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				if i < len(s.Rhs) {
					if t := typeFromValue(s.Rhs[i]); t != nil {
						out[id.Name] = t
					}
				}
			}
		}
		return true
	})
	return out
}

// collectPkgVarTypes maps package-level var identifiers → declared type.
func collectPkgVarTypes(f *ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || vs.Type == nil {
				continue
			}
			for _, nm := range vs.Names {
				out[nm.Name] = vs.Type
			}
		}
	}
	return out
}

func collectFieldListTypes(fl *ast.FieldList, out map[string]ast.Expr) {
	if fl == nil {
		return
	}
	for _, fld := range fl.List {
		for _, nm := range fld.Names {
			out[nm.Name] = fld.Type
		}
	}
}

// typeFromValue derives a declared type from a short-declaration RHS:
// `x := T{...}` / `x := make(T, ...)` / `x := new(T)` / `x := &T{...}`.
func typeFromValue(e ast.Expr) ast.Expr {
	switch v := unwrapParen(e).(type) {
	case *ast.CompositeLit:
		return v.Type
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return typeFromValue(v.X)
		}
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		if !ok {
			return nil
		}
		switch id.Name {
		case "make":
			if len(v.Args) > 0 {
				return v.Args[0]
			}
		case "new":
			if len(v.Args) == 1 {
				return &ast.StarExpr{X: v.Args[0]}
			}
		}
	}
	return nil
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
