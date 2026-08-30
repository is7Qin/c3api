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

// TestRuleHealthWiringOnce 生产接线唯一性证据（AST）：规则 typed 动作双面
// （SetHealthSink/SetPersistFunc）与 recover→PROBING 写入面（SetRecoverProber）
// 在 main 中各恰好一次——重复接线 = 后写覆盖前写的静默竞态源（sink/persistFn
// 是裸字段赋值，无 CAS）。
func TestRuleHealthWiringOnce(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	srcPath := filepath.Join(filepath.Dir(file), "main.go")
	src, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, parser.ParseComments)
	require.NoError(t, err)
	var mainFn *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			mainFn = fn
			break
		}
	}
	require.NotNil(t, mainFn, "main func not found")

	counts := map[string]int{
		"ruleEngine.SetHealthSink":  0,
		"ruleEngine.SetPersistFunc": 0,
		"svc.SetRecoverProber":      0,
	}
	ast.Inspect(mainFn, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		key := recv.Name + "." + sel.Sel.Name
		if _, want := counts[key]; want {
			counts[key]++
		}
		return true
	})
	for _, key := range []string{
		"ruleEngine.SetHealthSink",
		"ruleEngine.SetPersistFunc",
		"svc.SetRecoverProber",
	} {
		require.Equal(t, 1, counts[key], "%s 必须恰好接线一次", key)
	}
}
