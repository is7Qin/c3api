// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// relayWS 调用点 AST 门禁（spec custom-request-patch §7.2/§10 P4）。
//
// 目的：relayWS 的 clientFrameHook 是「客户端→上游帧」唯一的 request_patch 接线
// 面——漏传/传 nil 就是「配置对该上游静默失效」，没有任何行为测试会红。本门禁把
// 漏接线变成机械红：
//
//  1. 每次 p.relayWS(...) 调用必须传够全部形参（缺一个实参在 AST 上是 arity 不足）；
//  2. 第 3 个实参（clientFrameHook）显式 nil 只允许**车道白名单**：
//     今天 = codex 与 BPS 两处（SDK/prepared 面不消费 request_patch，spec §3 矩阵）；
//     静态 key 面必须传 p.wsClientFrameHook(sel)（非 nil 且名字可机械校验）；
//  3. 生产调用点计数 golden（今天 3 处：codex / BPS / 静态 key）——新增调用点不改
//     门禁即红；测试夹具里的 p.relayWS 调用不属于生产接线面，不扫描。
//
// 与 brief/spec 文本「relayWS 5 处调用点」的偏差（P4 报告 D1）：BASE 实测生产代码
// 只有 3 处（caller_responses_ws.go 的 codex/BPS/静态分支各一），另外 3 个
// p.relayWS 调用只存在于 _test.go 测试夹具。门禁钉实际生产面 3 处。

// relayWSCallSite 一次生产代码里的 p.relayWS 调用。
type relayWSCallSite struct {
	file string
	fn   string
	pos  token.Position
	args []ast.Expr
}

// relayWSClientFrameHookArg 是 clientFrameHook 在 relayWS 形参表里的下标
// （client, up, clientFrameHook, frameHook, ...）。
const relayWSClientFrameHookArg = 2

// relayWSWantArity 是 relayWS 今天的形参数（发现值必须与之相等——形参表变化
// 本身就是接线面变化，必须同步本门禁）。
const relayWSWantArity = 12

// relayWSNilAllow 是「clientFrameHook 允许显式 nil」的车道白名单：文件|函数 →
// 允许的 nil 调用点数（codex 与 BPS 两条分支同住 wsAttempt.call，故按计数钉）。
var relayWSNilAllow = map[string]int{
	"caller_responses_ws.go|call": 2,
}

// wantRelayWSCallSites / wantRelayWSNilSites 是生产调用点/显式 nil 计数 golden。
const (
	wantRelayWSCallSites = 3
	wantRelayWSNilSites  = 2
)

// TestRelayWSCallSitesExplicitHook 门禁主体（实仓扫描 + 三个锚点）。
func TestRelayWSCallSitesExplicitHook(t *testing.T) {
	arity := discoverRelayWSArity(t)
	require.Equal(t, relayWSWantArity, arity,
		"relayWS 形参数从 %d 变了：clientFrameHook 是 P4 新增接线面，改签名必须同提交更新本门禁", relayWSWantArity)

	sites := collectRelayWSCallSites(t)
	require.Len(t, sites, wantRelayWSCallSites,
		"生产代码 relayWS 调用点数量漂移（实际 %d）：codex/BPS 传 nil、静态 key 面传 p.wsClientFrameHook(sel)；新增/删除调用点必须同步本门禁", len(sites))

	violations := checkRelayWSCallSites(sites, arity, relayWSNilAllow)
	require.Empty(t, violations, "relayWS 调用点接线违例（共 %d 条）：%s", len(violations), strings.Join(violations, "；"))

	nilSites, hookSites := 0, 0
	for _, site := range sites {
		if isNilIdent(site.args[relayWSClientFrameHookArg]) {
			nilSites++
			continue
		}
		hookSites++
	}
	require.Equal(t, wantRelayWSNilSites, nilSites,
		"显式 nil 车道数漂移（实际 %d）：今天只允许 codex/BPS 两处", nilSites)
	require.Equal(t, wantRelayWSCallSites-wantRelayWSNilSites, hookSites,
		"非 nil hook 车道数漂移：静态 key 面必须且只能传 p.wsClientFrameHook(sel)")
}

// checkRelayWSCallSites 是唯一判据（自测矩阵与实仓扫描共用，不另写「看起来
// 等价」的规则）：返回违规描述列表（空 = 通过）。
func checkRelayWSCallSites(sites []relayWSCallSite, arity int, nilAllow map[string]int) []string {
	var violations []string
	nilPerFn := map[string]int{}
	for _, site := range sites {
		// ① 实参必须齐全：缺参 = 客户端帧 hook 被静默漏掉。
		if len(site.args) != arity {
			violations = append(violations,
				site.pos.String()+"：relayWS 调用实参数 "+strconv.Itoa(len(site.args))+" != 形参数 "+strconv.Itoa(arity)+
					"——clientFrameHook 实参必须显式传（nil 只允许白名单车道）")
			continue
		}
		hookArg := site.args[relayWSClientFrameHookArg]
		if isNilIdent(hookArg) {
			key := site.file + "|" + site.fn
			nilPerFn[key]++
			if nilPerFn[key] > nilAllow[key] {
				violations = append(violations,
					site.pos.String()+"：relayWS 的 clientFrameHook 传了显式 nil，车道 "+key+" 不在白名单/超额度——"+
						"静态 key 面必须传 p.wsClientFrameHook(sel)")
			}
			continue
		}
		// ② 非 nil 只允许 p.wsClientFrameHook(...)：防「随手塞一个别的函数」绕过车道语义。
		call, ok := hookArg.(*ast.CallExpr)
		if !ok {
			violations = append(violations, site.pos.String()+"：clientFrameHook 实参既不是 nil 也不是 p.wsClientFrameHook(...) 调用")
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "wsClientFrameHook" {
			violations = append(violations, site.pos.String()+"：clientFrameHook 非 nil 实参必须是 p.wsClientFrameHook(sel)（唯一的帧 hook 构造点）")
		}
	}
	return violations
}

// discoverRelayWSArity 从生产源 ws_relay.go 的 relayWS 方法声明取形参数（形参
// 表本身是接线面，写死不 discovery 会在签名变化时静默失明）。
func discoverRelayWSArity(t *testing.T) int {
	t.Helper()
	path := filepath.Join(repoRelDir(t, "internal/proxy"), "ws_relay.go")
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, path, nil, 0)
	require.NoError(t, err)
	for _, decl := range af.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "relayWS" || fn.Recv == nil || fn.Type.Params == nil {
			continue
		}
		return fieldArity(fn.Type.Params.List)
	}
	require.FailNow(t, "ws_relay.go 里找不到 relayWS 方法声明")
	return 0
}

// collectRelayWSCallSites 扫描 internal/proxy 全部**非测试**源，收集 p.relayWS
// 调用点（按函数体归属，记录文件/函数/位置/实参）。
func collectRelayWSCallSites(t *testing.T) []relayWSCallSite {
	t.Helper()
	var out []relayWSCallSite
	for _, path := range proxySourceFiles(t) {
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "解析 %s", path)
		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "relayWS" {
					return true
				}
				out = append(out, relayWSCallSite{
					file: filepath.Base(fset.Position(call.Pos()).Filename),
					fn:   fn.Name.Name,
					pos:  fset.Position(call.Pos()),
					args: call.Args,
				})
				return true
			})
		}
	}
	return out
}

// TestRelayWSCallSitesGateRejectsBypass 门禁红/绿自证（机械可复验，不靠人手删
// 生产代码）：合成源码覆盖四条判据——缺参、非白名单 nil、白名单超额 nil、非法
// hook 表达式；正例（静态 key 面传 p.wsClientFrameHook）必须放行。
func TestRelayWSCallSitesGateRejectsBypass(t *testing.T) {
	valid := []string{
		"p.relayWS(client, up, p.wsClientFrameHook(sel), nil, r, reqID, g, start, sel, model, typ, first)",
		"p.relayWS(client, buf, nil, frameHook, r, reqID, g, start, sel, model, typ, nil)",
		"p.relayWS(client, up, nil, nil, r, reqID, g, start, sel, model, typ, nil)",
	}
	parseSites := func(t *testing.T, calls ...string) []relayWSCallSite {
		t.Helper()
		var src strings.Builder
		src.WriteString("package proxy\n\nfunc synthetic(p *Proxy) {\n")
		for _, c := range calls {
			src.WriteString("\t" + c + "\n")
		}
		src.WriteString("}\n")
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, "synthetic.go", src.String(), 0)
		require.NoError(t, err)
		var out []relayWSCallSite
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "relayWS" {
				return true
			}
			out = append(out, relayWSCallSite{file: "caller_responses_ws.go", fn: "call", pos: fset.Position(call.Pos()), args: call.Args})
			return true
		})
		return out
	}

	// 正例：codex/BPS 两 nil 在白名单额度内 + 静态面 wsClientFrameHook → 绿。
	require.Empty(t, checkRelayWSCallSites(parseSites(t, valid...), relayWSWantArity, relayWSNilAllow),
		"合法接线（2 nil + 1 帧 hook）不得报违规")

	// 绿：单个静态 hook 调用（无 nil）通过。
	require.Empty(t, checkRelayWSCallSites(parseSites(t, valid[0]), relayWSWantArity, relayWSNilAllow),
		"单个静态 hook 调用必须绿")

	// 红①：非白名单函数里 nil（同文件不同函数 ⇒ 车道键不同）。
	outside := parseSites(t, valid[0], valid[1])
	outside[1].fn = "notInAllowlist"
	require.NotEmpty(t, checkRelayWSCallSites(outside, relayWSWantArity, relayWSNilAllow),
		"非白名单函数里的显式 nil 必须红")

	// 红②：白名单额度被用光后第三个 nil 必须红（静态 key 面改成 nil 的等价形态）。
	require.NotEmpty(t, checkRelayWSCallSites(parseSites(t, valid[1], valid[2], valid[2]), relayWSWantArity, relayWSNilAllow),
		"白名单额度外的第三个 nil 必须红")

	// 红③：缺参（AST 上是 arity 不足 ⇒ 客户端帧 hook 被静默漏掉）。
	short := "p.relayWS(client, up, nil, r, reqID, g, start, sel, model, typ, first)"
	require.NotEmpty(t, checkRelayWSCallSites(parseSites(t, short), relayWSWantArity, relayWSNilAllow),
		"缺 clientFrameHook 实参必须红")

	// 红④：非 nil 但不是 p.wsClientFrameHook(...) 的调用（随手塞别的函数）。
	bogus := "p.relayWS(client, up, someOtherHook(sel), nil, r, reqID, g, start, sel, model, typ, first)"
	require.NotEmpty(t, checkRelayWSCallSites(parseSites(t, bogus), relayWSWantArity, relayWSNilAllow),
		"非 wsClientFrameHook 的非 nil 实参必须红")
}
