// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConversationBookWiring 生产恒构造并注入 Codex 会话簿（spec §5.6）：main 里
// convBook := codexsdk.NewConversationBook(redisstore.New(rdb))，同一个 convBook 既经
// proxy.Deps{Conversations: convBook} 注入代理，又交给 proxy.NewConversationWorker。
// 删掉任一处（Deps 不注入 ⇒ 会话路径整体回落槽位，没有别的用例会红）本用例即失败。
func TestConversationBookWiring(t *testing.T) {
	_, f := parseMainGo(t)
	mainFn := findFuncDecl(t, f, "main")

	var built, injected, workerGetsBook bool
	ast.Inspect(mainFn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) != 1 || len(x.Rhs) != 1 {
				return true
			}
			lhs, ok := x.Lhs[0].(*ast.Ident)
			if !ok || lhs.Name != "convBook" {
				return true
			}
			ce, ok := x.Rhs[0].(*ast.CallExpr)
			if !ok || !isPkgCall(ce, "codexsdk", "NewConversationBook") || len(ce.Args) != 1 {
				return true
			}
			inner, ok := ce.Args[0].(*ast.CallExpr)
			if ok && isPkgCall(inner, "redisstore", "New") && len(inner.Args) == 1 {
				if id, ok := inner.Args[0].(*ast.Ident); ok && id.Name == "rdb" {
					built = true
				}
			}
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Deps" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "proxy" {
				return true
			}
			for _, e := range x.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				k, ok := kv.Key.(*ast.Ident)
				if !ok || k.Name != "Conversations" {
					continue
				}
				if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "convBook" {
					injected = true
				}
			}
		case *ast.CallExpr:
			if isPkgCall(x, "proxy", "NewConversationWorker") && len(x.Args) >= 1 {
				if id, ok := x.Args[0].(*ast.Ident); ok && id.Name == "convBook" {
					workerGetsBook = true
				}
			}
		}
		return true
	})
	require.True(t, built, "main 必须构造 convBook := codexsdk.NewConversationBook(redisstore.New(rdb))")
	require.True(t, injected, "main 必须经 proxy.Deps{Conversations: convBook} 注入会话簿")
	require.True(t, workerGetsBook, "会话簿 worker 必须 Flush 同一个 convBook")
}

// isPkgCall 判定 ce 是否为 pkg.Name(...) 形式的调用。
func isPkgCall(ce *ast.CallExpr, pkg, name string) bool {
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}
