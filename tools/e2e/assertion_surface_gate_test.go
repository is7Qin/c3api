// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 断言基线 gate（spec §4.3/§10.5；不带 e2e build tag => 普通 test job 跑）。
//
// 防漂移机制：
//  1. baseline 从 BASE commit 机械生成，gate 用 git show <source_commit> **重新计算**
//     并与 committed baseline 逐项比对 —— 同 commit 改 baseline 也会红；
//  2. 当前代码必须覆盖「BASE 断言面 - allowed_removals」（Test 名集合是 BASE 超集；
//     per-Test require 计数 / 指纹 multiset / t.Fatalf 计数不得低于下界）；
//  3. allowed_removals 仅 supplier/billing，且 evidence 必须能在 BASE 文件里找到；
//  4. 反空断言兜底（require.True(t,true) 等明显恒真项）；
//  5. CI 严格模式（GITHUB_ACTIONS=true）无法 git show => 红；本地无 git 历史才 skip+warn。
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// assertionBaselineSourceCommit baseline-v1 的源头 commit（spec §10.5：source_commit
// 必须等于 baseline 文件记录值；P1 生成 baseline-v2 时更新本常量并单独 commit）。
const assertionBaselineSourceCommit = "786b0072f8def5861e1e7d32b666caca7491023a"

// assertionBaselineRelPath baseline 文件（相对仓库根）。
const assertionBaselineRelPath = "tools/e2e/testdata/assertion_baseline.json"

// assertionAllowedRemovals P0 唯一允许的断言改写/删除白名单（仅 supplier/billing；
// 每条 evidence 是该断言在 BASE 源码里的可检索 token —— gate 会回查 BASE 原文）。
var assertionAllowedRemovals = []assertionRemoval{
	// P0 supplier 迁移：inline boot（DROP/CREATE 库、build、起 fakeupstream/网关、内联
	// -migrate）整体移交 harness => 该 Test 体内对应 NoError 断言消失（由 harness 的
	// require/t.Fatalf 语义接管；断言没有变弱，只是从场景搬到唯一生命周期所有者）。
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "a9b4469c3ecea2c7", Evidence: "C3API_REDIS_ADDR is required"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "d975fe627e7ac060", Evidence: "require.NoError(t, err)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "d975fe627e7ac060", Evidence: "require.NoError(t, err)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "d975fe627e7ac060", Evidence: "require.NoError(t, err)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "d975fe627e7ac060", Evidence: "require.NoError(t, err)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "d975fe627e7ac060", Evidence: "require.NoError(t, err)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "dee4ab9f5c50bfd7", Evidence: "require.NoError(t, cmd.Run(), \"build %s\", pkg)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "f67aed743f07cef6", Evidence: "up.Start()"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "e08fb212dffe5b67", Evidence: "supplierCfgTOML(supplierE2EAddr, jwtSecret, dsn, redisAddr, true)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "69a9ae4a59d2a2c6", Evidence: "require.NoError(t, cmd.Start())"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "d65280e555efc633", Evidence: "require.NoError(t, srv.Start())"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "f49ca957c4e62327", Evidence: "supplierCfgTOML(supplierE2EDisabledAddr, jwtSecret, dsn, redisAddr, false)"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "b6baba5300c9f39b", Evidence: "停用态 + 残留 liability ⇒ 拒绝启动"},
	// A23 停用 liability：手写 exec + 60s select 的 t.Fatalf 由 harness
	// ExpectExit{Code:1, Contains:"outstanding liability"} 承担（超时/退出码仍在断言）。
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "fatalf", Evidence: "停用态网关未在 60s 内退出"},
	// 断言漂移（当前 API 修复）：结算 JSON 字段已小写 id/revision/status；此处只改
	// 期望键名（"Id" -> "id"），期望语义不变。
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "f8b0fb2a115de4f5", Evidence: "jsonGet(t, rb, \"Id\")"},
	{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require", OldFingerprint: "ac7444aab71d52a2", Evidence: "jsonGet(t, rb, \"Status\")"},
}

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

	// 2) allowed_removals 白名单校验（作用域/证据/指纹存在性）。
	require.NoError(t, checkAllowedRemovals(t, baseline, baseFiles, base), "allowed_removals 非法")

	// 3) 当前代码覆盖 BASE 断言面（删/改白名单外断言 => 红）。
	currentFiles := readLocalTestFiles(t, filepath.Join(repoRoot, "tools", "e2e"))
	current := collectAssertionFiles(t, currentFiles)
	require.NoError(t, checkAssertionCoverage(baseline, base, current), "当前代码断言面低于 BASE 下界")

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
