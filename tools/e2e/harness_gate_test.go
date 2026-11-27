// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件不带 e2e build tag：断言/资源边界 gate 的共享实现，普通 test job 跑
// （spec §4.3/§4.4；本文件在 boundary gate 的显式白名单内 —— 变异测试会故意生成
// 固定端口等违规副本，只有 harness 自身可写这些字面量）。
package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// gateRepoRoot 仓库根目录（从本文件位置上跳 3 层：tools/e2e -> 根）。
func gateRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller 失败")
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// ---------- 断言面（assertion surface）采集 ----------

// assertionTestSurface 单个 Test 函数的断言面：require 调用计数 + 归一化 AST 指纹
// （multiset）+ t.Fatalf 计数。
type assertionTestSurface struct {
	File         string         `json:"file"`
	Requires     int            `json:"requires"`
	Fatalf       int            `json:"fatalf"`
	Fingerprints map[string]int `json:"fingerprints"`
}

// assertionBaseline 机械生成的 BASE 断言基线（gate 用 git show 重算比对；任何差异红）。
type assertionBaseline struct {
	SourceCommit    string                          `json:"source_commit"`
	Tests           map[string]assertionTestSurface `json:"tests"`
	AllowedRemovals []assertionRemoval              `json:"allowed_removals"`
}

// assertionRemoval 白名单断言改写/删除（仅 supplier/billing；evidence 必须在 BASE
// 文件里可找到，防止白名单被滥用成「随手删断言」）。
type assertionRemoval struct {
	File           string `json:"file"`
	Test           string `json:"test"`
	Kind           string `json:"kind"` // require | fatalf
	OldFingerprint string `json:"old_fingerprint,omitempty"`
	Evidence       string `json:"evidence"`
}

// assertionCollector 采集结果（附 expr 便于 gate 失败信息定位）。
type assertionCollector struct {
	Surfaces map[string]assertionTestSurface
	Exprs    map[string]string // fingerprint -> 归一化表达式
}

// requireFingerprint require 调用的归一化 AST 指纹：go/types.ExprString 生成与源码
// 位置无关的规范表达式，sha256 前缀 16 hex。字符串/数字字面量保留原值 => 改断言
// 期望值（如 200 -> 201）必然变指纹（须进 allowed_removals 白名单）。
func requireFingerprint(call *ast.CallExpr) string {
	expr := types.ExprString(call)
	sum := sha256.Sum256([]byte(expr))
	return hex.EncodeToString(sum[:])[:16]
}

// collectAssertionFiles 从「文件内容」采集 per-Test 断言面（BASE 与当前代码共用同一
// 采集器 => 机械可复算）。
func collectAssertionFiles(t *testing.T, files map[string][]byte) assertionCollector {
	t.Helper()
	out := assertionCollector{Surfaces: map[string]assertionTestSurface{}, Exprs: map[string]string{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, content, parser.ParseComments)
		require.NoError(t, err, "解析 %s", name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			surface := assertionTestSurface{File: name, Fingerprints: map[string]int{}}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				qual, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case qual.Name == "require":
					surface.Requires++
					fp := requireFingerprint(call)
					surface.Fingerprints[fp]++
					out.Exprs[fp] = types.ExprString(call)
				case qual.Name == "t" && sel.Sel.Name == "Fatalf":
					surface.Fatalf++
				}
				return true
			})
			if prev, exists := out.Surfaces[fn.Name.Name]; exists {
				// 同名 Test 函数（不允许）：合并会让覆盖判定失真 => 直接报错。
				require.Failf(t, "重复 Test 函数", "%s 同时定义于 %s 与 %s", fn.Name.Name, prev.File, name)
			}
			out.Surfaces[fn.Name.Name] = surface
		}
	}
	return out
}

// readLocalTestFiles 读目录下的 *_test.go（当前代码面）。
func readLocalTestFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "读目录 %s", dir)
	files := map[string][]byte{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, "读 %s", name)
		files[name] = data
	}
	return files
}

// gitRun 执行 git（gate 用；输出 trimmed）。
func gitRun(t *testing.T, repoRoot string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// gateBaseSources 取 BASE commit 的 tools/e2e/*_test.go 原文。
// CI 严格模式（GITHUB_ACTIONS=true）下无法取到 => Fatal（不允许 skip+warn，spec §9.2）；
// 本地非 CI 且根本无 git 历史 => Skip+warn；git 存在但 BASE 取不到 => 红。
func gateBaseSources(t *testing.T, repoRoot, commit string) map[string][]byte {
	t.Helper()
	if _, err := gitRun(t, repoRoot, "rev-parse", "--git-dir"); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("CI 严格模式：无法读取 git 历史，assertion gate 必须红（需要 fetch-depth: 0）：%v", err)
		}
		t.Skipf("本地无 git 历史，assertion gate 跳过（CI 严格模式不允许 skip）：%v", err)
	}
	out, err := gitRun(t, repoRoot, "ls-tree", "--name-only", commit, "tools/e2e/")
	if err != nil {
		t.Fatalf("CI/本地严格模式：BASE commit %s 不可达（浅克隆？需要 fetch-depth: 0）：%v", commit, err)
	}
	files := map[string][]byte{}
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimPrefix(strings.TrimSpace(line), "tools/e2e/")
		if name == "" || !strings.HasSuffix(name, "_test.go") || strings.Contains(name, "/") {
			continue
		}
		content, serr := gitRun(t, repoRoot, "show", commit+":tools/e2e/"+name)
		if serr != nil {
			t.Fatalf("读取 BASE 文件 %s 失败：%v", name, serr)
		}
		files[name] = []byte(content)
	}
	require.NotEmpty(t, files, "BASE commit 下没有任何 e2e *_test.go（source_commit 错？）")
	return files
}

// loadAssertionBaseline 读 committed baseline。
func loadAssertionBaseline(t *testing.T, repoRoot string) *assertionBaseline {
	t.Helper()
	path := filepath.Join(repoRoot, assertionBaselineRelPath)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "读 assertion baseline：%s", path)
	var baseline assertionBaseline
	require.NoError(t, json.Unmarshal(data, &baseline), "解析 assertion baseline")
	require.NotEmpty(t, baseline.SourceCommit, "baseline.source_commit 必须非空")
	require.NotEmpty(t, baseline.Tests, "baseline.tests 必须非空")
	return &baseline
}

// checkBaselineMatchesBase baseline 与 BASE 源码重算结果必须逐项一致。
// 「同 commit 改 baseline」会在这里红（baseline 不能用长断言吃掉真实断言）。
func checkBaselineMatchesBase(t *testing.T, baseline *assertionBaseline, base assertionCollector) error {
	if baseline.SourceCommit != assertionBaselineSourceCommit {
		return fmt.Errorf("baseline.source_commit=%s 与 gate 常量 source_commit=%s 不一致（改 baseline 必须单独 commit + review）",
			baseline.SourceCommit, assertionBaselineSourceCommit)
	}
	if len(baseline.Tests) != len(base.Surfaces) {
		return fmt.Errorf("baseline 记录了 %d 个 Test，BASE 源码有 %d 个", len(baseline.Tests), len(base.Surfaces))
	}
	names := make([]string, 0, len(base.Surfaces))
	for name := range base.Surfaces {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := base.Surfaces[name]
		got, ok := baseline.Tests[name]
		if !ok {
			return fmt.Errorf("baseline 缺少 BASE Test %s（file=%s）", name, want.File)
		}
		if got.File != want.File {
			return fmt.Errorf("baseline Test %s file=%s，BASE 实际 file=%s", name, got.File, want.File)
		}
		if got.Requires != want.Requires {
			return fmt.Errorf("baseline Test %s require 计数=%d，BASE 重算=%d", name, got.Requires, want.Requires)
		}
		if got.Fatalf != want.Fatalf {
			return fmt.Errorf("baseline Test %s t.Fatalf 计数=%d，BASE 重算=%d", name, got.Fatalf, want.Fatalf)
		}
		if len(got.Fingerprints) != len(want.Fingerprints) {
			return fmt.Errorf("baseline Test %s 指纹种类=%d，BASE 重算=%d（baseline 被篡改？）",
				name, len(got.Fingerprints), len(want.Fingerprints))
		}
		for fp, count := range want.Fingerprints {
			if got.Fingerprints[fp] != count {
				return fmt.Errorf("baseline Test %s 指纹 %s 计数=%d，BASE 重算=%d（expr=%s）",
					name, fp, got.Fingerprints[fp], count, base.Exprs[fp])
			}
		}
	}
	return nil
}

// checkAllowedRemovals 白名单校验：作用域仅 supplier/billing；evidence 必须能在 BASE
// 文件原文找到；old_fingerprint 必须确实存在于 BASE 该 Test 的指纹里。
func checkAllowedRemovals(t *testing.T, baseline *assertionBaseline, baseFiles map[string][]byte, base assertionCollector) error {
	removalsByTest := map[string]map[string]int{} // test -> fingerprint -> count
	fatalfByTest := map[string]int{}
	for i, removal := range baseline.AllowedRemovals {
		if removal.File != "supplier_earnings_e2e_test.go" && removal.File != "billing_e2e_test.go" {
			return fmt.Errorf("allowed_removals[%d] 文件 %q 越界（仅允许 supplier/billing，spec §4.3）", i, removal.File)
		}
		baseSurface, ok := base.Surfaces[removal.Test]
		if !ok {
			return fmt.Errorf("allowed_removals[%d] Test %q 不存在于 BASE", i, removal.Test)
		}
		if baseSurface.File != removal.File {
			return fmt.Errorf("allowed_removals[%d] Test %q 在 BASE 的文件是 %s（不是 %s）",
				i, removal.Test, baseSurface.File, removal.File)
		}
		content, ok := baseFiles[removal.File]
		if !ok || !strings.Contains(string(content), removal.Evidence) {
			return fmt.Errorf("allowed_removals[%d] evidence 在 BASE 文件 %s 中找不到：%q（白名单必须有 BASE 证据）",
				i, removal.File, removal.Evidence)
		}
		switch removal.Kind {
		case "require":
			if removal.OldFingerprint == "" {
				return fmt.Errorf("allowed_removals[%d] kind=require 必须给 old_fingerprint", i)
			}
			if baseSurface.Fingerprints[removal.OldFingerprint] == 0 {
				return fmt.Errorf("allowed_removals[%d] old_fingerprint=%s 不在 BASE Test %s 指纹里",
					i, removal.OldFingerprint, removal.Test)
			}
			if removalsByTest[removal.Test] == nil {
				removalsByTest[removal.Test] = map[string]int{}
			}
			removalsByTest[removal.Test][removal.OldFingerprint]++
		case "fatalf":
			fatalfByTest[removal.Test]++
		default:
			return fmt.Errorf("allowed_removals[%d] kind=%q 非法（require|fatalf）", i, removal.Kind)
		}
	}
	// 白名单不得超额（删除的条数不能超过 BASE 里实际存在的条数）。
	for test, fps := range removalsByTest {
		for fp, count := range fps {
			if base.Surfaces[test].Fingerprints[fp] < count {
				return fmt.Errorf("allowed_removals Test %s 指纹 %s 删除 %d 条，超过 BASE 实际 %d 条",
					test, fp, count, base.Surfaces[test].Fingerprints[fp])
			}
		}
	}
	for test, count := range fatalfByTest {
		if base.Surfaces[test].Fatalf < count {
			return fmt.Errorf("allowed_removals Test %s 删除 t.Fatalf %d 条，超过 BASE 实际 %d 条",
				test, count, base.Surfaces[test].Fatalf)
		}
	}
	return nil
}

// checkAssertionCoverage 当前代码必须覆盖「BASE 断言面 - allowed_removals」：
// Test 名集合是 BASE 超集；per-Test require 计数/t.Fatalf 计数/指纹 multiset 均不得
// 低于覆盖下界（删除白名单外断言 => 红）。
func checkAssertionCoverage(baseline *assertionBaseline, base, current assertionCollector) error {
	removalsByTest := map[string]map[string]int{}
	fatalfByTest := map[string]int{}
	for _, removal := range baseline.AllowedRemovals {
		switch removal.Kind {
		case "require":
			if removalsByTest[removal.Test] == nil {
				removalsByTest[removal.Test] = map[string]int{}
			}
			removalsByTest[removal.Test][removal.OldFingerprint]++
		case "fatalf":
			fatalfByTest[removal.Test]++
		}
	}
	for name, want := range base.Surfaces {
		got, ok := current.Surfaces[name]
		if !ok {
			return fmt.Errorf("当前代码缺少 BASE Test %s（断言基线要求 Test 名集合是 BASE 超集）", name)
		}
		removedRequires := 0
		for _, count := range removalsByTest[name] {
			removedRequires += count
		}
		minRequires := want.Requires - removedRequires
		if got.Requires < minRequires {
			return fmt.Errorf("Test %s require 断言被删除：当前 %d < 基线-%s 下界 %d",
				name, got.Requires, allowedRemovalLabel(removalsByTest[name]), minRequires)
		}
		minFatalf := want.Fatalf - fatalfByTest[name]
		if got.Fatalf < minFatalf {
			return fmt.Errorf("Test %s t.Fatalf 被删除：当前 %d < 下界 %d", name, got.Fatalf, minFatalf)
		}
		for fp, baseCount := range want.Fingerprints {
			needed := baseCount - removalsByTest[name][fp]
			if got.Fingerprints[fp] < needed {
				return fmt.Errorf("Test %s 断言被改写/删除（指纹 %s 当前 %d < 下界 %d）：%s",
					name, fp, got.Fingerprints[fp], needed, base.Exprs[fp])
			}
		}
	}
	return nil
}

// allowedRemovalLabel 白名单条数描述（错误信息用）。
func allowedRemovalLabel(fps map[string]int) string {
	if len(fps) == 0 {
		return "0"
	}
	total := 0
	for _, count := range fps {
		total += count
	}
	return fmt.Sprintf("%d", total)
}

// scanTautologicalAssertions 反空断言（「明显恒真兜底」，不宣称机械完备）：
// require.True/False 字面 bool、require.Nil/NotNil(nil)、require.Equal 两侧同一个
// 字面/表达式等明显恒真项。返回违规描述（file:line: reason）。
func scanTautologicalAssertions(t *testing.T, files map[string][]byte) []string {
	t.Helper()
	var violations []string
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, files[name], parser.ParseComments)
		if err != nil {
			continue // 语法错误由别的 gate/编译兜底
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qual, ok := sel.X.(*ast.Ident)
			if !ok || qual.Name != "require" {
				return true
			}
			line := fset.Position(call.Pos()).Line
			method := sel.Sel.Name
			switch method {
			case "True", "False":
				if isBoolLiteral(call.Args[1]) {
					violations = append(violations, fmt.Sprintf("%s:%d: require.%s 字面 bool 恒真断言", name, line, method))
				}
			case "Nil", "NotNil":
				if ident, ok := call.Args[1].(*ast.Ident); ok && ident.Name == "nil" {
					violations = append(violations, fmt.Sprintf("%s:%d: require.%s(nil) 明显恒真断言", name, line, method))
				}
			case "Equal", "EqualValues", "Same":
				if len(call.Args) >= 3 && types.ExprString(call.Args[1]) == types.ExprString(call.Args[2]) {
					violations = append(violations, fmt.Sprintf("%s:%d: require.%s 两侧同一表达式（恒真）：%s",
						name, line, method, types.ExprString(call.Args[1])))
				}
			}
			return true
		})
	}
	return violations
}

// isBoolLiteral 判定表达式是否为字面 true/false。
func isBoolLiteral(expr ast.Expr) bool {
	lit, ok := expr.(*ast.Ident)
	return ok && (lit.Name == "true" || lit.Name == "false")
}

// ---------- 文件树工具（变异测试 AV-D 用） ----------

// copyTree 递归复制目录（变异测试在副本内执行，绝不改真实工作区）。
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dst, 0o755))
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			copyTree(t, srcPath, dstPath)
			continue
		}
		data, err := os.ReadFile(srcPath)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dstPath, data, 0o644))
	}
}

// hashTree 目录树内容 sha256（验证变异测试/失败路径不改真实文件）。
func hashTree(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue // tools/e2e 下只有 testdata 子目录（baseline 单独计入下方）
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err)
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	// testdata 里的 baseline（gate 输入）同样纳入哈希。
	testdata := filepath.Join(root, "testdata")
	if entries, err := os.ReadDir(testdata); err == nil {
		for _, entry := range entries {
			data, rerr := os.ReadFile(filepath.Join(testdata, entry.Name()))
			if rerr != nil {
				continue
			}
			h.Write([]byte(entry.Name()))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------- AV-D：副本内三种变异必须 gate 红；恢复后绿且真实文件 sha256 不变 ----------

// TestHarnessGateMutationRed AV-D（spec §9.6/§10.7）：
//  1. 删真实断言 + 补 require.True(t,true) => 断言覆盖/反空断言红；
//  2. 同 commit 改 baseline => baseline 与 BASE 重算比对红；
//  3. 新增 harness_evil_test.go 固定端口 => boundary gate 红；
//     恢复副本（未变异）后两种 gate 都必须绿，且真实工作区 sha256 未变。
func TestHarnessGateMutationRed(t *testing.T) {
	repoRoot := gateRepoRoot(t)
	realE2EDir := filepath.Join(repoRoot, "tools", "e2e")
	beforeHash := hashTree(t, realE2EDir)

	baseline := loadAssertionBaseline(t, repoRoot)
	baseFiles := gateBaseSources(t, repoRoot, baseline.SourceCommit)
	base := collectAssertionFiles(t, baseFiles)

	// 干净副本：两种 gate 必须绿（先证明 gate 不是恒红）。
	cleanDir := filepath.Join(t.TempDir(), "clean")
	copyTree(t, realE2EDir, cleanDir)
	cleanFiles := readLocalTestFiles(t, cleanDir)
	clean := collectAssertionFiles(t, cleanFiles)
	require.NoError(t, checkBaselineMatchesBase(t, baseline, base), "干净副本 baseline 必须与 BASE 一致")
	require.NoError(t, checkAssertionCoverage(baseline, base, clean), "干净副本必须覆盖 BASE 断言面")
	require.Empty(t, scanTautologicalAssertions(t, cleanFiles), "干净副本不得有恒真断言")
	require.Empty(t, scanBoundaryViolations(cleanFiles), "干净副本 boundary gate 必须绿")

	// 变异①：删一条真实断言 + 补 require.True(t, true)。
	mutatedDir := filepath.Join(t.TempDir(), "mutated-assert")
	copyTree(t, realE2EDir, mutatedDir)
	target := filepath.Join(mutatedDir, "request_patch_e2e_test.go")
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	const realAssert = "\trequire.Equal(t, 200, code, \"A2 chat 必须 200: %s\", body)\n"
	require.Contains(t, string(content), realAssert, "变异目标断言必须存在（测试自身自检）")
	mutated := strings.Replace(string(content), realAssert, "\trequire.True(t, true)\n", 1)
	require.NoError(t, os.WriteFile(target, []byte(mutated), 0o644))
	mutatedFiles := readLocalTestFiles(t, mutatedDir)
	mutatedSurface := collectAssertionFiles(t, mutatedFiles)
	require.Error(t, checkAssertionCoverage(baseline, base, mutatedSurface), "删真实断言必须红")
	require.NotEmpty(t, scanTautologicalAssertions(t, mutatedFiles), "require.True(t, true) 必须红（反空断言）")

	// 变异②：同 commit 改 baseline（计数 +1）=> 与 BASE 重算不一致。
	tampered := *baseline
	tampered.Tests = map[string]assertionTestSurface{}
	for name, surface := range baseline.Tests {
		tampered.Tests[name] = surface
	}
	tamperedName := "TestBillingE2E"
	surface := tampered.Tests[tamperedName]
	surface.Requires++
	tampered.Tests[tamperedName] = surface
	require.Error(t, checkBaselineMatchesBase(t, &tampered, base), "同 commit 改 baseline 必须红")

	// 变异③：新增 harness_evil_test.go 固定端口 => boundary gate 红。
	evilDir := filepath.Join(t.TempDir(), "mutated-boundary")
	copyTree(t, realE2EDir, evilDir)
	evil := "package e2e\n\nimport \"testing\"\n\nconst evilFixedAddr = \"127.0.0.1:18094\"\n\nfunc TestHarnessEvil(t *testing.T) {\n\t_ = evilFixedAddr\n}\n"

	require.NoError(t, os.WriteFile(filepath.Join(evilDir, "harness_evil_test.go"), []byte(evil), 0o644))
	evilFiles := readLocalTestFiles(t, evilDir)
	require.NotEmpty(t, scanBoundaryViolations(evilFiles), "新增固定端口文件必须触发 boundary gate")

	// 恢复：真实工作区未被变异测试改动（sha256 一致）。
	afterHash := hashTree(t, realE2EDir)
	require.Equal(t, beforeHash, afterHash, "变异测试只在副本内执行，真实工作区 sha256 必须不变")
}
