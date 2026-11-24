// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是 U40（planKey 的 provider 事实折叠）的**形状**门禁。判据的分布是刻意的：
//
//   - 字段集与顺序：static_key_test.go 的 declaredPlanKeyFields（22 项，唯一一份
//     golden，不在这里再写一份）；
//   - 读点与键字段的对应：static_read_closure_test.go 的 ①–⑤；
//   - 值与接线：static_facts_test.go 的敏感度/不变性/单射；
//   - 字节：static_facts_kat_test.go 的 KAT（+ testdata/facts_digest_vectors.py）。
//
// 这里只放**上面几条都管不到**的形状：provider 命名禁令、[32]byte 人口普查、
// 编码器在唯一构造器里的接线（两条独立 AST 断言 + 符号活性锚点）、以及
// codexEmail 的永久读点注册表。
//
// 为什么这几条不能由前面几条推出：字段集断言会说"多了 codexFoo 字段"（红），但不会
// 说"这个字段名按 provider 命名"（换 provider 就得再改一次键的形状——本单元要消灭
// 的正是这件事）；读闭包门禁会说"读了没登记的路径"，但一次**被丢弃**的读取
// （`_ = ext.CodexEmail`）在它眼里与一次真正的装配没有区别。

// planKeyProviderPrefixes 是 provider 命名的禁止前缀（大小写不敏感）。
//
// 判据是**前缀**而不是具体名字：`codexAccountID` / `codexInstallation` / `bpsProfileDigest`
// 这些具体名字的枚举会腐化（删了一个、加了一个都得改表），而"字段名不得以 provider
// 开头"是一条与 provider 数量无关的结构规则。
var planKeyProviderPrefixes = []string{"codex", "bps"}

// planKeyProviderPrefixExemptions 是前缀禁令的**豁免登记表**：字段名 → 为什么它不是
// provider 事实槽位。
//
// 今天恰好一条，且它不是本单元的遗留：`codexResponsesTransport` 是模板侧出站传输
// 开关（spec §5），对所有通道同一含义——名字带 codex 是因为它只对 codex responses
// 端点有意义，不是"又给某个 provider 加了一格键字段"。U40 的折叠目标是按 provider
// 命名的**身份/凭据事实**（那五枚），不是所有含 provider 字样的模板开关；把这条改名
// 属于另一件事（字段改名会牵动 read-closure 表与冻结 oracle），不在本单元范围。
//
// 断言是**相等**（双向）：新增一个带前缀的字段必须显式登记并写理由（否则红），
// 字段被删/改名后旧条目立刻变陈旧（也红）。
var planKeyProviderPrefixExemptions = map[string]string{
	"codexResponsesTransport": "模板侧出站传输开关（spec §5），对所有通道同一含义；不是 provider 身份/凭据事实，U40 不折叠它",
}

// planKeyDigestFields 是 planKey 里 [32]byte 摘要字段的**全集**（人口普查的 golden）。
// 摘要字段是这里唯一允许的复合事实载体（值语义、可比较）；新增一枚就是新增一个
// "进键的事实族"，必须有意为之。
var planKeyDigestFields = []string{
	"modelsDigest",
	"formatModelsKey",
	"supportedFormats",
	"modelMappingKey",
	"upstreamPathsKey",
	"groupIDsDigest",
	"factsDigest",
}

// planKeyField 是 AST 层的一个字段（名字 + 类型表达式，够本文件全部判据用）。
type planKeyField struct {
	name string
	typ  ast.Expr
}

// planKeyFieldsOf 按**实际源码**取 planKey 的字段序（不经过反射：反射拿不到
// "字段被声明成什么形状"，而 provider 命名禁令要看的正是源码里的名字）。
func planKeyFieldsOf(t *testing.T, srcs []gateFile) []planKeyField {
	t.Helper()
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "planKey" {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				out := make([]planKeyField, 0, len(st.Fields.List))
				for _, fld := range st.Fields.List {
					for _, nm := range fld.Names {
						out = append(out, planKeyField{name: nm.Name, typ: fld.Type})
					}
				}
				require.NotEmpty(t, out, "planKey 必须有字段")
				return out
			}
		}
	}
	require.FailNow(t, "扫描面上必须存在 planKey")
	return nil
}

// TestPlanKeyHasNoProviderNamedFields 是 provider 命名禁令：planKey 的字段名不得
// 以 `codex` / `bps` 开头（大小写不敏感）。
//
// 为什么这条是本单元的核心形状：字段名按 provider 命名 ⇒ 字段数 O(providers)，
// 且 provider 知识漏进调度核心键。加 provider 只该改 ProviderFacts 的槽位与
// static_facts.go 的编码段——一旦有人重新加回 `codexFoo string`，这条立刻红。
func TestPlanKeyHasNoProviderNamedFields(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	fields := planKeyFieldsOf(t, srcs)

	var offending []string
	exempted := map[string]bool{}
	for _, f := range fields {
		lower := strings.ToLower(f.name)
		var hit string
		for _, prefix := range planKeyProviderPrefixes {
			if strings.HasPrefix(lower, prefix) {
				hit = prefix
				break
			}
		}
		if hit == "" {
			continue
		}
		why, ok := planKeyProviderPrefixExemptions[f.name]
		if !ok {
			offending = append(offending, f.name+"（前缀 "+hit+"）")
			continue
		}
		require.NotEmpty(t, strings.TrimSpace(why), "豁免必须写明理由：%s", f.name)
		exempted[f.name] = true
	}
	require.Empty(t, offending,
		"planKey 不得有按 provider 命名的字段：provider 事实一律折叠进 factsDigest（ProviderFacts 槽位 + static_facts.go 编码段）；违规：%v", offending)
	// 陈旧豁免：字段被删/改名后，登记表不得继续声称"它还在且已审阅"。
	for name := range planKeyProviderPrefixExemptions {
		require.True(t, exempted[name],
			"陈旧豁免：%s 已不是 planKey 里带 provider 前缀的字段（删掉这条登记）", name)
	}

	// 非空性：判据不能靠"什么都没扫到"通过。
	require.GreaterOrEqual(t, len(fields), 20, "扫描面必须真的取到 planKey 的字段（实际：%d）", len(fields))
}

// TestPlanKeyDigestFieldCensus 是 [32]byte 摘要字段的人口普查：恰好 7 枚，且其中
// 一枚是 factsDigest。
//
// 钉的是"摘要字段的族"：它们是键里唯一允许的复合事实载体（值语义、可比较）。
// 少一枚说明某个事实改用了别的载体（string/切片），多一枚说明新加了一个事实族。
func TestPlanKeyDigestFieldCensus(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	fields := planKeyFieldsOf(t, srcs)

	var digests []string
	for _, f := range fields {
		if !is32ByteArrayType(f.typ) {
			continue
		}
		digests = append(digests, f.name)
	}
	sort.Strings(digests)
	want := append([]string(nil), planKeyDigestFields...)
	sort.Strings(want)
	require.Equal(t, want, digests,
		"planKey 的 [32]byte 摘要字段集变了：新增一枚就是新增一个进键的事实族，必须有意改这张表")
	require.Len(t, digests, 7, "[32]byte 摘要字段恰好 7 枚")
	require.Contains(t, digests, "factsDigest", "factsDigest 必须在普查里（它是本单元的折叠点）")
}

// is32ByteArrayType 报告 e 是否裸 `[32]byte`。
func is32ByteArrayType(e ast.Expr) bool {
	at, ok := unwrapASTParen(e).(*ast.ArrayType)
	if !ok || at.Len == nil {
		return false
	}
	lit, ok := at.Len.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT || lit.Value != "32" {
		return false
	}
	elt, ok := at.Elt.(*ast.Ident)
	return ok && elt.Name == "byte"
}

func unwrapASTParen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// TestFactsDigestIsWiredOnceInConstructor 是两条**独立**的 AST 断言（刻意不合成
// 一条）+ 一条符号活性锚点：
//
//	① newSnapshotStatic 里 `FactsDigest(` 恰好出现 1 次；
//	② newSnapshotStatic 里对 `.factsDigest` 的赋值恰好 1 次；
//	③ FactsDigest 这个符号在非测试源码里至少被引用 1 次（防"搬空"后恒绿）。
//
// 为什么不合成一条：`d := FactsDigest(...); k.factsDigest = d` 是合法写法，合成
// 一条（例如"同一语句里既有调用又有赋值"）会误杀它；而两条独立断言各自钉一件
// 事——编码器只在这里被调用一次、摘要只在这里被写一次。
//
// 接收者不限（`k.factsDigest = …` 与 `av.key.factsDigest = …` 都算）：字段名在包内
// 唯一，按名字数比按局部变量名数更抗重命名。
func TestFactsDigestIsWiredOnceInConstructor(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	fn := funcDeclOf(t, srcs, "newSnapshotStatic")

	calls := 0
	assigns := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := unwrapCallFun(x.Fun).(*ast.Ident); ok && id.Name == "FactsDigest" {
				calls++
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				sel, ok := unwrapASTParen(lhs).(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "factsDigest" {
					assigns++
				}
			}
		case *ast.IncDecStmt:
			if sel, ok := unwrapASTParen(x.X).(*ast.SelectorExpr); ok && sel.Sel.Name == "factsDigest" {
				assigns++
			}
		}
		return true
	})
	require.Equal(t, 1, calls,
		"newSnapshotStatic 里 FactsDigest( 必须恰好出现 1 次（实际 %d）：读点只在这一处，编码器是纯值函数", calls)
	require.Equal(t, 1, assigns,
		"newSnapshotStatic 里 .factsDigest 必须恰好被赋值 1 次（实际 %d）：多一处就是第二次派生，键与叶会分叉", assigns)

	// ③ 符号活性锚点：函数体被搬空/调用被删后，上面两条会因为"扫不到"而失去意义
	// （找不到函数会 FailNow，但"函数还在、只是不再被调用"不会）。这里数**引用**。
	refs := countSymbolReferences(t, root, srcs, "FactsDigest")
	require.GreaterOrEqual(t, refs, 1,
		"FactsDigest 必须仍被引用（否则它已沦为死代码：编码器在，键里的事实却没人写）")
	require.GreaterOrEqual(t, refs, calls,
		"符号引用的计数必须覆盖构造器里那一处调用（引用 %d < 调用 %d）", refs, calls)
}

// countSymbolReferences 数「非测试源码里指向 symbol 的引用」：函数/类型声明名本身
// 不算（声明不是使用），其余标识符出现都算。
func countSymbolReferences(t *testing.T, root string, srcs []gateFile, symbol string) int {
	t.Helper()
	declNames := map[*ast.Ident]bool{}
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == symbol {
					declNames[d.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.Name == symbol {
							declNames[s.Name] = true
						}
					case *ast.ValueSpec:
						for _, nm := range s.Names {
							if nm.Name == symbol {
								declNames[nm] = true
							}
						}
					}
				}
			}
		}
	}
	count := 0
	for _, src := range srcs {
		ast.Inspect(src.file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != symbol || declNames[id] {
				return true
			}
			count++
			return true
		})
	}
	require.NotEmpty(t, srcs, "扫描面不得为空（%s）", root)
	return count
}

// codexEmailReadSites 是**永久**的 CodexEmail 读点注册表：`dir/file.go#Func → 理由`。
//
// 今天为**空**，且必须保持为空。理由：`planKey` 不再承载 email（它不是决策输入），
// 叶子复用后 `Selection.Ext` 是指向**旧**快照的指针——将来任何数据面读它都会拿到
// 陈旧值，而键不再设防。发现这条断言红了，正确处置是删掉那次读取，不是往表里加一行。
//
// 覆盖面：internal/proxy（热路径）、internal/sdkbridge（上游适配）、internal/scheduler
// （本包）的非测试代码。管理面（handler/service/repository）的 email 读写不在内——
// 那是 DB 行的字段，与叶子复用无关。
var codexEmailReadSites = map[string]string{}

// codexEmailScanDirs 是 codexEmail 读点门禁的扫描面。
var codexEmailScanDirs = []string{"internal/proxy", "internal/sdkbridge", "internal/scheduler"}

// codexEmailReadsOf 枚举扫描面里对 `.CodexEmail` 的选择器读取：`dir/file.go#Func` → 计数。
// 复合字面量的键（`domain.AccountExt{CodexEmail: …}`）是裸 Ident，不算读点。
func codexEmailReadsOf(t *testing.T, root string, dirs []string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, dir := range dirs {
		srcs := gateSources(t, root, []string{dir})
		for _, src := range srcs {
			for _, decl := range src.file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body == nil {
						continue
					}
					hits := 0
					ast.Inspect(d.Body, func(n ast.Node) bool {
						if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "CodexEmail" {
							hits++
						}
						return true
					})
					if hits > 0 {
						out[fmt.Sprintf("%s/%s#%s", dir, src.name, d.Name.Name)] += hits
					}
				case *ast.GenDecl:
					hits := 0
					ast.Inspect(d, func(n ast.Node) bool {
						if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "CodexEmail" {
							hits++
						}
						return true
					})
					if hits > 0 {
						out[fmt.Sprintf("%s/%s#(package scope)", dir, src.name)] += hits
					}
				}
			}
		}
	}
	return out
}

// codexEmailReadViolations 是判据本体（空 = 合规）：观测集与注册表**相等**。
//
// 相等（双向）而不是"观测 ⊆ 注册表"：注册表腐化成"曾经读过"同样是缺陷——它会让
// 下一个人以为那条读取还在，从而在错误的假设上做决定。
func codexEmailReadViolations(observed map[string]int, registry map[string]string) []string {
	var out []string
	for site, hits := range observed {
		if _, ok := registry[site]; !ok {
			out = append(out, fmt.Sprintf(
				"未登记的 CodexEmail 读点：%s（%d 处）——planKey 不再承载 email，叶子复用后 Selection.Ext 指向旧快照，读它会拿到陈旧值；正确处置是删掉这次读取", site, hits))
		}
	}
	for site, why := range registry {
		if strings.TrimSpace(why) == "" {
			out = append(out, fmt.Sprintf("注册表缺理由：%s", site))
		}
		if observed[site] == 0 {
			out = append(out, fmt.Sprintf("陈旧条目：注册表声称 %s 读 CodexEmail，但扫描面上它已不再读", site))
		}
	}
	sort.Strings(out)
	return out
}

// TestCodexEmailIsNotReadInDataPlane 正例：三个数据面/调度包的非测试代码里不得出现
// `.CodexEmail` 读点（今天应为空）。
func TestCodexEmailIsNotReadInDataPlane(t *testing.T) {
	root := gateRoot(t)
	observed := codexEmailReadsOf(t, root, codexEmailScanDirs)
	require.Empty(t, codexEmailReadViolations(observed, codexEmailReadSites),
		"CodexEmail 读点注册表必须与实际情况**相等**（今天为空）：观测到 %v", observed)
	require.Empty(t, codexEmailReadSites, "注册表今天必须为空")
}

// TestCodexEmailGateRejectsReads 负例：把 oracle 描述的两种读法当真实源码解析，
// 走同一条扫描链再交给同一个判据。只加正例等于自证。
func TestCodexEmailGateRejectsReads(t *testing.T) {
	readsOf := func(body string) map[string]int {
		t.Helper()
		src := "package scheduler\n\nfunc injected(ext *domain.AccountExt) {\n" + body + "\n}\n"
		f, err := parser.ParseFile(token.NewFileSet(), "injected.go", src, parser.SkipObjectResolution)
		require.NoError(t, err)
		require.Len(t, f.Decls, 1)
		fn, ok := f.Decls[0].(*ast.FuncDecl)
		require.True(t, ok, "注入的源码必须只有一处函数声明")
		hits := 0
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "CodexEmail" {
				hits++
			}
			return true
		})
		if hits == 0 {
			return nil
		}
		return map[string]int{"injected.go#injected": hits}
	}

	t.Run("直接读（装配）", func(t *testing.T) {
		got := codexEmailReadViolations(readsOf("_ = derefString(ext.CodexEmail)"), codexEmailReadSites)
		require.Len(t, got, 1, "只该报一条未登记读点：%v", got)
		require.Contains(t, got[0], "未登记的 CodexEmail 读点")
		require.Contains(t, got[0], "陈旧值", "红点必须说明为什么不能读（叶子复用后的陈旧指针）")
	})
	t.Run("丢弃式读（_ =）也拦", func(t *testing.T) {
		got := codexEmailReadViolations(readsOf("_ = ext.CodexEmail"), codexEmailReadSites)
		require.NotEmpty(t, got, "`_ = ext.CodexEmail` 同样是读点：门禁管形状，不管值有没有被用")
	})
	t.Run("复合字面量的键不算读点", func(t *testing.T) {
		require.Empty(t, readsOf(`_ = &domain.AccountExt{CodexEmail: new("e")}`),
			"`{CodexEmail: …}` 是裸 Ident 键，不是选择器读取")
	})
	t.Run("对照：读别的字段不报", func(t *testing.T) {
		require.Empty(t, readsOf("_ = ext.CodexAccountID"), "只钉 CodexEmail")
	})
	t.Run("注册表腐化", func(t *testing.T) {
		registry := map[string]string{"ghost.go#ghost": "历史遗留"}
		got := codexEmailReadViolations(nil, registry)
		require.Len(t, got, 1, "只该报一条陈旧条目：%v", got)
		require.Contains(t, got[0], "陈旧条目")
	})
}

// TestCredentialEquivalenceTakesKeysFromConstructor 是等价类测试的**入口**门禁：
// static_key_credential_test.go 的 1125 组必须仍经 newSnapshotStatic 取键，
// 且不得直接调用 FactsDigest。
//
// 为什么这条是承重的：那 1125 组断言的是「新键相等 ⟺ 改前进键的 (上游键, pat, 类型)
// 逐字节相等」。一旦有人把取键入口换成 `FactsDigest(...)`，测的就不再是**键**，
// 而是编码器的一个函数——凭据面以外的维度（模板/账号/出口/组）整个逃出等价类，
// 而所有断言仍会绿。
func TestCredentialEquivalenceTakesKeysFromConstructor(t *testing.T) {
	root := gateRoot(t)
	path := filepath.Join(root, "internal", "scheduler", "static_key_credential_test.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, "等价类测试文件必须存在：%s", path)

	var directFactsDigest, buildCalls []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := unwrapCallFun(call.Fun).(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name {
			case "FactsDigest":
				directFactsDigest = append(directFactsDigest, fmt.Sprintf("%s:%d", fn.Name.Name, fset.Position(call.Pos()).Line))
			case "newSnapshotStatic":
				buildCalls = append(buildCalls, fn.Name.Name)
			}
			return true
		})
	}
	require.Empty(t, directFactsDigest,
		"等价类测试不得直接调用 FactsDigest（%v）：取键入口必须留在 newSnapshotStatic，否则等价类测的不再是键", directFactsDigest)
	require.GreaterOrEqual(t, len(buildCalls), 2,
		"等价类测试必须仍经 newSnapshotStatic 取键（实际 %d 处）", len(buildCalls))
}

// 本题的编排说明（写给下一个人）：上面五条各自钉一件互不重叠的事——
//   - 名字：不得按 provider 命名（否则加 provider 就改键的形状）；
//   - 人口：摘要字段恰好 7 枚（否则事实族的载体被悄悄换掉）；
//   - 接线：编码器只调一次、摘要只写一次、符号仍被引用（否则键与叶分叉或编码器成死代码）；
//   - 读点：CodexEmail 在数据面永久为零（否则叶子复用带来陈旧读）；
//   - 入口：等价类测试仍从唯一构造器取键（否则那 1125 组测的不是键）。
//
// 判据"值真的进了编码"不在这里：那是 static_facts_test.go 的敏感度用例（门禁管读点，
// 用例管接线——M4 的已知边界正是这条分工的产物）。
