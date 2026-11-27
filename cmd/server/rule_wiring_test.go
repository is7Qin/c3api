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
// rule.New 恰 5 参、scheduler.New 恰 7 参；recover→健康清除面
// （RecoverHealthClear）经 service.New 的 Deps 恰好注入一次（关联性钉保留）。
//
// 钉的字段从 RecoverProber 换成 RecoverHealthClear 是随结构变更同步的：恢复链路
// 已不再写任何健康记录（PROBING 会让可选探针环成为恢复生效的必要条件），唯一
// 剩下的恢复健康依赖就是清除面。关联性钉的**形式与强度完全不变**。
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
	// persist 站的主语在 P5-1 随 rule_persist.go 迁到 internal/rulepersist：selector 文本
	// 从 scheduler.NewRulePersistFunc 变成 rulepersist.NewRulePersistFunc。
	orderKeys := []string{
		"latch.NewLatchStore",
		"latch.NewHub",
		"scheduler.NewLatchSink",
		"rulepersist.NewRulePersistFunc",
		"rule.New",
		"scheduler.New",
	}
	firstOffset := map[string]token.Pos{}
	argCounts := map[string][]int{}
	// newArgName 记录 service.New 收到的变量名，供「关联性钉」使用。
	var newArgName string

	// 视图 1：整包——缺席钉与唯一性钉（RecoverHealthClear 注入、参量钉）。
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
		// 值可引用，因此这里钉三件事——New 的实参是具名变量、该变量由 service.Deps
		// 字面量赋值、且这个字面量里带着 RecoverHealthClear（关联性钉，见下方 depsIdents）。
		if recv.Name == "service" && sel.Sel.Name == "New" && len(ce.Args) == 1 {
			if id, ok := ce.Args[0].(*ast.Ident); ok {
				counts["service.New:identArg"]++
				newArgName = id.Name
			}
		}
		return true
	})
	// Deps 字面量 → 变量名，以及该字面量是否带 RecoverHealthClear。
	//
	// 为什么不能只数全包的 RecoverHealthClear 出现次数：那样可以构造「重名但传错」的
	// 装配——包里另有一个带 RecoverHealthClear 的 Deps 字面量，而交给 New 的那个
	// 没有，计数照样是 1。必须把「New 收的变量」和「字面量里有没有该字段」连起来判。
	depsIdents := map[string]bool{}
	ast.Inspect(pkg, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		lit, ok := as.Rhs[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkgIdent, ok := sel.X.(*ast.Ident); !ok || pkgIdent.Name != "service" || sel.Sel.Name != "Deps" {
			return true
		}
		depsIdents[lhs.Name] = litHasKey(lit, "RecoverHealthClear")
		return true
	})
	// Deps 的 RecoverHealthClear 字段全包出现次数（唯一性钉，跨文件生效）。
	kvProber := 0
	ast.Inspect(pkg, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "RecoverHealthClear" {
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
	require.Equal(t, 1, kvProber, "RecoverHealthClear 必须恰好接线一次")
	// 关联性钉：New 收的变量必须就是带 RecoverHealthClear 的那个 Deps 字面量。
	// 只数「全包出现一次」不够——另造一个带该字段的字面量、交给 New 另一个没有
	// 该字段的字面量，计数依旧为 1。
	require.Len(t, depsIdents, 1, "装配层必须恰有一个 service.Deps 字面量")
	require.True(t, depsIdents[newArgName],
		"service.New 的实参 %q 必须来自含 RecoverHealthClear 的 Deps 字面量", newArgName)
	require.Equal(t, 1, counts["service.New:identArg"], "service.New 必须收具名 Deps 变量（先 Validate 再构造）")
	// 存在性钉（对 orderKeys **内容本身**）：改 selector 文本时顺手把条目删掉，会让这一站
	// 从顺序钉与归属钉（requireSubjectsOnlyIn 遍历的正是 orderKeys）里**同时消失**，
	// 且不会有任何断言变红——本单把 scheduler.NewRulePersistFunc 改名为
	// rulepersist.NewRulePersistFunc，正是最容易发生那次删除的时刻。
	require.Contains(t, orderKeys, "rulepersist.NewRulePersistFunc",
		"persist 构造站必须留在顺序钉里：删条目会让本站两处断言同时失效（0 防御）")
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

// litHasKey 判断复合字面量里有没有名为 name 的字段。
func litHasKey(lit *ast.CompositeLit, name string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}
