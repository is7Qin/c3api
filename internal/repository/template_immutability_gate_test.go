// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// U22 §2.3 的 immutability 门禁：载入器让同一次查询里同 template_id 的账号共享
// 同一枚 *domain.Template（见 mapping.go 的 toDomainAccountWithTemplateCache），
// 安全性完全建立在"query 输出的模板对象视为 immutable"上——派生事实在构造期一次
// 定型（scheduler.newSnapshotStatic），之后谁都不许再写模板字段。
//
// 门禁按语法断言：除白名单外，生产代码里没有"通过账号视图写模板"的赋值——
//
//	X.tpl.<field> = …   / X.tpl.<field> op= …  / X.tpl.<field>++   （字段写）
//	X.tpl = …                                                   （指针重绑）
//
// 以及 `X.Template.<field>` / `X.Template = …` 的同形写法（accountView 与
// domain.Account 两侧都覆盖）。`<field>` 不必是白名单里的名字：`X.tpl.F` 要能编译
// 就已经证明 F 是 domain.Template 的字段，所以"两层选择子"本身就是判据。
//
// 边界（写清楚，避免门禁被当成万能证明）：
//   - 只覆盖**通过视图/账号选择子**的写，不做数据流别名分析（`t := av.tpl; t.Models = …`
//     这类逃逸不在语法判据内）。真正的正确性来自"构造期定型"的用例
//     （scheduler.template_share_alias_guard_test.go），本门禁只是防未来的手滑。
//   - 生成代码 internal/ent 不计入：`n.Edges.Template = e`、`c.Template = NewTemplateClient(…)`
//     是行装配/客户端字段，不是模板对象的字段写，且目录由 go generate 拥有。
type templateWriteExemption struct {
	file   string // 相对仓库根的路径
	fn     string // 函数名（比较整名）
	reason string // 一行理由：为什么这里允许出现对模板的写
}

var templateWriteExemptions = []templateWriteExemption{
	{
		file: "internal/scheduler/static_key.go", fn: "newSnapshotStatic",
		reason: "构造函数：snapshotStatic.tpl 的唯一绑定点，派生事实在此一次定型；若某天要在构造期原地归一模板，只能在这里登记",
	},
	{
		file: "internal/repository/mapping.go", fn: "toDomainAccountWithTemplateScope",
		reason: "载入器转换点：首次把 ent 模板转成 domain 模板并发布指针（写缓存/Account.Template）；模板对象的生产边界就在这一处",
	},
	{
		file: "internal/repository/group_repo.go", fn: "loadGroupsAccounts",
		reason: "全量载入入口：创建 query-local 模板表并逐账号复用；表是本函数局部变量，跨 reload 不复用",
	},
	{
		file: "internal/repository/group_repo.go", fn: "LoadGroupAccounts",
		reason: "单组载入入口：与全量入口同形的 query-local 表；两条载入路径的指针不变量必须一致",
	},
}

// templateWriteTestExemption 是"测试"这一类豁免的理由（按文件名后缀判定）：
// 别名防护用例必须**故意**就地写模板字段，才能证明已发布的派生事实不会跟着变
// （scheduler.template_share_alias_guard_test.go）；夹具里的视图重绑同理。
const templateWriteTestExemption = "测试故意就地写模板字段/重绑视图，用来证明派生事实在构造期定型——生产代码不得效仿"

// templateWrite 一条违规（或一条被豁免的命中）。
type templateWrite struct {
	file   string // 相对仓库根
	fn     string // 所在函数名（包级为 ""）
	line   int
	kind   string // "field-write" | "rebind"
	target string // 渲染后的写目标，便于定位
}

func (w templateWrite) String() string {
	return fmt.Sprintf("%s:%d %s %s %s", w.file, w.line, w.fn, w.kind, w.target)
}

// TestTemplateObjectsStayImmutableAfterLoad 是门禁本体：扫 internal/ + cmd/ + pkg/
// 的全部 Go 文件（含测试，测试按后缀豁免），只允许白名单内的"通过视图写模板"。
func TestTemplateObjectsStayImmutableAfterLoad(t *testing.T) {
	root := gateRepoRoot(t)
	var all []templateWrite
	var scanned int
	for _, dir := range []string{"internal", "cmd", "pkg"} {
		found, n := scanTemplateWriteTree(t, root, dir)
		all = append(all, found...)
		scanned += n
	}
	require.Greater(t, scanned, 100, "门禁必须真的扫到整个仓库（不是空目录）")
	require.NotEmpty(t, all, "门禁至少要在测试里命中几条（%s）——否则判据写歪了也看不出来", templateWriteTestExemption)

	// 白名单必须锚定在真实存在的函数上：改名/删函数即红，不许静默失效。
	for _, ex := range templateWriteExemptions {
		require.NotEmpty(t, ex.reason, "白名单每项都要写一行理由")
		path := filepath.Join(root, filepath.FromSlash(ex.file))
		info, err := os.Stat(path)
		require.NoError(t, err, "白名单文件必须存在：%s", ex.file)
		require.False(t, info.IsDir(), "白名单条目必须是文件：%s", ex.file)
		require.True(t, funcDeclared(t, path, ex.fn),
			"白名单函数必须存在：%s#%s（改名前先改这里，并重新论证理由）", ex.file, ex.fn)
	}

	offending := filterExemptedTemplateWrites(all, templateWriteExemptions, "")
	require.Empty(t, offending,
		"载入后的模板对象是不可变共享物：除白名单（构造函数 / 载入器转换点 / 两个载入入口）与测试外，不得就地写模板字段或重绑视图。"+
			"若确需豁免，在 templateWriteExemptions 里登记并写明理由：\n%s", joinTemplateWrites(offending))
}

// TestTemplateImmutabilityGateRejectsInPlaceWrites 是门禁的注入式负例：字符串
// 夹具里造出每一种写形状（字段写、复合赋值、自增、括号解引用、map 元素写、
// 指针重绑、Account.Template 同形），断言全部被认出；同时断言**合法**形状不被
// 误报（新鲜模板的字段构造、无关 map 字面量），并证明白名单按 (文件, 函数) 生效、
// 同名函数换个文件仍会报、测试后缀豁免也生效。
func TestTemplateImmutabilityGateRejectsInPlaceWrites(t *testing.T) {
	const fixture = `package scheduler

import "github.com/is7qin/c3api/internal/domain"

func mutateView(av *accountView) {
	av.tpl.Models = nil
	av.tpl.BaseURL += "x"
	(*av.tpl).Name = "n"
	av.tpl.UpstreamPaths["k"] = "v"
	av.tpl = nil
	av.tpl.FormatModels = map[domain.RequestFormat][]string{}
}

func mutateAccount(acc *domain.Account) {
	acc.Template.Models = nil
	acc.Template.StripImageTools = true
	acc.Template = nil
}

func freshTemplate(t *domain.Template) {
	t.Models = nil
	_ = map[int]string{1: "x"}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/scheduler/fixture.go", fixture, parser.SkipObjectResolution)
	require.NoError(t, err)
	got := scanTemplateWrites("internal/scheduler/fixture.go", fset, f)

	byFn := map[string][]templateWrite{}
	for _, w := range got {
		byFn[w.fn] = append(byFn[w.fn], w)
	}
	require.Len(t, byFn["mutateView"], 6, "视图侧：5 条字段写 + 1 条重绑：%v", got)
	require.Len(t, byFn["mutateAccount"], 3, "账号侧：2 条字段写 + 1 条重绑：%v", got)
	require.NotContains(t, byFn, "freshTemplate",
		"新鲜模板的字段赋值不是别名写（构造，不是改写）；无关 map 字面量也不得误报：%v", got)

	kinds := map[string]int{}
	for _, w := range got {
		kinds[w.kind]++
	}
	require.Equal(t, 7, kinds["field-write"], "字段写计数：%v", got)
	require.Equal(t, 2, kinds["rebind"], "重绑计数：%v", got)
	for _, want := range []string{"av.tpl.Models", "(*av.tpl).Name", "av.tpl.UpstreamPaths", "acc.Template.Models"} {
		require.Contains(t, joinTemplateWrites(got), want, "写目标必须被渲染出来便于定位：%v", got)
	}

	// 白名单按 (文件, 函数) 生效：同名函数换个文件不豁免。
	whitelisted := filterExemptedTemplateWrites(got, templateWriteExemptions, "internal/repository/mapping.go")
	require.Len(t, whitelisted, len(got), "别的文件里没有这个名字的白名单豁免：%v", whitelisted)
	exempted := filterExemptedTemplateWrites(got, templateWriteExemptions, "internal/scheduler/fixture.go")
	require.Len(t, exempted, len(got), "夹具路径不在白名单里（白名单是精确文件名）")

	// 测试后缀豁免。
	require.Empty(t, filterExemptedTemplateWrites(got, templateWriteExemptions, "internal/scheduler/fixture_test.go"),
		"_test.go 里的故意写必须被豁免")

	// 命中白名单函数名 + 白名单文件 ⇒ 全部豁免。
	fset2 := token.NewFileSet()
	f2, err := parser.ParseFile(fset2, "internal/repository/mapping.go", `package repository

func toDomainAccountWithTemplateScope(acc *domain.Account) {
	acc.Template.Models = nil
	acc.Template = nil
}
`, parser.SkipObjectResolution)
	require.NoError(t, err)
	hit := scanTemplateWrites("internal/repository/mapping.go", fset2, f2)
	require.Len(t, hit, 2)
	require.Empty(t, filterExemptedTemplateWrites(hit, templateWriteExemptions, "internal/repository/mapping.go"),
		"白名单文件的同名函数必须被豁免：%v", hit)
}

// gateRepoRoot 由本文件的路径反推仓库根（internal/repository/ 上两级）。
func gateRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must locate this test file")
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// scanTemplateWriteTree 递归扫描 root/dir 下的 Go 文件（跳过生成代码 internal/ent）。
func scanTemplateWriteTree(t *testing.T, root, dir string) ([]templateWrite, int) {
	t.Helper()
	var out []templateWrite
	scanned := 0
	base := filepath.Join(root, filepath.FromSlash(dir))
	require.DirExists(t, base)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 生成代码：行/客户端装配里的 `.Template =` 不是模板字段写，见文件头边界说明。
			if d.Name() == "ent" && filepath.Dir(path) == filepath.Join(root, "internal") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		scanned++
		out = append(out, scanTemplateWrites(rel, fset, f)...)
		return nil
	})
	require.NoError(t, err)
	return out, scanned
}

// scanTemplateWrites 找出 f 里全部"通过视图写模板"的语句。
func scanTemplateWrites(fileRel string, fset *token.FileSet, f *ast.File) []templateWrite {
	var out []templateWrite
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			var lhs []ast.Expr
			switch s := n.(type) {
			case *ast.AssignStmt:
				lhs = s.Lhs
			case *ast.IncDecStmt:
				lhs = []ast.Expr{s.X}
			default:
				return true
			}
			for _, e := range lhs {
				kind, ok := templateWriteKind(e)
				if !ok {
					continue
				}
				out = append(out, templateWrite{
					file: fileRel, fn: name, line: fset.Position(e.Pos()).Line,
					kind: kind, target: renderTemplateTarget(e),
				})
			}
			return true
		})
	}
	return out
}

// templateWriteKind 判定 e 是不是"通过 .tpl / .Template 选择子"的写目标。
func templateWriteKind(e ast.Expr) (string, bool) {
	sel, ok := stripWriteTarget(e).(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if inner, ok := stripWriteTarget(sel.X).(*ast.SelectorExpr); ok &&
		(inner.Sel.Name == "tpl" || inner.Sel.Name == "Template") {
		return "field-write", true
	}
	if sel.Sel.Name == "tpl" || sel.Sel.Name == "Template" {
		return "rebind", true
	}
	return "", false
}

// stripWriteTarget 剥掉包在写目标外层的括号/解引用/下标/切片：`(*av.tpl).Name`
// 的 LHS 外层是 SelectorExpr，但 `av.tpl.UpstreamPaths["k"]` 外层是 IndexExpr。
func stripWriteTarget(e ast.Expr) ast.Expr {
	for {
		switch v := e.(type) {
		case *ast.ParenExpr:
			e = v.X
		case *ast.StarExpr:
			e = v.X
		case *ast.IndexExpr:
			e = v.X
		case *ast.SliceExpr:
			e = v.X
		default:
			return e
		}
	}
}

// renderTemplateTarget 把写目标渲染成可读路径（用于失败信息定位）。
func renderTemplateTarget(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return renderTemplateTarget(v.X) + "." + v.Sel.Name
	case *ast.IndexExpr:
		return renderTemplateTarget(v.X) + "[]"
	case *ast.SliceExpr:
		return renderTemplateTarget(v.X) + "[:]"
	case *ast.StarExpr:
		return "*" + renderTemplateTarget(v.X)
	case *ast.ParenExpr:
		return "(" + renderTemplateTarget(v.X) + ")"
	}
	return "<expr>"
}

// filterExemptedTemplateWrites 去掉豁免命中：fileOverride 非空时按它判文件
// （负例夹具用它把同一份源码挂到不同"文件"上），空则用命中自带的 file。
// 生成的代码（internal/ent）与 _test.go 一律豁免，其余按 (文件, 函数) 精确匹配。
func filterExemptedTemplateWrites(all []templateWrite, exempt []templateWriteExemption, fileOverride string) []templateWrite {
	var out []templateWrite
	for _, w := range all {
		file := w.file
		if fileOverride != "" {
			file = fileOverride
		}
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		exempted := false
		for _, ex := range exempt {
			if ex.file == file && ex.fn == w.fn {
				exempted = true
				break
			}
		}
		if !exempted {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// funcDeclared 报告 path 里是否声明了名为 name 的函数/方法。
func funcDeclared(t *testing.T, path, name string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, "parse %s", path)
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return true
		}
	}
	return false
}

// joinTemplateWrites 把命中拼成多行（失败信息里直接可读）。
func joinTemplateWrites(ws []templateWrite) string {
	if len(ws) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		parts = append(parts, w.String())
	}
	return strings.Join(parts, "\n")
}
