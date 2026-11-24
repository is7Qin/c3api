// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package identityfence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 归属门禁（P5-1，R2 的 D5(a)）：**canonical owner 必须机械可见**。
//
// 为什么需要它：generation_symbol_gate_test.go 以**集合**枚举 revision 命名的标识符，
// 而 alias 与定义同名——`var ErrMissingExpectedRevision = identityfence.ErrMissingExpectedRevision`
// 在那种口径下只计一次，集合相等照样绿。也就是说：把三个 errors.New 搬回 scheduler、
// 让 identityfence 反向 alias scheduler，集合门禁**看不见**。本文件把四件事钉成 AST 断言：
//
//	(a) 三个哨兵的 errors.New 与 CandidateFingerprint 的**实现体**必须在本包；
//	(b) internal/scheduler/candidate_identity.go 里这三个名字只能是 `identityfence.ErrX`
//	    的赋值，两个函数体只能是单条 `return identityfence.X(...)`（别名必须是**同一
//	    error 值**，不是 fmt.Errorf 重建——重建会让 errors.Is 判等断裂）；
//	(c) 该文件内不许出现 errors.New，且任何函数体都不许有第二个语句；
//	(d) internal/sdkbridge/failure.go 里这三个名字同样只能是 `identityfence.ErrX` 的
//	    别名，且整包不再自建它们（P3c 前的反方向现状钉死已按 P5-1 v3.3 §G 删除）。
//
// 扫描面纪律：模块根由 go.mod 向上查找；每处扫描目录都 require.NotEmpty，
// 否则路径写错（或目录被清空）会让全部断言在空集合上恒绿。

// fenceModuleRoot 从本文件向上找含 go.mod 的目录。找不到即 FailNow。
func fenceModuleRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) 必须可用")
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "向上未找到 go.mod：模块根定位失败")
		dir = parent
	}
}

// fenceParseDir 解析 dir 下全部**非测试** .go。目录必须存在且真的解析到文件。
func fenceParseDir(t *testing.T, root, rel string) []*ast.File {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	ents, err := os.ReadDir(abs)
	require.NoError(t, err, "扫描目录必须存在：%s", rel)
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(abs, n), nil, parser.SkipObjectResolution)
		require.NoError(t, perr, "解析失败：%s/%s", rel, n)
		out = append(out, f)
	}
	require.NotEmpty(t, out, "扫描面不得为空（目录写错会让本门禁恒绿）：%s", rel)
	return out
}

// fenceFunc 在 files 里找**包级**函数（Recv == nil）定义；missing 时 FailNow。
func fenceFunc(t *testing.T, files []*ast.File, name string) *ast.FuncDecl {
	t.Helper()
	var hits []*ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != name {
				continue
			}
			hits = append(hits, fn)
		}
	}
	require.Len(t, hits, 1, "%s 必须是本包里**恰好一处**的包级函数定义（0 = 定义不在本包；2 = 权威分裂）", name)
	return hits[0]
}

// fenceSentinelNames 收集 `var X = errors.New(...)` 形式的包级哨兵名。
func fenceSentinelNames(files []*ast.File) []string {
	var out []string
	isErrorsNew := func(e ast.Expr) bool {
		ce, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "errors" && sel.Sel.Name == "New"
	}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				if isErrorsNew(vs.Values[0]) {
					out = append(out, vs.Names[0].Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// fenceHasSelector 判断 file 内是否出现 pkg.Sel 形态的选择器。
func fenceHasSelector(n ast.Node, pkg, sel string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		se, ok := x.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := se.X.(*ast.Ident)
		if ok && id.Name == pkg && se.Sel.Name == sel {
			found = true
		}
		return true
	})
	return found
}

// fenceImportSet 返回 file 的 import 路径集合（去引号）。
func fenceImportSet(t *testing.T, f *ast.File) map[string]bool {
	t.Helper()
	out := make(map[string]bool, len(f.Imports))
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err, "import 字面量必须可解引号")
		out[p] = true
	}
	return out
}

// TestCanonicalOwnershipLivesHere 是判据 (a)：三个 errors.New 与 CandidateFingerprint
// 的实现体必须在本包，且实现体必须**真的消费**自己那一枚哨兵（alias 到别处 = 换 owner）。
func TestCanonicalOwnershipLivesHere(t *testing.T) {
	root := fenceModuleRoot(t)
	files := fenceParseDir(t, root, "internal/identityfence")

	want := []string{
		"ErrCandidateFingerprintMismatch",
		"ErrMissingCandidateFingerprint",
		"ErrMissingExpectedRevision",
	}
	require.Equal(t, want, fenceSentinelNames(files),
		"identityfence 必须恰好持这三枚哨兵：少一枚 ⇒ 哨兵搬走了；多一枚 ⇒ 新哨兵必须显式登记")

	fn := fenceFunc(t, files, "CandidateFingerprint")
	require.NotNil(t, fn.Body, "CandidateFingerprint 必须有函数体（不是外部链接的声明）")
	require.True(t, fenceHasSelector(fn, "domain", "AccountCandidateFingerprint"),
		"CandidateFingerprint 必须经 domain.AccountCandidateFingerprint 派生（单一事实源）")
	require.True(t, fenceHasSelector(fn, "domain", "CandidateFPHex"),
		"CandidateFingerprint 必须用 domain.CandidateFPHex 出线格式（不许本地重写 hex）")

	// 实现体必须把**本包的** ErrMissingCandidateFingerprint 交给 fmt.Errorf 做 wrap。
	// 这是"哨兵与实现同源"的证据：把哨兵 alias 到别处时这里立刻不成立。
	wrapped := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != "fmt" || sel.Sel.Name != "Errorf" {
			return true
		}
		for _, a := range ce.Args {
			if ai, ok := a.(*ast.Ident); ok && ai.Name == "ErrMissingCandidateFingerprint" {
				wrapped = true
			}
		}
		return true
	})
	require.True(t, wrapped,
		"CandidateFingerprint 必须 wrap 本包的 ErrMissingCandidateFingerprint（缺身份字段的分类不许退化）")
}

// TestSchedulerKeepsOnlyAliasesAndForwards 是判据 (b)：scheduler 侧只剩别名 + 单条转发。
func TestSchedulerKeepsOnlyAliasesAndForwards(t *testing.T) {
	root := fenceModuleRoot(t)
	files := fenceParseDir(t, root, "internal/scheduler")
	target := fenceFileByName(t, root, "internal/scheduler", "candidate_identity.go")

	// 三个名字：恰好一处 ValueSpec，值是 identityfence.<同名>。
	aliased := map[string]int{}
	for _, d := range target.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			sel, ok := vs.Values[0].(*ast.SelectorExpr)
			if !ok {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "identityfence" {
				continue
			}
			require.Equal(t, vs.Names[0].Name, sel.Sel.Name,
				"别名必须指向同名哨兵（%s = identityfence.%s）：换名意味着两枚不同的 error 值",
				vs.Names[0].Name, vs.Names[0].Name)
			aliased[vs.Names[0].Name]++
		}
	}
	require.Equal(t, map[string]int{
		"ErrMissingCandidateFingerprint":  1,
		"ErrCandidateFingerprintMismatch": 1,
		"ErrMissingExpectedRevision":      1,
	}, aliased,
		"三个哨兵在 scheduler 里必须**恰好各有一次** `var ErrX = identityfence.ErrX`：0 = 名字被搬走或改指；2 = 别名分裂")

	// 两个函数：单条 return identityfence.CandidateFingerprint(a)。
	for _, name := range []string{"candidateFingerprint", "CandidateFingerprint"} {
		fn := fenceFunc(t, files, name)
		require.Len(t, fn.Body.List, 1,
			"%s 只能是单条转发（出现第二个语句 = 又有了一份实现）", name)
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		require.True(t, ok, "%s 的唯一语句必须是 return", name)
		// 纯转发的形状是 `return identityfence.CandidateFingerprint(a)`：一个 Results
		// 元素、两个返回值（Go 允许把多值调用直接 return）。若有人拆成
		// `fp, err := …; return fp, err`，Body.List 就变成 2 条，上面那条先红。
		require.Len(t, ret.Results, 1, "%s 必须 return 转发调用本身（不是拆开再拼回去）", name)
		ce, ok := ret.Results[0].(*ast.CallExpr)
		require.True(t, ok, "%s 必须 return 一次调用（不是常量/字段）", name)
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		require.True(t, ok, "%s 的 return 必须调用包级函数", name)
		id, ok := sel.X.(*ast.Ident)
		require.True(t, ok, "%s 的转发必须写成 identityfence.X(...)", name)
		require.Equal(t, "identityfence", id.Name, "%s 必须转发到 identityfence", name)
		require.Equal(t, "CandidateFingerprint", sel.Sel.Name, "%s 必须转发到 CandidateFingerprint", name)
	}
}

// TestSchedulerSideHasNoSecondImplementation 是判据 (c)：该文件既没有 errors.New，
// 也没有任何"两个语句"的函数体。import 面收窄到 domain + identityfence 是最强的一条：
// 没有 errors 就没有第二份哨兵，没有 fmt 就没有 wrap 重建。
func TestSchedulerSideHasNoSecondImplementation(t *testing.T) {
	root := fenceModuleRoot(t)
	f := fenceFileByName(t, root, "internal/scheduler", "candidate_identity.go")

	errorsNew := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == "errors" && sel.Sel.Name == "New" {
			errorsNew++
		}
		return true
	})
	require.Zero(t, errorsNew,
		"candidate_identity.go 不得出现 errors.New（第二份哨兵 ⇒ errors.Is 判等与新值无关）")

	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		require.Len(t, fn.Body.List, 1,
			"%s 只能有单条语句：兼容层是转发，不是实现", fn.Name.Name)
	}

	allowed := map[string]bool{
		"github.com/is7qin/c3api/internal/domain":        true,
		"github.com/is7qin/c3api/internal/identityfence": true,
	}
	got := fenceImportSet(t, f)
	require.NotEmpty(t, got, "兼容层必须至少 import identityfence（空 import 面 = 转发被删而不是被改）")
	require.Contains(t, got, "github.com/is7qin/c3api/internal/identityfence",
		"兼容层必须 import identityfence：别名的主语在那里")
	for imp := range got {
		require.True(t, allowed[imp],
			"candidate_identity.go 只许 import domain + identityfence，发现 %s（errors/fmt 回流即第二份实现）", imp)
	}
}

// TestSdkbridgeAliasesIdentityfenceSentinels 是 P3c 后的**反向**断言：sdkbridge 不再
// 自持第二套同名哨兵——三个导出名必须是 identityfence canonical 的变量别名（同一
// error 值），且不得再以自建 error 的形态出现。
//
// 为什么要机械钉住：跨包 `errors.Is` 成立与否**只**取决于两边是不是同一个值。把别名
// 换回本包自建（哪怕文案照抄）会让跨轴判等重新静默 false，而编译、行为测试、以及本
// 文件的 canonical 唯一性断言都不会红——只有 AST 看得见。
//
// 本用例的前身是 TestSdkbridgeStillOwnsSecondSentinelSet（P5-1 v3.3 §G 的现状钉死，
// 当时写明"P3c 统一时显式删除"）；它现在钉的是同一件事的反面，判据 (b) 的 sdkbridge
// 版本（见 TestSchedulerKeepsOnlyAliasesAndForwards）。
func TestSdkbridgeAliasesIdentityfenceSentinels(t *testing.T) {
	root := fenceModuleRoot(t)
	files := fenceParseDir(t, root, "internal/sdkbridge")
	target := fenceFileByName(t, root, "internal/sdkbridge", "failure.go")

	// (1) 三个名字：在 failure.go 里恰好各一处 `var ErrX = identityfence.ErrX`。
	aliased := map[string]int{}
	for _, d := range target.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			sel, ok := vs.Values[0].(*ast.SelectorExpr)
			if !ok {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "identityfence" {
				continue
			}
			require.Equal(t, vs.Names[0].Name, sel.Sel.Name,
				"别名必须指向同名哨兵（%s = identityfence.%s）：换名意味着两枚不同的 error 值",
				vs.Names[0].Name, vs.Names[0].Name)
			aliased[vs.Names[0].Name]++
		}
	}
	require.Equal(t, map[string]int{
		"ErrMissingCandidateFingerprint":  1,
		"ErrCandidateFingerprintMismatch": 1,
		"ErrMissingExpectedRevision":      1,
	}, aliased,
		"sdkbridge 的三个身份哨兵必须**恰好各有一次** `var ErrX = identityfence.ErrX`：0 = 名字被搬走或改指；2 = 别名分裂")

	// (2) 三个名字不得再落在 `var X = errors.New(...)` 形态里。扫**整包**而不只是
	// failure.go：换个文件重建同样是第二份（fenceSentinelNames 只收那一种形态，
	// 命中即等于"该包独立自持了这枚哨兵"）。
	got := fenceSentinelNames(files)
	for name := range aliased {
		require.NotContains(t, got, name,
			"%s 仍由 sdkbridge 自建：别名被换回自建值 ⇒ 跨包 errors.Is 会重新静默 false", name)
	}

	// (3) 别名的前提：主语必须真的在 import 面里。
	require.Contains(t, fenceImportSet(t, target), "github.com/is7qin/c3api/internal/identityfence",
		"failure.go 必须 import identityfence：三个别名的值只能来自那里（没有 import 就没有同一 error 值）")
}

// fenceFileByName 解析 dir 下的单个文件（相对模块根的 relDir）。
func fenceFileByName(t *testing.T, root, relDir, name string) *ast.File {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relDir), name)
	src, err := os.ReadFile(path)
	require.NoError(t, err, "被钉的文件必须存在：%s/%s", relDir, name)
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	require.NoError(t, perr, "解析失败：%s/%s", relDir, name)
	return f
}
