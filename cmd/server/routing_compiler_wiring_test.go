// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

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

// TestRoutingCompilerWiring pins the Task18 production arming: main.go must
// call sched.SetWindowedQualitySource with the windowed provider
// (NewWindowedQualityProvider(qualityRecorder, ...)) and sched.SetPricesSource
// with the real pricing snapshot source (svc.ResolvedPricesByModel), BEFORE
// snapReg.ReloadAll (initial scheduler reload must fire an armed compile) and
// BEFORE wm.StartAll (the compileLoop consumer starts with the scheduler
// worker). Without a real caller the compiler stays dormant
// (compileArmed=false) — this test is the anti-dormancy gate. It also pins
// the unique scheduler snapshot registration (one publisher, one registry
// entry — the registry rejects duplicate names).
func TestRoutingCompilerWiring(t *testing.T) {
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

	var (
		setQualityPos token.Pos
		setPricesPos  token.Pos
		qualityArgOK  bool
		pricesArgOK   bool
		reloadAllPos  token.Pos
		startAllPos   token.Pos
		schedSnapLits int
	)
	ast.Inspect(mainFn, func(n ast.Node) bool {
		if cl, ok := n.(*ast.CompositeLit); ok {
			if tn, ok := cl.Type.(*ast.Ident); ok && tn.Name == "schedSnapshot" {
				schedSnapLits++
			}
			return true
		}
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		se, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := se.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case id.Name == "sched" && se.Sel.Name == "SetWindowedQualitySource":
			setQualityPos = ce.Pos()
			if len(ce.Args) == 1 {
				if inner, ok := ce.Args[0].(*ast.CallExpr); ok {
					if fun, ok := inner.Fun.(*ast.Ident); ok && fun.Name == "NewWindowedQualityProvider" && len(inner.Args) == 2 {
						if a, ok := inner.Args[0].(*ast.Ident); ok && a.Name == "qualityRecorder" {
							qualityArgOK = true
						}
					}
				}
			}
		case id.Name == "sched" && se.Sel.Name == "SetPricesSource":
			setPricesPos = ce.Pos()
			if len(ce.Args) == 1 {
				if fl, ok := ce.Args[0].(*ast.FuncLit); ok {
					ast.Inspect(fl, func(x ast.Node) bool {
						if c2, ok := x.(*ast.CallExpr); ok {
							if se2, ok := c2.Fun.(*ast.SelectorExpr); ok {
								if id2, ok := se2.X.(*ast.Ident); ok && id2.Name == "svc" && se2.Sel.Name == "ResolvedPricesByModel" {
									pricesArgOK = true
								}
							}
						}
						return true
					})
				}
			}
		case id.Name == "snapReg" && se.Sel.Name == "ReloadAll":
			reloadAllPos = ce.Pos()
		case id.Name == "wm" && se.Sel.Name == "StartAll":
			startAllPos = ce.Pos()
		}
		return true
	})

	require.True(t, setQualityPos.IsValid(), "sched.SetWindowedQualitySource(...) not found in main.go — compiler must have a real production caller")
	require.True(t, setPricesPos.IsValid(), "sched.SetPricesSource(...) not found in main.go")
	require.True(t, qualityArgOK, "quality source must be NewWindowedQualityProvider(qualityRecorder, ...)")
	require.True(t, pricesArgOK, "prices source must call svc.ResolvedPricesByModel")
	require.True(t, reloadAllPos.IsValid(), "snapReg.ReloadAll not found")
	require.True(t, startAllPos.IsValid(), "wm.StartAll not found")
	require.Less(t, int(setQualityPos), int(reloadAllPos), "SetWindowedQualitySource must precede snapReg.ReloadAll (initial reload fires the armed compile)")
	require.Less(t, int(setPricesPos), int(reloadAllPos), "SetPricesSource must precede snapReg.ReloadAll")
	require.Less(t, int(setQualityPos), int(startAllPos), "SetWindowedQualitySource must precede wm.StartAll (assembly-time contract)")
	require.Less(t, int(setPricesPos), int(startAllPos), "SetPricesSource must precede wm.StartAll")
	require.Equal(t, 1, schedSnapLits, "scheduler snapshot registered exactly once (unique publisher/registry)")
}
