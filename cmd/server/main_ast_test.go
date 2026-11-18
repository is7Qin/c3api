// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件的定位：装配契约测试的脚手架，不是被搬迁的钉本身。装配按阶段拆成
// main.go + wiring_*.go 之后，两个新问题必须在这里一次性解决：
//
//	(1) 缺席钉/唯一性钉：**整包合并视图**（parsePkgSrc）——"main.go 里没有"不再
//	    等价于"包里没有"，回填 setter 写进任一 wiring_*.go 都会被看见；
//	(2) 存在钉/顺序钉：**指名单文件**（parseSrcFile + findFuncDecl）——并且由
//	    requireSubjectsOnlyIn 机械地禁止"主语被拆到两个文件"（拆开即 FailNow，
//	     不许降级成"某一文件里找不到就算了"）。

// pkgFileNames 列出 cmd/server 全部**非测试** `.go` 文件名（排序 ⇒ 稳定）。
func pkgFileNames(t *testing.T) []string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	entries, err := os.ReadDir(filepath.Dir(self))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	require.NotEmpty(t, names, "cmd/server 非测试源文件为空：扫面在空转")
	return names
}

// parseSrcFile 解析与本包同目录的 Go 源文件并返回 AST——接线测试（装配契约）
// 的共享脚手架，替代各测试文件内重复的 Caller+ReadFile+ParseFile 片段。
// 断言语义不变：文件缺失或解析失败即 FailNow。
func parseSrcFile(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return parseSrcFileIn(t, filepath.Dir(file), name)
}

// parseSrcFileIn 解析 dir 下的 name（供合并视图复用，避免重复 Caller 样板）。
func parseSrcFileIn(t *testing.T, dir, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	srcPath := filepath.Join(dir, name)
	src, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, parser.ParseComments)
	require.NoError(t, err)
	return fset, f
}

// parsePkgSrc 解析 cmd/server **全部非测试 .go** 的合并视图（Decls 并集 +
// Imports 并集，文件名排序 ⇒ 稳定）。
//
// 缺席钉（"某 setter 必须 0 次"）与唯一性钉（"某字面量恰 1 次"）在合并视图上
// 求值：装配按阶段拆文件之后，"main.go 里没有"不再等价于"包里没有"——回填
// setter 只要写进任一 wiring_*.go 就会在这里被看见（比只看单文件更强）。
func parsePkgSrc(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(self)
	names := pkgFileNames(t)

	fset := token.NewFileSet()
	merged := &ast.File{Name: ast.NewIdent("main")}
	for _, name := range names {
		src, readErr := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, readErr)
		f, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), src, parser.ParseComments)
		require.NoError(t, parseErr)
		merged.Decls = append(merged.Decls, f.Decls...)
		merged.Imports = append(merged.Imports, f.Imports...)
	}
	return fset, merged
}

// 本守卫只索引赋值与调用；纯函数值引用（把符号当参数传递、取方法值）不触发。
// 需要覆盖那类引用时用精确 AST matcher（Ident|SelectorExpr），不要以为守卫默认全覆盖。
//
// pkgSubjectFiles 建"主语 → 出现过的非测试文件（排序去重）"索引。主语口径：
//   - 赋值/`var` 声明的 LHS 末段名（`schedSrc := …` / `var warningWorker …`）；
//   - 调用名（`latch.NewLatchStore` 取 `recv.Sel`；裸 `Ident` 取本身）。
func pkgSubjectFiles(t *testing.T) map[string][]string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(self)
	index := make(map[string]map[string]bool)
	add := func(subject, file string) {
		if subject == "" {
			return
		}
		if index[subject] == nil {
			index[subject] = make(map[string]bool, 1)
		}
		index[subject][file] = true
	}
	for _, name := range pkgFileNames(t) {
		_, f := parseSrcFileIn(t, dir, name)
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for _, id := range x.Names {
					add(id.Name, name)
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if subject, ok := exprName(lhs); ok {
						add(subject, name)
					}
				}
			case *ast.CallExpr:
				subject, ok := callKey(x)
				if !ok {
					return true
				}
				add(subject, name)
			}
			return true
		})
	}
	out := make(map[string][]string, len(index))
	for subject, files := range index {
		list := make([]string, 0, len(files))
		for file := range files {
			list = append(list, file)
		}
		sort.Strings(list)
		out[subject] = list
	}
	return out
}

// callKey 把调用表达式还原成"主语键"：`pkg.New()` → `pkg.New`，`x.M()` → `x.M`，
// `f()` → `f`。其它形态（字段调用链、函数字面量）返回 false。
func callKey(ce *ast.CallExpr) (string, bool) {
	switch fun := ce.Fun.(type) {
	case *ast.Ident:
		return fun.Name, true
	case *ast.SelectorExpr:
		recv, ok := fun.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		return recv.Name + "." + fun.Sel.Name, true
	}
	return "", false
}

// requireSubjectsOnlyIn 断言这些"顺序钉/存在钉的主语"**只**出现在 owner 这一个
// 非测试文件里。拆分是拆阶段后最隐蔽的退化：主语还在、但搬到了另一个文件，旧的
// 单文件测试只会报"没找到"，看不出是拆分还是删除。这里直接 FailNow 并打印实际
// 文件集合（momus M3：「主语被拆开即停，不许降级」）。
func requireSubjectsOnlyIn(t *testing.T, owner string, subjects ...string) {
	t.Helper()
	index := pkgSubjectFiles(t)
	for _, subject := range subjects {
		files := index[subject]
		if len(files) == 0 {
			require.FailNow(t, "pin subject missing",
				"%s 在 cmd/server 的非测试文件里完全不存在（顺序/存在钉的主语被删）", subject)
		}
		for _, file := range files {
			if file != owner {
				require.FailNow(t, "pin subjects split across files",
					"%s 出现在 %v，但该钉只认 %s：主语被拆开即返工（不许降级成「某一文件里找不到就算了」）",
					subject, files, owner)
			}
		}
	}
}

// findMethodDecl 在文件内定位带接收者的方法；缺失即 FailNow。
//
// findFuncDecl 只认 fn.Recv == nil，所以 (*runtime).shutdown / (*runtime).start
// 这类目标必须走本函数——否则「把解析目标改成方法」的那几行门在第一步就会
// FailNow，实施者最省事的解法是把门删掉（静默降级）。
func findMethodDecl(t *testing.T, f *ast.File, recv, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != name || len(fn.Recv.List) != 1 {
			continue
		}
		if recvTypeName(fn.Recv.List[0].Type) == recv {
			return fn
		}
	}
	require.FailNow(t, "method not found", "expected method (%s).%s", recv, name)
	return nil
}

// recvTypeName 剥掉 *T / T[P] 外壳，取类型名。
func recvTypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return recvTypeName(x.X)
	case *ast.IndexExpr:
		return recvTypeName(x.X)
	}
	return ""
}

// findFuncDecl 在文件内定位顶层无接收者函数；缺失即 FailNow（原各测试的
// "main func not found" / "shutdownTail func not found" 断言收敛于此）。
func findFuncDecl(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Recv == nil {
			return fn
		}
	}
	require.FailNow(t, "func not found", "expected top-level func %s", name)
	return nil
}

// exprName 取表达式的**末段**名：`Ident` 与 `f.field` / `pkg.Name` 一视同仁。
// 装配拆阶段后"符号名不变、前缀多了阶段名"仍是同一主语，钉子的约束逐字不变。
func exprName(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name, true
	case *ast.SelectorExpr:
		return v.Sel.Name, true
	}
	return "", false
}

// readSrcFile 返回与本包同目录的 name 原文——纯文本接线钉走此 helper，
// 保留断言原文与意图。
func readSrcFile(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), name))
	require.NoError(t, err)
	return string(src)
}
