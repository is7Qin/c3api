// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 断言基线 gate（spec §4.3/§10.5；不带 e2e build tag => 普通 test job 跑）。
// 当前为 baseline-v2：source=P1 迁移 commit（rebase 后 17963fdc），allowed_removals 已清空。
//
// 防漂移机制：
//  1. baseline 从 BASE commit 机械生成，gate 用 git show <source_commit> **重新计算**
//     并与 committed baseline 逐项比对 —— 同 commit 改 baseline 也会红；
//  2. 当前代码必须覆盖「BASE 断言面 - allowed_removals」（Test 名集合是 BASE 超集；
//     per-Test require 计数 / 指纹 multiset / t.Fatalf 计数不得低于下界）；
//  3. allowed_removals 只允许作用于场景 *_test.go（不许 harness/自测/gate 文件），
//     且 evidence 必须能在 BASE 文件里找到并与被移除断言相关；
//  4. 反空断言兜底（require.True(t,true) 等明显恒真项）；
//  5. CI 严格模式（GITHUB_ACTIONS=true）无法 git show => 红；本地无 git 历史才 skip+warn。
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// assertionBaselineSourceCommit baseline-v1 的源头 commit（spec §10.5：source_commit
// 必须等于 baseline 文件记录值；P1 生成 baseline-v2 时更新本常量并单独 commit）。
const assertionBaselineSourceCommit = "17963fdccb526bfe986c5039eee072b02becd2f1"

// assertionBaselineRelPath baseline 文件（相对仓库根）。
const assertionBaselineRelPath = "tools/e2e/testdata/assertion_baseline.json"

// assertionAllowedRemovals 唯一允许的断言改写/删除白名单（只作用于场景 *_test.go；
// 每条 evidence 是该断言在 BASE 源码里的可检索 token —— gate 会回查 BASE 原文与该
// 断言调用源码的相关性）。
//
// baseline-v2（spec §10.5/§12）已清空白名单：迁移完成后的断言面以 baseline-v2 为
// 唯一事实源。此后任何删除/改写都必须单独 commit（改本切片 + 重新生成 baseline）并
// 人工逐条核 evidence；白名单与 baseline 变更不得与「删断言」混在同一 commit
// （committed gate 的固有极限，见 P1 报告「review 纪律」）。
var assertionAllowedRemovals = []assertionRemoval{}

// TestAssertionSurfaceGate 断言基线 gate 主入口。
func TestAssertionSurfaceGate(t *testing.T) {
	repoRoot := gateRepoRoot(t)
	baseline := loadAssertionBaseline(t, repoRoot)

	// 1) baseline 必须与 BASE 源码重算结果逐项一致。
	baseFiles := gateBaseSources(t, repoRoot, baseline.SourceCommit)
	base := collectAssertionFiles(t, baseFiles)
	require.NoError(t, checkBaselineMatchesBase(t, baseline, base), "baseline 与 BASE 源码不一致")
	require.Equal(t, assertionBaselineSourceCommit, baseline.SourceCommit,
		"baseline.source_commit 必须等于 gate 常量（baseline 变更需单独 commit + review）")

	// 2) allowed_removals 白名单校验（作用域/证据相关性/指纹存在性）。
	require.NoError(t, checkAllowedRemovals(t, baseline, baseFiles, base), "allowed_removals 非法")

	// 2b) 白名单唯一事实源 = gate 源码 assertionAllowedRemovals：baseline JSON 必须
	// 逐项完全一致（同 commit 只往 JSON 追加白名单 + 删断言 => 红；oracle I1）。
	require.NoError(t, checkAllowedRemovalsSource(baseline, assertionAllowedRemovals),
		"baseline.allowed_removals 与 gate 源码 assertionAllowedRemovals 不一致")

	// 3) 当前代码覆盖 BASE 断言面（删/改白名单外断言 => 红）。
	currentFiles := readLocalTestFiles(t, filepath.Join(repoRoot, "tools", "e2e"))
	current := collectAssertionFiles(t, currentFiles)
	require.NoError(t, checkAssertionCoverage(baseline, base, current), "当前代码断言面低于 BASE 下界")

	// 3c) 每条白名单都必须对应「BASE 存在、当前已真正移除/改写」的断言（空转豁免 => 红）。
	require.NoError(t, checkRemovalsActuallyRemoved(baseline, base, current),
		"allowed_removals 与当前代码不一致（声称移除的断言仍在执行）")

	// 3b) Test 名集合必须是 BASE 超集（checkAssertionCoverage 已含此判定，单独再列
	// 一次便于失败信息可读）。
	var baseTestNames []string
	for name := range base.Surfaces {
		baseTestNames = append(baseTestNames, name)
	}
	sort.Strings(baseTestNames)
	for _, name := range baseTestNames {
		require.Contains(t, current.Surfaces, name, "BASE Test %s 必须仍然存在（可迁移，不可删除）", name)
	}

	// 4) 反空断言兜底（明显恒真项）。
	violations := scanTautologicalAssertions(t, currentFiles)
	require.Empty(t, violations, "反空断言门禁：明显恒真断言")

	// 4b) 不可达补偿兜底（spec §13 / P0 review momus Minor-2）：require/t.Fatalf 位于
	// if false / for false / 同一 block 内无条件 return/panic 之后 => 红（表面计数不能
	// 证明断言会执行）。
	unreachable := scanUnreachableAssertions(t, currentFiles)
	require.Empty(t, unreachable, "不可达上下文断言门禁：require/t.Fatalf 永不执行\n%s",
		strings.Join(unreachable, "\n"))
}

// TestAssertionUnreachableScanner 不可达补偿检测自测（spec §13 / P0 review momus
// Minor-2）：if false / for false / 无条件 return/panic 之后的断言必红；可达分支、
// 条件 return、非 Test 函数体不得误报（可见性口径与断言面采集一致）。
func TestAssertionUnreachableScanner(t *testing.T) {
	header := "package e2e\n\nimport \"testing\"\n\n"
	mk := func(body string) map[string][]byte {
		return map[string][]byte{"scenario_test.go": []byte(header + body)}
	}
	red := []struct{ name, body string }{
		{"if_false", "func TestUnreachableIf(t *testing.T) {\n\tvar err error\n\tif false {\n\t\trequire.NoError(t, err)\n\t}\n}\n"},
		{"for_false", "func TestUnreachableFor(t *testing.T) {\n\tvar err error\n\tfor false {\n\t\trequire.NoError(t, err)\n\t}\n}\n"},
		{"after_return", "func TestUnreachableReturn(t *testing.T) {\n\tvar err error\n\t_ = err\n\treturn\n\trequire.NoError(t, err)\n}\n"},
		{"after_panic", "func TestUnreachablePanic(t *testing.T) {\n\tvar err error\n\tpanic(\"boom\")\n\tt.Fatalf(\"never\")\n}\n"},
	}
	for _, tc := range red {
		require.NotEmpty(t, scanUnreachableAssertions(t, mk(tc.body)), "不可达补偿 %s 必须红", tc.name)
	}
	green := []struct{ name, body string }{
		{"conditional_return", "func TestReachableReturn(t *testing.T) {\n\tvar err error\n\tif err != nil {\n\t\treturn\n\t}\n\trequire.NoError(t, err)\n}\n"},
		{"if_true", "func TestReachableIf(t *testing.T) {\n\tvar err error\n\tif true {\n\t\trequire.NoError(t, err)\n\t}\n}\n"},
		// 非 Test 函数体不计入断言面（可见性口径一致），不可达断言与计数无关。
		{"helper_func", "func helper() {\n\tvar err error\n\tpanic(\"boom\")\n\trequire.NoError(t, err)\n}\n"},
	}
	for _, tc := range green {
		require.Empty(t, scanUnreachableAssertions(t, mk(tc.body)), "可达对照 %s 不得误报", tc.name)
	}
}

// TestAssertionRemovalScope 白名单作用域自测（spec §4.3/§14）：场景 *_test.go 可进
// 白名单；harness/自测/gate 文件与非测试文件一律越界（gate 不能自我豁免）。
func TestAssertionRemovalScope(t *testing.T) {
	for _, file := range []string{
		"billing_e2e_test.go", "routing_e2e_test.go", "routing_multi_e2e_test.go",
		"supplier_earnings_e2e_test.go", "request_patch_e2e_test.go", "harness_selftest_test.go",
	} {
		require.True(t, assertionRemovalScope(file), "场景文件 %s 必须在白名单作用域内", file)
	}
	for _, file := range []string{
		"harness_test.go", "harness_gate_test.go", "harness_boundary_gate_test.go",
		"harness_registry_test.go", "harness_process_test.go", "harness_services_test.go",
		"assertion_surface_gate_test.go", "harness_env.go", "",
	} {
		require.False(t, assertionRemovalScope(file), "%s 不得进白名单作用域（gate 不能自我豁免）", file)
	}
}

// TestAssertionBaselineGenerate baseline 机械生成器（只在显式环境变量下运行；baseline
// 变更必须单独 commit + review，禁止在日常 gate 中静默重写）。
//
//	E2E_BASELINE_WRITE=1 go test ./tools/e2e -run TestAssertionBaselineGenerate
//	E2E_BASELINE_DUMP=1  额外打印 BASE per-Test 指纹与表达式（人工核对白名单用）
func TestAssertionBaselineGenerate(t *testing.T) {
	if os.Getenv("E2E_BASELINE_WRITE") != "1" {
		t.Skip("设置 E2E_BASELINE_WRITE=1 才重新生成 baseline（单独 commit + review）")
	}
	repoRoot := gateRepoRoot(t)
	baseFiles := gateBaseSources(t, repoRoot, assertionBaselineSourceCommit)
	base := collectAssertionFiles(t, baseFiles)
	baseline := assertionBaseline{
		SourceCommit:    assertionBaselineSourceCommit,
		Tests:           map[string]assertionTestSurface{},
		AllowedRemovals: assertionAllowedRemovals,
	}
	testNames := make([]string, 0, len(base.Surfaces))
	for name := range base.Surfaces {
		testNames = append(testNames, name)
	}
	sort.Strings(testNames)
	for _, name := range testNames {
		baseline.Tests[name] = base.Surfaces[name]
	}
	data, err := json.MarshalIndent(baseline, "", "  ")
	require.NoError(t, err)
	data = append(data, '\n')
	outPath := filepath.Join(repoRoot, assertionBaselineRelPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(outPath), 0o755))
	require.NoError(t, os.WriteFile(outPath, data, 0o644), "写 baseline：%s", outPath)
	t.Logf("已生成 baseline（source_commit=%s，%d 个 Test）：%s",
		baseline.SourceCommit, len(baseline.Tests), outPath)

	if os.Getenv("E2E_BASELINE_DUMP") == "1" {
		for _, name := range testNames {
			surface := base.Surfaces[name]
			t.Logf("TEST %s file=%s requires=%d fatalf=%d fingerprints=%d",
				name, surface.File, surface.Requires, surface.Fatalf, len(surface.Fingerprints))
			fps := make([]string, 0, len(surface.Fingerprints))
			for fp := range surface.Fingerprints {
				fps = append(fps, fp)
			}
			sort.Strings(fps)
			for _, fp := range fps {
				t.Logf("  FP %s count=%d expr=%s", fp, surface.Fingerprints[fp], base.Exprs[fp])
			}
		}
	}
}
