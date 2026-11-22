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
// 同一枚 *domain.Template（见 mapping.go 的 toDomainAccountWithTemplateScope），
// 安全性完全建立在"query 输出的模板对象视为 immutable"上——派生事实在构造期一次
// 定型（scheduler.newSnapshotStatic），之后谁都不许再写模板字段。
//
// 门禁按**表达式类型**断言，而不是按"函数整体"豁免——「载入器内部」的写必须同样
// 看得见。判据只覆盖通过视图/账号选择子的写：
//
//	X.tpl.<field> = …        / X.tpl.<field> op= …  / X.tpl.<field>++   （.tpl 字段写）
//	X.tpl = …                                                          （.tpl 重绑）
//	X.Template.<field> = …                                             （账号字段写）
//	X.Template = …                                                     （账号重绑）
//
// `<field>` 不必是白名单里的名字：`X.tpl.F` 要能编译就已经证明 F 是 domain.Template
// 的字段，所以"两层选择子"本身就是判据。
//
// 白名单分两类，**都精确到表达式形态**：
//
//  1. 整视图重绑 `X.Template = expr`（不碰字段）：允许出现在工厂与载入入口。
//     载入入口拿到的是共享对象，**整视图重绑**只换账号持有的指针，不写共享对象本身。
//  2. 模板字段写 `X.Template.<field> = …`：只允许出现在"构造新对象"的工厂里，且
//     每个白名单条目必须写明"为什么这里拿到的一定是新鲜对象"。载入入口一律禁止
//     字段写——那里 `X.Template` 指向的是作用域内被多个账号共享的对象。
//
// `X.tpl`（*snapshotStatic 视图）上的写**永不被豁免**：视图不拥有模板，任何写都是
// 对已发布共享对象的就地改写。newSnapshotStatic 的 `tpl: tpl` 是结构体字面量绑定，
// 不是选择子赋值，本就不在判据内。
//
// 边界（写清楚，避免门禁被当成万能证明）：
//   - 只覆盖**通过视图/账号选择子**的写，不做数据流别名分析（`t := av.tpl; t.Models = …`
//     这类逃逸不在语法判据内）。真正的正确性来自"构造期定型"的用例
//     （scheduler.template_share_alias_guard_test.go），本门禁只是防未来的手滑。
//   - 直接对新鲜模板变量写字段（`d := &domain.Template{}; d.Models = …`）是构造，
//     不是别名写，不报（toDomainTemplate 就是这种形状）；判据只抓"经由视图/账号
//     选择子"的写。
//   - 生成代码 internal/ent 不计入：`n.Edges.Template = e`、`c.Template = NewTemplateClient(…)`
//     是行装配/客户端字段，不是模板对象的字段写，且目录由 go generate 拥有。
type templateWriteExemption struct {
	file   string // 相对仓库根的路径
	fn     string // 函数名（比较整名）
	reason string // 一行理由：为什么这里允许这类表达式写
}

// templateWriteWhitelist 是表达式级白名单：重绑与字段写各一张表，语义不同，
// 不能互相顶替。
type templateWriteWhitelist struct {
	rebind     []templateWriteExemption // `X.Template = expr`
	fieldWrite []templateWriteExemption // `X.Template.<field> = …`
}

var templateWrites = templateWriteWhitelist{
	rebind: []templateWriteExemption{
		{
			file: "internal/repository/mapping.go", fn: "toDomainAccountWithTemplateScope",
			reason: "载入工厂：Account.Template 在此首次绑定；允许整视图重绑，但禁止字段写——scope 非 nil 时这里拿到的是作用域共享对象",
		},
		{
			file: "internal/repository/group_repo.go", fn: "loadGroupsAccounts",
			reason: "全量载入入口：作用域由调用方持有，账号模板可能是共享对象；只允许整视图重绑，禁止字段写",
		},
		{
			file: "internal/repository/group_repo.go", fn: "LoadGroupAccounts",
			reason: "单组载入入口（委托 LoadGroupAccountsScoped）：与全量入口同形；只允许整视图重绑，禁止字段写",
		},
		{
			file: "internal/repository/group_repo.go", fn: "LoadGroupAccountsScoped",
			reason: "单组载入的真实实现：同一作用域下模板跨组共享；只允许整视图重绑，禁止字段写",
		},
	},
	// 生产侧当前为空：构造新鲜模板的工厂（toDomainTemplate）直接写 d.<field>，
	// 不走 `X.Template.<field>` 形态，本就不在判据内。留空 = 任何经由账号视图写
	// 模板字段的地方都视为违规；未来若某个"构造全新账号 + 全新模板"的工厂确实要
	// 经 `X.Template.<field>` 初始化，必须在此登记并写明为什么 X.Template 一定是
	// 新对象（而不是作用域里那张共享表里的）。
	fieldWrite: []templateWriteExemption{},
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
	view   string // "tpl" | "Template"
	target string // 渲染后的写目标，便于定位
}

func (w templateWrite) String() string {
	return fmt.Sprintf("%s:%d %s %s %s %s", w.file, w.line, w.fn, w.kind, w.view, w.target)
}

// TestTemplateObjectsStayImmutableAfterLoad 是门禁本体：扫 internal/ + cmd/ + pkg/
// 的全部 Go 文件（含测试，测试按后缀豁免），只允许白名单内的表达式写。
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
	for _, ex := range combinedExemptions(templateWrites) {
		require.NotEmpty(t, ex.reason, "白名单每项都要写一行理由")
		path := filepath.Join(root, filepath.FromSlash(ex.file))
		info, err := os.Stat(path)
		require.NoError(t, err, "白名单文件必须存在：%s", ex.file)
		require.False(t, info.IsDir(), "白名单条目必须是文件：%s", ex.file)
		require.True(t, funcDeclared(t, path, ex.fn),
			"白名单函数必须存在：%s#%s（改名前先改这里，并重新论证理由）", ex.file, ex.fn)
	}

	offending := filterExemptedTemplateWrites(all, templateWrites, "")
	require.Empty(t, offending,
		"载入后的模板对象是不可变共享物：除白名单的**整视图重绑**（工厂/载入入口）与"+
			"构造新鲜对象的**字段写**外，不得就地写模板字段或重绑视图；`X.tpl.<field>` 上的写一律禁止。"+
			"若确需豁免，在 templateWrites 对应类别里登记并写明理由：\n%s", joinTemplateWrites(offending))
}

// TestTemplateImmutabilityGateRejectsInPlaceWrites 是门禁的注入式负例：字符串
// 夹具里造出每一种写形状（字段写、复合赋值、自增、括号解引用、map 元素写、
// 指针重绑、Account.Template 同形），断言全部被认出；同时断言**合法**形状不被
// 误报（新鲜模板的字段构造、无关 map 字面量），并证明白名单按
// (文件, 函数, 表达式类型, 视图) 生效——重绑豁免不掩盖字段写，`.tpl` 视图写永不被豁免。
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

func toDomainTemplate(t *domain.Template) *domain.Template {
	d := &domain.Template{}
	d.Models = nil
	return d
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
	require.NotContains(t, byFn, "toDomainTemplate",
		"工厂对本地新建模板的直写（d.<field>）是构造，不是别名写，不得误报：%v", got)

	kinds := map[string]int{}
	for _, w := range got {
		kinds[w.kind]++
	}
	require.Equal(t, 7, kinds["field-write"], "字段写计数：%v", got)
	require.Equal(t, 2, kinds["rebind"], "重绑计数：%v", got)
	for _, want := range []string{"av.tpl.Models", "(*av.tpl).Name", "av.tpl.UpstreamPaths", "acc.Template.Models"} {
		require.Contains(t, joinTemplateWrites(got), want, "写目标必须被渲染出来便于定位：%v", got)
	}

	// 白名单按 (文件, 函数) 精确生效：同名函数换个文件不豁免。
	require.Len(t, filterExemptedTemplateWrites(got, templateWrites, "internal/repository/mapping.go"), len(got),
		"别的文件里没有这个名字的白名单豁免：%v", got)
	require.Len(t, filterExemptedTemplateWrites(got, templateWrites, "internal/scheduler/fixture.go"), len(got),
		"夹具路径不在白名单里（白名单是精确文件名）")

	// 测试后缀豁免。
	require.Empty(t, filterExemptedTemplateWrites(got, templateWrites, "internal/scheduler/fixture_test.go"),
		"_test.go 里的故意写必须被豁免")

	// 载入入口语义：整视图重绑绿，字段写与任何 `.tpl` 写红。
	const loaderFixture = `package repository

func LoadGroupAccounts(acc *domain.Account, tpl *domain.Template) {
	acc.Template = tpl
	acc.Template.Models = nil
	acc.tpl = tpl
	acc.tpl.Name = "x"
}
`
	fset2 := token.NewFileSet()
	f2, err := parser.ParseFile(fset2, "internal/repository/group_repo.go", loaderFixture, parser.SkipObjectResolution)
	require.NoError(t, err)
	loaderHits := scanTemplateWrites("internal/repository/group_repo.go", fset2, f2)
	require.Len(t, loaderHits, 4)
	loaderOffending := filterExemptedTemplateWrites(loaderHits, templateWrites, "internal/repository/group_repo.go")
	require.Len(t, loaderOffending, 3,
		"载入入口只豁免整视图重绑；字段写 + 两条 .tpl 写必须红：%v", loaderOffending)
	rendered := joinTemplateWrites(loaderOffending)
	require.Contains(t, rendered, "acc.Template.Models", "载入入口不得字段写模板：%v", rendered)
	require.Contains(t, rendered, "acc.tpl", "`.tpl` 视图写永不被豁免：%v", rendered)

	// 字段写豁免只覆盖"构造新鲜对象"的工厂；且仅覆盖 `X.Template.<field>`。
	const factoryFixture = `package repository

func freshAccount(tpl *domain.Template) *domain.Account {
	d := &domain.Account{}
	d.Template = tpl
	d.Template.Models = nil
	d.tpl = tpl
	d.tpl.Name = "x"
	return d
}
`
	fset3 := token.NewFileSet()
	f3, err := parser.ParseFile(fset3, "internal/repository/factory.go", factoryFixture, parser.SkipObjectResolution)
	require.NoError(t, err)
	factoryHits := scanTemplateWrites("internal/repository/factory.go", fset3, f3)
	require.Len(t, factoryHits, 4)

	// 不登记字段写豁免：工厂里的 `d.Template.Models` 也必须红。
	require.Contains(t, joinTemplateWrites(filterExemptedTemplateWrites(factoryHits, templateWrites, "internal/repository/factory.go")),
		"d.Template.Models", "没有字段写豁免时，工厂里的 X.Template.<field> 也必须红")

	// 登记字段写豁免（模拟一个白名单内的构造工厂）：
	//   - `d.Template.Models`（字段写）绿；
	//   - `d.Template = tpl`（重绑）仍红（重绑与字段写是两张表）；
	//   - `d.tpl.*`（视图写）仍红（.tpl 永不被豁免）。
	withFactory := templateWrites
	withFactory.fieldWrite = append(append([]templateWriteExemption(nil), templateWrites.fieldWrite...),
		templateWriteExemption{file: "internal/repository/factory.go", fn: "freshAccount", reason: "测试用：本地新建 d 与模板，字段写构造新对象"})
	factoryOffending := filterExemptedTemplateWrites(factoryHits, withFactory, "internal/repository/factory.go")
	factoryRendered := joinTemplateWrites(factoryOffending)
	require.NotContains(t, factoryRendered, "d.Template.Models", "字段写豁免内的 X.Template.<field> 必须绿：%v", factoryRendered)
	require.Contains(t, factoryRendered, "freshAccount rebind Template d.Template", "重绑豁免与字段写豁免互不顶替：%v", factoryRendered)
	require.Contains(t, factoryRendered, "d.tpl", "`.tpl` 视图写永不被豁免，即使同函数登记了字段写：%v", factoryRendered)
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

// scanTemplateWrites 找出 f 里全部"通过视图/账号选择子写模板"的语句。
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
				kind, view, ok := templateWriteKind(e)
				if !ok {
					continue
				}
				out = append(out, templateWrite{
					file: fileRel, fn: name, line: fset.Position(e.Pos()).Line,
					kind: kind, view: view, target: renderTemplateTarget(e),
				})
			}
			return true
		})
	}
	return out
}

// templateWriteKind 判定 e 是不是"通过 .tpl / .Template 选择子"的写目标，并区分
// 表达式形态（字段写 vs 重绑）与视图（tpl vs Template）。
func templateWriteKind(e ast.Expr) (kind, view string, ok bool) {
	sel, ok := stripWriteTarget(e).(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	if inner, ok := stripWriteTarget(sel.X).(*ast.SelectorExpr); ok &&
		(inner.Sel.Name == "tpl" || inner.Sel.Name == "Template") {
		return "field-write", inner.Sel.Name, true
	}
	if sel.Sel.Name == "tpl" || sel.Sel.Name == "Template" {
		return "rebind", sel.Sel.Name, true
	}
	return "", "", false
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

// combinedExemptions 把两类白名单拼成一份（仅用于锚定函数存在性检查）。
func combinedExemptions(wl templateWriteWhitelist) []templateWriteExemption {
	out := make([]templateWriteExemption, 0, len(wl.rebind)+len(wl.fieldWrite))
	out = append(out, wl.rebind...)
	out = append(out, wl.fieldWrite...)
	return out
}

// exempts 报告 w 是否被白名单豁免。规则是表达式级的：
//   - `_test.go` 一律豁免；
//   - `.tpl` 视图上的写**永不**豁免（视图不拥有模板）；
//   - 重绑只查 rebind 表，字段写只查 fieldWrite 表，两者不互相顶替。
func (wl templateWriteWhitelist) exempts(w templateWrite, file string) bool {
	if strings.HasSuffix(file, "_test.go") {
		return true
	}
	if w.view != "Template" {
		return false
	}
	var list []templateWriteExemption
	switch w.kind {
	case "rebind":
		list = wl.rebind
	case "field-write":
		list = wl.fieldWrite
	default:
		return false
	}
	for _, ex := range list {
		if ex.file == file && ex.fn == w.fn {
			return true
		}
	}
	return false
}

// filterExemptedTemplateWrites 去掉豁免命中：fileOverride 非空时按它判文件
// （负例夹具用它把同一份源码挂到不同"文件"上），空则用命中自带的 file。
func filterExemptedTemplateWrites(all []templateWrite, wl templateWriteWhitelist, fileOverride string) []templateWrite {
	var out []templateWrite
	for _, w := range all {
		file := w.file
		if fileOverride != "" {
			file = fileOverride
		}
		if !wl.exempts(w, file) {
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
