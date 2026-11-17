// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPricingSyncCtorWiring pins the cleanup: the
// SetPriceFetcher backfill is gone — main wires the fetcher into the pricing
// worker via the constructor (SyncWorkerConfig) and the worker into the handler
// via the constructor (OpsOptions.PricingSync), never via a service setter.
// Preview membership comes from the same constructor (SyncWorkerConfig.Snapshot).
func TestPricingSyncCtorWiring(t *testing.T) {
	// 缺席钉 + SyncWorkerConfig 钉扫**整包**（worker 构造在 wiring_codex.go；
	// 回填 setter 写进任一文件都会红）；OpsOptions 字面量钉在 wiring_run.go
	// ——handler.New 的唯一构造点。
	_, pkg := parsePkgSrc(t)

	opsPricingSync := false
	workerSnapshot := false
	ast.Inspect(pkg, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "SetPriceFetcher" {
			t.Errorf("cmd/server 仍含 SetPriceFetcher 回填调用")
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
	// OpsOptions.PricingSync：在 handler.New 所在的 wiring_run.go 里点名求值
	// （ops 面唯一注入点；主语被搬走即红）。
	_, f := parseSrcFile(t, "wiring_run.go")
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "OpsOptions" {
			return true
		}
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "PricingSync" {
				opsPricingSync = true
			}
		}
		return true
	})
	require.True(t, opsPricingSync, "handler.New 必须经 OpsOptions.PricingSync 构造注入同步 worker")
	require.True(t, workerSnapshot, "pricing worker 必须经 SyncWorkerConfig.Snapshot 构造注入快照源")
}
