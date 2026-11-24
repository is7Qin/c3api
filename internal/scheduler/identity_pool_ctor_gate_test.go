// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// AST 误用门禁（P5b 修正 4 的后半）：会话池的**构造入口**只允许出现在两处——
// `buildIdentityPools`（装配期建池/同步）与 `claimCodexSession`（全忙兜底的临时
// 身份）。别处出现 `identitypool.Spec{…}` 或 `identitypool.NewSession(…)` 都是
// 请求路径把池当每请求对象用：k=8 是 58 allocs/3360 B/4.5 µs，k=1024 是
// 7170 allocs/419 KB/478 µs，而热路径本身是 0 alloc——误用的代价是 58× 分配，
// 且在 publisher.mu 里开 K 条线程。
//
// 为什么门禁长这样（沿用 snapshot_static_ctor_gate_test.go 的形态）：
//   - 扫**整个仓库**的非测试 .go（不只是本包）：真正的风险在别的包调这两个入口；
//   - 判据是"所在函数名"而不是"行号"：函数被移动/重命名时门禁仍然有效；
//   - 带负例自检（同一台扫描器必须能钉住违规样例），否则门禁路径写歪也无从发现。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// poolCtorAllowed 是构造入口的白名单（函数名）。identitypool 包自身不扫：
// Resize/Sync/NewSession 的实现就在那里，构造是它们的本职。
var poolCtorAllowed = map[string]string{
	"buildIdentityPools": "装配期按 byID 建池/复用（publisher.mu 内，一次 reload 一次）",
	"claimCodexSession":  "全忙兜底的临时身份（不落池、无代际/退役面）",
}

// TestIdentityPoolConstructedOnlyByWhitelistedCallSites 生产代码里只有那两处能碰
// 池/会话的构造入口。
func TestIdentityPoolConstructedOnlyByWhitelistedCallSites(t *testing.T) {
	root := gateRoot(t)
	var offending []string
	for _, top := range []string{"internal", "cmd", "pkg"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// 池包自身豁免：构造入口的实现与它的白盒用例都在里面。
				if d.Name() == "identitypool" {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			offending = append(offending, scanPoolCtorSites(filepath.ToSlash(rel), f)...)
			return nil
		})
		require.NoError(t, err)
	}
	sort.Strings(offending)
	require.Empty(t, offending,
		"会话池/会话只能经 buildIdentityPools / claimCodexSession 构造（每请求建池 = 58× 分配）：%v", offending)
}

// scanPoolCtorSites 报告 f 里所有 `identitypool.Spec{…}` 复合字面量与
// `identitypool.NewSession(…)` 调用，且所在函数不在白名单里。
func scanPoolCtorSites(filename string, f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		fname := fn.Name.Name
		if _, allowed := poolCtorAllowed[fname]; allowed {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				if isIdentityPoolSelector(v.Type, "Spec") {
					out = append(out, filename+"#"+fname+" identitypool.Spec{}")
				}
			case *ast.CallExpr:
				if isIdentityPoolSelector(v.Fun, "NewSession") {
					out = append(out, filename+"#"+fname+" identitypool.NewSession()")
				}
			}
			return true
		})
	}
	return out
}

// isIdentityPoolSelector 判定 e 是否为 `identitypool.<name>`。
func isIdentityPoolSelector(e ast.Expr, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "identitypool"
}

// TestIdentityPoolCtorGateRejectsBypasses 门禁的负例自检：同一台扫描器必须钉住
// 请求路径上的 Spec 字面量与 NewSession 调用，同时**不**误报白名单里的两处——
// 没有这段自检，门禁一旦写歪（比如只匹配裸 `Spec{`）就会静默放行。
func TestIdentityPoolCtorGateRejectsBypasses(t *testing.T) {
	const fixture = `package proxy

import "github.com/is7qin/c3api/internal/identitypool"

type Spec = identitypool.Spec

func requestPathBuild() {
	_ = identitypool.Spec{AccountID: 1, K: 8}
}

func requestPathSession() {
	_ = identitypool.NewSession("inst", identitypool.RotatePolicy{})
}

func buildIdentityPools() {
	_ = identitypool.Spec{AccountID: 1, K: 8}
}

func claimCodexSession() {
	_ = identitypool.NewSession("inst", identitypool.RotatePolicy{})
}

func unrelated() {
	type local struct{ K int }
	_ = local{K: 8}
}

func aliasedSpec() {
	_ = Spec{K: 8} // 经类型别名：名字不是 identitypool.Spec，本门不追（见测试注释）
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", fixture, parser.SkipObjectResolution)
	require.NoError(t, err)
	findings := scanPoolCtorSites("fixture.go", f)
	joined := strings.Join(findings, "\n")
	require.Contains(t, joined, "fixture.go#requestPathBuild identitypool.Spec{}", "请求路径上的 Spec 字面量必须被拒：%v", findings)
	require.Contains(t, joined, "fixture.go#requestPathSession identitypool.NewSession()", "请求路径上的 NewSession 必须被拒：%v", findings)
	require.NotContains(t, joined, "buildIdentityPools", "白名单调用点不得被误报：%v", findings)
	require.NotContains(t, joined, "claimCodexSession", "白名单调用点不得被误报：%v", findings)
	require.NotContains(t, joined, "unrelated", "非目标类型不得被误报：%v", findings)
	require.Len(t, findings, 2, "恰好两处违规（别名写法本门不追，那是 reviews 的事）：%v", findings)
}
