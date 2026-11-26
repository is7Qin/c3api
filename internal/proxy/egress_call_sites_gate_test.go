// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 出站调用点 AST 门禁（spec custom-request-patch §7.1 评审 oracle I9 / momus I-D）。
//
// 目的：request_patch 的装饰器是**每一个**数据面出站点的必经之路。手写 call site
// 会漂（本轮 14 个接线点 + rawJSON 1 个内部透传 + WS dial 1 个 P4 占位），漏一个
// 就是「配置对该上游静默失效」——没有任何行为测试会红。本门禁把「漏接线」变成
// 机械红：
//
//  1. 从 pkg/aiclient 生产代码**动态枚举**全部出站方法：Factory 上最后一个参数类型
//     是 func(*http.Request) 或 func(*websocket.DialOptions) 的导出方法（今天 6+1；
//     新增出站方法自动纳入，不改门禁即被测到）；
//  2. 扫描 internal/proxy 全部生产源文件：这些方法的调用 + rawJSON（第 8 参装饰器）
//     必须传够形参，且**最后一个实参不是 nil**；
//  3. 显式 nil 只允许白名单（文件+函数+方法）：
//     - caller_responses_ws.go / call / ResponsesWSDial —— P3 阶段边界（WS 数据面
//     P4 接线；该文件必须保留 TODO(P4) 标记，删标记即红）；
//     - codex/BPS 车道今天不调 aiclient（SDK/prepared 面），故白名单为空集——
//     将来若某车道确需 nil，必须在此登记并写明理由；
//  4. 计数 golden：proxy 侧 aiclient 出站调用 11 处（a 10 + c 1）+ rawJSON 4 处。
//     新增/删除调用点必须同步改门禁（spec 明确要求「不改门禁即红」）。
//
// probe 的 5 个 factory 调用是 P5 数据面（spec §10 P5）：本门禁**不扫描** probe
// （pending）；TestEgressProbePendingForP5 只记录「仍是 5 处显式 nil」，P5 接线后
// 自动静默（不阻塞 P5），真正纳入装饰器检查在 P5 把 probe 加入扫描集时完成。

// outboundMethod 从 pkg/aiclient 动态枚举出来的出站方法（名字 → 形参数）。
type outboundMethod struct {
	name  string
	arity int
}

// egressCallSite 一次出站调用点（违规或统计用）。
type egressCallSite struct {
	file     string
	fn       string
	method   string
	pos      token.Position
	lastArg  ast.Expr
}

// isNilIdent 判定表达式是否是字面 nil。
func isNilIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "nil"
}

// nilAllowKey 是「显式 nil 允许」白名单键：文件名（base）+ 函数名 + 方法名。
type nilAllowKey struct {
	file   string
	fn     string
	method string
}

var egressNilAllowlist = map[nilAllowKey]string{
	// P3 阶段边界：WS 握手装饰器（header + Host + {api_key}）与帧 hook 是 P4；
	// 本阶段显式 nil，由文件内 TODO(P4) 注释标记。P4 接线时删除本条目。
	{"caller_responses_ws.go", "call", "ResponsesWSDial"}: "P4",
}

// todoMarkerForStage 白名单阶段对应的 TODO 标记（文件里必须存在，防止「补了接线
// 忘了删白名单」或「白名单留着但 TODO 丢了」两种静默漂移）。
var todoMarkerForStage = map[string]string{"P4": "TODO(P4)"}

// expectedOutboundMethods 显式 pin 住今天应有的 7 个出站方法名（动态枚举之外的第二
// 道锁：枚举逻辑本身失明时，数量断言仍会红）。
var expectedOutboundMethods = []string{
	"RawPost", "ChatCompletionRaw", "ResponseRaw", "AnthMessageRaw", "SearchRaw",
	"ImagesRaw", "ResponsesWSDial",
}

// wantProxyOutboundCalls / wantProxyRawJSONCalls 是调用点计数 golden（见文件头）。
const (
	wantProxyOutboundCalls = 11
	wantProxyRawJSONCalls  = 4
)

// TestEgressCallSitesDecorated 门禁主体：动态枚举 + 全量扫描 + 白名单/计数断言。
func TestEgressCallSitesDecorated(t *testing.T) {
	methods := discoverOutboundMethods(t)
	for _, want := range expectedOutboundMethods {
		_, ok := methods[want]
		require.True(t, ok, "aiclient 出站方法 %s 不在枚举结果里：门禁枚举失明", want)
	}
	require.Len(t, methods, len(expectedOutboundMethods)+1, "出站方法集合变了（含 rawJSON）：%v", methodNames(methods))

	files := proxySourceFiles(t)
	require.Positive(t, len(files))

	seen := map[string]bool{}
	rawJSONCalls, outboundCalls := 0, 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		base := filepath.Base(path)
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		require.NoError(t, err)

		for _, site := range collectEgressCalls(af, fset, methods) {
			// 计数按**调用点**（不是去重键）：同一函数里两个 ImagesRaw（流式/非
			// 流式分支）是两个接线点。
			key := site.file + "|" + site.fn + "|" + site.method
			if site.method == "rawJSON" {
				rawJSONCalls++
			} else {
				outboundCalls++
			}
			seen[key] = true
			require.NotNil(t, site.lastArg, "%s：调用点参数缺失（装饰器实参必须到位）", site.pos)
			if isNilIdent(site.lastArg) {
				key := nilAllowKey{file: base, fn: site.fn, method: site.method}
				stage, ok := egressNilAllowlist[key]
				require.True(t, ok,
					"%s：出站调用 %s 传了显式 nil——装饰器只有 P4/P5/白名单车道允许 nil；非白名单调用点必须传 headerDecorator(...) 闭包",
					site.pos, site.method)
				marker := todoMarkerForStage[stage]
				require.Contains(t, string(src), marker,
					"%s 被白名单放行（阶段 %s），但文件里没有 %s 标记——接线/白名单必须成对维护",
					base, stage, marker)
			}
		}
	}

	require.Equal(t, wantProxyOutboundCalls, outboundCalls,
		"proxy 侧 aiclient 出站调用点数量变了（实际 %d）：新增/删除调用点必须同步本门禁与 spec 记账", outboundCalls)
	require.Equal(t, wantProxyRawJSONCalls, rawJSONCalls,
		"rawJSON（第 8 参装饰器）调用点数量变了（实际 %d）", rawJSONCalls)

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("出站接线点清单（%d 个 文件|函数|方法）：%s", len(keys), strings.Join(keys, "; "))
}

// TestEgressCallSitesGateRejectsBypass 门禁红/绿自证（机械可复验，不靠人手删代码）：
// 合成源码里非白名单 nil → 违规；非 nil → 放行；形参数量不足 → 违规哨兵。
func TestEgressCallSitesGateRejectsBypass(t *testing.T) {
	methods := map[string]outboundMethod{
		"ResponsesWSDial": {name: "ResponsesWSDial", arity: 9},
		"RawPost":         {name: "RawPost", arity: 10},
		"rawJSON":         {name: "rawJSON", arity: 8},
	}
	src := strings.Join([]string{
		"package proxy",
		"",
		"func nonWhitelisted(x clientIface) {",
		"	_, _, _ = x.ResponsesWSDial(a, b, c, d, e, f, g, h, nil)",
		"}",
		"",
		"func decorated(p *Proxy) {",
		"	_, _ = p.clients.RawPost(a, b, c, d, e, f, g, h, decorator)",
		"	_, _, _, _ = p.rawJSON(a, b, c, d, e, f, g, decorator)",
		"}",
	}, "\n")
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "mutation_sample.go", src, parser.ParseComments)
	require.NoError(t, err)
	sites := collectEgressCalls(af, fset, methods)
	require.Len(t, sites, 3, "合成源码里应识别出 3 个出站调用")

	nilCount, decoratedCount := 0, 0
	for _, s := range sites {
		if isNilIdent(s.lastArg) {
			nilCount++
		} else {
			decoratedCount++
		}
	}
	require.Equal(t, 1, nilCount, "非白名单函数里的 nil 实参必须可被识别（门禁不能空转）")
	require.Equal(t, 2, decoratedCount)

	_, ok := egressNilAllowlist[nilAllowKey{file: "caller_responses_ws.go", fn: "call", method: "ResponsesWSDial"}]
	require.True(t, ok, "P4 阶段白名单必须存在（否则 P3 WS dial 显式 nil 会红）")
	_, ok = egressNilAllowlist[nilAllowKey{file: "forward.go", fn: "rawJSON", method: "RawPost"}]
	require.False(t, ok, "rawJSON 内部 RawPost 透传不是白名单——它必须把实参原样传下去")

	// 形参不足（「删一个实参」在 AST 上的等价形态）必须可识别：lastArg=nil 哨兵。
	shortSrc := strings.Join([]string{
		"package proxy",
		"",
		"func f(p *Proxy) {",
		"	_, _ = p.clients.RawPost(a, b, c, d, e, f, g, h)",
		"}",
	}, "\n")
	shortFset := token.NewFileSet()
	shortAF, err := parser.ParseFile(shortFset, "short_sample.go", shortSrc, 0)
	require.NoError(t, err)
	shortSites := collectEgressCalls(shortAF, shortFset, methods)
	require.Len(t, shortSites, 1)
	require.Nil(t, shortSites[0].lastArg, "形参数量不足必须判红（lastArg=nil 哨兵）")
}

// TestEgressProbePendingForP5 probe 的 5 个 factory 调用是 P5 数据面：P3 只记录
// 「显式 nil pending」，不阻塞、不扫描（spec §10 P3/P5 边界）。P5 接线后本用例
// 自动静默（检测到非 nil 即返回），真正纳入检查在 P5 把 probe 加入扫描集。
func TestEgressProbePendingForP5(t *testing.T) {
	methods := discoverOutboundMethods(t)
	dir := repoRelDir(t, "internal/probe")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	calls, nilCalls := 0, 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		require.NoError(t, err)
		for _, site := range collectEgressCalls(af, fset, methods) {
			calls++
			if isNilIdent(site.lastArg) {
				nilCalls++
			}
		}
	}
	if nilCalls < calls {
		// P5 已接线：本用例使命结束，P5 负责把 probe 加入主门禁扫描集。
		return
	}
	require.Equal(t, 5, calls, "probe factory 出站调用应为 5 处（P5 待接清单）")
	require.Equal(t, 5, nilCalls, "P3 阶段 probe 应全部显式 nil（P5 TODO）")
}

// discoverOutboundMethods 从 pkg/aiclient 生产源码动态枚举出站方法：
// Factory 接收者 + 导出名 + 最后一个形参类型是装饰器函数类型。返回 名字→形参数量。
func discoverOutboundMethods(t *testing.T) map[string]outboundMethod {
	t.Helper()
	dir := repoRelDir(t, "pkg/aiclient")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string]outboundMethod{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Type.Params == nil {
				continue
			}
			if !isFactoryReceiver(fn.Recv) || !ast.IsExported(fn.Name.Name) {
				continue
			}
			fields := fn.Type.Params.List
			if len(fields) == 0 || !isDecoratorFuncType(fields[len(fields)-1].Type) {
				continue
			}
			out[fn.Name.Name] = outboundMethod{name: fn.Name.Name, arity: fieldArity(fields)}
		}
	}
	out["rawJSON"] = outboundMethod{name: "rawJSON", arity: 8}
	return out
}

// collectEgressCalls 扫描一个文件的全部出站调用（密钥方法按 SelectorExpr 名命中，
// 不看接收者类型：漏判接收者类型会让门禁失明；同名误报可由白名单/人读兜底）。
func collectEgressCalls(af *ast.File, fset *token.FileSet, methods map[string]outboundMethod) []egressCallSite {
	var out []egressCallSite
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
			if !ok {
				return true
			}
			m, ok := methods[sel.Sel.Name]
			if !ok {
				return true
			}
			site := egressCallSite{
				file:   filepath.Base(fset.Position(call.Pos()).Filename),
				fn:     fn.Name.Name,
				method: sel.Sel.Name,
				pos:    fset.Position(call.Pos()),
			}
			if len(call.Args) == m.arity {
				site.lastArg = call.Args[len(call.Args)-1]
			}
			out = append(out, site)
			return true
		})
	}
	return out
}

// isFactoryReceiver 判定方法接收者类型是 Factory（值或指针）。
func isFactoryReceiver(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) == 0 {
		return false
	}
	switch typ := recv.List[0].Type.(type) {
	case *ast.StarExpr:
		id, ok := typ.X.(*ast.Ident)
		return ok && id.Name == "Factory"
	case *ast.Ident:
		return typ.Name == "Factory"
	}
	return false
}

// isDecoratorFuncType 判定形参类型是装饰器签名 func(*http.Request) 或
// func(*websocket.DialOptions)。
func isDecoratorFuncType(expr ast.Expr) bool {
	ft, ok := expr.(*ast.FuncType)
	if !ok || ft.Params == nil || len(ft.Params.List) != 1 {
		return false
	}
	star, ok := ft.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "Request" || sel.Sel.Name == "DialOptions"
}

// fieldArity 计算形参场的展开数量（a, b string 是两个形参）。
func fieldArity(fields []*ast.Field) int {
	n := 0
	for _, f := range fields {
		if len(f.Names) == 0 {
			n++
			continue
		}
		n += len(f.Names)
	}
	return n
}

func methodNames(m map[string]outboundMethod) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// repoRelDir 由测试文件位置推导仓库内目录的绝对路径。
func repoRelDir(t *testing.T, rel string) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Dir(filepath.Dir(filepath.Dir(self))) // internal/proxy → repo root
	return filepath.Join(root, rel)
}
