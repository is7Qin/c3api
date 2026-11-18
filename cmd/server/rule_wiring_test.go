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

// TestRuleHealthWiringOnce 生产接线唯一性证据（AST，根因重开后）：
// 回填 setter（SetHealthSink/SetPersistFunc）与旧 HealthController 装配必须
// 缺席（各 0 次）；latch→hub→sink→persistFn→ruleEngine→sched 构造序钉死；
// rule.New 恰 5 参、scheduler.New 恰 7 参；recover→PROBING 写入面
// （RecoverProber）经 service.New 的 Deps 恰好注入一次（原不断言保留）。
//
// 装配按阶段拆文件后的两个视图（都不弱于旧版）：
//   - 缺席钉 / 唯一性钉 → parsePkgSrc（cmd/server 全部非测试 .go 的合并视图）：
//     回填 setter 写进任一 wiring_*.go 都会红，旧版只看 main.go 反而更弱；
//   - 构造序钉 → wiring_health.go 的 newHealthFace：六站若被拆到两个文件，
//     require.Contains 立即 FailNow（顺序钉主语被拆开 ⇒ 返工），不降级成
//     「某一文件里找不到就算了」。
func TestRuleHealthWiringOnce(t *testing.T) {
	fset, f := parseSrcFile(t, "wiring_health.go")
	healthFn := findFuncDecl(t, f, "newHealthFace")
	_, pkg := parsePkgSrc(t)

	counts := map[string]int{
		"ruleEngine.SetHealthSink":  0,
		"ruleEngine.SetPersistFunc": 0,
		"service.New:identArg":      0,
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

	// 视图 1：整包——缺席钉与唯一性钉（RecoverProber 注入、参量钉）。
	ast.Inspect(pkg, func(n ast.Node) bool {
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
				argCounts[want] = append(argCounts[want], len(ce.Args))
			}
		}
		// service.New 必须与 Deps 字面量分离：装配层要先 deps.Validate() 再
		// service.New(deps)（spec 2026-11-18 §5.2）。内联字面量会让 Validate 无
		// 值可引用，因此这里钉两件事——New 的实参是具名变量，且 RecoverProber
		// 全包恰好接线一次（计数在下方 kvProber）。
		if recv.Name == "service" && sel.Sel.Name == "New" && len(ce.Args) == 1 {
			if _, ok := ce.Args[0].(*ast.Ident); ok {
				counts["service.New:identArg"]++
			}
		}
		return true
	})
	// Deps 的 RecoverProber 字段全包出现次数（唯一性钉，跨文件生效）。
	kvProber := 0
	ast.Inspect(pkg, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "RecoverProber" {
			kvProber++
		}
		return true
	})

	// 视图 2：wiring_health.go——六站构造序的主语所在文件。
	ast.Inspect(healthFn, func(n ast.Node) bool {
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
		for _, want := range orderKeys {
			if key == want {
				if _, seen := firstOffset[want]; !seen {
					firstOffset[want] = ce.Pos()
				}
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
	require.Equal(t, 1, kvProber, "RecoverProber 必须恰好接线一次")
	require.Equal(t, 1, counts["service.New:identArg"], "service.New 必须收具名 Deps 变量（先 Validate 再构造）")
	// 构造序钉：六站全在且严格递增。
	for _, key := range orderKeys {
		require.Contains(t, firstOffset, key, "%s 必须在 newHealthFace 中构造（顺序钉主语被拆到其它文件 ⇒ 返工）", key)
	}
	for i := 1; i < len(orderKeys); i++ {
		prev, cur := orderKeys[i-1], orderKeys[i]
		require.Less(t, fset.Position(firstOffset[prev]).Offset, fset.Position(firstOffset[cur]).Offset,
			"装配序必须 %s 先于 %s", prev, cur)
	}
	// 拆分守卫：六站主语只允许出现在 wiring_health.go——搬到其它文件即 FailNow
	// （"找不到就算了"是禁止的降级）。
	requireSubjectsOnlyIn(t, "wiring_health.go", orderKeys...)
	// 参量钉：rule.New 恰 5 参、scheduler.New 恰 7 参（各恰好一次调用）。
	require.Equal(t, []int{5}, argCounts["rule.New"], "rule.New 必须恰 5 参（cfg,store,log,sink,persist）")
	require.Equal(t, []int{7}, argCounts["scheduler.New"], "scheduler.New 必须恰 7 参（cfg,loader,rule,h,log,latch,hub）")
}
