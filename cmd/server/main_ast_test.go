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
	"testing"

	"github.com/stretchr/testify/require"
)

// parseSrcFile 解析与本包同目录的 Go 源文件并返回 AST——接线测试（main.go
// 装配契约）的共享脚手架，替代各测试文件内重复的 Caller+ReadFile+ParseFile
// 片段。断言语义不变：文件缺失或解析失败即 FailNow。
func parseSrcFile(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	srcPath := filepath.Join(filepath.Dir(file), name)
	src, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, parser.ParseComments)
	require.NoError(t, err)
	return fset, f
}

// parseMainGo 解析 main.go（装配根）的 AST。
func parseMainGo(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	return parseSrcFile(t, "main.go")
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

// readMainSrc 返回 main.go 原文——纯文本接线钉（如 plan-ready 门的
// Contains 断言）走此 helper，保留断言原文与意图。
func readMainSrc(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "main.go"))
	require.NoError(t, err)
	return string(src)
}
