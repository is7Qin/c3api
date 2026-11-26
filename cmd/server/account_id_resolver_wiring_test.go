// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAccountIDResolverCtorWiring pins the same cleanup shape as
// TestUsageSnapshotCtorWiring, for the new codex account id port: the PAT
// whoami面 must be injected through the constructor (OpsOptions.AccountIDResolver),
// never back-filled through a setter afterwards.
//
// 为什么这条 AST 门禁比"在 handler 里加个 nil 检查"可靠：nil 检查只能保证
// 「未装配不 panic」，保证不了「生产装配真的接上了」。把装配点钉成结构事实，
// 漏接就是红，而不是运行期静默降级成"PAT 永远补不出 account id"。
func TestAccountIDResolverCtorWiring(t *testing.T) {
	// 缺席钉扫**整包**：回填式 setter 写进任一 wiring_*.go 都红。
	_, pkg := parsePkgSrc(t)
	ast.Inspect(pkg, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "SetAccountIDResolver" || id.Name == "SetUsageAccountsIDResolver") {
			t.Errorf("cmd/server 仍含 %s 回填调用——必须经 OpsOptions 构造注入", id.Name)
			return false
		}
		return true
	})

	// 存在钉：OpsOptions 字面量钉在 wiring_run.go——handler.New 的唯一构造点。
	_, f := parseSrcFile(t, "wiring_run.go")
	key := false
	value := ""
	compositeType := ""
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
			k, ok := kv.Key.(*ast.Ident)
			if !ok || k.Name != "AccountIDResolver" {
				continue
			}
			key = true
			switch v := kv.Value.(type) {
			case *ast.CompositeLit:
				if vs, ok := v.Type.(*ast.SelectorExpr); ok {
					compositeType = vs.Sel.Name
				}
			case *ast.Ident:
				value = v.Name
			}
		}
		return true
	})
	require.True(t, key, "handler.New 必须经 OpsOptions.AccountIDResolver 构造注入 PAT whoami 面")
	require.Equal(t, "PATWhoami", compositeType,
		"AccountIDResolver 必须接 sdkbridge.PATWhoami{}（方法集匹配 ⇒ 赋值即编译期证明装配成立）；不要改成闭包")
	require.Empty(t, value, "不要用中间变量绕过——直接写字面量才能被这条门禁看见")
}
