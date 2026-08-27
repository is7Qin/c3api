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

func isWmRegister(ce *ast.CallExpr) bool {
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Register" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "wm"
}

func TestWorkerRegistrationPartialOrder(t *testing.T) {
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
	var order []string
	guarded := false
	var walk func([]ast.Stmt, bool)
	walk = func(list []ast.Stmt, billGuard bool) {
		for _, s := range list {
			switch x := s.(type) {
			case *ast.ExprStmt:
				ce, ok := x.X.(*ast.CallExpr)
				if !ok || !isWmRegister(ce) {
					continue
				}
				for _, a := range ce.Args {
					if id, ok := a.(*ast.Ident); ok {
						order = append(order, id.Name)
						if id.Name == "billFlusher" && billGuard {
							guarded = true
						}
					}
				}
			case *ast.IfStmt:
				isGuard := false
				if bin, ok := x.Cond.(*ast.BinaryExpr); ok && bin.Op.String() == "!=" {
					if l, ok := bin.X.(*ast.Ident); ok && l.Name == "billFlusher" {
						if r, ok := bin.Y.(*ast.Ident); ok && r.Name == "nil" {
							isGuard = true
						}
					}
				}
				walk(x.Body.List, billGuard || isGuard)
			}
		}
	}
	walk(mainFn.Body.List, false)
	require.True(t, guarded, "billFlusher must be under if billFlusher != nil")
	pos := make(map[string]int, len(order))
	for i, n := range order {
		_, dup := pos[n]
		require.False(t, dup, "worker %s appears more than once: %v", n, order)
		pos[n] = i
	}
	mustBefore := func(a, b string) {
		pa, okA := pos[a]
		pb, okB := pos[b]
		require.True(t, okA, "worker %s not found in %v", a, order)
		require.True(t, okB, "worker %s not found in %v", b, order)
		require.Less(t, pa, pb, "expected %s before %s, got %v", a, b, order)
	}
	mustBefore("billFlusher", "rec")
	mustBefore("rec", "errlogW")
	business := []string{"billFlusher", "inv", "sched", "ruleEngine", "rec", "errlogW", "pricingSync", "retention", "statsAgg", "mailW", "concSync", "accConcSync"}
	for _, b := range business {
		mustBefore(b, "disco")
	}
	for _, b := range append(business, "disco") {
		mustBefore(b, "listener")
		mustBefore(b, "authSync")
	}
	lastTwo := map[string]bool{order[len(order)-2]: true, order[len(order)-1]: true}
	require.True(t, lastTwo["listener"] && lastTwo["authSync"], "final two must be listener and authSync, got %v", order)
}
