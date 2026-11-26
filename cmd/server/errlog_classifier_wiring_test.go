// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestErrLogClassifierCtorWiring 钉住用户面 err_logs 脱敏端口的装配形状：
// 它必须作为 userapi.Router 的**独立构造实参**注入，不得回填、不得塞进 Features。
//
// 为什么需要这条门禁：端口化的收益（handler/user 不再 import 规则引擎）完全
// 建立在"组合根真的把适配器接上了"这个前提上——而漏接的后果**不是编译错误**，
// 而是运行期静默降级：ErrLogClassifier 为 nil ⇒ 用户面 err_logs 的 error_message
// 原样输出，平台内部错误文本（上游账户、账单细节）直接泄漏给终端用户，且所有
// 既有测试仍然全绿（nil 语义本身就是合法的"不脱敏"）。把装配点钉成结构事实，
// 漏接就是红。
func TestErrLogClassifierCtorWiring(t *testing.T) {
	// 缺席钉扫**整包**：回填式 setter 写进任一 wiring_*.go 都红。
	// 端口本身没有 setter——这条钉的是"将来有人图省事加一个"。
	_, pkg := parsePkgSrc(t)
	ast.Inspect(pkg, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "SetErrLogClassifier" || id.Name == "SetRules" || id.Name == "SetRuleEngine") {
			t.Errorf("cmd/server 仍含 %s 回填调用——必须经 userapi.Router 构造注入", id.Name)
			return false
		}
		return true
	})

	// 存在钉：userapi.Router 的实参必须在 wiring_run.go，且脱敏端口那一格
	// 必须是 errLogRules（其声明点的字面量类型由下一条钉住）。
	_, f := parseSrcFile(t, "wiring_run.go")
	routerCalls := 0
	portArg := ""
	featuresHasRules := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Router" {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || pkgIdent.Name != "userapi" {
			return true
		}
		routerCalls++
		// 第 4 个实参 = 规则判定端口（见 userapi.Router 的签名顺序）。
		require.Greaterf(t, len(call.Args), 3, "userapi.Router 实参不足 4 个：签名变了，本门禁需要同步")
		if id, ok := call.Args[3].(*ast.Ident); ok {
			portArg = id.Name
		}
		// 端口不得塞进 Features：它是行为依赖，不是开关。
		ast.Inspect(call, func(m ast.Node) bool {
			lit, ok := m.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ft, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || ft.Sel.Name != "Features" {
				return true
			}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && (k.Name == "ErrLogClassifier" || k.Name == "Rules") {
						featuresHasRules = true
					}
				}
			}
			return true
		})
		return true
	})
	require.Equal(t, 1, routerCalls, "userapi.Router 必须恰有 1 个装配点（本门禁按唯一构造点求值）")
	require.Equal(t, "errLogRules", portArg,
		"userapi.Router 的第 4 实参必须是 errLogRules（独立参数，不是 nil 也不是内联字面量）")
	require.False(t, featuresHasRules,
		"规则端口不得塞进 userapi.Features——它是行为依赖，不是配置开关")

	// 类型钉：errLogRules 必须声明成 **userapi.ErrLogClassifier 端口类型**（而不是
	// 具体实现类型）——这保证 Router 收到的确实是端口（赋值即编译期证明方法集
	// 匹配），也防止有人改成 *rule.RuleEngine 后让 handler/user 重新需要 import rule。
	// 值钉：必须由 errLogClassifier 适配器构造，且引擎来自该装配阶段的 ruleEngine。
	var declType, adapterType, engineName string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range vs.Names {
			if id.Name != "errLogRules" {
				continue
			}
			if i < len(vs.Values) {
				if lit, ok := vs.Values[i].(*ast.CompositeLit); ok {
					switch st := lit.Type.(type) {
					case *ast.Ident: // 本包类型：errLogClassifier{...}
						adapterType = st.Name
					case *ast.SelectorExpr: // 外部类型：pkg.Adapter{...}
						adapterType = st.Sel.Name
					}
					for _, e := range lit.Elts {
						kv, ok := e.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "engine" {
							if v, ok := kv.Value.(*ast.Ident); ok {
								engineName = v.Name
							}
						}
					}
				}
			}
			if vs.Type != nil {
				if st, ok := vs.Type.(*ast.SelectorExpr); ok {
					declType = st.Sel.Name
				}
			}
		}
		return true
	})
	require.Equal(t, "ErrLogClassifier", declType,
		"errLogRules 必须声明成 userapi.ErrLogClassifier 端口类型（声明成具体类型就等于把实现形状带进用户面）")
	require.Equal(t, "errLogClassifier", adapterType,
		"端口必须由 errLogClassifier 适配器实现（翻译 ErrLogQuery → rule.Event）")
	require.Equal(t, "ruleEngine", engineName,
		"适配器必须拿装配阶段的 ruleEngine 构造——不要另建第二个引擎")
}
