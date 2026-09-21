// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package main

import (
	"go/ast"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRoutingCompilerWiring pins the structural arming (setters
// deleted): main.go must build schedSrc := &scheduler.CompilerSources with
// the windowed provider (NewWindowedQualityProvider(qualityRecorder, ...))
// and the real pricing snapshot source (svc.ResolvedPricesByModel), hand it
// to schedW := schedWorker{...}, and register schedW in orderedWorkers at
// the exact position sched held (reverse-drain semantics) — all BEFORE
// snapReg.ReloadAll and wm.StartAll. Without a real source the compiler
// stays dormant (sources==nil → RequestCompile no-op) — this test is the
// anti-dormancy gate. It also pins the absence of the deleted setters and
// the unique scheduler snapshot registration (one publisher, one registry
// entry — the registry rejects duplicate names).
func TestRoutingCompilerWiring(t *testing.T) {
	_, f := parseMainGo(t)
	mainFn := findFuncDecl(t, f, "main")

	var (
		srcPos        token.Pos
		qualityArgOK  bool
		pricesArgOK   bool
		schedWPos     token.Pos
		schedWIdx     = -1
		schedWPrev    string
		schedWNext    string
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
		// schedW := schedWorker{...} pins the Start-time handoff adapter.
		if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
			if lhs, ok := as.Lhs[0].(*ast.Ident); ok && lhs.Name == "schedW" {
				if cl, ok := as.Rhs[0].(*ast.CompositeLit); ok {
					if tn, ok := cl.Type.(*ast.Ident); ok && tn.Name == "schedWorker" {
						schedWPos = as.Pos()
					}
				}
			}
			return true
		}
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		se, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			// orderedWorkers(mailW, warningWorker, billingWorker, inv, schedW, ...)
			if fun, ok := ce.Fun.(*ast.Ident); ok && fun.Name == "orderedWorkers" {
				for i, a := range ce.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == "schedW" {
						schedWIdx = i
						if i > 0 {
							if prev, ok := ce.Args[i-1].(*ast.Ident); ok {
								schedWPrev = prev.Name
							}
						}
						if i+1 < len(ce.Args) {
							if next, ok := ce.Args[i+1].(*ast.Ident); ok {
								schedWNext = next.Name
							}
						}
					}
				}
			}
			return true
		}
		id, ok := se.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case id.Name == "snapReg" && se.Sel.Name == "ReloadAll":
			reloadAllPos = ce.Pos()
		case id.Name == "wm" && se.Sel.Name == "StartAll":
			startAllPos = ce.Pos()
		case (id.Name == "sched" && (se.Sel.Name == "SetWindowedQualitySource" || se.Sel.Name == "SetPricesSource")):
			t.Errorf("deleted setter sched.%s still called in main.go — sources must flow structurally via schedWorker", se.Sel.Name)
		}
		return true
	})

	// schedSrc composite literal: walk assignments separately (the &UnaryExpr
	// sits in an AssignStmt, not a CallExpr).
	ast.Inspect(mainFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "schedSrc" {
			return true
		}
		ue, ok := as.Rhs[0].(*ast.UnaryExpr)
		if !ok {
			return true
		}
		cl, ok := ue.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		se, ok := cl.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := se.X.(*ast.Ident); !ok || id.Name != "scheduler" || se.Sel.Name != "CompilerSources" {
			return true
		}
		srcPos = as.Pos()
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Quality":
				if inner, ok := kv.Value.(*ast.CallExpr); ok {
					if fun, ok := inner.Fun.(*ast.Ident); ok && fun.Name == "NewWindowedQualityProvider" && len(inner.Args) == 2 {
						if a, ok := inner.Args[0].(*ast.Ident); ok && a.Name == "qualityRecorder" {
							qualityArgOK = true
						}
					}
				}
			case "Prices":
				if fl, ok := kv.Value.(*ast.FuncLit); ok {
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
		}
		return true
	})

	require.True(t, srcPos.IsValid(), "schedSrc := &scheduler.CompilerSources{...} not found in main.go — compiler must have a real production source")
	require.True(t, qualityArgOK, "quality source must be NewWindowedQualityProvider(qualityRecorder, ...)")
	require.True(t, pricesArgOK, "prices source must call svc.ResolvedPricesByModel")
	require.True(t, schedWPos.IsValid(), "schedW := schedWorker{...} not found in main.go — sources hand off at Start, never via setters")
	require.Equal(t, 4, schedWIdx, "schedW must hold sched's exact position in orderedWorkers (index 4)")
	require.Equal(t, "inv", schedWPrev, "schedW predecessor must stay inv")
	require.Equal(t, "ruleEngine", schedWNext, "schedW successor must stay ruleEngine")
	require.True(t, reloadAllPos.IsValid(), "snapReg.ReloadAll not found")
	require.True(t, startAllPos.IsValid(), "wm.StartAll not found")
	require.Less(t, int(srcPos), int(reloadAllPos), "schedSrc must precede snapReg.ReloadAll (sources assembled before first reload)")
	require.Less(t, int(schedWPos), int(reloadAllPos), "schedW must precede snapReg.ReloadAll")
	require.Less(t, int(srcPos), int(startAllPos), "schedSrc must precede wm.StartAll (Start-time handoff contract)")
	require.Less(t, int(schedWPos), int(startAllPos), "schedW must precede wm.StartAll")
	require.Equal(t, 1, schedSnapLits, "scheduler snapshot registered exactly once (unique publisher/registry)")
}
