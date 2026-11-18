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
	_, f := parseSrcFile(t, "wiring_run.go")
	mainFn := findFuncDecl(t, f, "buildRuntime")
	startFn := findMethodDecl(t, f, "appRuntime", "start")
	runFn := findFuncDecl(t, f, "run")

	var (
		srcPos        token.Pos
		qualityArgOK  bool
		pricesArgOK   bool
		schedWPos     token.Pos
		reloadAllPos  token.Pos
		startAllPos   token.Pos
		schedSnapLits int
	)
	// schedSnapshot 字面量的**唯一性**钉扫整包（注册点已搬进 wiring_billing.go；
	// 合并视图比"只看 main.go"更强：第二处注册写进任何文件都会红）。
	_, pkg := parsePkgSrc(t)
	ast.Inspect(pkg, func(n ast.Node) bool {
		if cl, ok := n.(*ast.CompositeLit); ok {
			if tn, ok := cl.Type.(*ast.Ident); ok && tn.Name == "schedSnapshot" {
				schedSnapLits++
			}
		}
		return true
	})
	ast.Inspect(mainFn, func(n ast.Node) bool {
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
			return true
		}
		id, ok := se.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case (id.Name == "sched" && (se.Sel.Name == "SetWindowedQualitySource" || se.Sel.Name == "SetPricesSource")):
			t.Errorf("deleted setter sched.%s still called in buildRuntime — sources must flow structurally via schedWorker", se.Sel.Name)
		}
		return true
	})

	// 启动段：快照首刷与 worker 启动在 (*appRuntime).start 里。
	ast.Inspect(startFn, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		se, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// start 里是方法接收者形态（r.snapReg.ReloadAll / r.wm.StartAll），
		// 所以取选择器的末段名而不是 Ident。
		recv := ""
		switch x := se.X.(type) {
		case *ast.Ident:
			recv = x.Name
		case *ast.SelectorExpr:
			recv = x.Sel.Name
		}
		switch {
		case recv == "snapReg" && se.Sel.Name == "ReloadAll":
			reloadAllPos = ce.Pos()
		case recv == "wm" && se.Sel.Name == "StartAll":
			startAllPos = ce.Pos()
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
	require.True(t, schedWPos.IsValid(), "schedW := schedWorker{...} not found in buildRuntime — sources hand off at Start, never via setters")
	require.Equal(t, 4, workerSlotIndex("schedW"), "schedW must hold sched's exact position in workerOrder (index 4)")
	require.Equal(t, "inv", workerOrder[workerSlotIndex("schedW")-1].name, "schedW predecessor must stay inv")
	require.Equal(t, "ruleEngine", workerOrder[workerSlotIndex("schedW")+1].name, "schedW successor must stay ruleEngine")
	require.True(t, reloadAllPos.IsValid(), "snapReg.ReloadAll not found in (*appRuntime).start")
	require.True(t, startAllPos.IsValid(), "wm.StartAll not found in (*appRuntime).start")
	// 跨函数的顺序不再靠 token 位置：buildRuntime 必须先于 start 被调用，
	// 否则「快照源在首次 Reload 之前就绪」这条契约不成立。
	buildPos, startCallPos := token.NoPos, token.NoPos
	ast.Inspect(runFn, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := ce.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "buildRuntime" {
				buildPos = ce.Pos()
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "start" {
				startCallPos = ce.Pos()
			}
		}
		return true
	})
	require.True(t, buildPos.IsValid() && startCallPos.IsValid(), "run 必须调用 buildRuntime 与 start")
	require.Less(t, int(buildPos), int(startCallPos), "buildRuntime 必须先于 start（快照源先就绪再首刷）")
	require.Equal(t, 1, schedSnapLits, "scheduler snapshot registered exactly once (unique publisher/registry)")
	// 拆分守卫：三条顺序断言的主语只允许出现在 wiring_run.go（拆开即 FailNow）。
	// 主语归属：schedSrc/schedW 只允许出现在 wiring_run.go。ReloadAll/StartAll
	// 已改成方法接收者形态（r.snapReg.ReloadAll / r.wm.StartAll），存在性由上面的
	// reloadAllPos/startAllPos 断言钉住，不再作为「点号主语」索引。
	requireSubjectsOnlyIn(t, "wiring_run.go", "schedSrc", "schedW")
}
