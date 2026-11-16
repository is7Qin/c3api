// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSniffFatalPrefilterRunsBeforeBridge 判死嗅探的**成本契约**：先跑本地的零分配
// 预筛，命中才问 bridge。
//
// 判据（哪些码算账号死亡）归 SDK；"这一帧值不值得跨包问一次"是本进程的成本决定。
// 顺序一旦反过来，1 MiB 内容帧就会白付一次跨包调用——而内容帧是绝大多数。
// 这里用 AST 钉住调用顺序，因为这条契约在类型系统里不可表达，在基准里只能"看起来
// 变慢"，没有别的机制会在它被改坏时变红。
func TestSniffFatalPrefilterRunsBeforeBridge(t *testing.T) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "codex_responses_ws.go", nil, 0)
	require.NoError(t, err)

	var fn *ast.FuncDecl
	for _, d := range af.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "sniffCodexWSDeath" {
			fn = f
		}
	}
	require.NotNil(t, fn, "找不到 sniffCodexWSDeath：改名请同步本门禁")

	var order []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case pkg.Name == "bytes" && sel.Sel.Name == "Contains":
			order = append(order, "bytes.Contains")
		case pkg.Name == "sdkbridge" && sel.Sel.Name == "ClassifyFatal":
			order = append(order, "sdkbridge.ClassifyFatal")
		}
		return true
	})
	require.Equal(t, []string{"bytes.Contains", "sdkbridge.ClassifyFatal"}, order,
		"判死路径必须「先本地零分配预筛、后跨包判据」；顺序反了，每个内容帧都会白付一次跨包调用")
}
