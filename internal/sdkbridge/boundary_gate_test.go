// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"go/build"
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

// 边界门禁：**除了本包，任何包都不许 import codex-sdk。**
//
// 为什么不是 `rg`：文本搜索分不清 import、注释、字符串与文档；别名（`sdk "…"`）、
// 点导入、`//go:build` 排除的文件、生成文件、`_test.go` 都是绕过面。这里按
// `go/parser` 读 `ImportSpec.Path` 的**字面路径**，并用 `go/build.MatchFile` 让
// 构建约束生效（含 `_test.go`）——别名改不了路径，注释不会被当成 import。
//
// 断言是**双向相等**：新增一处越界 import ⇒ 红；把某处修好而没同步收缩白名单 ⇒
// 也红（陈旧条目同样算错）。这样"允许集"永远等于"尚未迁移的清单"，而不是一张
// 只会变长的豁免表。
//
// 测试文件用**上限棘轮**而不是逐文件白名单：`internal/proxy` 与 `internal/scheduler`
// 的用例大量构造 SDK 值（IdentityState、RotatePolicy、ConversationRequest），逐个
// 列出来只会变成噪音。这里只钉"最多还有几个测试文件持 SDK"，上限只许降，T3 降到 0。

// sdkImportAllowlist 非测试文件里仍允许 import codex-sdk 的站点，值写"为什么还不能
// 清"。T1 快照；T2 删三行（Fatal 面）；T3 清空。
var sdkImportAllowlist = map[string]string{
	"internal/proxy/codex_responses_ws.go": "T3：*codexsdk.Client / Option / Identity 的 dial 参数包（CallRequest 未落地）",
	"internal/proxy/codex_responses_http.go": "T3：codexsdk.Identity 作为 codexRequestPrep 的返回值（CallRequest 未落地）",
	"internal/scheduler/scheduler.go":       "T3：Config.RotatePolicy / IdentityFor 返回 SDK 值",
	"internal/scheduler/identity_pool.go":   "T3：IdentityState / Step / Session / Turn / Window 的持有者",
}

// sdkTestImportCeiling 仍持 SDK 的**测试文件**数上限（不含本包）。只许降；T3 归零。
const sdkTestImportCeiling = 8

// sdkImportPath 是被垄断的 import 路径前缀（含子包，如 .../redisstore）。
const sdkImportPath = "github.com/is7Qin/codex-sdk"

// selfPkgDir 本包目录（相对模块根）。
const selfPkgDir = "internal/sdkbridge"

// TestSDKImportsOnlyInBridge 边界门禁本体。
func TestSDKImportsOnlyInBridge(t *testing.T) {
	root := moduleRoot(t)
	var prodHits, testHits []string

	for _, f := range goFiles(t, root) {
		rel := relSlash(root, f)
		if strings.HasPrefix(rel, selfPkgDir+"/") {
			continue // 本包是这段边界的唯一所有者
		}
		importsSDK := false
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		require.NoError(t, err, "解析 %s", rel)
		for _, spec := range af.Imports {
			p := strings.Trim(spec.Path.Value, `"`)
			if p == sdkImportPath || strings.HasPrefix(p, sdkImportPath+"/") {
				importsSDK = true
				break
			}
		}
		if !importsSDK {
			continue
		}
		if strings.HasSuffix(rel, "_test.go") {
			testHits = append(testHits, rel)
		} else {
			prodHits = append(prodHits, rel)
		}
	}
	sort.Strings(prodHits)
	sort.Strings(testHits)

	// 1) 非测试文件：实际集合 == 白名单（双向）。
	got := map[string]bool{}
	for _, f := range prodHits {
		got[f] = true
		if _, ok := sdkImportAllowlist[f]; !ok {
			t.Errorf("越界 import：%s 导入了 codex-sdk。SDK 只允许出现在 %s/ ——"+
				"要放行必须显式加进 sdkImportAllowlist 并写明理由（等同评审）", f, selfPkgDir)
		}
	}
	for f, why := range sdkImportAllowlist {
		if !got[f] {
			t.Errorf("白名单陈旧：%s 已不再 import codex-sdk（%s）。请删掉这一行——"+
				"白名单必须恒等于「尚未迁移的清单」", f, why)
		}
	}

	// 2) 测试文件：数量棘轮（只降不升）。
	if len(testHits) > sdkTestImportCeiling {
		t.Errorf("持 SDK 的测试文件从 %d 涨到 %d（上限 sdkTestImportCeiling=%d）："+
			"新用例请改用 sdkbridge 的宿主类型\n%s",
			sdkTestImportCeiling, len(testHits), sdkTestImportCeiling,
			strings.Join(testHits, "\n"))
	}
	t.Logf("边界快照：非测试文件 %d 处（白名单 %d 条）、测试文件 %d 个（上限 %d）",
		len(prodHits), len(sdkImportAllowlist), len(testHits), sdkTestImportCeiling)
}

// moduleRoot 从当前测试的工作目录向上找 go.mod。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到模块根（go.mod）")
		}
		dir = parent
	}
}

// goFiles 枚举模块内所有**被当前构建约束接受**的 .go 文件（含 _test.go 与生成代码）。
// 生成目录（internal/ent）也扫：它同样不许 import SDK。
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	ctx := build.Default
	skip := map[string]bool{
		".git": true, "node_modules": true, "testdata": true,
		"bin": true, "tmp": true, ".crush": true, "web": true,
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		ok, err := ctx.MatchFile(filepath.Dir(path), name)
		if err != nil {
			return err
		}
		if ok {
			out = append(out, path)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// relSlash 模块内相对路径（统一 / 分隔，与白名单键一致）。
func relSlash(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
