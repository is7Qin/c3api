// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是 U39「模板凭据真值」的两条 AST 门禁。它们钉住的不是"某处写法好不好看"，
// 而是 spec §0 的唯一不变量：身份投影分支只许由**模板类型**决定，ext 行的类型
// 只在静态通道模板下作为遗留行的兜底（v4 修正 2）。
//
//	G-A：projectAccountIdentity / projectBPSIdentity / projectCodexIdentity 的
//	     函数体内不得出现**第一参数**（ext）的 `.CredentialType` 选择器。
//	     活性锚点：projectBPSIdentity 必须仍含 `validateBPSExt(` 调用（防函数被
//	     搬空/改名后门禁恒绿）。
//
//	G-B：全仓非测试代码里，BPS 位谓词（`.IsBPS()` / `.IsCodex()`）与
//	     `credential.ChannelOf(...)` 作用在**ext 行的凭据类型载体**上，只允许出现
//	     在 assertExtCredentialType 内（注册表 1 行、rowsMax = 1、只许变短）。
//	     载体有两种形态（判据见 extTypeCarrier，两条判据共用）：
//	       - `X.CredentialType`：X 是变量 `ext`，或任一以 `.Ext` / `.ext` 结尾的
//	         选择器（`a.Ext`、`row.ext`、`snap.ext` …——快照合并里用的是小写）；
//	       - 裸载体 `extCredentialType`（含 `row.extCredentialType` 选择器形态）：
//	         `row.extCredentialType.IsBPS()` 与 `credential.ChannelOf(row.extCredentialType)`
//	         （accountExtSnapshotRow 新增的类型载体，不与 `.CredentialType` 同现）。
//	     活性锚点：该函数体必须含 ErrCredentialTypeMismatch。
//	     **不约束**普通的 `.CredentialType` 读（列搬运、序列化、写面 setter、
//	     导入查重全部合法），也不约束模板侧（`tpl.CredentialType`、
//	     `d.Template.CredentialType`）。
//
// 判据只有一份（gate* 函数）：门禁本体与自测喂同一个函数，自测不得另写一份判定。

// gateScanDirs 是 G-B 的扫描面：承载凭据读写的全部手写包。
var gateScanDirs = []string{"internal", "cmd", "pkg"}

// gateSkipGenerated 是生成代码（ent 客户端/行装配）：它不含凭据分支，且由 go
// generate 拥有——扫描它只会把门禁绑到生成器版本上。
const gateSkipGenerated = "internal/ent"

// u39GateFile 一个已解析的源文件（非测试、非生成）。
type u39GateFile struct {
	rel  string // 相对仓库根的斜杠路径
	fset *token.FileSet
	file *ast.File
}

// u39GateFiles 解析 root 下 dirs 的全部非测试、非生成 Go 文件。
func u39GateFiles(t *testing.T, root string, dirs []string) []u39GateFile {
	t.Helper()
	var out []u39GateFile
	for _, dir := range dirs {
		base := filepath.Join(root, filepath.FromSlash(dir))
		require.DirExists(t, base, "扫描目录必须存在：%s", dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == gateSkipGenerated {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".gen.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			out = append(out, u39GateFile{rel: rel, fset: fset, file: f})
			return nil
		})
		require.NoError(t, err, "扫描 %s 失败", dir)
	}
	require.NotEmpty(t, out, "扫描面不得为空（空转的门禁比没有门禁更危险）")
	return out
}

// funcDecl 在 src 里找名为 name 的函数声明。
func funcDecl(src u39GateFile, name string) *ast.FuncDecl {
	for _, decl := range src.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// funcContainsIdent 报告函数体内是否出现名为 name 的标识符（含选择器的字段名
// 与包级标识符——活性锚点两种形态都算数）。
func funcContainsIdent(fn *ast.FuncDecl, name string) bool {
	if fn == nil || fn.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// funcContainsCall 报告函数体内是否调用了名为 name 的函数（`name(`）。
func funcContainsCall(fn *ast.FuncDecl, name string) bool {
	if fn == nil || fn.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// ---- G-A：身份投影函数不得读第一参数的 .CredentialType ----

// projectIdentityBranchFuncs 是 G-A 的作用域（spec J3：分支在调用方按模板类型选，
// 三个投影函数体内不许再看 ext 的类型）。
var projectIdentityBranchFuncs = []string{
	"projectAccountIdentity",
	"projectBPSIdentity",
	"projectCodexIdentity",
}

// projectBPSAnchor 是 G-A 的活性锚点：BPS 投影必须仍做结构性校验。
const projectBPSAnchor = "validateBPSExt"

// firstParamCredentialTypeReads 返回 fn 函数体内"第一参数.CredentialType"的位置。
// 只认**第一参数名**作为接收者：`branchType.IsBPS()`、`env[...]` 都不在此列。
func firstParamCredentialTypeReads(rel string, fset *token.FileSet, fn *ast.FuncDecl) []string {
	if fn == nil || fn.Body == nil || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return nil
	}
	first := fn.Type.Params.List[0]
	if len(first.Names) == 0 {
		return nil
	}
	name := first.Names[0].Name
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "CredentialType" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
			out = append(out, fmtSite(rel, fset.Position(sel.Pos()).Line))
		}
		return true
	})
	return out
}

// fmtSite 渲染 `<file>:<line>`。
func fmtSite(rel string, line int) string { return rel + ":" + strconv.Itoa(line) }

// TestProjectIdentityNeverBranchesOnExtType 是 G-A 本体。
func TestProjectIdentityNeverBranchesOnExtType(t *testing.T) {
	root := gateRepoRoot(t)
	src := u39GateFiles(t, root, []string{"internal/repository"})
	var violations []string
	for _, name := range projectIdentityBranchFuncs {
		var found *ast.FuncDecl
		var fset *token.FileSet
		var rel string
		for _, f := range src {
			if fn := funcDecl(f, name); fn != nil {
				found, fset, rel = fn, f.fset, f.rel
				break
			}
		}
		require.NotNil(t, found, "G-A 作用域函数必须存在：%s（改名/删除先改这里）", name)
		violations = append(violations, firstParamCredentialTypeReads(rel, fset, found)...)
	}
	require.Empty(t, violations,
		"身份投影函数体内不得读第一参数（ext）的 .CredentialType：分支只许由模板类型决定。命中：\n%s",
		strings.Join(violations, "\n"))

	// 活性锚点：BPS 投影仍做结构性校验（否则函数可以被搬空而门禁恒绿）。
	var bps *ast.FuncDecl
	for _, f := range src {
		if fn := funcDecl(f, "projectBPSIdentity"); fn != nil {
			bps = fn
			break
		}
	}
	require.NotNil(t, bps, "projectBPSIdentity 必须存在")
	require.True(t, funcContainsCall(bps, projectBPSAnchor),
		"G-A 活性锚点：projectBPSIdentity 必须仍调用 %s（函数被搬空时门禁必须红）", projectBPSAnchor)
}

// TestProjectIdentityGateSelfTest 是 G-A 的注入式负例：合成源码里"读第一参数类型"
// 必须被认出，"读分支参数"与"读第二参数"必须放行。
func TestProjectIdentityGateSelfTest(t *testing.T) {
	const body = `package repository

func projectAccountIdentity(ext *domain.AccountExt, env *domain.IdentityEnvelope, branchType credential.Type) error {
	if branchType.IsBPS() { // 分支参数：允许
		return nil
	}
	return nil
}

func projectBPSIdentity(ext *domain.AccountExt, env *domain.IdentityEnvelope) error {
	_ = ext.CredentialType // 违规：读第一参数的类型
	return nil
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/repository/synthetic.go", body, parser.SkipObjectResolution)
	require.NoError(t, err)
	src := u39GateFile{rel: "internal/repository/synthetic.go", fset: fset, file: f}

	require.Empty(t, firstParamCredentialTypeReads(src.rel, fset, funcDecl(src, "projectAccountIdentity")),
		"读 branchType 不是读 ext 的类型：不得误报")
	require.Len(t, firstParamCredentialTypeReads(src.rel, fset, funcDecl(src, "projectBPSIdentity")), 1,
		"第一参数的 .CredentialType 必须被认出")
}

// ---- G-B：ext 行的 BPS 位只许在 assertExtCredentialType 里读 ----

// extBranchSiteExemption 是 G-B 的唯一注册行：允许在 ext 行的类型上问 BPS 位的
// 函数。anchor 是活性锚点（函数体必须仍含该标识符），保证"注册行"不是空壳。
type extBranchSiteExemption struct {
	file   string
	fn     string
	anchor string
}

// extBranchSites 是 G-B 的注册表：**1 行、rowsMax = 1、只许变短**。
var extBranchSites = []extBranchSiteExemption{
	{
		file:   "internal/repository/mapping.go",
		fn:     "assertExtCredentialType",
		anchor: "ErrCredentialTypeMismatch",
	},
}

// extBranchSitesMax 是注册表行数上限（spec J6：注册表 1 行、rowsMax = 1）。
const extBranchSitesMax = 1

// extTypeCarrier 判定一个表达式是不是"ext 行的凭据类型载体"。它是 G-B 的**唯一**
// 载体判据，谓词收方与 ChannelOf 实参两条判据共用：
//
//   - 裸载体 `extCredentialType`（含 `row.extCredentialType` 选择器形态）——
//     accountExtSnapshotRow 新增的类型载体，不与 `.CredentialType` 同现；
//   - `X.CredentialType`，且 X 是变量 `ext` 或任一以 `.Ext` / `.ext` 结尾的选择器。
//     小写 `.ext` 是快照合并（`row.ext` / `snap.ext`）里真实使用的形态；漏掉它
//     正是本门禁第一次落地的假绿根因（`row.ext.CredentialType.IsBPS()` 漏判）。
//
// 模板侧（`tpl.CredentialType`、`d.Template.CredentialType`）不是载体。
func extTypeCarrier(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "extCredentialType"
	case *ast.SelectorExpr:
		if e.Sel.Name == "extCredentialType" {
			return true
		}
		if e.Sel.Name != "CredentialType" {
			return false
		}
		switch owner := e.X.(type) {
		case *ast.Ident:
			return owner.Name == "ext"
		case *ast.SelectorExpr:
			return owner.Sel.Name == "Ext" || owner.Sel.Name == "ext"
		}
	}
	return false
}

// extBranchSite 一处命中：文件名 + 所在函数 + 行号。
type extBranchSite struct {
	file string
	fn   string
	line int
}

func (s extBranchSite) String() string {
	return s.file + ":" + strconv.Itoa(s.line) + " " + s.fn
}

// extBranchSitesIn 枚举 src 中作用在 ext 行类型上的 BPS 位判定。
func extBranchSitesIn(src u39GateFile) []extBranchSite {
	var out []extBranchSite
	for _, decl := range src.file.Decls {
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
			switch sel.Sel.Name {
			case "IsBPS", "IsCodex":
				if extTypeCarrier(sel.X) {
					out = append(out, extBranchSite{src.rel, fn.Name.Name, src.fset.Position(call.Pos()).Line})
				}
			case "ChannelOf":
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "credential" || len(call.Args) != 1 {
					return true
				}
				if extTypeCarrier(call.Args[0]) {
					out = append(out, extBranchSite{src.rel, fn.Name.Name, src.fset.Position(call.Pos()).Line})
				}
			}
			return true
		})
	}
	return out
}

// TestExtCredentialTypeOnlyReadByAssertion 是 G-B 本体。
func TestExtCredentialTypeOnlyReadByAssertion(t *testing.T) {
	root := gateRepoRoot(t)
	require.LessOrEqual(t, len(extBranchSites), extBranchSitesMax,
		"G-B 注册表只许变短（rowsMax = %d）：新增豁免必须先论证为什么它仍然是'唯一的 BPS 位读点'", extBranchSitesMax)

	allowed := make(map[string]bool, len(extBranchSites))
	for _, ex := range extBranchSites {
		require.NotEmpty(t, strings.TrimSpace(ex.file), "注册行必须写明文件")
		require.NotEmpty(t, strings.TrimSpace(ex.fn), "注册行必须写明函数")
		require.FileExists(t, filepath.Join(root, filepath.FromSlash(ex.file)), "注册文件必须存在：%s", ex.file)
		require.True(t, funcDeclared(t, filepath.Join(root, filepath.FromSlash(ex.file)), ex.fn),
			"注册函数必须存在：%s#%s（改名/删除先改注册表，并重新论证豁免）", ex.file, ex.fn)
		require.NotEmpty(t, strings.TrimSpace(ex.anchor), "注册行必须写明活性锚点：%s#%s", ex.file, ex.fn)
		allowed[ex.file+"#"+ex.fn] = true
	}

	srcs := u39GateFiles(t, root, gateScanDirs)
	var violations []extBranchSite
	for _, src := range srcs {
		for _, site := range extBranchSitesIn(src) {
			if allowed[site.file+"#"+site.fn] {
				continue
			}
			violations = append(violations, site)
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
	rendered := make([]string, 0, len(violations))
	for _, v := range violations {
		rendered = append(rendered, v.String())
	}
	require.Empty(t, violations,
		"ext 行的 BPS 位只许在 assertExtCredentialType 里读（G-B）。命中：\n%s", strings.Join(rendered, "\n"))

	// 活性锚点：注册函数必须仍承载"两侧类型不一致"这个事实。
	anchorFound := false
	for _, src := range srcs {
		for _, ex := range extBranchSites {
			if src.rel != ex.file {
				continue
			}
			if fn := funcDecl(src, ex.fn); fn != nil && funcContainsIdent(fn, ex.anchor) {
				anchorFound = true
			}
		}
	}
	require.True(t, anchorFound, "G-B 活性锚点：assertExtCredentialType 必须仍含 ErrCredentialTypeMismatch")
}

// TestExtCredentialTypeGateSelfTest 是 G-B 的注入式负例矩阵。**每一种载体形态都必须
// 有自己的形状**：门禁第一次落地的自测只覆盖了 `ext` 与 `a.Ext` 两形，于是
// `row.ext.CredentialType.IsBPS()`（小写 `.ext`，快照合并的真实形态）与
// `row.extCredentialType.IsBPS()`（本单新增的裸载体）两种漏判都没被发现。
//
// 红（7 形）：`ext`、`a.Ext`、`av.acc.Ext`、`row.ext`、`row.extCredentialType`
// 的谓词/ChannelOf 收方；绿（4 形）：模板侧两形、普通比较、非 ext 载体谓词。
func TestExtCredentialTypeGateSelfTest(t *testing.T) {
	const body = `package repository

import "github.com/is7qin/c3api/internal/credential"

func redExt(ext *domain.AccountExt) bool { return ext.CredentialType.IsBPS() }

func redEdge(a *domain.Account) bool { return a.Ext.CredentialType.IsCodex() }

func redNestedEdge(av *accountView) bool { return av.acc.Ext.CredentialType.IsCodex() }

func redRowExt(row *accountExtSnapshotRow) bool { return row.ext.CredentialType.IsBPS() }

func redSnapExt(snap *accountExtSnapshot) bool { return snap.ext.CredentialType.IsCodex() }

func redCarrierPredicate(row *accountExtSnapshotRow) bool {
	return row.extCredentialType.IsBPS()
}

func redExtChannel(ext *domain.AccountExt) credential.Channel {
	return credential.ChannelOf(ext.CredentialType)
}

func redCarrierChannel(row *accountExtSnapshotRow) credential.Channel {
	return credential.ChannelOf(row.extCredentialType)
}

func greenTemplate(tpl *domain.Template) bool { return tpl.CredentialType.IsBPS() }

func greenNestedTemplate(d *domain.Account) bool { return d.Template.CredentialType.IsCodex() }

func greenCompare(ext *domain.AccountExt) bool { return ext.CredentialType == credential.TypeBPS }

func greenPlain(acc *domain.Account) bool { return acc.CredentialType.IsBPS() }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "internal/repository/synthetic.go", body, parser.SkipObjectResolution)
	require.NoError(t, err)
	sites := extBranchSitesIn(u39GateFile{rel: "internal/repository/synthetic.go", fset: fset, file: f})

	byFn := map[string]int{}
	for _, s := range sites {
		byFn[s.fn]++
	}
	// 红：所有 ext 载体形态（含小写 `.ext` 与裸 `extCredentialType`）。
	for _, fn := range []string{
		"redExt", "redEdge", "redNestedEdge", "redRowExt", "redSnapExt",
		"redCarrierPredicate", "redExtChannel", "redCarrierChannel",
	} {
		require.Equal(t, 1, byFn[fn], "%s 必须被认出一条：%v", fn, sites)
	}
	// 绿：模板侧、普通比较、非 ext 载体——一条都不许误报。
	for _, fn := range []string{"greenTemplate", "greenNestedTemplate", "greenCompare", "greenPlain"} {
		require.NotContains(t, byFn, fn, "%s 不得误报：%v", fn, sites)
	}
}
