// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 运行期哨兵在数据面的**单边使用**（P3c I-B 修正案 §C/§D 的机械半边）。
//
// 判据只有一句：internal/proxy 的**生产**代码里，五条哨兵只许出现 `api.ErrX`。
// 为什么非有这条不可：`scheduler.ErrX` 是别名，值相同 ⇒ 任何行为断言都察觉不到
// "有人把某个返回点/判定点改回旧拼写"。半搬状态（返回用旧值、判定用新值，或反之）
// 在别名机制下**行为完全等价**，于是它会一直留在那里，直到将来删别名的那天炸开。
// 静态拼写是这种漂移唯一的观测面。
//
// 与 `internal/scheduler/errors_compat_test.go` 的分工：那边钉"别名是同一 error 值"
// （值身份），这边钉"生产代码不靠别名活着"（拼写面）。两条合起来才是 §D 的终态：
// proxy 全部指 api.*，scheduler 只留 `var ErrX = api.ErrX`。
//
// 扫描面 = 本目录非测试 .go（与 datapath/credential 门禁同源，见 proxySourceFiles）；
// 测试文件不在扫描面内：`error_mapping_test.go` / `failover_plan_test.go` 用旧拼写
// 喂映射函数是**允许**的（别名 ⇒ 同一值，语义等价），它们也是别名还要留着的理由。
const runtimeSentinelAPISitesFloor = 18

// runtimeSentinelNames 是 I-B 的五条哨兵（api.* 与 scheduler.* 同名）。
var runtimeSentinelNames = map[string]struct{}{
	"ErrGroupNotFound":     {},
	"ErrFormatUnavailable": {},
	"ErrPlanNotReady":      {},
	"ErrNoAvailable":       {},
	"ErrAttemptsExhausted": {},
}

func TestProxyProductionUsesAPISentinelsOnly(t *testing.T) {
	paths := proxySourceFiles(t)
	require.NotEmpty(t, paths, "internal/proxy 非测试源为 0：门禁在空转")

	fset := token.NewFileSet()
	var violations []string
	apiSites := 0
	for _, path := range paths {
		af, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "解析 %s", path)
		ast.Inspect(af, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			if _, isSentinel := runtimeSentinelNames[name]; !isSentinel {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch pkg.Name {
			case "scheduler":
				violations = append(violations,
					fset.Position(sel.Pos()).String()+"：`scheduler."+name+"` —— 返回点/判定点必须指 api."+name)
			case "api":
				apiSites++
			}
			return true
		})
	}
	sort.Strings(violations)
	require.Empty(t, violations, "数据面生产代码引用了别名而非 canonical（共 %d 条）：\n%s",
		len(violations), strings.Join(violations, "\n"))

	// 反向 tripwire：没有 api.* 引用说明判定/返回面被删空了，或扫描面静默缩小。
	// 今天 18 个位点（forward.go 映射面 12 / caller.go 转换判定 3 / failover_plan.go 返回 3）。
	require.GreaterOrEqual(t, apiSites, runtimeSentinelAPISitesFloor,
		"api 哨兵位点 %d < %d：映射/返回面被削掉了一层（或扫描面缩小）", apiSites, runtimeSentinelAPISitesFloor)
	t.Logf("扫描面：%d 个非测试源，api 哨兵位点 %d，scheduler 别名引用 %d", len(paths), apiSites, len(violations))
}
