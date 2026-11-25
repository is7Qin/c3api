// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"errors"
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

	"github.com/is7qin/c3api/internal/storeerr"
)

// 存储哨兵归属门禁（R1-a）：canonical 在 internal/storeerr，本包只许**同值别名**。
//
// 为什么必须有它：把 canonical 搬到 storeerr 后，本包若在某一处写回
// `errors.New("repository: conflict")`，一切**文案**断言照样绿（reflect.DeepEqual 对
// *errors.errorString 只比文案），但 `repository.ErrConflict == storeerr.ErrConflict`
// 静默变 false，包外 40+ 处 errors.Is 与 == 判定一起失效。sentinelNames 那一族门禁
// 不覆盖这 4 个名字（sentinelTotalDecls 只管它自己那 8 个），所以这里独立看守。
//
// 断言四件事（缺一即红）：
//  1. 本包非测试 .go 里这 4 个名字**恰好各有一次**包级声明；
//  2. 形状**恰好是** `var ErrX = storeerr.ErrX`（单一选择器、同名，主语按 import 路径
//     解析到 internal/storeerr，而不是按字面量猜包名）；
//  3. 本包必须 import internal/storeerr（没有 import 就没有"同一 error 值"）；
//  4. storeerr 里这 4 个名字**恰好各有一次** canonical 声明，且**逐名强制 RHS 形状**。
//
// 为什么 (4) 要逐名钉形状而不是"errors.New 或 fmt.Errorf"这种宽谓词：宽谓词会放过
// 把 stale 写成 errors.New 的错误形态——此时 errors.Is(stale, ErrConflict) 断裂，
// 而纯文案断言恰好看不出差别（stale 的文案就是自带前缀的字符串）。
//
// 扫描面纪律（照搬 internal/identityfence/ownership_gate_test.go）：模块根由 go.mod
// 向上查找；每处扫描都 require.NotEmpty —— 路径写错或目录被清空会让断言在空集合上
// 恒绿，那是最坏的一种"验证"。

// storeerrAliases 是"名字 -> 必须满足的 canonical 形状"。它是本门的**单一登记表**：
// 新增一枚走别名的存储哨兵，必须在这里登记，否则 (1) 的对账断言会红。
var storeerrAliases = []string{
	"ErrNotFound",
	"ErrConflict",
	"ErrStaleRevision",
	"ErrStaleIdentityRevision",
}

// storeerrAliasConsumersBaseline 是 (a) 冻结的"仍在用 repository 旧名"的**文件路径**集合：
// 包外、非测试生产代码，且**真实引用**（AST selector）这 4 个名字的文件。
//
// 为什么冻结文件集合而不是引用次数：次数会随无关编辑漂移（加一行 log 就变），文件
// 集合稳定，且足以表达"谁还在用旧名"。为什么必须 AST 而不是文本 rg：文本会把**注释**
// 里的 `repository.ErrX` 当引用（本仓实测有 5 个纯注释/无关符号文件会被误算，其中
// internal/rule/persist_error.go 与 internal/service/proxy.go / supplier_admin.go
// 只碰别的符号）。
//
// 本表是**迁移落地后**的实测基线：R1-a 只搬 rule / sdkbridge（4 个文件改指 storeerr），
// service 与 rulepersist 的迁移属于后续变更（见 spec §6 非目标），它们仍用旧名是
// 合法现状，因此必须留在表里 —— 这正是"别名有账期"的可执行形态。
//
// 移除条件（三者同时满足即可删除别名，另开一个变更）：
//
//  1. 包内引用归零（只看生产代码，排除测试）：
//     rg -n 'Err(NotFound|Conflict|StaleRevision|StaleIdentityRevision)\b' \
//     internal/repository -g '*.go' -g '!*_test.go'
//     ⇒ 只剩 4 行别名声明（迁移前基线 177 matches / 172 lines / 21 files）。
//     1b. 测试侧引用归零（可后于第 1 条）：
//     rg -n 'repository\.Err(NotFound|Conflict|StaleRevision|StaleIdentityRevision)' \
//     internal/ cmd/ pkg/ -g '*_test.go'   ⇒ 0 处
//  2. 下表 ⇒ 空集；
//  3. 同一提交里删除 4 个别名变量，并把下表清空。
//
// 本表冻结的是**路径**：重命名/拆分/合并文件都会让它红。这是有意的 fail-closed ——
// 逼人在同一个提交里显式更新基线，评审就能看见"消费者集合变了"。处理流程：确认是纯
// 路径迁移 ⇒ 更新本表并在提交信息里说明"纯重命名"；确认是新增引用 ⇒ 拒绝，新代码必须
// import storeerr 而不是继续用 repository 旧名。
var storeerrAliasConsumersBaseline = []string{
	"internal/rulepersist/rule_persist.go",
	"internal/service/account_usage.go",
	"internal/service/ext_codex.go",
	"internal/service/ext_codex_import.go",
	"internal/service/mailer.go",
	"internal/service/payment.go",
	"internal/service/probe.go",
	"internal/service/redemption.go",
	"internal/service/service.go",
	"internal/service/supplier.go",
	"internal/service/user.go",
}

const (
	storeerrImportPath    = "github.com/is7qin/c3api/internal/storeerr"
	repositoryImportPath  = "github.com/is7qin/c3api/internal/repository"
	storeerrAliasScanDir  = "internal/repository"
	storeerrCanonicalDir  = "internal/storeerr"
	storeerrConsumerRoots = "internal cmd pkg demo"
)

// TestRepositoryAliasesStoreerrSentinels 断言 (1)(2)(3)：本包的 4 个哨兵是 storeerr 的
// 同值别名，不是本包自建的第二份。
func TestRepositoryAliasesStoreerrSentinels(t *testing.T) {
	root := storeerrGateModuleRoot(t)
	files := storeerrGateParseDir(t, root, storeerrAliasScanDir)

	// 逐名收集包级声明：名字 -> 该名字以 storeerr 主语出现的次数。
	// 不只看 `var X = ...` 形态：任何以这 4 个名字声明的包级 var 都算本包自持。
	declared := map[string]int{}
	aliased := map[string]int{}
	for _, f := range files {
		// 主语按 import 路径解析，不按字面量猜：本包若把 storeerr 改别名 import，
		// 字面量 `id.Name == "storeerr"` 会误判成"没别名"。
		local := storeerrAliasLocalName(t, f)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 {
					continue
				}
				name := vs.Names[0].Name
				if !isStoreerrAliasName(name) {
					continue
				}
				declared[name]++
				if len(vs.Values) != 1 {
					continue
				}
				sel, ok := vs.Values[0].(*ast.SelectorExpr)
				if !ok {
					continue
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != local {
					continue
				}
				require.Equal(t, name, sel.Sel.Name,
					"万一又出现别名，它至少必须指向同名哨兵（%s = storeerr.%s）：换名意味着两枚不同的 error 值",
					name, name)
				aliased[name]++
			}
		}
	}

	wantAliased := map[string]int{}
	for _, n := range storeerrAliases {
		wantAliased[n] = 1
	}
	require.Equal(t, wantAliased, aliased,
		"%s 的 4 个存储哨兵必须**恰好各有一次** `var ErrX = storeerr.ErrX`：0 = 名字被搬走或改指；2 = 别名分裂",
		storeerrAliasScanDir)
	require.Equal(t, wantAliased, declared,
		"%s 的 4 个名字必须**恰好各有一次**包级声明：多于 1 次 = 包内出现了第二份（别名 + 自建并存）",
		storeerrAliasScanDir)

	// 值身份用 == 证明（testify 的 Equal 对 *errors.errorString 只比文案，
	// 对"新实例、同文案"的变异恒真 —— 那不是证明）。
	require.True(t, ErrNotFound == storeerr.ErrNotFound,
		"ErrNotFound 必须是 storeerr.ErrNotFound 的同值别名，不是文案相同的第二个实例")
	require.True(t, ErrConflict == storeerr.ErrConflict,
		"ErrConflict 必须是 storeerr.ErrConflict 的同值别名，不是文案相同的第二个实例")
	require.True(t, ErrStaleRevision == storeerr.ErrStaleRevision,
		"ErrStaleRevision 必须是 storeerr.ErrStaleRevision 的同值别名，不是文案相同的第二个实例")
	require.True(t, ErrStaleIdentityRevision == storeerr.ErrStaleIdentityRevision,
		"ErrStaleIdentityRevision 必须是 storeerr.ErrStaleIdentityRevision 的同值别名，不是文案相同的第二个实例")

	// 文案断言必须同时钉在**本包别名**上：只钉 storeerr 的文案留不住
	// 「别名换成 errors.New("repository: CONFLICT")」这类变异（文案变了，storeerr 一字未动）。
	// 与上面的 == 互补：== 抓"新实例、同文案"，文案抓"这是另一枚值且文案也变了"。
	require.Equal(t, "repository: not found", ErrNotFound.Error())
	require.Equal(t, "repository: conflict", ErrConflict.Error())
	require.Equal(t, "repository: conflict: stale lifecycle_revision", ErrStaleRevision.Error())
	require.Equal(t, "repository: conflict: stale identity_revision", ErrStaleIdentityRevision.Error())

	// (3) 别名的前提：本包至少有一个文件真的 import 了 storeerr。
	imported := false
	for _, f := range files {
		if storeerrAliasLocalName(t, f) != "" {
			imported = true
			break
		}
	}
	require.True(t, imported,
		"%s 必须 import %s：4 个别名的值只能来自那里（没有 import 就没有同一 error 值）",
		storeerrAliasScanDir, storeerrImportPath)
}

// TestStoreerrHoldsCanonicalSentinels 断言 (4)：storeerr 是这 4 枚的 canonical 所在地，
// 且**逐名强制 RHS 形状**。
func TestStoreerrHoldsCanonicalSentinels(t *testing.T) {
	root := storeerrGateModuleRoot(t)
	files := storeerrGateParseDir(t, root, storeerrCanonicalDir)

	// 名字 -> 声明形态（"errors.New" / "fmt.Errorf" / 其他）。
	shapes := map[string]string{}
	// stale 的期望后缀：必须逐字钉住。`fmt.Errorf("%w", ErrConflict)` 的 errors.Is
	// 链仍然成立（所以 Is 断言抓不到），但 Error() 退化成 "repository: conflict"，
	// 丢失 stale 分类 ⇒ 日志/用户面语义改变。后缀只能靠这里的字面量断言钉住。
	staleSuffix := map[string]string{
		"ErrStaleRevision":         "%w: stale lifecycle_revision",
		"ErrStaleIdentityRevision": "%w: stale identity_revision",
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
				name := vs.Names[0].Name
				if !isStoreerrAliasName(name) || shapes[name] != "" {
					continue
				}
				call, ok := vs.Values[0].(*ast.CallExpr)
				if !ok {
					shapes[name] = "not-a-call"
					continue
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					shapes[name] = "not-a-selector"
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					shapes[name] = "not-an-ident"
					continue
				}
				switch {
				case pkg.Name == "errors" && sel.Sel.Name == "New":
					shapes[name] = "errors.New"
					require.Len(t, call.Args, 1, "%s: errors.New 必须恰好一个参数", name)
				case pkg.Name == "fmt" && sel.Sel.Name == "Errorf":
					shapes[name] = "fmt.Errorf"
					// 第一个参数必须是字面量后缀，第二个必须是 ErrConflict。
					require.Len(t, call.Args, 2,
						"%s 必须是 fmt.Errorf(\"%%w: ...\", ErrConflict) 两参数形态", name)
					lit, ok := call.Args[0].(*ast.BasicLit)
					require.True(t, ok && lit.Kind == token.STRING,
						"%s: fmt.Errorf 的首参必须是字符串字面量（后缀要能被机械核对，拼出来的不算）", name)
					unq, uerr := strconv.Unquote(lit.Value)
					require.NoError(t, uerr, "%s: 格式字面量必须可解引号", name)
					wantSuffix, isStale := staleSuffix[name]
					require.True(t, isStale,
						"%s 不允许用 fmt.Errorf 构造：只有两枚 stale 是 ErrConflict 的 %%w 派生，其余必须是 errors.New", name)
					require.Equal(t, wantSuffix, unq,
						"%s 的 %%w 后缀必须逐字为 %q（少了后缀，errors.Is 仍成立但 Error() 丢掉 stale 分类）",
						name, wantSuffix)
					second, ok := call.Args[1].(*ast.Ident)
					require.True(t, ok && second.Name == "ErrConflict",
						"%s 必须 wrap 本包的 ErrConflict（%%w 链是 errors.Is(stale, ErrConflict) 的唯一来源）", name)
				default:
					shapes[name] = pkg.Name + "." + sel.Sel.Name
				}
			}
		}
	}

	require.Equal(t, map[string]string{
		"ErrNotFound":              "errors.New",
		"ErrConflict":              "errors.New",
		"ErrStaleRevision":         "fmt.Errorf",
		"ErrStaleIdentityRevision": "fmt.Errorf",
	}, shapes,
		"%s 必须持有这 4 枚 canonical 哨兵且形状逐名固定：NotFound/Conflict = errors.New，"+
			"两枚 stale = fmt.Errorf(%%w: ...)。stale 写成 errors.New 会静默打断 errors.Is 链",
		storeerrCanonicalDir)

	// 文案与 spec §1.4 的实测值逐字节相同（这也是"搬家没改语义"的唯一直接证据）。
	require.Equal(t, "repository: not found", storeerr.ErrNotFound.Error())
	require.Equal(t, "repository: conflict", storeerr.ErrConflict.Error())
	require.Equal(t, "repository: conflict: stale lifecycle_revision", storeerr.ErrStaleRevision.Error())
	require.Equal(t, "repository: conflict: stale identity_revision", storeerr.ErrStaleIdentityRevision.Error())

	// 跨包 errors.Is 链：这是唯一有判别力的语义断言。
	// 不用 errors.Is(repository.ErrStaleX, repository.ErrConflict) —— "同一份 errors.New
	// 自洽重建"时它仍为 true，没有判别力。
	require.True(t, errors.Is(ErrStaleRevision, storeerr.ErrConflict),
		"errors.Is(ErrStaleRevision, storeerr.ErrConflict) 必须成立：%%w 派生链断了")
	require.True(t, errors.Is(ErrStaleIdentityRevision, storeerr.ErrConflict),
		"errors.Is(ErrStaleIdentityRevision, storeerr.ErrConflict) 必须成立：%%w 派生链断了")
}

// TestStoreerrAliasConsumersFrozen 断言 (a)：包外、非测试生产代码里**真实引用**（AST
// selector）这 4 个名字的文件集合，恰好等于落地时实测的基线。新增引用 ⇒ 红。
func TestStoreerrAliasConsumersFrozen(t *testing.T) {
	root := storeerrGateModuleRoot(t)

	got := map[string]bool{}
	scanned := 0
	for _, dir := range strings.Fields(storeerrConsumerRoots) {
		base := filepath.Join(root, filepath.FromSlash(dir))
		// 目录可以不存在（demo/ 是 .gitignore 的本地目录，工作区副本里没有）；
		// 但整轮扫描一个 .go 都没有时下面 require.NotEmpty 会兜住。
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, werr error) error {
			if werr != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			// 本包自身的别名声明不算"包外消费方"。
			if strings.HasPrefix(rel, storeerrAliasScanDir+"/") {
				return nil
			}
			scanned++
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return nil
			}
			// 主语必须是**绑定到 internal/repository 的本地包名**。不能用 storeerr 的
			// 本地名：迁移后的文件写的是 storeerr.ErrX，那是新名，不是"还在用旧名"。
			local := storeerrGateImportLocal(t, f, repositoryImportPath)
			if local == "" {
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != local || !isStoreerrAliasName(sel.Sel.Name) {
					return true
				}
				got[rel] = true
				return true
			})
			return nil
		})
		require.NoError(t, err, "扫描 %s 失败", dir)
	}
	require.NotEmpty(t, scanned, "扫描面不得为空（roots 写错会让本门禁恒绿）")

	gotList := make([]string, 0, len(got))
	for p := range got {
		gotList = append(gotList, p)
	}
	sort.Strings(gotList)

	want := append([]string(nil), storeerrAliasConsumersBaseline...)
	sort.Strings(want)

	require.Equal(t, want, gotList,
		"仍在用 repository 旧名（AST 真实引用，非注释）的生产文件集合变了。\n"+
			"新增引用 ⇒ 拒绝：新代码必须 import %s，而不是继续用 repository 旧名。\n"+
			"纯路径重命名/拆分 ⇒ 更新 storeerrAliasConsumersBaseline 并在提交信息里说明。\n"+
			"减少 ⇒ 好事，同提交把基线收窄（全部归零后按移除条件删别名）。",
		storeerrImportPath)
}

// isStoreerrAliasName 这 4 个名字是否属于本门禁看守的哨兵集合。
func isStoreerrAliasName(name string) bool {
	for _, n := range storeerrAliases {
		if n == name {
			return true
		}
	}
	return false
}

// storeerrAliasLocalName 返回 f 中绑定到 internal/storeerr 的**本地包名**；没有则 ""。
// 按 import 路径解析而不是按字面量猜：别名 import（`s "…/storeerr"`）也必须被认出来。
func storeerrAliasLocalName(t *testing.T, f *ast.File) string {
	t.Helper()
	return storeerrGateImportLocal(t, f, storeerrImportPath)
}

// storeerrGateImportLocal 返回 f 中绑定到 path 的**本地包名**（默认取包名末段）；
// 没有该 import 则 ""。按 import 路径解析，别名 import（`r "…/repository"`）也认得。
func storeerrGateImportLocal(t *testing.T, f *ast.File, path string) string {
	t.Helper()
	want := path[strings.LastIndex(path, "/")+1:]
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return want
	}
	return ""
}

// storeerrGateModuleRoot 从本文件向上找含 go.mod 的目录。找不到即 FailNow —— 定位失败
// 时若返回空路径，后续扫描会变成"扫了个空目录却全绿"。
func storeerrGateModuleRoot(t *testing.T) string {
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

// storeerrGateParseDir 解析 dir 下全部**非测试** .go。目录必须存在且真的解析到文件。
func storeerrGateParseDir(t *testing.T, root, rel string) []*ast.File {
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
