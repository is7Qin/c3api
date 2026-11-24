// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// 本文件钉住 NewBatch / repoBatch 的两条**实现侧**契约（loadport.BatchedGroupLoader 的
// 注释把它们写成"违约"而不是"建议"），且**不碰 PG**：
//   - NewBatch 必须给非 nil、非 typed-nil、且跨批次独立的作用域；
//   - LoadGroup 必须把**会话自持的那一张**作用域交给载入，不许每次新建。
//
// 为什么非 PG 侧必须有它：跨组模板指针共享这条不变量此前的唯一行为防线是两条 PG 用例
// （pg_template_share_test.go 的跨组 Same、digest_cache_pg_test.go 的 misses==1）。
// 没有 PG 的 CI 上那两条整条跳过 ⇒ 让 LoadGroup 每次新建一张表（跨组共享归零、
// 每组各转一枚 *domain.Template）会**100% 静默**：功能等价、性能悄悄变差。
// 所以这里用两条独立判据把同一件事钉在无库环境：
//  1. 形状：LoadGroup 只许把 b.scope 交出去（AST，见 TestRepoBatchLoadGroupUsesSessionScope）；
//  2. 行为：会话自持的那张表确实让"两个不同组、两枚不同的 ent 模板实体"折成一枚
//     *domain.Template —— 这正是跨组命中所依赖的性质。

// TestGroupRepoNewBatchReturnsUsableSession 钉住批次壳与作用域的所有权。
func TestGroupRepoNewBatchReturnsUsableSession(t *testing.T) {
	repo := &GroupRepo{}

	batch := repo.NewBatch()
	require.NotNil(t, batch, "NewBatch 必须返回非 nil 的批次")
	// typed-nil（接口非 nil、底层指针 nil）由上面这条抓到：testify 的 NotNil 会反射进
	// 接口取动态值，(*repoBatch)(nil) 会被判成 nil。理论上的漏洞是"另一个指针类型"，
	// 故下面再钉具体类型（只判 Kind == reflect.Ptr 会让 *anything 都过）。
	require.IsType(t, &repoBatch{}, batch,
		"生产实现必须是 *repoBatch（只判 Kind 会让任意指针类型过）")

	// 会话必须自持一张非 nil 作用域：scope 为 nil 时 LoadGroupAccountsScoped 会**每次
	// 新建**一张表，跨组共享静默消失（功能等价、性能归零）。
	rb, ok := batch.(*repoBatch)
	require.True(t, ok, "生产实现必须是 *repoBatch")
	require.NotNil(t, rb.scope, "批次必须自持一张模板作用域（nil 会把跨组共享退化成每组一份）")
	require.Same(t, repo, rb.repo, "会话必须回指同一仓库实例")

	// 每次 NewBatch 必须是**独立**的批次与会话：跨批次复用会让上一轮的模板对象漏进
	// 新快照（模板改动不生效）。用"写一张、查另一张"验，不比较 map 内部指针。
	second := repo.NewBatch()
	require.NotSame(t, batch, second, "两次 NewBatch 必须是两个会话")
	rb2, ok := second.(*repoBatch)
	require.True(t, ok)
	require.NotSame(t, rb.scope, rb2.scope, "两次 NewBatch 必须是两张独立的作用域")
	probe := &domain.Template{ID: 9001}
	rb.scope.templates[probe.ID] = probe
	_, leaked := rb2.scope.templates[probe.ID]
	require.False(t, leaked, "写第一张作用域不得让第二张看见（共享表 = 跨批次陈旧摘要）")
}

// TestRepoBatchLoadGroupUsesSessionScope 是"会话自持一张表"在**无库环境**下的两条判据。
//
// 判据 1（形状 / AST）直接读 group_repo.go：`LoadGroup` 的唯一 return 必须把 `b.scope`
// 作为作用域实参传下去，且函数体内不得出现 `NewTemplateLoadScope()`。这一条专抓
// "每次 LoadGroup 新建一张表"——它在纯单元测试里完全不可见（每张新表都是合法的
// TemplateLoadScope），只有真库 + 跨组才能显形。
func TestRepoBatchLoadGroupUsesSessionScope(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) 必须可用")
	path := filepath.Join(filepath.Dir(self), "group_repo.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, "解析 %s", path)

	loadGroup := findMethod(t, f, "repoBatch", "LoadGroup")
	require.Len(t, loadGroup.Body.List, 1, "LoadGroup 只能是单条 return：载入委托不该有第二个分支")

	ret, ok := loadGroup.Body.List[0].(*ast.ReturnStmt)
	require.True(t, ok, "LoadGroup 的唯一语句必须是 return")
	// 纯委托的形状是 `return b.repo.X(ctx, gid, b.scope)`：一个 Results 元素、两个返回值
	// （Go 允许把多值调用直接 return）。拆成 `accs, err := …; return accs, err` 会让
	// Body.List 变 2 条，上面那条先红。
	require.Len(t, ret.Results, 1, "LoadGroup 必须 return 委托调用本身（不是拆开再拼回去）")
	call, ok := ret.Results[0].(*ast.CallExpr)
	require.True(t, ok, "LoadGroup 必须 return 一次载入调用")
	require.Len(t, call.Args, 3, "委托目标必须吃 (ctx, groupID, scope)")
	scopeArg, ok := call.Args[2].(*ast.SelectorExpr)
	require.True(t, ok, "第 3 个实参必须是选择器（b.scope），发现 %T", call.Args[2])
	recv, ok := scopeArg.X.(*ast.Ident)
	require.True(t, ok)
	require.Equal(t, "b", recv.Name, "作用域必须来自会话自身（b.scope）")
	require.Equal(t, "scope", scopeArg.Sel.Name,
		"作用域实参必须是 b.scope：写成 NewTemplateLoadScope() 会让跨组共享归零，且非 PG 用例全绿")

	// 反向：建表只能发生在 NewBatch 里，且恰好一次（每批次一张）。
	require.NotContains(t, identsIn(loadGroup.Body), "NewTemplateLoadScope",
		"LoadGroup 内不得建表：那等于每组每模板一次（无 PG 的 CI 上完全不可见）")
	newBatch := findMethod(t, f, "GroupRepo", "NewBatch")
	require.Equal(t, 1, countCalls(newBatch.Body, "NewTemplateLoadScope"),
		"NewBatch 必须恰好建一张表（每次调用一张，跨批次不共享）")

	// 判据 2（行为）：会话自持的那张表，让两个"不同的组"的两次 Ent 查询（两枚**不同**
	// 的 ent 模板实体、同一个 template_id）折成同一枚 *domain.Template。这正是
	// TemplateLoadScope 注释里"跨查询复用"那条语义，也是摘要缓存跨组命中的前提。
	repo := &GroupRepo{}
	session := repo.NewBatch().(*repoBatch)
	rowG1 := shareEntAccount(1, 11, shareEntTemplate(11, "tpl-a", "m-a"))
	rowG2 := shareEntAccount(2, 11, shareEntTemplate(11, "tpl-a", "m-a")) // 另一枚实体，同一模板 ID
	accG1 := toDomainAccountWithTemplateScope(rowG1, session.scope)
	accG2 := toDomainAccountWithTemplateScope(rowG2, session.scope)
	require.NotSame(t, rowG1.Edges.Template, rowG2.Edges.Template, "夹具前提：两枚不同的 ent 实体")
	require.Same(t, accG1.Template, accG2.Template,
		"同一会话内跨两次组载入必须共享同一枚 *domain.Template（跨组命中的唯一来源）")

	// 反向：下一个批次不许复用这张表（模板内容改动必须可见）。
	next := repo.NewBatch().(*repoBatch)
	accNext := toDomainAccountWithTemplateScope(rowG1, next.scope)
	require.NotSame(t, accG1.Template, accNext.Template,
		"跨批次必须是新对象：会话不得跨批次复用作用域")
}

// findMethod 找 (recv *T) Name 的方法声明；缺失即 FailNow。
func findMethod(t *testing.T, f *ast.File, recv, name string) *ast.FuncDecl {
	t.Helper()
	var hits []*ast.FuncDecl
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != name || len(fn.Recv.List) != 1 {
			continue
		}
		if recvName(fn.Recv.List[0].Type) == recv {
			hits = append(hits, fn)
		}
	}
	require.Len(t, hits, 1, "必须恰好有一处 (*%s).%s", recv, name)
	return hits[0]
}

// recvName 取接收者类型名（兼容 T / *T / *T[P]）。
func recvName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return recvName(v.X)
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr:
		return recvName(v.X)
	case *ast.IndexListExpr:
		return recvName(v.X)
	}
	return ""
}

// countCalls 数 node 内对名为 name 的函数的调用次数（包级或限定调用都算）。
func countCalls(node ast.Node, name string) int {
	n := 0
	ast.Inspect(node, func(x ast.Node) bool {
		ce, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := ce.Fun.(type) {
		case *ast.Ident:
			if fun.Name == name {
				n++
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == name {
				n++
			}
		}
		return true
	})
	return n
}

// identsIn 把 node 下全部标识符拼成一行（只用于"不得出现某符号"的存在性检查）。
func identsIn(node ast.Node) string {
	var out []byte
	ast.Inspect(node, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok {
			out = append(out, id.Name...)
			out = append(out, ' ')
		}
		return true
	})
	return string(out)
}
