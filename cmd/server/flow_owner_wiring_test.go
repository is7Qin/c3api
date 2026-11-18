// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQualityFlowOwnerWiring owner 必须来自 recorder 的 FlowOwner()，且注册序上
// 早于 quality-sync（反向排空 ⇒ quality-sync 先关、owner 后关，refill 依赖它）。
//
// 注册序不再是 `managedWorkers := orderedWorkers(...)` 的字面量，而是 workerOrder
// 表；因此这里查表下标，表内容由 worker_order_table_test 的全序断言负责。
func TestQualityFlowOwnerWiring(t *testing.T) {
	_, f := parseSrcFile(t, "wiring_run.go")
	buildFn := findFuncDecl(t, f, "buildRuntime")

	ownerFromRecorder := false
	ast.Inspect(buildFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "qualityFlowOwner" {
			return true
		}
		ce, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		se, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if rec, ok := se.X.(*ast.Ident); ok && rec.Name == "qualityRecorder" && se.Sel.Name == "FlowOwner" {
			ownerFromRecorder = true
		}
		return true
	})
	require.True(t, ownerFromRecorder, "main must take the owner via qualityRecorder.FlowOwner()")

	require.NotEqual(t, -1, workerSlotIndex("qualityFlowOwner"), "owner 必须在注册序表里")
	require.NotEqual(t, -1, workerSlotIndex("qualitySync"))
	require.Less(t, workerSlotIndex("qualityFlowOwner"), workerSlotIndex("qualitySync"),
		"quality-flow-owner must be registered BEFORE quality-sync: reverse shutdown then closes quality-sync first and the owner second (refill dependency)")
}
