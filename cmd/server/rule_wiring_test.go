// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRuleHealthWiringOnce 生产接线唯一性证据（AST）：规则 typed 动作双面
// （SetHealthSink/SetPersistFunc）在 main 中各恰好一次——重复接线 = 后写覆盖
// 前写的静默竞态源（sink/persistFn 是裸字段赋值，无 CAS）；recover→PROBING
// 写入面（RecoverProber）经 service.New 的 ServiceDeps 恰好注入一次。
func TestRuleHealthWiringOnce(t *testing.T) {
	_, f := parseMainGo(t)
	mainFn := findFuncDecl(t, f, "main")

	counts := map[string]int{
		"ruleEngine.SetHealthSink":  0,
		"ruleEngine.SetPersistFunc": 0,
		"service.New:RecoverProber": 0,
	}
	ast.Inspect(mainFn, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		key := recv.Name + "." + sel.Sel.Name
		if _, want := counts[key]; want {
			counts[key]++
			return true
		}
		// service.New 的 ServiceDeps 复合字面量中 RecoverProber 字段出现次数。
		if recv.Name == "service" && sel.Sel.Name == "New" {
			for _, arg := range ce.Args {
				ast.Inspect(arg, func(m ast.Node) bool {
					kv, ok := m.(*ast.KeyValueExpr)
					if !ok {
						return true
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "RecoverProber" {
						counts["service.New:RecoverProber"]++
					}
					return true
				})
			}
		}
		return true
	})
	for _, key := range []string{
		"ruleEngine.SetHealthSink",
		"ruleEngine.SetPersistFunc",
		"service.New:RecoverProber",
	} {
		require.Equal(t, 1, counts[key], "%s 必须恰好接线一次", key)
	}
}
