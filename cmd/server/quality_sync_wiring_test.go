// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQualitySyncWiring pins the quality-sync lane assembly (Task 9):
// recorder → SyncWorker(qualityRecorder, rdb, repos.Partitions) → worker
// lifecycle (managedWorkers) + ops visibility, with shutdown ordering
// worker drain → recorder finalization → Redis client close.
func TestQualitySyncWiring(t *testing.T) {
	_, f := parseMainGo(t)
	mainFn := findFuncDecl(t, f, "main")

	isCall := func(n ast.Node, recv, sel string) (*ast.CallExpr, bool) {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return nil, false
		}
		se, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok || se.Sel.Name != sel {
			return nil, false
		}
		id, ok := se.X.(*ast.Ident)
		if !ok || id.Name != recv {
			return nil, false
		}
		return ce, true
	}

	var (
		recorderCtor bool // qualityRecorder, err := quality.NewRecorder(...)
		recorderSet  bool // proxy.Deps{Recorder: qualityRecorder} wired into proxy.New(...)
		syncCtor     bool // qualitySync := quality.NewSyncWorker(qualityRecorder, rdb, repos.Partitions, ...)
		syncNotify   bool // ctor 尾参 = sched.RequestCompile（缺陷 B 事件驱动编译，构造器注入、无回填）
		inManaged    bool // qualitySync ∈ orderedWorkers(...) args
		managedFound bool
		tailCalled   bool // main delegates the shutdown tail: shutdownTail(...)
		seq          []string
	)
	appendSeq := func(m string) { seq = append(seq, m) }

	// managedWorkers := orderedWorkers(...) — collect ident args.
	var walkOrdered func(n ast.Node) bool
	walkOrdered = func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "managedWorkers" {
			return true
		}
		ce, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		fun, ok := ce.Fun.(*ast.Ident)
		if !ok || fun.Name != "orderedWorkers" {
			return true
		}
		managedFound = true
		for _, a := range ce.Args {
			if id, ok := a.(*ast.Ident); ok && id.Name == "qualitySync" {
				inManaged = true
			}
		}
		return true
	}

	ast.Inspect(mainFn, func(n ast.Node) bool {
		walkOrdered(n)
		if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) >= 1 && len(as.Rhs) == 1 {
			if ce, ok := as.Rhs[0].(*ast.CallExpr); ok {
				if se, ok := ce.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "NewRecorder" {
					if x, ok := se.X.(*ast.Ident); ok && x.Name == "quality" {
						for _, l := range as.Lhs {
							if id, ok := l.(*ast.Ident); ok && id.Name == "qualityRecorder" {
								recorderCtor = true
							}
						}
					}
				}
			}
		}
		if cl, ok := n.(*ast.CompositeLit); ok {
			if se, ok := cl.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "Deps" {
				if x, ok := se.X.(*ast.Ident); ok && x.Name == "proxy" {
					for _, e := range cl.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Recorder" {
								if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "qualityRecorder" {
									recorderSet = true
								}
							}
						}
					}
				}
			}
		}
		if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
			if lhs, ok := as.Lhs[0].(*ast.Ident); ok && lhs.Name == "qualitySync" {
				if ce, ok := as.Rhs[0].(*ast.CallExpr); ok {
					if se, ok := ce.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "NewSyncWorker" {
						if x, ok := se.X.(*ast.Ident); ok && x.Name == "quality" && len(ce.Args) >= 4 {
							a0, ok0 := ce.Args[0].(*ast.Ident)
							a1, ok1 := ce.Args[1].(*ast.Ident)
							a2, ok2 := ce.Args[2].(*ast.SelectorExpr)
							partitions := ok2 && a2.X.(*ast.Ident) != nil && a2.X.(*ast.Ident).Name == "repos" && a2.Sel.Name == "Partitions"
							if ok0 && a0.Name == "qualityRecorder" && ok1 && a1.Name == "rdb" && partitions {
								syncCtor = true
							}
							// 缺陷 B 接线必须走构造器尾参（回填已删）：第 6 参 == sched.RequestCompile。
							if len(ce.Args) == 6 {
								if sel, ok := ce.Args[5].(*ast.SelectorExpr); ok && sel.Sel.Name == "RequestCompile" {
									if id, ok := sel.X.(*ast.Ident); ok && id.Name == "sched" {
										syncNotify = true
									}
								}
							}
						}
					}
				}
			}
		}
		if ce, ok := n.(*ast.CallExpr); ok {
			if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == "shutdownTail" {
				tailCalled = true
			}
		}
		return true
	})

	// 停机尾部（shutdown.go）：三步顺序调用收敛在独立函数内——排空失败在
	// 该函数内显式 Error 上报、不宣称 clean shutdown（行为锚定见
	// shutdown_tail_test.go）；此处锚定调用结构与顺序。
	_, fTail := parseSrcFile(t, "shutdown.go")
	tailFn := findFuncDecl(t, fTail, "shutdownTail")
	ast.Inspect(tailFn, func(n ast.Node) bool {
		if _, ok := isCall(n, "qualityRecorder", "CloseWithContext"); ok {
			appendSeq("recorder.close")
		}
		if _, ok := isCall(n, "wm", "Shutdown"); ok {
			appendSeq("wm.shutdown")
		}
		if _, ok := isCall(n, "redisx", "Close"); ok {
			appendSeq("redis.close")
		}
		return true
	})

	require.True(t, recorderCtor, "qualityRecorder := quality.NewRecorder(...) not found")
	require.True(t, recorderSet, "proxy.Deps{Recorder: qualityRecorder} not found (must be wired via constructor, no setters)")
	require.True(t, syncCtor, "qualitySync := quality.NewSyncWorker(qualityRecorder, rdb, repos.Partitions, ...) not found")
	require.True(t, syncNotify, "quality.NewSyncWorker(..., sched.RequestCompile) not found (must be injected via constructor, no setters)")
	require.True(t, managedFound, "managedWorkers := orderedWorkers(...) not found")
	require.True(t, inManaged, "qualitySync must be registered via orderedWorkers(...) for lifecycle + ops stats")
	require.True(t, tailCalled, "main must delegate the shutdown tail to shutdownTail(...)")

	// Shutdown ordering: worker manager drain (quality-sync Close flushes and,
	// on flush failure, refills the recorder — it must still be open) →
	// recorder close (finalization: no further enqueue) → Redis client close
	// (last, drain commands run on the live pool).
	recClose := -1
	wmShutdown := -1
	redisClose := -1
	for i, m := range seq {
		switch m {
		case "recorder.close":
			if recClose < 0 {
				recClose = i
			}
		case "wm.shutdown":
			if wmShutdown < 0 {
				wmShutdown = i
			}
		case "redis.close":
			if redisClose < 0 {
				redisClose = i
			}
		}
	}
	require.GreaterOrEqual(t, recClose, 0, "qualityRecorder.CloseWithContext not found")
	require.GreaterOrEqual(t, wmShutdown, 0, "wm.Shutdown not found")
	require.GreaterOrEqual(t, redisClose, 0, "redisx.Close not found")
	require.Less(t, wmShutdown, recClose, "wm.Shutdown must precede recorder close (quality-sync drain may refill the recorder on flush failure; a closed recorder rejects refill and silently drops)")
	require.Less(t, recClose, redisClose, "recorder close must precede redisx.Close (drain runs on the live pool)")
}
