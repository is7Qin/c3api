// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package credential

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

// ─── 本文件钉什么 ────────────────────────────────────────────────────────────
//
// 钉：五个消费层目录里对三个**凭据类型谓词**的调用点，**逐文件精确计数，只许变短**。
// 谓词是 IsCodex / IsBPS / IsResponsesProvider——S3b 保留它们的签名（84 处调用点零
// 改动），但每次新增调用都是「同一策略又写了一遍」：加一个凭据类型时，这些调用点
// 各自判断、各自可能漏改。通道分发（ChannelOf）是**新增代码**的方向；本门禁是既有
// 调用点的**预算**，不是禁令。
//
// 口径（oracle 定，别改）：统计非测试 Go 文件里 SelectorExpr.Sel 名 ∈ 三个谓词，
// **包括**普通调用 `x.IsBPS()`、方法值 `f := x.IsBPS`（不带括号）、作为参数传递
// `g(x.IsBPS)`。`switch` 里的 `case credential.TypeBPS` 不是这三个名字，**不**计。
//
// 为什么判据落在 SelectorExpr 上而不是 CallExpr 上：方法值形式带括号前就已经是选择器，
// 按调用判会漏（这条被 mutation ② 现场取证）。
//
// 精度：**不按名字盲计**。路线 A（go/types 确认 selector 属于 credential.Type）实测
// 超时被否——`go list -export -deps` 需要 476 个包的 export data：冷 GOCACHE 24.1s、
// 改过任一被扫包后（本门禁自身的日常场景）10.5s、全热 0.55s，远超 1.5s 门槛；同时会
// 让本仓库测试套件第一次依赖 `go` 可执行文件（全仓 *_test.go 目前零 os/exec）。
// 故按 brief 回退路线 B，两道护栏补精度：
//
//	① 只扫「**包** import 了 internal/credential」的目录。注意粒度必须是包而不是文件：
//	   `tpl.CredentialType.IsBPS()` 不需要在调用文件 import credential（类型来自
//	   domain），repository/service 里 27/16 处调用分别只有 13/9 处落在 import 了
//	   credential 的文件里——按文件过滤会静默漏掉一半以上。
//	② 伴随断言 assertOnlyCredentialTypeDeclaresPredicates：仓库内**只有**
//	   credential.Type 声明这三个方法名（receiver 方法、接口方法、函数类型字段都扫）。
//	   一旦别处出现同名方法/接口，目录里的 `x.IsBPS` 就不再等价于「凭据谓词调用点」，
//	   门禁先红在护栏上，而不是给出一个错误的计数。
//
// 已知残余（名字计数的固有边界，靠 review，不假装已覆盖）：反射 MethodByName、字符串
// 拼接 / fmt.Sprintf 构造的方法名、跨仓库（依赖模块）里的同名方法。
//
// 预算只许改小：predicateBudgetTotal == 各目录 predicateBudgetCeiling 之和 ==
// predicateBudgetSites 清单总和，三条相等断言在 assertPredicateBudgetRegistry 里。加一处
// 调用必须同时改清单与常量（diff 里看得见）；删一处调用而不收紧预算同样红
// （mutation ③：预算表是**清单**，不是上限）。

// credentialImportPath 是护栏 ① 的目标包：只有 import 了它的**包**才参与名字计数。
const credentialImportPath = "github.com/is7qin/c3api/internal/credential"

// predicateBudgetNames 是三个凭据类型谓词的方法名。它们都是 credential.Type 的方法，
// 也是调用方判断凭据形态的入口。
var predicateBudgetNames = map[string]bool{
	"IsCodex":             true,
	"IsBPS":               true,
	"IsResponsesProvider": true,
}

// predicateBudgetDirs 是受预算约束的五个消费层目录（相对模块根）。刻意不含
// internal/proxy 与 internal/scheduler：那两个包已有**零命中**门禁
// （credential_channel_gate_test.go / credential_branch_gate_test.go），不属于预算口径；
// 也不含 internal/credential 自身（谓词定义在这里，没有调用点）。
var predicateBudgetDirs = []string{
	"internal/repository",
	"internal/service",
	"internal/domain",
	"internal/sdkbridge",
	"internal/probe",
}

// predicateBudgetSites 是**完整清单**：目录 → 文件 → 该文件里的谓词选择器次数
// （非测试、非注释；见 predicateBudgetNames 的口径）。它是基线而不是上限：门禁要求
// 实际计数**逐文件等于**这里，所以新增与删除都会红。
var predicateBudgetSites = map[string]map[string]int{
	"internal/repository": {
		"account_repo.go":   1,
		"batch.go":          6,
		"codex_write.go":    7,
		"ext_codex_repo.go": 4,
		"mapping.go":        6,
		"template_repo.go":  4,
	},
	"internal/service": {
		"account.go":   5,
		"ext_codex.go": 5,
		"probe.go":     2,
		"service.go":   2,
		"template.go":  2,
	},
	"internal/domain": {
		"accountcred.go":     3,
		"bps.go":             1,
		"codex_transport.go": 2,
		"effective_url.go":   1,
		"routing.go":         1,
	},
	"internal/sdkbridge": {
		"failure.go":        1,
		"quota_resolver.go": 1,
	},
	"internal/probe": {
		"probe.go": 1,
	},
}

// predicateBudgetCeiling 是逐目录上限，必须**恰好等于** predicateBudgetSites 该目录的
// 求和（assertPredicateBudgetRegistry 断言）。它存在的唯一目的是把「加一处调用」变成
// 一次必须显式改大的编辑，并在评审里显形。
//
// U39 把身份投影的分支来源从「ext 行的类型」改成「模板类型」，新增 1 处模板 BPS 位判定
// （`projectExtIdentity`）。同一提交把 `ext_codex_repo.go` 写面原本内联的两处 ext 类型判定
// 收敛进 `assertExtCredentialType`（净 0），因此 repository 只净增 1。此后仍只许改小。
//
// 只许改小。今天的数字就是全部现状：55 处。
var predicateBudgetCeiling = map[string]int{
	"internal/repository": 28,
	"internal/service":    16,
	"internal/domain":     8,
	"internal/sdkbridge":  2,
	"internal/probe":      1,
}

// predicateBudgetTotal 是全局总数上限，必须等于各目录 ceiling 之和、也等于清单总和。
// 三层（清单 → 逐目录 ceiling → 总数）都写死，是为了让「加一处调用」至少要改两处、
// 删一处调用至少要改两处：任何单边修改都在 diff 里显形，而不是静默漂移。
//
// 只许改小。
const predicateBudgetTotal = 55

// predicateSite 是一处谓词选择器：文件名（目录由调用方的 map 键携带）、行号、谓词名。
type predicateSite struct {
	file string
	line int
	name string
}

// TestPredicateCallSitesStayWithinBudget 门禁本体：逐文件精确计数 + 伴随断言。
func TestPredicateCallSitesStayWithinBudget(t *testing.T) {
	assertPredicateBudgetRegistry(t)

	root := predicateBudgetRoot(t)
	// 顺序刻意如此：护栏先跑。名字计数的合法性建立在这条上，护栏红了之后任何计数
	// 结论都不作数。
	assertOnlyCredentialTypeDeclaresPredicates(t, root)

	got, sites := scanPredicateSites(t, root)

	gotTotal := 0
	for _, files := range got {
		for _, n := range files {
			gotTotal += n
		}
	}
	require.Positive(t, gotTotal, "扫描面数到的谓词选择器为 0：解析/遍历失效，门禁在空转")

	// 逐目录、逐文件比较：失败信息里带该目录全量明细（每一处 selector 的行号与谓词名），
	// 这样红的时候不需要另跑一遍脚本才知道差在哪。
	for _, dir := range predicateBudgetDirs {
		require.Equal(t, predicateBudgetSites[dir], got[dir],
			"%s 的谓词调用点必须逐文件等于清单（预算只许改小）:\n%s",
			dir, predicateBudgetDetail(predicateBudgetSites[dir], got[dir], sites[dir]))
	}
}

// assertPredicateBudgetRegistry 钉住注册表自身的形状：目录集合与扫描面同源、每行都
// 非空、`sum(清单[dir]) == ceiling[dir]`、`sum(ceiling) == predicateBudgetTotal ==
// sum(清单)`。注册表写错时先在这里红，而不是给出一个错误的计数。
func assertPredicateBudgetRegistry(t *testing.T) {
	t.Helper()
	require.NotEmpty(t, predicateBudgetSites, "预算清单为空：门禁在空转")
	require.Equal(t, len(predicateBudgetDirs), len(predicateBudgetSites),
		"预算清单的目录集合与 predicateBudgetDirs 必须一致")
	for _, dir := range predicateBudgetDirs {
		files, ok := predicateBudgetSites[dir]
		require.True(t, ok, "%s 不在预算清单里", dir)
		require.NotEmpty(t, files, "%s 清单为空：该目录的预算没有意义", dir)

		sum := 0
		for file, n := range files {
			require.Positive(t, n, "%s/%s：0 次的清单行没有意义，删掉它", dir, file)
			sum += n
		}
		require.Equal(t, sum, predicateBudgetCeiling[dir],
			"%s：predicateBudgetCeiling 必须恰好等于清单求和（%d）——预算是清单，不是随手写的上限", dir, sum)
	}
	require.Len(t, predicateBudgetCeiling, len(predicateBudgetDirs),
		"predicateBudgetCeiling 的目录集合与 predicateBudgetDirs 必须一致")

	// 三层相等：清单总和 == 逐目录 ceiling 之和 == predicateBudgetTotal。
	inventoryTotal, ceilingTotal := 0, 0
	for _, dir := range predicateBudgetDirs {
		ceilingTotal += predicateBudgetCeiling[dir]
		for _, n := range predicateBudgetSites[dir] {
			inventoryTotal += n
		}
	}
	require.Equal(t, predicateBudgetTotal, inventoryTotal,
		"predicateBudgetTotal 必须等于清单总和")
	require.Equal(t, predicateBudgetTotal, ceilingTotal,
		"predicateBudgetTotal 必须等于各目录 ceiling 之和")
}

// scanPredicateSites 扫 predicateBudgetDirs，返回 目录→文件→次数 与逐点明细。
//
// 每个目录是一个 Go 包：只要它的**任一**非测试文件 import 了 internal/credential，
// 整包参与计数（护栏 ①，理由见文件头）。解析失败直接 FailNow——fail-closed，绝不
// 「跳过这个文件」式静默降级。
func scanPredicateSites(t *testing.T, root string) (map[string]map[string]int, map[string][]predicateSite) {
	t.Helper()
	counts := make(map[string]map[string]int, len(predicateBudgetDirs))
	sites := make(map[string][]predicateSite, len(predicateBudgetDirs))
	for _, dir := range predicateBudgetDirs {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		entries, err := os.ReadDir(abs)
		require.NoError(t, err, "预算目录必须存在：%s", dir)

		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		require.NotEmpty(t, names, "%s 下没有非测试 .go：扫描面在空转", dir)

		fset := token.NewFileSet()
		parsed := make([]*ast.File, 0, len(names))
		importsCredential := false
		for _, name := range names {
			af, err := parser.ParseFile(fset, filepath.Join(abs, name), nil, parser.SkipObjectResolution)
			require.NoError(t, err, "解析失败（fail-closed，不跳过）：%s/%s", dir, name)
			parsed = append(parsed, af)
			if importsCredentialPackage(af) {
				importsCredential = true
			}
		}
		require.True(t, importsCredential,
			"%s 没有任何非测试文件 import internal/credential：护栏 ① 失效，名字计数不再等价于凭据谓词", dir)

		perFile := make(map[string]int, len(names))
		for _, af := range parsed {
			for _, site := range countPredicateSelectors(fset, af) {
				perFile[site.file]++
				sites[dir] = append(sites[dir], site)
			}
		}
		counts[dir] = perFile
	}
	return counts, sites
}

// countPredicateSelectors 返回 f 里三个谓词选择器的位置（按行号升序）。生产判据只有
// 这一份：门禁本体与自测喂同一个函数，自测不得复制判定逻辑。
func countPredicateSelectors(fset *token.FileSet, f *ast.File) []predicateSite {
	var out []predicateSite
	ast.Inspect(f, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || !predicateBudgetNames[sel.Sel.Name] {
			return true
		}
		pos := fset.Position(sel.Pos())
		out = append(out, predicateSite{file: filepath.Base(pos.Filename), line: pos.Line, name: sel.Sel.Name})
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// importsCredentialPackage 判断文件是否 import 了 internal/credential（默认名、别名、
// 点导入都算：判据是 import 路径，不是本地名）。
func importsCredentialPackage(f *ast.File) bool {
	for _, spec := range f.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if path == credentialImportPath || strings.HasSuffix(path, "/internal/credential") {
			return true
		}
	}
	return false
}

// assertOnlyCredentialTypeDeclaresPredicates 是护栏 ②：仓库内（非测试 .go）声明这三个
// 方法名的类型必须**只有** credential.Type。
//
// 扫三种声明形状：receiver 方法（`func (t Type) IsBPS()`）、接口方法、函数类型字段
// （`IsBPS func() bool`）——后两者会让 `x.IsBPS` 指向的不是 credential.Type。
//
// 只看非测试文件：预算也只听非测试文件，测试里的假实现影响不了计数。先做字节预筛
// （源码里必须出现字面方法名才能声明它），避免为了这条护栏解析全仓。
func assertOnlyCredentialTypeDeclaresPredicates(t *testing.T, root string) {
	t.Helper()
	credentialFile := filepath.ToSlash(filepath.Join("internal/credential", "credential.go"))
	scanned, parsedFiles := 0, 0
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".omo", "node_modules", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !mentionsPredicateName(src) {
			return nil
		}
		parsedFiles++
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("解析 %s（fail-closed）: %w", path, err)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		checkFieldNames := func(names []*ast.Ident, pos token.Pos) error {
			for _, name := range names {
				if !predicateBudgetNames[name.Name] {
					continue
				}
				return fmt.Errorf("%s：%s 声明了 %s——只有 credential.Type 可以声明这三个谓词名（护栏 ②）",
					fset.Position(pos), rel, name.Name)
			}
			return nil
		}

		for _, decl := range af.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || !predicateBudgetNames[d.Name.Name] {
					continue
				}
				require.Equal(t, credentialFile, rel,
					"%s：%s 声明了 receiver 方法 %s——名字计数的前提是这三个方法只属于 credential.Type（护栏 ②）",
					fset.Position(d.Pos()), rel, d.Name.Name)
				require.Len(t, d.Recv.List, 1, "%s：%s 的 receiver 形状意外", fset.Position(d.Pos()), d.Name.Name)
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				ident, ok := recv.(*ast.Ident)
				require.True(t, ok, "%s：%s 的 receiver 类型名解析失败", fset.Position(d.Pos()), d.Name.Name)
				require.Equal(t, "Type", ident.Name,
					"%s：%s（receiver %s）声明了 %s——只有 credential.Type 可以（护栏 ②）",
					fset.Position(d.Pos()), rel, ident.Name, d.Name.Name)
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					switch typ := ts.Type.(type) {
					case *ast.StructType:
						for _, field := range typ.Fields.List {
							if err := checkFieldNames(field.Names, field.Pos()); err != nil {
								return err
							}
						}
					case *ast.InterfaceType:
						for _, method := range typ.Methods.List {
							if err := checkFieldNames(method.Names, method.Pos()); err != nil {
								return err
							}
						}
					}
				}
			}
		}
		return nil
	})
	require.NoError(t, walkErr, "护栏 ② 扫描失败（fail-closed）")
	require.Positive(t, scanned, "护栏 ② 没有扫到任何非测试 .go：遍历失效")
	require.Positive(t, parsedFiles, "护栏 ② 只做了字节预筛、一个文件都没解析：预筛条件失效")
}

// mentionsPredicateName 是护栏 ② 的字节预筛：声明这三个方法名，源码里必然出现字面
// 方法名（Go 的声明语法无法用拼接构造标识符）。
func mentionsPredicateName(src []byte) bool {
	return strings.Contains(string(src), "IsCodex") ||
		strings.Contains(string(src), "IsBPS") ||
		strings.Contains(string(src), "IsResponsesProvider")
}

// predicateBudgetDetail 把「清单 vs 实测」的差写成可读明细：对每个计数不同的文件，
// 列出实测到的每一处 selector（行号 + 谓词名），最后给逐目录合计。
func predicateBudgetDetail(want, got map[string]int, sites []predicateSite) string {
	byFile := make(map[string][]string, len(sites))
	for _, site := range sites {
		byFile[site.file] = append(byFile[site.file], fmt.Sprintf("%d %s", site.line, site.name))
	}
	files := make(map[string]bool, len(want)+len(got))
	for file := range want {
		files[file] = true
	}
	for file := range got {
		files[file] = true
	}
	names := make([]string, 0, len(files))
	for file := range files {
		names = append(names, file)
	}
	sort.Strings(names)

	var b strings.Builder
	wantTotal, gotTotal := 0, 0
	for _, file := range names {
		wantTotal += want[file]
		gotTotal += got[file]
		if want[file] == got[file] {
			continue
		}
		fmt.Fprintf(&b, "\n  %s: 清单 %d 处，实测 %d 处", file, want[file], got[file])
		if len(byFile[file]) == 0 {
			b.WriteString("（该文件已无任何谓词选择器）")
			continue
		}
		fmt.Fprintf(&b, "；实测位置：%s", strings.Join(byFile[file], ", "))
	}
	fmt.Fprintf(&b, "\n  合计：清单 %d 处，实测 %d 处", wantTotal, gotTotal)
	return b.String()
}

// predicateBudgetRoot 定位模块根：本文件在 <root>/internal/credential/ 下，向上找到
// 含 go.mod 的目录。
func predicateBudgetRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller 必须可用")
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "向上未找到 go.mod：模块根定位失败")
		dir = parent
	}
}

// TestCountPredicateSelectorsSelfTest 自测：合成源码 → countPredicateSelectors
// （**同一个生产判据**，不复制判定逻辑）。钉住口径里最容易搞错的三种形状：
// 方法值（不带括号）计入、作为参数传递计入、常量选择器（credential.TypeBPS）不计。
func TestCountPredicateSelectorsSelfTest(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "普通调用计入",
			src:  "package x\n\ntype T struct{}\n\nfunc (T) IsBPS() bool { return true }\n\nfunc f(t T) bool { return t.IsBPS() }\n",
			want: []string{"IsBPS"},
		},
		{
			name: "方法值不带括号同样计入",
			src:  "package x\n\ntype T struct{}\n\nfunc (T) IsBPS() bool { return true }\n\nfunc f(t T) func() bool { return t.IsBPS }\n",
			want: []string{"IsBPS"},
		},
		{
			name: "作为参数传递计入",
			src:  "package x\n\ntype T struct{}\n\nfunc (T) IsCodex() bool { return true }\n\nfunc f(t T, g func() bool) bool { return g() }\n\nfunc h(t T) bool { return f(t, t.IsCodex) }\n",
			want: []string{"IsCodex"},
		},
		{
			name: "常量选择器不计（case credential.TypeBPS 不是谓词名）",
			src:  "package credential\n\nvar _ = credential.TypeBPS\n",
		},
		{
			name: "注释、字符串、声明里的名字不计",
			src:  "package x\n\n// IsBPS 是注释\nvar _ = \"IsBPS\"\ntype T struct{}\n\nfunc (T) IsBPS() bool { return true }\n",
		},
		{
			name: "非谓词方法不计",
			src:  "package x\n\ntype T struct{}\n\nfunc (T) IsCustom() bool { return true }\n\nfunc f(t T) bool { return t.IsCustom() }\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			af, err := parser.ParseFile(fset, "synthetic.go", tc.src, parser.SkipObjectResolution)
			require.NoError(t, err, "合成源码必须可解析")
			got := countPredicateSelectors(fset, af)
			names := make([]string, 0, len(got))
			for _, site := range got {
				names = append(names, site.name)
			}
			if len(tc.want) == 0 {
				require.Empty(t, names, "不该计的名字被计了")
				return
			}
			require.Equal(t, tc.want, names)
		})
	}
}
