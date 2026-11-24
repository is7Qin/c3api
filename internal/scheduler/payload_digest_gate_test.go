// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是 P3a 的两条 **AST 形状门禁**（H1/H2）+ 一条接线门禁。它们与行为用例
// （payload_key_test.go 的 H3/H4）的分工是刻意的：
//
//   - H3/H4 管"值对不对"：改源对象不动键、就地轮转判不等。
//   - H1 管"比较期有没有现场派生"：`staticKeyOf` 体内不得出现 sha256 /
//     payloadDigestOf / FactsDigest，且被调名字集**恰好**是
//     {planKeyOf, payloadKeyOf, specOfDomain, Key}。现场算摘要的实现在 H3/H4 下
//     仍会绿（它算得对），但它把"构造期一次定型"变成了"比较期再次派生"——键与叶
//     从此有两条派生路径，任何一侧漏写都只会在生产上以陈旧读的形式出现。
//   - H2 管"缓存有没有人读"：`payloadKeyOf` 体内零调用、零 `$S.acc` 路径，语句形状
//     恰好是"nil 判空 + return av.payload"。这是 M3（缓存写了没人读）的唯一抓点：
//     那种实现下 H3/H4 全绿，静态读闭包 ④ 的读者集也仍可能满足（如果构造函数同时
//     还读明文）——只有形状会红。

// staticKeyOfBannedIdents 是 staticKeyOf 体内**不得出现**的标识符（按名字，含做值
// 用的裸标识符与选择器的根：`sha256.Sum256` 的 `sha256` 就是裸 Ident）。
var staticKeyOfBannedIdents = []string{"sha256", "payloadDigestOf", "FactsDigest"}

// staticKeyOfAllowedCalls 是 staticKeyOf 允许**直接调用**的名字全集（相等断言，双向）。
//
// `Key` 是 `egress.Spec.Key()`——出口规格的 16 字节摘要，值方法、无分配，且它本来就是
// "判据期从原料重算出口摘要"这条既有设计的载体（见 staticKeyOf 的注释）；它不是本次
// 折叠的对象，也不读凭据。
var staticKeyOfAllowedCalls = map[string]string{
	"planKeyOf":    "决策输入投影：读回构造期定型的 av.key（含 factsDigest）",
	"payloadKeyOf": "载荷投影：读回构造期定型的 av.payload（载荷摘要）",
	"specOfDomain": "出口规格转换：*domain.Proxy → egress.Spec，判据期允许读原料",
	"Key":          "`egress.Spec.Key()`：出口规格的 16 字节摘要（值方法，不读凭据）",
}

// callNameOf 取一次调用的可辨认名字：包内/跨包函数取名字，方法取方法名，函数字面量
// 与非平凡被调表达式取哨兵值（它们同样要被"未登记"分支拦下来）。Go 内建与预声明
// 类型转换不算调用（否则白名单要为每次 `uint64(x)` 加一行噪音）。
func callNameOf(call *ast.CallExpr) string {
	switch fn := unwrapCallFun(call.Fun).(type) {
	case *ast.Ident:
		if goBuiltinCalls[fn.Name] {
			return ""
		}
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.FuncLit:
		return "<func-literal>"
	default:
		return "<unknown-callee>"
	}
}

// staticKeyOfBodyViolations 是 H1 的判据本体（空 = 合规）。
func staticKeyOfBodyViolations(fn *ast.FuncDecl) []string {
	var out []string
	banned := map[string]bool{}
	seen := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			for _, name := range staticKeyOfBannedIdents {
				if x.Name == name && !banned[name] {
					banned[name] = true
					out = append(out, fmt.Sprintf(
						"staticKeyOf 体内出现 %s：比较期不得现场算摘要——摘要只在 newSnapshotStatic 构造期定型，比较期只读回缓存", name))
				}
			}
		case *ast.CallExpr:
			if name := callNameOf(x); name != "" {
				seen[name] = true
			}
		}
		return true
	})
	for name := range seen {
		if _, ok := staticKeyOfAllowedCalls[name]; !ok {
			out = append(out, fmt.Sprintf(
				"staticKeyOf 调用了未登记的名字 `%s`：比较期只许 %v——现场派生会让键与叶从两条路径生成",
				name, sortedKeys(staticKeyOfAllowedCalls)))
		}
	}
	for _, name := range sortedKeys(staticKeyOfAllowedCalls) {
		if !seen[name] {
			out = append(out, fmt.Sprintf("白名单陈旧：staticKeyOf 不再调用 `%s`（原登记：%s）",
				name, staticKeyOfAllowedCalls[name]))
		}
	}
	sort.Strings(out)
	return out
}

// TestStaticKeyOfComputesNoDigest 是 H1 的正例：真实源码里 staticKeyOf 的体内不得
// 出现任何摘要计算，被调名字集必须**恰好**等于白名单。
func TestStaticKeyOfComputesNoDigest(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	fn := funcDeclOf(t, srcs, "staticKeyOf")
	require.Empty(t, staticKeyOfBodyViolations(fn),
		"staticKeyOf 体内不得现场派生摘要（H1）")
	for name, why := range staticKeyOfAllowedCalls {
		require.NotEmpty(t, strings.TrimSpace(why), "调用白名单必须写明理由：%s", name)
	}
}

// payloadKeyOfReadPath 是 payloadKeyOf 唯一允许读的路径（构造期缓存的载荷摘要）。
const payloadKeyOfReadPath = snapRootAlias + ".payload"

// payloadKeyOfBodyViolations 是 H2 的判据本体（空 = 合规）：
//
//	① 体内零调用表达式（`derefString(...)` / `payloadDigestOf(...)` / `ext.…()` 都不许）；
//	② 读路径**恰好**是 {$S.payload}（出现 `$S.acc.*` 就说明比较期仍在碰源对象）；
//	③ 语句形状恰好两句：`if av == nil { return payloadKey{} }` + `return av.payload`。
//
// 三条缺一不可：① 挡"再派生"，② 挡"仍在读明文"，③ 挡"多出一段逻辑"（两句话之外的
// 每一行都是新的判据，必须在门禁与评审面上显形）。
func payloadKeyOfBodyViolations(fn *ast.FuncDecl) []string {
	var out []string

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			out = append(out, fmt.Sprintf(
				"payloadKeyOf 体内出现调用表达式 `%s`：载荷摘要只读回缓存，不许再派生或解引用",
				types.ExprString(call)))
		}
		return true
	})

	roots := snapRoots(fn)
	paths := snapPaths(fn, roots, snapAliases(fn, roots))
	for p := range paths {
		if p != payloadKeyOfReadPath {
			out = append(out, fmt.Sprintf(
				"payloadKeyOf 读到 %s：它只许读回 %s（出现 $S.acc 路径意味着比较期仍在碰源对象/凭据原文）",
				p, payloadKeyOfReadPath))
		}
	}
	if !paths[payloadKeyOfReadPath] {
		out = append(out, fmt.Sprintf(
			"payloadKeyOf 没读 %s：缓存写了没人读 ⇒ 比较期仍在现场读凭据原文", payloadKeyOfReadPath))
	}

	stmts := fn.Body.List
	if len(stmts) != 2 {
		out = append(out, fmt.Sprintf(
			"payloadKeyOf 只允许两句（nil 判空 + return %s），实际 %d 句：多一句就是又一段派生逻辑",
			payloadKeyOfReadPath, len(stmts)))
		return out
	}
	ifStmt, ok := stmts[0].(*ast.IfStmt)
	if !ok {
		out = append(out, "payloadKeyOf 的第一句必须是 nil 判空（`if av == nil { return payloadKey{} }`）")
	} else {
		if ifStmt.Init != nil || ifStmt.Else != nil {
			out = append(out, "payloadKeyOf 的 nil 判空不得带 init/else 分支")
		}
		bin, isBin := unwrapASTParen(ifStmt.Cond).(*ast.BinaryExpr)
		if !isBin || bin.Op != token.EQL || !isNilIdent(bin.Y) {
			out = append(out, "payloadKeyOf 的判空条件必须是 `av == nil`")
		}
		if len(ifStmt.Body.List) != 1 {
			out = append(out, "payloadKeyOf 的 nil 分支只允许一句 return 零值")
		}
	}
	ret, ok := stmts[len(stmts)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		out = append(out, fmt.Sprintf("payloadKeyOf 最后一句必须是单个结果的 return（`return %s`）", payloadKeyOfReadPath))
		return out
	}
	if r, parts, ok := snapChain(ret.Results[0]); !ok || roots[r] != snapRootAlias || strings.Join(parts, ".") != "payload" {
		out = append(out, fmt.Sprintf("payloadKeyOf 的返回值必须是 %s（实际：%s）",
			payloadKeyOfReadPath, types.ExprString(ret.Results[0])))
	}
	return out
}

// isNilIdent 报告 e 是否是裸 `nil`。
func isNilIdent(e ast.Expr) bool {
	id, ok := unwrapASTParen(e).(*ast.Ident)
	return ok && id.Name == "nil"
}

// TestPayloadKeyOfReadsOnlyTheCache 是 H2 的正例：payloadKeyOf 的形状必须恰好是
// "nil 判空 + return av.payload"。
func TestPayloadKeyOfReadsOnlyTheCache(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	require.Empty(t, payloadKeyOfBodyViolations(funcDeclOf(t, srcs, "payloadKeyOf")),
		"payloadKeyOf 必须只读回构造期缓存的载荷摘要（H2）")
}

// injectedFunc 把一段**函数体源码**解析成 `func name(av *snapshotStatic) <result>`，
// 供 H1/H2 的负例走与正例完全相同的判据。
func injectedFunc(t *testing.T, name, result, body string) *ast.FuncDecl {
	t.Helper()
	src := "package scheduler\n\nfunc " + name + "(av *snapshotStatic) " + result + " {\n" + body + "\n}\n"
	f, err := parser.ParseFile(token.NewFileSet(), "injected.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	require.Len(t, f.Decls, 1)
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	require.True(t, ok, "注入的源码必须只有一处函数声明")
	return fn
}

// TestStaticKeyOfGateRejectsOnTheFlyDigest 是 H1 的负例：把两种"现场算摘要"的写法当
// 真实源码解析出来，走同一条判据。只加正例等于自证。
func TestStaticKeyOfGateRejectsOnTheFlyDigest(t *testing.T) {
	const compliant = `
	k := staticKey{planKey: planKeyOf(av), payloadKey: payloadKeyOf(av)}
	if av != nil {
		k.planKey.proxyDigest = specOfDomain(av.acc.Proxy).Key()
	}
	return k`
	require.Empty(t, staticKeyOfBodyViolations(injectedFunc(t, "staticKeyOf", "staticKey", compliant)),
		"对照：合规形状（三个调用 + 方法 Key）不得被拒")

	t.Run("比较期现算载荷摘要（M4/M7 的可编译形态）", func(t *testing.T) {
		got := staticKeyOfBodyViolations(injectedFunc(t, "staticKeyOf", "staticKey", `
	k := staticKey{planKey: planKeyOf(av), payloadKey: payloadKey{payloadDigest: payloadDigestOf("a", "b", 0)}}
	return k`))
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "payloadDigestOf")
		require.Contains(t, joined, "现场算摘要")
		require.Contains(t, joined, "未登记的名字", "被调名字集也必须红（否则白名单形同虚设）")
	})

	t.Run("直接调 sha256", func(t *testing.T) {
		got := staticKeyOfBodyViolations(injectedFunc(t, "staticKeyOf", "staticKey", `
	k := staticKey{planKey: planKeyOf(av), payloadKey: payloadKeyOf(av)}
	_ = sha256.Sum256(nil)
	return k`))
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "sha256")
		require.Contains(t, joined, "现场算摘要")
	})

	t.Run("把投影整体换成 FactsDigest", func(t *testing.T) {
		got := staticKeyOfBodyViolations(injectedFunc(t, "staticKeyOf", "staticKey", `
	k := staticKey{planKey: planKeyOf(av)}
	k.payloadKey.payloadDigest = FactsDigest(ProviderFacts{}, "", "")
	return k`))
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "FactsDigest")
		require.Contains(t, joined, "payloadKeyOf", "少调用也必须红（白名单陈腐）")
	})
}

// TestPayloadKeyOfGateRejectsOnTheFlyRead 是 H2 的负例：M3（缓存写了没人读）的两种
// 可编译形态。H3/H4 在它们下面都会是绿的——这正是 H2 存在的理由。
func TestPayloadKeyOfGateRejectsOnTheFlyRead(t *testing.T) {
	const compliant = `
	if av == nil {
		return payloadKey{}
	}
	return av.payload`
	require.Empty(t, payloadKeyOfBodyViolations(injectedFunc(t, "payloadKeyOf", "payloadKey", compliant)),
		"对照：合规形状不得被拒")

	t.Run("缓存写了没人读（仍读 av.acc.Ext）", func(t *testing.T) {
		got := payloadKeyOfBodyViolations(injectedFunc(t, "payloadKeyOf", "payloadKey", `
	if av == nil {
		return payloadKey{}
	}
	_ = derefString(av.acc.Ext.CodexOAuthToken)
	return av.payload`))
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "$S.acc.Ext.CodexOAuthToken", "必须点名越界读的路径")
		require.Contains(t, joined, "调用表达式", "derefString 是调用，必须红")
		require.Contains(t, joined, "只允许两句", "三句话就说明多了一段逻辑")
	})

	t.Run("不读缓存（现场算摘要）", func(t *testing.T) {
		got := payloadKeyOfBodyViolations(injectedFunc(t, "payloadKeyOf", "payloadKey", `
	if av == nil {
		return payloadKey{}
	}
	return payloadKey{payloadDigest: payloadDigestOf("a", "b", 0)}`))
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "payloadDigestOf")
		require.Contains(t, joined, "$S.payload", "红点必须指出该读哪个缓存位点")
	})

	t.Run("多一句话", func(t *testing.T) {
		got := payloadKeyOfBodyViolations(injectedFunc(t, "payloadKeyOf", "payloadKey", `
	if av == nil {
		return payloadKey{}
	}
	k := av.payload
	return k`))
		require.NotEmpty(t, got, "局部变量也算一段逻辑：形状必须恰好两句")
		require.Contains(t, strings.Join(got, "\n"), "只允许两句")
	})
}

// TestPayloadDigestIsWiredOnceInConstructor 是接线门禁（与 FactsDigest 那条同形）：
//
//	① newSnapshotStatic 里 `payloadDigestOf(` 恰好出现 1 次；
//	② newSnapshotStatic 里对 `.payload` 的赋值恰好 1 次；
//	③ payloadDigestOf 这个符号在非测试源码里至少被引用 1 次（防"搬空"后恒绿）。
//
// 不合成一条的理由与 U40 相同：`d := payloadDigestOf(...); av.payload = payloadKey{d}`
// 是合法写法，合成一条会误杀；而两条各自钉一件事——编码器只在这里被调用一次、缓存
// 位点只在这里被写一次。多一处就是第二次派生，键与叶会分叉。
func TestPayloadDigestIsWiredOnceInConstructor(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	fn := funcDeclOf(t, srcs, "newSnapshotStatic")

	calls, assigns := 0, 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := unwrapCallFun(x.Fun).(*ast.Ident); ok && id.Name == "payloadDigestOf" {
				calls++
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := unwrapASTParen(lhs).(*ast.SelectorExpr); ok && sel.Sel.Name == "payload" {
					assigns++
				}
			}
		}
		return true
	})
	require.Equal(t, 1, calls,
		"newSnapshotStatic 里 payloadDigestOf( 必须恰好出现 1 次（实际 %d）：读点只在这一处，编码器是纯值函数", calls)
	require.Equal(t, 1, assigns,
		"newSnapshotStatic 里 .payload 必须恰好被赋值 1 次（实际 %d）：多一处就是第二次派生，键与叶会分叉", assigns)

	refs := countSymbolReferences(t, root, srcs, "payloadDigestOf")
	require.GreaterOrEqual(t, refs, 1,
		"payloadDigestOf 必须仍被引用（否则它已沦为死代码：编码器在，缓存却没人写）")
}
