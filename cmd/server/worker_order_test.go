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
	var ordered []string
	managedFound := false
	expansionFound := false
	guardedBilling := false
	guardedWarning := false

	isBillGuard := func(cond ast.Expr) bool {
		bin, ok := cond.(*ast.BinaryExpr)
		if !ok || bin.Op.String() != "!=" {
			return false
		}
		l, ok := bin.X.(*ast.Ident)
		if !ok || l.Name != "billFlusher" {
			return false
		}
		r, ok := bin.Y.(*ast.Ident)
		return ok && r.Name == "nil"
	}
	isWarningGuard := func(cond ast.Expr) bool {
		bin, ok := cond.(*ast.BinaryExpr)
		if !ok || bin.Op.String() != "!=" {
			return false
		}
		l, ok := bin.X.(*ast.Ident)
		if !ok || l.Name != "warningW" {
			return false
		}
		r, ok := bin.Y.(*ast.Ident)
		return ok && r.Name == "nil"
	}

	var walk func([]ast.Stmt)
	walk = func(list []ast.Stmt) {
		for _, s := range list {
			switch x := s.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					if lhs, ok := x.Lhs[0].(*ast.Ident); ok && lhs.Name == "managedWorkers" {
						if ce, ok := x.Rhs[0].(*ast.CallExpr); ok {
							if fun, ok := ce.Fun.(*ast.Ident); ok && fun.Name == "orderedWorkers" {
								managedFound = true
								ordered = ordered[:0]
								for _, a := range ce.Args {
									id, ok := a.(*ast.Ident)
									require.True(t, ok, "orderedWorkers arg must be ident, got %T", a)
									ordered = append(ordered, id.Name)
								}
							}
						}
					}
				}
			case *ast.ExprStmt:
				ce, ok := x.X.(*ast.CallExpr)
				if !ok || !isWmRegister(ce) {
					continue
				}
				if len(ce.Args) == 1 && ce.Ellipsis != token.NoPos {
					if id, ok := ce.Args[0].(*ast.Ident); ok && id.Name == "managedWorkers" {
						require.True(t, managedFound, "wm.Register(managedWorkers...) without prior managedWorkers := orderedWorkers(...)")
						require.NotEmpty(t, ordered, "orderedWorkers args empty")
						order = append(order, ordered...)
						expansionFound = true
						continue
					}
				}
				for _, a := range ce.Args {
					if id, ok := a.(*ast.Ident); ok {
						order = append(order, id.Name)
					}
				}
			case *ast.IfStmt:
				billGuard := isBillGuard(x.Cond)
				warnGuard := isWarningGuard(x.Cond)
				if billGuard || warnGuard {
					for _, st := range x.Body.List {
						if as, ok := st.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
							if lhs, ok := as.Lhs[0].(*ast.Ident); ok {
								if rhs, ok := as.Rhs[0].(*ast.Ident); ok {
									if billGuard && lhs.Name == "billingWorker" && rhs.Name == "billFlusher" {
										guardedBilling = true
									}
									if warnGuard && lhs.Name == "warningWorker" && rhs.Name == "warningW" {
										guardedWarning = true
									}
								}
							}
						}
					}
				}
				walk(x.Body.List)
				if x.Else != nil {
					if blk, ok := x.Else.(*ast.BlockStmt); ok {
						walk(blk.List)
					}
				}
			}
		}
	}
	walk(mainFn.Body.List)
	require.True(t, managedFound, "managedWorkers := orderedWorkers(...) not found")
	require.True(t, expansionFound, "wm.Register(managedWorkers...) expansion not found")
	require.True(t, guardedBilling, "billingWorker = billFlusher must be under if billFlusher != nil")
	require.True(t, guardedWarning, "warningWorker = warningW must be under if warningW != nil")
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
	mustBefore("mailW", "warningWorker")
	mustBefore("warningWorker", "billingWorker")
	mustBefore("billingWorker", "rec")
	mustBefore("rec", "errlogW")
	business := []string{"mailW", "warningWorker", "billingWorker", "inv", "sched", "ruleEngine", "rec", "errlogW", "pricingSync", "retention", "statsAgg", "concSync", "accConcSync"}
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
