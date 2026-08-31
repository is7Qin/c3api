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
// call sched.SetCompilerSources with the real quality lane source
// (compilerQualitySource(qualityRecorder)) and the real pricing snapshot
// source (svc.ResolvedPricesByModel), BEFORE snapReg.ReloadAll (initial
// scheduler reload must fire an armed compile) and BEFORE wm.StartAll (the
// compileLoop consumer starts with the scheduler worker). Without a real
// caller the compiler stays dormant (compileArmed=false) — this test is the
// anti-dormancy gate. It also pins the unique scheduler snapshot registration
// (one publisher, one registry entry — the registry rejects duplicate names).
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
		setSourcesPos token.Pos
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
		case id.Name == "sched" && se.Sel.Name == "SetCompilerSources":
			setSourcesPos = ce.Pos()
			if len(ce.Args) == 2 {
				if inner, ok := ce.Args[0].(*ast.CallExpr); ok {
					if fun, ok := inner.Fun.(*ast.Ident); ok && fun.Name == "compilerQualitySource" && len(inner.Args) == 1 {
						if a, ok := inner.Args[0].(*ast.Ident); ok && a.Name == "qualityRecorder" {
							qualityArgOK = true
						}
					}
				}
				if fl, ok := ce.Args[1].(*ast.FuncLit); ok {
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

	require.True(t, setSourcesPos.IsValid(), "sched.SetCompilerSources(...) not found in main.go — compiler must have a real production caller")
	require.True(t, qualityArgOK, "quality source must be compilerQualitySource(qualityRecorder)")
	require.True(t, pricesArgOK, "prices source must call svc.ResolvedPricesByModel")
	require.True(t, reloadAllPos.IsValid(), "snapReg.ReloadAll not found")
	require.True(t, startAllPos.IsValid(), "wm.StartAll not found")
	require.Less(t, int(setSourcesPos), int(reloadAllPos), "SetCompilerSources must precede snapReg.ReloadAll (initial reload fires the armed compile)")
	require.Less(t, int(setSourcesPos), int(startAllPos), "SetCompilerSources must precede wm.StartAll (assembly-time contract)")
	require.Equal(t, 1, schedSnapLits, "scheduler snapshot registered exactly once (unique publisher/registry)")
}
