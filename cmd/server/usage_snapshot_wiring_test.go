// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageSnapshotCtorWiring pins the cleanup: the
// SetUsageSnapshotter backfill is gone — main wires the codex adapter into
// the handler via the constructor (OpsOptions.UsageSnap), never via a
// service setter.
func TestUsageSnapshotCtorWiring(t *testing.T) {
	_, f := parseMainGo(t)

	usageSnapKey := false
	usageSnapValue := ""
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "SetUsageSnapshotter" {
			t.Errorf("main.go 仍含 SetUsageSnapshotter 回填调用")
			return false
		}
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
			k, ok := kv.Key.(*ast.Ident)
			if !ok || k.Name != "UsageSnap" {
				continue
			}
			usageSnapKey = true
			if v, ok := kv.Value.(*ast.Ident); ok {
				usageSnapValue = v.Name
			}
		}
		return true
	})
	require.True(t, usageSnapKey, "handler.New 必须经 OpsOptions.UsageSnap 构造注入快照数据源")
	require.Equal(t, "codexAdapter", usageSnapValue, "UsageSnap 必须接 codexAdapter（构造远早于 handler）")
}
