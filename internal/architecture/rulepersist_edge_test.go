// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testFixtureAllow 里的 "internal/rulepersist" 是**测试侧全局放宽**（任何包的测试都能
// import 它）。本用例是它的对冲：把"生产侧没有这条边"从"矩阵碰巧没写"变成一条显式断言。
//
// 为什么需要对冲：矩阵的 scheduler 规则里没有 internal/rulepersist，看起来已经禁止了；
// 但那条规则同时被 testFixtureAllow 放宽成"测试可以 import"。一旦有人把
// internal/rulepersist 加进 scheduler 的**生产** to（为了让某个 helper 能编译），
// 矩阵会当场变绿——而 scheduler → rulepersist → repository 会把本单刚刚删掉的那条边
// 以两条边的形式请回来。故这里直接读源码的 import 块，不经矩阵。
func TestSchedulerProductionDoesNotImportRulepersist(t *testing.T) {
	root := repoRootForEdge(t)
	dir := filepath.Join(root, "internal", "scheduler")
	ents, err := os.ReadDir(dir)
	require.NoError(t, err, "internal/scheduler 必须存在")
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ImportsOnly)
		require.NoError(t, perr, "解析失败：%s", n)
		scanned++
		for _, spec := range f.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			require.NoError(t, uerr)
			require.NotEqual(t, "github.com/is7qin/c3api/internal/rulepersist", p,
				"%s import 了 internal/rulepersist：scheduler 的生产代码只许持批次会话契约，"+
					"规则落库（含它对 internal/repository 的依赖）必须留在装配根之外", n)
		}
	}
	// 反向 tripwire：扫描面必须真的看到 scheduler 的生产文件（目录写错 ⇒ 0 文件 ⇒ 恒绿）。
	require.NotEmpty(t, scanned, "扫描面不得为空：0 个生产文件会让本断言恒绿")
}

// repoRootForEdge 定位模块根（含 go.mod）。找不到即 FailNow 而不是返回猜测路径。
func repoRootForEdge(t *testing.T) string {
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
