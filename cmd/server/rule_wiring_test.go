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

// TestRuleHealthWiringOnce 生产接线唯一性证据（AST，B18/B19 根因重开后）：
// 回填 setter（SetHealthSink/SetPersistFunc）与旧 HealthController 装配必须
// 缺席（各 0 次）；latch→hub→sink→persistFn→ruleEngine→sched 构造序钉死；
// rule.New 恰 5 参、scheduler.New 恰 7 参；recover→PROBING 写入面
// （RecoverProber）经 service.New 的 ServiceDeps 恰好注入一次（原不断言保留）。
func TestRuleHealthWiringOnce(t *testing.T) {
	fset, f := parseMainGo(t)
	mainFn := findFuncDecl(t, f, "main")

	counts := map[string]int{
		"ruleEngine.SetHealthSink":  0,
		"ruleEngine.SetPersistFunc": 0,
		"service.New:RecoverProber": 0,
	}
	// 旧装配残留：HealthController 构造与 sched 派生锁存一律不得出现。
	absentSels := map[string]int{
		"NewHealthController":              0,
		"NewHealthControllerWithScheduler": 0,
	}
	absentRecvSels := map[string]int{
		"sched.LatchStore": 0,
	}
	// 构造序钉：首次出现偏移必须严格递增。
	orderKeys := []string{
		"latch.NewLatchStore",
		"latch.NewHub",
		"scheduler.NewLatchSink",
		"scheduler.NewRulePersistFunc",
		"rule.New",
		"scheduler.New",
	}
	firstOffset := map[string]token.Pos{}
	argCounts := map[string][]int{}

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
		}
		if _, want := absentSels[sel.Sel.Name]; want {
			absentSels[sel.Sel.Name]++
		}
		if _, want := absentRecvSels[key]; want {
			absentRecvSels[key]++
		}
		for _, want := range orderKeys {
			if key == want {
				if _, seen := firstOffset[want]; !seen {
					firstOffset[want] = ce.Pos()
				}
				argCounts[want] = append(argCounts[want], len(ce.Args))
			}
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
	// 回填 setter 必须缺席（0 次）——出现即返工。
	for _, key := range []string{
		"ruleEngine.SetHealthSink",
		"ruleEngine.SetPersistFunc",
	} {
		require.Equal(t, 0, counts[key], "%s 必须缺席（构造注入替代回填）", key)
	}
	for sel, n := range absentSels {
		require.Equal(t, 0, n, "%s 必须缺席（旧装配已删）", sel)
	}
	for key, n := range absentRecvSels {
		require.Equal(t, 0, n, "%s 必须缺席（main 自持 latch，不再派生）", key)
	}
	require.Equal(t, 1, counts["service.New:RecoverProber"], "service.New:RecoverProber 必须恰好接线一次")
	// 构造序钉：六站全在且严格递增。
	for _, key := range orderKeys {
		require.Contains(t, firstOffset, key, "%s 必须在 main 中构造", key)
	}
	for i := 1; i < len(orderKeys); i++ {
		prev, cur := orderKeys[i-1], orderKeys[i]
		require.Less(t, fset.Position(firstOffset[prev]).Offset, fset.Position(firstOffset[cur]).Offset,
			"装配序必须 %s 先于 %s", prev, cur)
	}
	// 参量钉：rule.New 恰 5 参、scheduler.New 恰 7 参（各恰好一次调用）。
	require.Equal(t, []int{5}, argCounts["rule.New"], "rule.New 必须恰 5 参（cfg,store,log,sink,persist）")
	require.Equal(t, []int{7}, argCounts["scheduler.New"], "scheduler.New 必须恰 7 参（cfg,loader,rule,h,log,latch,hub）")
}
