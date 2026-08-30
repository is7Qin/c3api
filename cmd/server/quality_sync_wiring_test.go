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

// TestQualitySyncWiring pins the quality-sync lane assembly (Task 9):
// recorder → SyncWorker(qualityRecorder, rdb, repos.Partitions) → worker
// lifecycle (managedWorkers) + ops visibility, with shutdown ordering
// worker drain → recorder finalization → Redis client close.
func TestQualitySyncWiring(t *testing.T) {
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
		recorderSet  bool // px.SetQualityRecorder(qualityRecorder)
		syncCtor     bool // qualitySync := quality.NewSyncWorker(qualityRecorder, rdb, repos.Partitions, ...)
		inManaged    bool // qualitySync ∈ orderedWorkers(...) args
		managedFound bool
		seq          []string // ordering markers in source order
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
		if _, ok := isCall(n, "px", "SetQualityRecorder"); ok {
			recorderSet = true
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
						}
					}
				}
			}
		}
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
	require.True(t, recorderSet, "px.SetQualityRecorder(qualityRecorder) not found")
	require.True(t, syncCtor, "qualitySync := quality.NewSyncWorker(qualityRecorder, rdb, repos.Partitions, ...) not found")
	require.True(t, managedFound, "managedWorkers := orderedWorkers(...) not found")
	require.True(t, inManaged, "qualitySync must be registered via orderedWorkers(...) for lifecycle + ops stats")

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
