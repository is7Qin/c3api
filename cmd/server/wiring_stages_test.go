// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShutdownChainOrderAndNoDeferInStages 钉住装配拆阶段后**此前没有任何用例**
// 覆盖的三条结构不变量（spec §2.4.6 / §2.4.7）：
//
//  1. 停机链顺序：run() 里 5 步 == [shutdownHTTPServer, CloseAllWS,
//     waitForInflight, shutdownTail, log.Sync]，且 `<-ctx.Done()` 必须在整条链
//     **之前**（先等信号再拆）。反例：把 CloseAllWS 挪到 Shutdown 之前，或把
//     信号等待挪到链后，两条断言各红一条。
//  2. defer 不下移：**阶段构造函数**（wiring_infra / health / notify / billing /
//     codex）内零 defer，生命周期层（wiring_run.go 的 run）恰好 1 个——停机预算的
//     cancel（§2.4.7 要求 pool.Close()/stop()/cancel() 留在 main/run 同一层）。
//     §2.4.6 原文写"wiring_*.go 零 defer"，但 cancel 必须与 5 步同层，两条只在
//     "阶段函数零 defer"这一读法下可同时成立，故这里按可同时成立的口径机械化。
//  3. wm.Register 唯一性：wiring_run.go 恰 6 次，实参集合与 worker_order_test 的
//     mustBefore 表逐一对应（顺序本身由那个测试钉，这里钉"没有第四个注册点"、
//     也没有漏注册的 worker）。把某个 Register 搬到别的文件 ⇒ 次数不足,红。
func TestShutdownChainOrderAndNoDeferInStages(t *testing.T) {
	_, f := parseSrcFile(t, "wiring_run.go")
	runFn := findFuncDecl(t, f, "run")

	// 1) 5 步链：顺序在 (*appRuntime).shutdown 里；信号必须先于链。
	shutFn := findMethodDecl(t, f, "appRuntime", "shutdown")
	chain := map[string]bool{
		"shutdownHTTP": true,
		"closeWS":      true,
		"waitInflight": true,
		"tail":         true,
		"Sync":         true,
	}
	var got []string
	ast.Inspect(shutFn, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name, ok := exprName(ce.Fun)
		if !ok || !chain[name] {
			return true
		}
		got = append(got, name)
		return true
	})
	require.Equal(t,
		[]string{"shutdownHTTP", "closeWS", "waitInflight", "tail", "Sync"},
		got, "停机链顺序必须固定：Shutdown → 强断长连接 → 等在途归零 → wm 反向排空 + 尾部 → 终态日志")

	// 信号必须先于链：<-ctx.Done() 之后才允许 r.shutdown(...)。
	ctxDonePos, shutCallPos := token.NoPos, token.NoPos
	ast.Inspect(runFn, func(n ast.Node) bool {
		if ue, ok := n.(*ast.UnaryExpr); ok && ue.Op == token.ARROW {
			if ce, ok := ue.X.(*ast.CallExpr); ok {
				if se, ok := ce.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "Done" {
					if id, ok := se.X.(*ast.Ident); ok && id.Name == "ctx" && !ctxDonePos.IsValid() {
						ctxDonePos = ue.Pos()
					}
				}
			}
			return true
		}
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if se, ok := ce.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "shutdown" && !shutCallPos.IsValid() {
			shutCallPos = ce.Pos()
		}
		return true
	})
	require.True(t, ctxDonePos.IsValid(), "run() 必须在 <-ctx.Done() 上等停机信号（否则停机链无人触发）")
	require.True(t, shutCallPos.IsValid(), "run() 必须在信号后调用 r.shutdown(...)")
	require.Less(t, int(ctxDonePos), int(shutCallPos), "必须先等到 <-ctx.Done() 再动停机链（信号在前）")

	// 2) defer 只允许在生命周期层。
	for _, name := range []string{
		"wiring_infra.go",
		"wiring_health.go",
		"wiring_notify.go",
		"wiring_billing.go",
		"wiring_codex.go",
	} {
		require.Equal(t, 0, countDeferStmts(t, name), "%s 的阶段构造函数里不得出现 defer（defer 留在 main/run 同层）", name)
	}
	require.Equal(t, 1, countDeferStmts(t, "wiring_run.go"),
		"wiring_run.go 只允许 1 个 defer（停机预算 cancel）：多出来的都是下移的生命周期守卫")
	_, rf := parseSrcFile(t, "wiring_run.go")
	deferInsideRun := false
	ast.Inspect(findFuncDecl(t, rf, "run"), func(n ast.Node) bool {
		if _, ok := n.(*ast.DeferStmt); ok {
			deferInsideRun = true
		}
		return true
	})
	require.True(t, deferInsideRun, "唯一的 defer 必须在 run() 内（与停机链同层）")

	// 3) 注册点唯一：全包只允许一处 .Register(，且它消费 workerOrder 装配出来的 plan。
	// 原来的「恰 6 次 + 实参名集合」由 worker_order_table_test 的全序断言取代——
	// 全序相等比集合相等更强，且能一并封住反向遍历与截断。
	_, def := parseSrcFile(t, "wiring_run.go")
	regCalls := 0
	ast.Inspect(def, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := ce.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Register" {
			regCalls++
		}
		return true
	})
	require.Equal(t, 1, regCalls,
		"wiring_run.go 只允许一处 Register 调用（单点注册）；新增注册点必须走 workerOrder 表")
}

// countDeferStmts 数一个源文件里 defer 语句的个数（AST，不数注释/字符串）。
func countDeferStmts(t *testing.T, name string) int {
	t.Helper()
	_, f := parseSrcFile(t, name)
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		if _, ok := node.(*ast.DeferStmt); ok {
			n++
		}
		return true
	})
	return n
}

// collectRegisterArgs 按源码顺序收集 wm.Register 的调用次数与实参名：直接实参按
// 末段名取，`...(managedWorkers)` 展开成 orderedWorkers 的参数 + managedWorkers 的
// append 追加项（与 worker_order_test 的展开口径一致）。
func collectRegisterArgs(fn *ast.FuncDecl) (int, []string) {
	var ordered, appended, names []string
	count := 0
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
			lhs, lok := exprName(as.Lhs[0])
			ce, cok := as.Rhs[0].(*ast.CallExpr)
			if lok && cok && lhs == "managedWorkers" {
				if fun, ok := exprName(ce.Fun); ok {
					switch fun {
					case "orderedWorkers":
						for _, a := range ce.Args {
							if name, ok := exprName(a); ok {
								ordered = append(ordered, name)
							}
						}
					case "append":
						if len(ce.Args) < 2 {
							return true
						}
						if head, ok := exprName(ce.Args[0]); !ok || head != "managedWorkers" {
							return true
						}
						for _, a := range ce.Args[1:] {
							if name, ok := exprName(a); ok {
								appended = append(appended, name)
							}
						}
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
		if !ok || se.Sel.Name != "Register" {
			return true
		}
		if id, ok := se.X.(*ast.Ident); !ok || id.Name != "wm" {
			return true
		}
		count++
		if len(ce.Args) == 1 && ce.Ellipsis != token.NoPos {
			if name, ok := exprName(ce.Args[0]); ok && name == "managedWorkers" {
				names = append(names, ordered...)
				names = append(names, appended...)
				return true
			}
		}
		for _, a := range ce.Args {
			if name, ok := exprName(a); ok {
				names = append(names, name)
			}
		}
		return true
	})
	return count, names
}
