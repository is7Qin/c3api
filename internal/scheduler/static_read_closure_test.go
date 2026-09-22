// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"go/ast"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 静态读闭包门禁：把"键覆盖消费面"从人工核验变成机械断言。
//
// 叶子复用判据是 `staticKeyOf(old) == staticKeyOf(new)`。判据一旦真的生效，无关重载
// 上就会**首次**复用旧叶子——任何"读了静态字段却不在键里"的消费点立刻变成陈旧读。
// 故必须枚举"谁读了快照的哪些静态事实"，并断言每个读都落在 planKey ∪ payloadKey。
//
// 为什么根标识符不能全局按名字收：`av` 被 `*snapshotStatic` 之外的类型共用（同包里
// 还有 `*RoutingView` 的 `cur`/`v`），按名字全局收集会把无关类型的选择器混进来。
// 故扫描面先收在**显式的函数集**内，函数内再按根绑定枚举：根 = 该函数的
// `*snapshotStatic` 参数/接收者、`X := <...>.static.Load()`、`X := &snapshotStatic{}`。
// 局部别名（`ext := av.acc.Ext`）解析到规范路径。
//
// 为什么粒度必须是**标量解引用路径**而不是字段名：`av.acc` 这类整结构体读确实存在
// （有 15 处，既是深路径的前缀也是真实读取），以字段名判定会把载荷算成"被读"，
// 门不可执行。故路径终到标量，整结构体/指针/切片读单独分类（container / field）。

const (
	snapRootAlias = "$S" // 规范根名：所有 *snapshotStatic 根归一为它
)

// staticReaderFuncs 是允许读快照静态事实的函数全集，值写明它为何要读。断言是**相等**
// （双向）：新增一个读静态视图的函数 ⇒ 失败；某个函数不再读 ⇒ 陈旧条目 ⇒ 失败。
var staticReaderFuncs = map[string]string{
	"Classify":                   "事件分类：按账号的 template_id 归质量键",
	"InvalidateAccount":          "取账号所属组集合做定向重载",
	"InvalidateGroup":            "组级重载：复用/替换叶子并登记引用集",
	"IsLatched":                  "latch 谓词按 (指纹, K) 判定",
	"MarkResult":                 "运行结果记账：状态、K、template_id、事件组",
	"ProbeAccount":               "探针：把整个账号交给候选指纹权威",
	"Runtimes":                   "运行时视图：并发上限与名字",
	"attachCompilerFacts":        "把逐账号编译事实挂到静态根",
	"buildRoutes":                "路由索引：模板的格式集",
	"buildSnapshots":             "构造快照：复用判定需读旧叶子",
	"deriveCompilerAccountFacts": "编译事实派生（决策输入）",
	"eventGID":                   "事件组 ID（group_ids 的确定性派生）",
	"failureEvent":               "失效事件构造：K、template_id、事件组",
	"indexRoutesForAccount":      "账号的组引用集登记",
	"modelSet":                   "模型集合：模板的模型/映射/格式",
	"newAccountSnapshot":         "叶子构造：账号 ID",
	"onRuleFailure":              "失效事件的 fail-closed fence（K）",
	"payloadKeyOf":               "载荷投影",
	"planKeyOf":                  "决策输入投影",
	"reload":                     "全量重载：K 变化检测",
	"reserveOnView":              "预留谓词与 Selection 装配",
	"staticKeyOf":                "叶子复用判据",
}

// 路径分类：field 落到某个键字段；derived 由另一条路径覆盖；container 是整结构体/
// 指针/切片读取（前缀、nil 检查、整体交给权威），本身不是标量判据。
const (
	staticPathField     = "field"
	staticPathDerived   = "derived"
	staticPathContainer = "container"
)

type staticReadSpec struct {
	class  string
	detail string
}

// staticReadPaths 是扫描面上允许出现的**全部**静态读路径（根名已归一为 `$S`）。
// 断言是**相等**（双向）。`field` 类的 detail 必须是 planKey ∪ payloadKey 的字段名，
// 且这些字段名之集必须**恰好**等于两枚键的全部字段——既证明键覆盖消费面，也证明键
// 里没有消费者读不到的冗余字段。
var staticReadPaths = map[string]staticReadSpec{
	// --- 整结构体/指针/切片读取 ---
	"$S.acc":                   {staticPathContainer, "整账号：深路径前缀、整体交给候选指纹权威（ProbeAccount 的 &av.acc）"},
	"$S.tpl":                   {staticPathContainer, "模板指针：nil 检查与前缀"},
	"$S.acc.Ext":               {staticPathContainer, "凭据扩展指针：nil 检查 + 装配到 Selection.Ext 的穿透把手（收窄属计划 fence 消费面收口的范围）"},
	"$S.acc.Ext.CodexIdentity": {staticPathContainer, "身份四元组指针：nil 检查与前缀"},

	// --- 账号侧标量 ---
	"$S.acc.ID":                       {staticPathField, "accountID"},
	"$S.acc.Name":                     {staticPathField, "name"},
	"$S.acc.TemplateID":               {staticPathField, "templateID"},
	"$S.acc.BaseURL":                  {staticPathField, "baseURL"},
	"$S.acc.UpstreamKey":              {staticPathField, "upstreamKey"},
	"$S.acc.MaxConcurrency":           {staticPathField, "maxConcurrency"},
	"$S.acc.Enabled":                  {staticPathField, "enabled"},
	"$S.acc.CacheDomain":              {staticPathField, "cacheDomain"},
	"$S.acc.UpstreamCostMultiplierBp": {staticPathField, "upstreamCostMultiplierBp"},
	"$S.acc.IdentityRevision":         {staticPathField, "identityRevision"},

	// --- 账号凭据面（决策输入子集） ---
	"$S.acc.Ext.CodexAccountID":               {staticPathField, "codexAccountID"},
	"$S.acc.Ext.CodexEmail":                   {staticPathField, "codexEmail"},
	"$S.acc.Ext.CodexPATKey":                  {staticPathField, "codexPATKey"},
	"$S.acc.Ext.CodexIdentity.InstallationID": {staticPathField, "codexInstallation"},
	"$S.acc.Ext.CodexIdentity.SessionID":      {staticPathField, "codexSession"},
	"$S.acc.Ext.CodexIdentity.ThreadID":       {staticPathField, "codexThread"},
	"$S.acc.Ext.CodexIdentity.WindowID":       {staticPathField, "codexWindow"},

	// --- 账号凭据面（载荷：只影响叶子内容是否新鲜） ---
	"$S.acc.Ext.CodexOAuthToken":        {staticPathField, "codexOAuthToken"},
	"$S.acc.Ext.CodexOAuthRefreshToken": {staticPathField, "codexOAuthRefresh"},
	"$S.acc.Ext.CodexOAuthExpiresAt":    {staticPathField, "codexOAuthExpires"},

	// --- 模板侧源字段（候选指纹 I 不覆盖的那些） ---
	"$S.tpl.ID":               {staticPathField, "templateID"}, // 与 $S.acc.TemplateID 同源：加载器按 template_id 联结，键用账号侧拼写覆盖同一事实
	"$S.tpl.BaseURL":          {staticPathField, "baseURL"},
	"$S.tpl.CredentialType":   {staticPathField, "credentialType"},
	"$S.tpl.StripImageTools":  {staticPathField, "stripImageTools"},
	"$S.tpl.Models":           {staticPathField, "modelsDigest"},
	"$S.tpl.FormatModels":     {staticPathField, "formatModelsKey"},
	"$S.tpl.SupportedFormats": {staticPathField, "supportedFormats"},
	"$S.tpl.ModelMapping":     {staticPathField, "modelMappingKey"},

	// --- 组归属 ---
	"$S.groupIDs": {staticPathField, "groupIDsDigest"},
}

// payloadProjectionFuncs 是允许读**载荷**字段（OAuth 三元组）的函数全集：载荷只用于
// 上游鉴权，任何决策/装配路径读它都意味着"token 刷新会 churn 计划"。读取面收在这两处，
// 消费者拿令牌只能经 Selection.Ext（叶子内容，非静态读）。
var payloadProjectionFuncs = map[string]string{
	"payloadKeyOf": "载荷投影：只读 OAuth 三元组",
	"staticKeyOf":  "叶子复用判据：调用两个投影，自身不直接读",
}

// --- 静态读的语法解析 ---

// snapStaticLoadCall 报告 e 是否为 `<recv>.static.Load()`。`accountSnapshot.static` 是
// 本包唯一带 `static` 字段的 atomic.Pointer，故该形态唯一指向 *snapshotStatic。
func snapStaticLoadCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Load" {
		return false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	return ok && recv.Sel.Name == "static"
}

func snapStar(e ast.Expr) bool {
	st, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := st.X.(*ast.Ident)
	return ok && id.Name == "snapshotStatic"
}

func snapComposite(e ast.Expr) bool {
	if ue, ok := e.(*ast.UnaryExpr); ok {
		e = ue.X
	}
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	id, ok := cl.Type.(*ast.Ident)
	return ok && id.Name == "snapshotStatic"
}

// snapChain 把表达式解析为（根标识符, 选择器链）。内联的 `<recv>.static.Load()` 链
// 解析为规范根 snapRootAlias。
func snapChain(e ast.Expr) (root string, parts []string, ok bool) {
	for {
		switch v := e.(type) {
		case *ast.SelectorExpr:
			parts = append([]string{v.Sel.Name}, parts...)
			e = v.X
		case *ast.ParenExpr:
			e = v.X
		case *ast.StarExpr:
			e = v.X
		case *ast.IndexExpr:
			e = v.X
		case *ast.CallExpr:
			if snapStaticLoadCall(v) {
				return snapRootAlias, parts, true
			}
			return "", nil, false
		case *ast.Ident:
			return v.Name, parts, true
		default:
			return "", nil, false
		}
	}
}

// snapRoots 收集函数内绑定为 *snapshotStatic 的根标识符。
func snapRoots(fn *ast.FuncDecl) map[string]bool {
	roots := map[string]bool{snapRootAlias: true}
	for _, fl := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
		if fl == nil {
			continue
		}
		for _, p := range fl.List {
			if !snapStar(p.Type) {
				continue
			}
			for _, nm := range p.Names {
				roots[nm.Name] = true
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if snapStaticLoadCall(as.Rhs[0]) || snapComposite(as.Rhs[0]) {
			roots[lhs.Name] = true
		}
		return true
	})
	return roots
}

// snapAliases 把局部别名（`ext := av.acc.Ext`）解析到规范路径。迭代到不动点，
// 因为别名可以链式（`id := ext.CodexIdentity`）。
func snapAliases(fn *ast.FuncDecl, roots map[string]bool) map[string]string {
	alias := map[string]string{}
	for i := 0; i < 8; i++ {
		changed := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok || roots[lhs.Name] {
				return true
			}
			r, parts, ok := snapChain(as.Rhs[0])
			if !ok || len(parts) == 0 {
				return true
			}
			var canon string
			if roots[r] {
				canon = snapRootAlias + "." + strings.Join(parts, ".")
			} else {
				a, ok := alias[r]
				if !ok {
					return true
				}
				canon = a + "." + strings.Join(parts, ".")
			}
			if alias[lhs.Name] != canon {
				alias[lhs.Name] = canon
				changed = true
			}
			return true
		})
		if !changed {
			break
		}
	}
	return alias
}

// snapPaths 枚举函数内出现的全部静态读路径。作为方法调用接收者的选择器不算字段读
// （`av.eventGID()` 是派生方法，不是快照字段）。
func snapPaths(fn *ast.FuncDecl, roots map[string]bool, alias map[string]string) map[string]bool {
	callFuns := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if _, isSel := c.Fun.(*ast.SelectorExpr); isSel {
				callFuns[c.Fun] = true
			}
		}
		return true
	})
	paths := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		se, ok := n.(*ast.SelectorExpr)
		if !ok || callFuns[n] {
			return true
		}
		r, parts, ok := snapChain(se)
		if !ok || len(parts) == 0 {
			return true
		}
		if roots[r] {
			paths[snapRootAlias+"."+strings.Join(parts, ".")] = true
			return true
		}
		if a, ok := alias[r]; ok {
			paths[a+"."+strings.Join(parts, ".")] = true
		}
		return true
	})
	return paths
}

// --- 断言 ---

// TestStaticReadClosureCoversEveryConsumerPath 枚举面上每一个静态读都必须落在
// planKey ∪ payloadKey 的字段上，且键字段与消费字段必须**一一对上**。
func TestStaticReadClosureCoversEveryConsumerPath(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})

	planShape, ok := gateStructShape(t, srcs, "internal/scheduler", "planKey")
	require.True(t, ok, "planKey 必须存在")
	payloadShape, ok := gateStructShape(t, srcs, "internal/scheduler", "payloadKey")
	require.True(t, ok, "payloadKey 必须存在")

	observedFuncs := map[string]bool{}
	observedPaths := map[string]bool{}
	pathFuncs := map[string]map[string]bool{}
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			roots := snapRoots(fn)
			paths := snapPaths(fn, roots, snapAliases(fn, roots))
			// roots 恒含规范根（内联链），故只有**具名根**（*snapshotStatic 参数/
			// 接收者/绑定）或实际路径才算读了静态视图。两者都没有的函数不登记。
			if len(paths) == 0 && len(roots) == 1 {
				continue
			}
			observedFuncs[fn.Name.Name] = true
			for p := range paths {
				observedPaths[p] = true
				if pathFuncs[p] == nil {
					pathFuncs[p] = map[string]bool{}
				}
				pathFuncs[p][fn.Name.Name] = true
			}
		}
	}

	// ① 读静态视图的函数集必须**恰好**是声明的允许集。
	gotFuncs := make([]string, 0, len(observedFuncs))
	for f := range observedFuncs {
		gotFuncs = append(gotFuncs, f)
	}
	sort.Strings(gotFuncs)
	require.Equal(t, sortedKeys(staticReaderFuncs), gotFuncs,
		"读快照静态事实的函数集与声明允许集必须相等：新增读取点必须先声明它落在哪个键字段")

	// ② 读路径集必须**恰好**是声明的路径集。
	gotPaths := make([]string, 0, len(observedPaths))
	for p := range observedPaths {
		gotPaths = append(gotPaths, p)
	}
	sort.Strings(gotPaths)
	declaredPaths := make([]string, 0, len(staticReadPaths))
	for p := range staticReadPaths {
		declaredPaths = append(declaredPaths, p)
	}
	sort.Strings(declaredPaths)
	require.Equal(t, declaredPaths, gotPaths,
		"静态读路径集与声明路径集必须相等")

	// ③ field 类路径的键字段必须存在，且覆盖面**恰好**等于两枚键的字段全集。
	planFields := make(map[string]bool, len(planShape.fields))
	for f := range planShape.fields {
		planFields[f] = true
	}
	payloadFields := make(map[string]bool, len(payloadShape.fields))
	for f := range payloadShape.fields {
		payloadFields[f] = true
	}
	allKeyFields := map[string]bool{}
	for f := range planFields {
		allKeyFields[f] = true
	}
	for f := range payloadFields {
		allKeyFields[f] = true
	}
	consumed := map[string]bool{}
	for p, spec := range staticReadPaths {
		require.NotEmpty(t, strings.TrimSpace(spec.detail), "路径分类必须写明理由：%s", p)
		switch spec.class {
		case staticPathField:
			require.True(t, allKeyFields[spec.detail],
				"路径 %s 声称落在键字段 %s，但该字段不在 planKey ∪ payloadKey", p, spec.detail)
			consumed[spec.detail] = true
		case staticPathDerived, staticPathContainer:
			// 派生/整结构体：由另一条路径或权威函数覆盖，不占键字段。
		default:
			t.Fatalf("路径 %s 的分类未知：%q", p, spec.class)
		}
	}
	gotConsumed := make([]string, 0, len(consumed))
	for f := range consumed {
		gotConsumed = append(gotConsumed, f)
	}
	sort.Strings(gotConsumed)
	wantConsumed := make([]string, 0, len(allKeyFields))
	for f := range allKeyFields {
		wantConsumed = append(wantConsumed, f)
	}
	sort.Strings(wantConsumed)
	require.Equal(t, wantConsumed, gotConsumed,
		"键字段集与消费到的字段集必须相等：键里不得有消费者读不到的字段，消费面也不得读到键外的字段")

	// ④ 载荷字段只允许被载荷投影函数读到——决策/装配路径读到 token 就意味着
	// "SDK 刷新 token 会 churn 计划"。
	for p, spec := range staticReadPaths {
		if spec.class != staticPathField || !payloadFields[spec.detail] {
			continue
		}
		funcs := make([]string, 0, len(pathFuncs[p]))
		for f := range pathFuncs[p] {
			funcs = append(funcs, f)
		}
		sort.Strings(funcs)
		for _, f := range funcs {
			require.Contains(t, payloadProjectionFuncs, f,
				"载荷字段 %s（路径 %s）不得被 %s 读到：载荷只服务上游鉴权", spec.detail, p, f)
		}
		require.NotEmpty(t, funcs, "载荷路径 %s 必须真的被投影函数读到", p)
	}
}

// TestEveryStaticLoadSiteIsDeclared `.static.Load()` 是取得静态视图的**唯一**入口。
// 每个调用点都必须落在声明的读者函数内——否则一个"取了视图但暂时只读了几何事实"
// 的函数会绕过上面那张表。
func TestEveryStaticLoadSiteIsDeclared(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	var sites []string
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			hit := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && snapStaticLoadCall(call) {
					hit = true
				}
				return true
			})
			if hit {
				sites = append(sites, fn.Name.Name)
			}
		}
	}
	sort.Strings(sites)
	require.NotEmpty(t, sites, "扫描面必须至少有一处静态视图取用点")
	for _, f := range sites {
		require.Contains(t, staticReaderFuncs, f,
			"%s 取了静态视图却未在 staticReaderFuncs 中声明", f)
	}
}

// TestPayloadProjectionIsTheOnlyCredentialReader payloadKey 的字段集必须与载荷投影
// 函数实际读到的路径**一一对应**：投影多读一个字段（该字段不在键里）会让键漏掉它，
// 少读一个（键里有但没人读）会让键带上无人消费的字段。
func TestPayloadProjectionIsTheOnlyCredentialReader(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	payloadShape, ok := gateStructShape(t, srcs, "internal/scheduler", "payloadKey")
	require.True(t, ok, "payloadKey 必须存在")

	declared := map[string]bool{}
	for p, spec := range staticReadPaths {
		if spec.class == staticPathField && payloadShape.fields[spec.detail] {
			declared[spec.detail] = true
			require.True(t, strings.HasPrefix(p, snapRootAlias+".acc.Ext.CodexOAuth"),
				"载荷字段 %s 的读路径必须落在凭据三元组上，实际是 %s", spec.detail, p)
		}
	}
	want := make([]string, 0, len(payloadShape.fields))
	for f := range payloadShape.fields {
		want = append(want, f)
	}
	sort.Strings(want)
	got := make([]string, 0, len(declared))
	for f := range declared {
		got = append(got, f)
	}
	sort.Strings(got)
	require.Equal(t, want, got, "payloadKey 字段集与声明为载荷的读路径必须相等")
	require.NotEmpty(t, payloadProjectionFuncs, "载荷投影白名单不得为空")
	for f, why := range payloadProjectionFuncs {
		require.NotEmpty(t, strings.TrimSpace(why), "载荷投影白名单必须写明理由：%s", f)
		require.Contains(t, staticReaderFuncs, f, "载荷投影函数必须是声明的静态读者：%s", f)
	}
}
