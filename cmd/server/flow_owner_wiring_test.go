// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/worker"
)

// TestQualityFlowOwnerWiring pins the async-routing-quality-telemetry
// assembly (AST evidence, same style as TestQualitySyncWiring): main must
// take the owner from the recorder and register it in the shared
// managedWorkers list BEFORE quality-sync, so reverse shutdown closes
// quality-sync first (its failed flushes still refill the owner) and the
// owner second.
func TestQualityFlowOwnerWiring(t *testing.T) {
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

	ownerFromRecorder := false
	var ordered []string
	ast.Inspect(mainFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if lhs.Name == "qualityFlowOwner" {
			if ce, ok := as.Rhs[0].(*ast.CallExpr); ok {
				if se, ok := ce.Fun.(*ast.SelectorExpr); ok {
					if rec, ok := se.X.(*ast.Ident); ok && rec.Name == "qualityRecorder" && se.Sel.Name == "FlowOwner" {
						ownerFromRecorder = true
					}
				}
			}
		}
		if lhs.Name == "managedWorkers" {
			if ce, ok := as.Rhs[0].(*ast.CallExpr); ok {
				if fun, ok := ce.Fun.(*ast.Ident); ok && fun.Name == "orderedWorkers" {
					for _, a := range ce.Args {
						if id, ok := a.(*ast.Ident); ok {
							ordered = append(ordered, id.Name)
						}
					}
				}
			}
		}
		return true
	})
	require.True(t, ownerFromRecorder, "main must take the owner via qualityRecorder.FlowOwner()")
	require.Contains(t, ordered, "qualityFlowOwner", "owner must be in the shared managedWorkers registration/ops list")
	require.Contains(t, ordered, "qualitySync")
	pos := func(name string) int {
		for i, n := range ordered {
			if n == name {
				return i
			}
		}
		return -1
	}
	require.Less(t, pos("qualityFlowOwner"), pos("qualitySync"),
		"quality-flow-owner must be registered BEFORE quality-sync: reverse shutdown then closes quality-sync first and the owner second (refill dependency)")
}

// TestOpsWorkersQualityFlowOwner pins ops visibility through the real
// assembly path (statsProviders assertion over managed workers, handler
// route): exactly one quality-flow-owner entry whose stats carry the full
// 14-field contract shape from openapi.yaml, no more, no less.
func TestOpsWorkersQualityFlowOwner(t *testing.T) {
	rec, err := quality.NewRecorder(50000)
	require.NoError(t, err)
	owner := rec.FlowOwner()
	require.Equal(t, "quality-flow-owner", owner.Name())
	var _ worker.Worker = owner

	providers := statsProviders([]worker.Worker{owner}, nil)
	require.Len(t, providers, 1, "owner must satisfy the StatsProvider ops contract")
	h := handler.New(nil, handler.OpsOptions{Workers: providers})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/ops/workers", nil)
	recw := httptest.NewRecorder()
	h.Router().ServeHTTP(recw, req)
	require.Equal(t, 200, recw.Code)
	var resp handler.WorkersResponse
	require.NoError(t, json.Unmarshal(recw.Body.Bytes(), &resp))
	var entries []map[string]any
	for _, w := range resp.Workers {
		if w.Name != "quality-flow-owner" {
			continue
		}
		st, ok := w.Stats.(map[string]any)
		require.True(t, ok, "stats must serialize as an object")
		entries = append(entries, st)
	}
	require.Len(t, entries, 1, "exactly one quality-flow-owner entry")
	st := entries[0]
	expected := []string{
		"running", "queued", "queue_cap", "accepted", "processed", "overflowed",
		"edge_rows_accepted", "edge_rows_dropped", "pending_minutes", "pending_bytes",
		"last_merge_duration_ms", "residual_submissions", "residual_rows", "close_unix_ms",
	}
	require.Len(t, st, len(expected), "owner stats carry exactly the openapi-pinned fields")
	for _, k := range expected {
		_, exists := st[k]
		require.True(t, exists, "missing field %s", k)
	}
	require.Equal(t, float64(quality.FlowOwnerQueueCap), st["queue_cap"], "fixed capacity 8192 published")
	require.Equal(t, float64(0), st["queued"])

	// Shape stability after real traffic: one accepted submission is
	// visible as queued/accepted without any drain.
	require.Equal(t, quality.SubmitAccepted, owner.Submit(1000, nil))
	st2 := owner.Stats().(quality.FlowOwnerStats)
	require.Equal(t, int64(1), st2.Accepted)
	require.Equal(t, 1, st2.Queued)
}
