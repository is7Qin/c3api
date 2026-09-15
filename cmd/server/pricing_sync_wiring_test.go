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

// TestPricingSyncCtorWiring pins the W3-T2 cleanup (inventory B4): the
// SetPriceFetcher backfill is gone — main wires the fetcher into the pricing
// worker via the constructor (SyncWorkerConfig) and the worker into the handler
// via the constructor (OpsOptions.PricingSync), never via a service setter.
// Preview membership comes from the same constructor (SyncWorkerConfig.Snapshot).
func TestPricingSyncCtorWiring(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	srcPath := filepath.Join(filepath.Dir(file), "main.go")
	src, err := os.ReadFile(srcPath)
	require.NoError(t, err)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, parser.ParseComments)
	require.NoError(t, err)

	opsPricingSync := false
	workerSnapshot := false
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "SetPriceFetcher" {
			t.Errorf("main.go 仍含 SetPriceFetcher 回填调用")
			return false
		}
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		var want string
		switch sel.Sel.Name {
		case "OpsOptions":
			want = "PricingSync"
		case "SyncWorkerConfig":
			want = "Snapshot"
		default:
			return true
		}
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			k, ok := kv.Key.(*ast.Ident)
			if !ok || k.Name != want {
				continue
			}
			if want == "PricingSync" {
				opsPricingSync = true
			} else {
				workerSnapshot = true
			}
		}
		return true
	})
	require.True(t, opsPricingSync, "handler.New 必须经 OpsOptions.PricingSync 构造注入同步 worker")
	require.True(t, workerSnapshot, "pricing worker 必须经 SyncWorkerConfig.Snapshot 构造注入快照源")
}
