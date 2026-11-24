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
// 让 identityfence 反向 alias scheduler，集合门禁**看不见**。本文件把三件事钉成 AST 断言：
//
//	(a) 三个哨兵的 errors.New 与 CandidateFingerprint 的**实现体**必须在本包；
//	(b) internal/scheduler/candidate_identity.go 里这三个名字只能是 `identityfence.ErrX`
//	    的赋值，两个函数体只能是单条 `return identityfence.X(...)`（别名必须是**同一
//	    error 值**，不是 fmt.Errorf 重建——重建会让 errors.Is 判等断裂）；
//	(c) 该文件内不许出现 errors.New，且任何函数体都不许有第二个语句。
//
// 扫描面纪律：模块根由 go.mod 向上查找；两个目录各自**逐个** require.NotEmpty，
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

// TestSdkbridgeStillOwnsSecondSentinelSet 把"世界上还有第二套同名哨兵"这条**现状**钉住。
//
// 为什么必须钉：本文件上面三条断言只证明"identityfence 是 scheduler/rulepersist 这条轴
// 的权威"，它们**证明不了**"全局只有一处"——internal/sdkbridge/failure.go:151-153 仍自持
// 一套同名三哨兵（`sdkbridge:` 前缀，各一枚独立的 errors.New），failure.go:277 用它自己
// 那三枚做 errors.Is。跨轴比较（scheduler 家族的 error × sdkbridge 的判定）今天会
// 静默 false。基线即如此、本单未合并（P5-1 非目标：不动 sdkbridge 的同名哨兵）。
//
// 钉住的形态与 generation_symbol_gate 的词表同款：**现状可见、改动必须显式**。
// P3c 把 sdkbridge 统一到 identityfence 时，这条断言会红——那不是回归，是提醒你把
// 它删掉并更新本注释（以及两个包注释里"已知第二套"的说明）。
func TestSdkbridgeStillOwnsSecondSentinelSet(t *testing.T) {
	root := fenceModuleRoot(t)
	files := fenceParseDir(t, root, "internal/sdkbridge")

	// fenceSentinelNames 只收 `var X = errors.New(...)` 形态：一个名字出现在这里，
	// 就等于"该包**独立自持**了这枚哨兵"，不可能是 identityfence 的别名。
	got := fenceSentinelNames(files)
	want := []string{
		"ErrCandidateFingerprintMismatch",
		"ErrMissingCandidateFingerprint",
		"ErrMissingExpectedRevision",
	}
	require.Subset(t, got, want,
		"sdkbridge 的第二套哨兵必须仍在（P5-1 非目标：不合并）。若已统一到 identityfence，请删除本断言并同步两处包注释")

	// 独立性的第二条证据：sdkbridge 不得 import identityfence（别名会以 import 为前提）。
	for i, f := range files {
		for imp := range fenceImportSet(t, f) {
			require.NotEqual(t, "github.com/is7qin/c3api/internal/identityfence", imp,
				"internal/sdkbridge 的第 %d 个源文件 import 了 identityfence：那说明第二套已被别名取代，"+
					"本断言与两处包注释必须一起更新", i+1)
		}
	}
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
