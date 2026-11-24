// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

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

// 类型唯一化 + lane 常量单一来源门禁（P3c II）。
//
// 为什么需要它（两条都是**实测**过的静默漂移，不是假想）：
//
//	(a) 在副本里把完整的 16 字段 Attempt 与一份复制版 Validate 放回
//	    internal/scheduler（api 保留一份）⇒ `go build` 成功、代际符号门禁
//	    `TestIdentityCarryingStructsUseTheRenamedSpellings` **全绿**。原因是那条
//	    门禁按 dir 查 *ast.StructType，只证明"某个位置有这个形状"，证明不了"只有
//	    这一处"。别名是 *ast.SelectorExpr ⇒ 两边都登记反而会直接红，所以"搬走了"
//	    这件事没有任何机械保证，只能在这里钉。
//	(b) 只在 scheduler 加一条 `AttemptLaneFallback = api.AttemptLane("fallback")`
//	    ⇒ build 绿、现有门禁全绿，**运行时**才被 api.Attempt.Validate 以
//	    "Lane must be valid" 判死。常量集合裂成两套没有任何静态防线。
//
// 判据分四个用例：api 恰好持四个真定义 / scheduler 只持 `type X = api.X` /
// scheduler 没有这四个类型的第二个方法声明 / 三条 lane 常量逐名逐值相等。
//
// 扫描面纪律：模块根由 go.mod 向上查找；每处扫描都 require 非空，否则路径写错
// （或目录被清空）会让全部断言在空集合上恒绿。

// typeGateAPI / typeGateScheduler 是判据的两端：canonical 在 api，兼容层在 scheduler。
const (
	typeGateAPI       = "internal/api"
	typeGateScheduler = "internal/scheduler"
)

// typeGateNames 是本单下沉的四个值类型。顺序即失败信息里的顺序。
var typeGateNames = []string{"Attempt", "AttemptLane", "AttemptPlanIdentity", "RouteRef"}

// typeGateLaneNames 是 lane 常量的**全部**成员（不多不少）：新增一条必须显式改这里，
// 这正是 §B 想要的红灯。
var typeGateLaneNames = []string{"AttemptLaneDegraded", "AttemptLaneExplore", "AttemptLanePrimary"}

// typeGateLaneValues 钉住 api 侧的字面量取值：值集合本身也是契约的一部分
// （落库 / 观测折叠按这些字符串解释）。
var typeGateLaneValues = map[string]string{
	"AttemptLanePrimary":  "primary",
	"AttemptLaneExplore":  "explore",
	"AttemptLaneDegraded": "degraded",
}

// typeGateSource 是一个已解析的源文件（带文件名，便于失败信息定位）。
type typeGateSource struct {
	Name string
	File *ast.File
}

// typeGateModuleRoot 从本文件向上找含 go.mod 的目录。找不到即 FailNow。
func typeGateModuleRoot(t *testing.T) string {
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

// typeGateSources 解析 dir 下全部**非测试** .go。目录必须存在且真的解析到文件。
//
// 只扫非测试是刻意的：scheduler 的测试里 `scheduler.Attempt` 等旧拼写是允许的
// （别名与 api 的类型是**同一个类型**，go vet 证明不必改），把测试放进扫描面会让
// 这条判据变成"测试命名规范"，那就不是唯一性门禁了。
func typeGateSources(t *testing.T, root, rel string) []typeGateSource {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	ents, err := os.ReadDir(abs)
	require.NoError(t, err, "扫描目录必须存在：%s", rel)
	fset := token.NewFileSet()
	var out []typeGateSource
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(abs, n), nil, parser.SkipObjectResolution)
		require.NoError(t, perr, "解析失败：%s/%s", rel, n)
		out = append(out, typeGateSource{Name: n, File: f})
	}
	require.NotEmpty(t, out, "扫描面不得为空（目录写错会让本门禁恒绿）：%s", rel)
	return out
}

// typeGateTypeSpecs 枚举 srcs 里的全部包级 type 声明，返回 name → 声明序列。
// 同时返回扫到的 TypeSpec 总数，供"防空转"断言使用。
func typeGateTypeSpecs(srcs []typeGateSource) (map[string][]*ast.TypeSpec, int) {
	out := map[string][]*ast.TypeSpec{}
	scanned := 0
	for _, src := range srcs {
		for _, d := range src.File.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok {
					continue
				}
				scanned++
				out[ts.Name.Name] = append(out[ts.Name.Name], ts)
			}
		}
	}
	return out, scanned
}

// typeGateReceiverName 取方法接收者的基类型名（`Attempt` / `*Attempt` 都得 Attempt）。
func typeGateReceiverName(e ast.Expr) string {
	for {
		switch v := e.(type) {
		case *ast.StarExpr:
			e = v.X
		case *ast.ParenExpr:
			e = v.X
		case *ast.IndexExpr: // 泛型接收者 T[P]
			e = v.X
		case *ast.IndexListExpr:
			e = v.X
		case *ast.Ident:
			return v.Name
		default:
			return ""
		}
	}
}

// TestTheFourValueTypesHaveExactlyOneHomeInAPI 是判据 (a)：api 是唯一的家。
func TestTheFourValueTypesHaveExactlyOneHomeInAPI(t *testing.T) {
	root := typeGateModuleRoot(t)
	srcs := typeGateSources(t, root, typeGateAPI)
	specs, scanned := typeGateTypeSpecs(srcs)
	require.NotZero(t, scanned, "api 里一个 type 声明都没扫到：本门禁在空转")
	require.NotEmpty(t, srcs, "扫描面为空：唯一性断言会在空集合上恒绿")

	for _, name := range typeGateNames {
		decls := specs[name]
		require.Len(t, decls, 1,
			"api.%s 必须**恰好一处**定义（0 = 类型没搬进来；2 = 家分裂成两处）", name)
		ts := decls[0]
		switch name {
		case "AttemptLane":
			require.False(t, ts.Assign.IsValid(),
				"api.AttemptLane 必须是真的类型定义（不是 alias）：Valid() 只能定义在本包")
			id, ok := ts.Type.(*ast.Ident)
			require.True(t, ok, "api.AttemptLane 的底层类型必须是内置 string（今天是 *ast.Ident）")
			require.Equal(t, "string", id.Name, "api.AttemptLane 的底层类型必须是 string")
		default:
			require.False(t, ts.Assign.IsValid(),
				"api.%s 必须是真的类型定义（不是 alias）", name)
			_, ok := ts.Type.(*ast.StructType)
			require.True(t, ok,
				"api.%s 必须是 struct 定义（今天是 *ast.StructType）：换成别名等于把家又搬走了", name)
		}
	}
}

// TestSchedulerHoldsOnlyAliasesForTheFourValueTypes 是判据 (b)：兼容层只能是
// `type X = api.X`。任何 StructType 都会在这里红——这正是 (a) 变异要抓的东西。
func TestSchedulerHoldsOnlyAliasesForTheFourValueTypes(t *testing.T) {
	root := typeGateModuleRoot(t)
	srcs := typeGateSources(t, root, typeGateScheduler)
	specs, scanned := typeGateTypeSpecs(srcs)
	require.NotZero(t, scanned, "scheduler 里一个 type 声明都没扫到：本门禁在空转")

	found := map[string]int{}
	for _, name := range typeGateNames {
		for _, ts := range specs[name] {
			require.True(t, ts.Assign.IsValid(),
				"scheduler.%s 必须是别名 `type %s = api.%s`：出现真定义（StructType/定义类型）"+
					"就是第二份家，两份字段从此可以各自漂移", name, name, name)
			sel, ok := ts.Type.(*ast.SelectorExpr)
			require.True(t, ok,
				"scheduler.%s 的别名右值必须是选择器 api.%s（今天是 %T）", name, name, ts.Type)
			id, ok := sel.X.(*ast.Ident)
			require.True(t, ok, "scheduler.%s = api.%s 的主语必须是标识符 api", name, name)
			require.Equal(t, "api", id.Name,
				"scheduler.%s 只能指向契约层 api（指向别处 ⇒ 又长出一个家）", name)
			require.Equal(t, name, sel.Sel.Name,
				"别名必须同名（scheduler.%s = api.%s）：换名会让两个包各持一个不同的类型", name)
			found[name]++
		}
		require.Equal(t, 1, found[name],
			"scheduler 里必须**恰好一条** `type %s = api.%s`（0 = 别名被删，旧拼写会立刻失联；"+
				"2 = 别名分裂）", name, name)
	}
}

// TestSchedulerDeclaresNoMethodsOnTheFourValueTypes 是判据 (c)：方法必须与 canonical
// 定义同包，否则 alias 上会长出第二个 Validate / Valid 实现。
func TestSchedulerDeclaresNoMethodsOnTheFourValueTypes(t *testing.T) {
	root := typeGateModuleRoot(t)
	srcs := typeGateSources(t, root, typeGateScheduler)

	methods := 0
	for _, src := range srcs {
		for _, d := range src.File.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			methods++
			recv := typeGateReceiverName(fn.Recv.List[0].Type)
			require.NotContains(t, typeGateNames, recv,
				"%s/%s：scheduler 里出现了 `%s` 的方法声明。这四个类型已下沉 api，方法必须"+
					"与定义同包——在别名上补方法等于第二份实现（两份 Validate 各自漂移，"+
					"编译与既有门禁都不会红）", src.Name, fn.Name.Name, recv)
			// Validate / Valid 是这四个类型今天仅有的两个方法：它们只许在 api。
			// 将来若真有无关类型需要同名方法，必须显式改这一行——这是刻意的摩擦，
			// 因为"又长出一份 Valid"正是本条要抓的形态。
			require.NotContains(t, []string{"Valid", "Validate"}, fn.Name.Name,
				"%s：scheduler 不得声明方法 %s（唯一实现只许在 api；接收者 %s）",
				src.Name, fn.Name.Name, recv)
		}
	}
	require.NotZero(t, methods, "scheduler 里一个方法都没扫到：本门禁在空转")
}

// TestLaneConstantsHaveASingleSource 是判据 (d)：lane 常量逐名相等、逐值相等。
//
// "逐值相等"的实现方式是**要求名对名的别名**（scheduler 的每一条值必须是
// `api.<同名>` 选择器），而不是把两边的字面量都解出来比对：只要值是选择器，两边就是
// **同一个 Go 常量**，改 api 的字面量两边同时动；写成 `= "primary"` 这类复制则会红。
// api 侧的字面量本身由 typeGateLaneValues 单独钉住，所以"值集合"两头都锁。
func TestLaneConstantsHaveASingleSource(t *testing.T) {
	root := typeGateModuleRoot(t)
	apiSrcs := typeGateSources(t, root, typeGateAPI)
	schedSrcs := typeGateSources(t, root, typeGateScheduler)

	apiConsts := typeGateLaneConsts(apiSrcs)
	require.Len(t, apiConsts, len(typeGateLaneNames),
		"api 侧的 lane 常量必须恰好 %d 条：%v", len(typeGateLaneNames), typeGateLaneNames)
	for name, want := range typeGateLaneValues {
		lit, ok := apiConsts[name]
		require.True(t, ok, "api 里缺 lane 常量 %s", name)
		require.Equal(t, want, lit, "api.%s 的字面量变了：值集合是契约的一部分", name)
	}

	schedConsts := typeGateLaneConsts(schedSrcs)
	require.NotEmpty(t, schedConsts, "scheduler 里一条 lane 常量都没扫到：本门禁在空转")

	gotNames := make([]string, 0, len(schedConsts))
	for n := range schedConsts {
		gotNames = append(gotNames, n)
	}
	sort.Strings(gotNames)
	wantNames := append([]string(nil), typeGateLaneNames...)
	sort.Strings(wantNames)
	require.Equal(t, wantNames, gotNames,
		"scheduler 的 lane 常量名集合必须与 api 逐名相等：多一条 = 只要你只在 scheduler 加"+
			"（`AttemptLaneFallback = \"fallback\"`），build 绿、现有门禁绿，运行时才被 "+
			"api.Attempt.Validate 判 Lane must be valid；少一条 = 别名被删")

	for name, target := range schedConsts {
		require.Equal(t, "api."+name, target,
			"scheduler.%s 的值必须是 api.%s 本身（同一条 Go 常量）；今天是 %s —— 复制字面量"+
				"或换名都会让常量集合裂成两套", name, name, target)
	}
}

// typeGateLaneConsts 收集 srcs 里**与 lane 词汇有关**的常量：显式类型写了
// AttemptLane，或值里出现 AttemptLane / AttemptLane* 标识符（含
// `api.AttemptLane("x")` 这种转换、`api.AttemptLanePrimary` 这种选择器）。
//
// 返回 name → 值的可读渲染（选择器渲染成 `pkg.Sel`，字符串字面量渲染成解引号后的
// 内容，其它渲染成 `<expr>`）——判据只看渲染结果，避免在这里再写一遍 recognize 逻辑。
func typeGateLaneConsts(srcs []typeGateSource) map[string]string {
	out := map[string]string{}
	mentions := func(e ast.Expr) bool {
		if e == nil {
			return false
		}
		hit := false
		ast.Inspect(e, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && strings.HasPrefix(id.Name, "AttemptLane") {
				hit = true
			}
			return true
		})
		return hit
	}
	render := func(e ast.Expr) string {
		if sel, ok := e.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				return id.Name + "." + sel.Sel.Name
			}
		}
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				return s
			}
		}
		return "<expr>"
	}
	for _, src := range srcs {
		for _, d := range src.File.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok || len(vs.Values) != 1 {
					continue
				}
				if !mentions(vs.Type) && !mentions(vs.Values[0]) {
					continue
				}
				for _, n := range vs.Names {
					out[n.Name] = render(vs.Values[0])
				}
			}
		}
	}
	return out
}
