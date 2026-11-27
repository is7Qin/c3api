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

// assertionCollector 采集结果（附 expr 便于 gate 失败信息定位；CallTexts/FatalfTexts
// 保存 BASE 源码里的断言调用原文，白名单 evidence 必须能在**被白名单断言的相关调用**
// 里找到——不能拿 BASE 任意 token 充数，oracle P0 review I1）。
type assertionCollector struct {
	Surfaces    map[string]assertionTestSurface
	Exprs       map[string]string              // fingerprint -> 归一化表达式
	CallTexts   map[string]map[string][]string // Test -> fingerprint -> require 调用源码原文
	FatalfTexts map[string][]string            // Test -> t.Fatalf 调用源码原文
}

// callSourceText 取调用表达式的源码原文（evidence 相关性校验用；Offset 相对
// parser.ParseFile 传入的 content）。
func callSourceText(content []byte, fset *token.FileSet, call *ast.CallExpr) string {
	start := fset.Position(call.Pos()).Offset
	end := fset.Position(call.End()).Offset
	if start < 0 || end > len(content) || start >= end {
		return ""
	}
	return string(content[start:end])
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
	out := assertionCollector{
		Surfaces:    map[string]assertionTestSurface{},
		Exprs:       map[string]string{},
		CallTexts:   map[string]map[string][]string{},
		FatalfTexts: map[string][]string{},
	}
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
					if out.CallTexts[fn.Name.Name] == nil {
						out.CallTexts[fn.Name.Name] = map[string][]string{}
					}
					out.CallTexts[fn.Name.Name][fp] = append(out.CallTexts[fn.Name.Name][fp],
						callSourceText(content, fset, call))
				case qual.Name == "t" && sel.Sel.Name == "Fatalf":
					surface.Fatalf++
					out.FatalfTexts[fn.Name.Name] = append(out.FatalfTexts[fn.Name.Name],
						callSourceText(content, fset, call))
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

// assertionRemovalScope 白名单作用域：只允许**场景**测试文件（*_test.go 且不在
// boundary 豁免文件集内）。harness/自测/gate 文件不得进白名单——白名单作用于 gate
// 文件等于让 gate 自我豁免（spec §4.3/§14：作用域收回到场景文件 + evidence 强校验）。
func assertionRemovalScope(file string) bool {
	return strings.HasSuffix(file, "_test.go") && !boundaryExemptFiles[file]
}

// checkAllowedRemovals 白名单校验：作用域=场景 *_test.go；evidence 必须能在 BASE
// 文件原文找到且与被移除断言相关；old_fingerprint 必须确实存在于 BASE 该 Test 的指纹里。
// baseline-v2 后白名单为空，本函数保留为机制与变异测试（AV-D）的事实源。
func checkAllowedRemovals(t *testing.T, baseline *assertionBaseline, baseFiles map[string][]byte, base assertionCollector) error {
	removalsByTest := map[string]map[string]int{} // test -> fingerprint -> count
	fatalfByTest := map[string]int{}
	for i, removal := range baseline.AllowedRemovals {
		if !assertionRemovalScope(removal.File) {
			return fmt.Errorf("allowed_removals[%d] 文件 %q 越界（白名单只允许场景 *_test.go，不允许 harness/自测/gate 文件）", i, removal.File)
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
			// evidence 必须与被白名单的断言调用相关（出现在该指纹的 BASE 调用源码里），
			// 不能拿 BASE 里任意 token 充数（oracle P0 review I1）。
			related := false
			for _, text := range base.CallTexts[removal.Test][removal.OldFingerprint] {
				if strings.Contains(text, removal.Evidence) {
					related = true
					break
				}
			}
			if !related {
				return fmt.Errorf("allowed_removals[%d] evidence %q 不在 BASE Test %s 指纹 %s 的断言调用源码中（证据必须与被移除断言相关）",
					i, removal.Evidence, removal.Test, removal.OldFingerprint)
			}
			if removalsByTest[removal.Test] == nil {
				removalsByTest[removal.Test] = map[string]int{}
			}
			removalsByTest[removal.Test][removal.OldFingerprint]++
		case "fatalf":
			fatalfByTest[removal.Test]++
			related := false
			for _, text := range base.FatalfTexts[removal.Test] {
				if strings.Contains(text, removal.Evidence) {
					related = true
					break
				}
			}
			if !related {
				return fmt.Errorf("allowed_removals[%d] evidence %q 不在 BASE Test %s 的 t.Fatalf 调用源码中（证据必须与被移除断言相关）",
					i, removal.Evidence, removal.Test)
			}
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

// checkAllowedRemovalsSource allowed_removals 唯一事实源 = gate 源码里的
// assertionAllowedRemovals：baseline JSON 必须与之逐项完全一致。否则「同 commit 往
// baseline JSON 追加一条白名单 + 删掉对应断言」即可把 gate 变绿（oracle P0 review I1，
// spec §13）——checkBaselineMatchesBase 只比对 Tests 面，覆盖不到白名单面。
func checkAllowedRemovalsSource(baseline *assertionBaseline, source []assertionRemoval) error {
	if len(baseline.AllowedRemovals) != len(source) {
		return fmt.Errorf("baseline.allowed_removals 有 %d 条，gate 源码 assertionAllowedRemovals 有 %d 条（白名单唯一事实源在 gate 源码）",
			len(baseline.AllowedRemovals), len(source))
	}
	for i := range source {
		if baseline.AllowedRemovals[i] != source[i] {
			return fmt.Errorf("baseline.allowed_removals[%d] 与 gate 源码 assertionAllowedRemovals[%d] 不一致（JSON=%+v 源码=%+v）",
				i, i, baseline.AllowedRemovals[i], source[i])
		}
	}
	return nil
}

// checkRemovalsActuallyRemoved 每条白名单都必须对应「BASE 存在、当前已真正移除或改写」
// 的断言：白名单若指向一条仍在当前代码中执行的断言，就是空转豁免（oracle I1 的
// 白名单滥用面之一）。按 (Test, fingerprint) 只要求当前计数严格小于 BASE——迁移
// 场景可能用新断言补回部分指纹，不把计数差当硬性下界。
func checkRemovalsActuallyRemoved(baseline *assertionBaseline, base, current assertionCollector) error {
	for i, removal := range baseline.AllowedRemovals {
		want, ok := base.Surfaces[removal.Test]
		if !ok {
			return fmt.Errorf("allowed_removals[%d] Test %q 不存在于 BASE", i, removal.Test)
		}
		got, ok := current.Surfaces[removal.Test]
		if !ok {
			continue // Test 被删由 checkAssertionCoverage 报错，避免重复/误导
		}
		switch removal.Kind {
		case "require":
			if want.Fingerprints[removal.OldFingerprint] <= got.Fingerprints[removal.OldFingerprint] {
				return fmt.Errorf("allowed_removals[%d] 声称移除 Test %s 的断言 %s，但当前代码仍有 %d 条（未真正移除/改写）",
					i, removal.Test, removal.OldFingerprint, got.Fingerprints[removal.OldFingerprint])
			}
		case "fatalf":
			if want.Fatalf <= got.Fatalf {
				return fmt.Errorf("allowed_removals[%d] 声称移除 Test %s 的 t.Fatalf，但当前代码仍有 %d 条（未真正移除）",
					i, removal.Test, got.Fatalf)
			}
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

// ---------- 不可达断言（unreachable assertion）检测 ----------

// scanUnreachableAssertions 检测「不可达补偿」绕过（spec §13 / P0 review momus
// Minor-2）：require.* / t.Fatalf 出现在 if false / for false 的 body 内，或在同一
// block 内无条件 return/panic 之后——表面计数与指纹保住了，但断言永不执行。可见性
// 口径与 collectAssertionFiles 一致（只扫描 Test* 函数体）。返回 file:line:reason。
func scanUnreachableAssertions(t *testing.T, files map[string][]byte) []string {
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
			continue // 语法错误由编译/boundary gate 兜底
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			found := map[token.Pos]string{}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.IfStmt:
					if isConstBoolLiteral(n.Cond, false) {
						collectUnreachableAssertions(n.Body,
							"require/t.Fatalf 位于 if false 分支（不可达补偿，永不执行）", found)
					}
				case *ast.ForStmt:
					if n.Cond != nil && isConstBoolLiteral(n.Cond, false) {
						collectUnreachableAssertions(n.Body,
							"require/t.Fatalf 位于 for false 循环体（不可达补偿，永不执行）", found)
						if n.Post != nil {
							collectUnreachableAssertions(n.Post,
								"require/t.Fatalf 位于 for false 的 post 语句（不可达补偿，永不执行）", found)
						}
					}
				case *ast.BlockStmt:
					collectAfterTerminator(n.List, found)
				case *ast.CaseClause:
					collectAfterTerminator(n.Body, found)
				case *ast.CommClause:
					collectAfterTerminator(n.Body, found)
				}
				return true
			})
			lines := make([]int, 0, len(found))
			reasonByLine := make(map[int]string, len(found))
			for pos, reason := range found {
				line := fset.Position(pos).Line
				if _, exists := reasonByLine[line]; !exists {
					lines = append(lines, line)
				}
				reasonByLine[line] = reason
			}
			sort.Ints(lines)
			for _, line := range lines {
				violations = append(violations, fmt.Sprintf("%s:%d: %s", name, line, reasonByLine[line]))
			}
		}
	}
	return violations
}

// collectAfterTerminator 同一 block（BlockStmt / case 体）内无条件 return/panic 之后的
// 语句全部不可达：把其中断言一次收齐（与父区域已收集的按位置去重）。
func collectAfterTerminator(stmts []ast.Stmt, found map[token.Pos]string) {
	for i, stmt := range stmts {
		term, isTerm := assertionTerminator(stmt)
		if !isTerm {
			continue
		}
		for _, rest := range stmts[i+1:] {
			collectUnreachableAssertions(rest,
				"require/t.Fatalf 位于无条件 "+term+" 之后的不可达代码", found)
		}
		return
	}
}

// assertionTerminator 无条件终止控制流的语句：spec §13 的不可达判据只含 return/panic
// （分支/跳转按其自身可达性分析，避免把 label 之后的合法代码误判为不可达）。
func assertionTerminator(stmt ast.Stmt) (string, bool) {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		return "return", true
	case *ast.ExprStmt:
		if call, ok := s.X.(*ast.CallExpr); ok {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "panic" {
				return "panic", true
			}
		}
	}
	return "", false
}

// collectUnreachableAssertions 把 node 子树中的 require.* / t.Fatalf 断言计入 found
// （位置去重；reason 说明不可达上下文）。
func collectUnreachableAssertions(node ast.Node, reason string, found map[token.Pos]string) {
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isAssertionCall(call) {
			return true
		}
		if _, exists := found[call.Pos()]; !exists {
			found[call.Pos()] = reason
		}
		return true
	})
}

// isAssertionCall require.* / t.Fatalf 调用（与 collectAssertionFiles 的断言面口径一致）。
func isAssertionCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	qual, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return qual.Name == "require" || (qual.Name == "t" && sel.Sel.Name == "Fatalf")
}

// isConstBoolLiteral 字面 true/false（去括号）。
func isConstBoolLiteral(expr ast.Expr, want bool) bool {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = paren.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok || (ident.Name != "true" && ident.Name != "false") {
		return false
	}
	return (ident.Name == "true") == want
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

// TestHarnessGateMutationRed AV-D（spec §9.6/§10.7 + §13）：
//  1. 删真实断言 + 补 require.True(t,true) => 断言覆盖/反空断言红；
//     1b. 删真实断言 + 用 if false { require... } 保计数 => 覆盖门可绕过但不可达检测红；
//  2. 同 commit 改 baseline => baseline 与 BASE 重算比对红；
//  3. 新增固定端口 / 计算型拼接端口 / net.Listen 文件 => boundary gate 红（httptest 对照绿）；
//  4. 同 commit 只往 baseline JSON 追加 allowed_removals + 删对应断言 => 覆盖门可绕过但
//     白名单源码一致性红（oracle P0 review I1）；
//  5. 改代码侧白名单 evidence 为无关 token => 证据相关性红。
//     恢复副本（未变异）后全部 gate 都必须绿，且真实工作区 sha256 未变。
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
	require.NoError(t, checkAllowedRemovals(t, baseline, baseFiles, base), "干净副本白名单必须合法（证据与被移除断言相关）")
	require.NoError(t, checkAllowedRemovalsSource(baseline, assertionAllowedRemovals),
		"干净副本 baseline JSON 白名单必须与 gate 源码完全一致")
	require.NoError(t, checkRemovalsActuallyRemoved(baseline, base, clean),
		"干净副本白名单必须对应 BASE 存在且当前已真正移除的断言")
	require.NoError(t, checkAssertionCoverage(baseline, base, clean), "干净副本必须覆盖 BASE 断言面")
	require.Empty(t, scanTautologicalAssertions(t, cleanFiles), "干净副本不得有恒真断言")
	require.Empty(t, scanUnreachableAssertions(t, cleanFiles), "干净副本不得有不可达断言")
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

	// 变异①b：删真实断言 + 用「if false { 同一断言 }」保住计数与指纹（不可达补偿）：
	// 表面覆盖门确实被绕过（证明需要不可达检测），但不可达检测必须红。
	unreachableDir := filepath.Join(t.TempDir(), "mutated-unreachable")
	copyTree(t, realE2EDir, unreachableDir)
	unreachableTarget := filepath.Join(unreachableDir, "request_patch_e2e_test.go")
	unreachableContent, err := os.ReadFile(unreachableTarget)
	require.NoError(t, err)
	require.Contains(t, string(unreachableContent), realAssert, "不可达补偿变异目标断言必须存在（自检）")
	unreachableMutated := strings.Replace(string(unreachableContent), realAssert,
		"\tif false {\n\t\trequire.Equal(t, 200, code, \"A2 chat 必须 200: %s\", body)\n\t}\n", 1)
	require.NoError(t, os.WriteFile(unreachableTarget, []byte(unreachableMutated), 0o644))
	unreachableFiles := readLocalTestFiles(t, unreachableDir)
	unreachableSurface := collectAssertionFiles(t, unreachableFiles)
	require.NoError(t, checkAssertionCoverage(baseline, base, unreachableSurface),
		"不可达补偿在表面计数上确实能绕过覆盖门（证明不可达检测的必要性）")
	require.NotEmpty(t, scanUnreachableAssertions(t, unreachableFiles),
		"if false 内的补偿断言必须被不可达检测红")

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

	// 变异④（oracle P0 review I1）：同一 commit 只往 baseline JSON 的 allowed_removals
	// 追加一条 + 删掉对应真实断言。覆盖门会被这条「JSON 白名单」绕过（证明必须校验
	// 白名单源码一致性），gate 必须因 baseline 与代码侧 assertionAllowedRemovals
	// 不一致而红。
	const supplierAssert = "\trequire.Equal(t, 200, c, \"approve: %s\", rb)\n"
	const supplierAssertFP = "1d25b3c35663ca52"
	require.NotZero(t, base.Surfaces["TestSupplierEarningsE2E"].Fingerprints[supplierAssertFP],
		"白名单绕过变异目标指纹必须存在于 BASE（自检）")
	whitelistDir := filepath.Join(t.TempDir(), "mutated-whitelist")
	copyTree(t, realE2EDir, whitelistDir)
	whitelistTarget := filepath.Join(whitelistDir, "supplier_earnings_e2e_test.go")
	whitelistContent, err := os.ReadFile(whitelistTarget)
	require.NoError(t, err)
	require.Contains(t, string(whitelistContent), supplierAssert, "白名单绕过变异目标断言必须存在（自检）")
	require.NoError(t, os.WriteFile(whitelistTarget,
		[]byte(strings.Replace(string(whitelistContent), supplierAssert, "", 1)), 0o644))
	whitelistFiles := readLocalTestFiles(t, whitelistDir)
	whitelistSurface := collectAssertionFiles(t, whitelistFiles)
	extraRemoval := assertionRemoval{
		File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E",
		Kind: "require", OldFingerprint: supplierAssertFP, Evidence: "approve: %s",
	}
	jsonOnlyBaseline := assertionBaseline{
		SourceCommit:    baseline.SourceCommit,
		Tests:           baseline.Tests,
		AllowedRemovals: append(append([]assertionRemoval{}, baseline.AllowedRemovals...), extraRemoval),
	}
	require.NoError(t, checkAssertionCoverage(&jsonOnlyBaseline, base, whitelistSurface),
		"只看 baseline JSON 白名单时覆盖门确实可被绕过（证明源码一致性检查的必要性）")
	require.Error(t, checkAllowedRemovalsSource(&jsonOnlyBaseline, assertionAllowedRemovals),
		"同 commit 只改 baseline JSON 加白名单 + 删断言必须红（白名单唯一事实源在 gate 源码）")

	// 变异⑤：白名单 evidence 取 BASE 任意无关 token（模拟连代码侧白名单一起改）=> 红。
	// baseline-v2 清空白名单后代码侧为空：合成一条 BASE 存在的条目继续验证 evidence
	// 相关性检查仍然生效（否则「证据相关性」在 v2 后再无变异覆盖）。
	evilRemovals := append([]assertionRemoval{}, assertionAllowedRemovals...)
	if len(evilRemovals) == 0 {
		evilRemovals = append(evilRemovals, assertionRemoval{
			File: "billing_e2e_test.go", Test: "TestBillingE2E", Kind: "require",
			OldFingerprint: "a9b4469c3ecea2c7", Evidence: "C3API_REDIS_ADDR is required",
		})
	}
	require.NotEmpty(t, evilRemovals, "白名单自检：代码侧必须有条目")
	evilRemovals[0].Evidence = "unrelated-token-not-in-base"
	evilBaseline := assertionBaseline{
		SourceCommit:    baseline.SourceCommit,
		Tests:           baseline.Tests,
		AllowedRemovals: evilRemovals,
	}
	require.NoError(t, checkAllowedRemovalsSource(&evilBaseline, evilRemovals),
		"（模拟攻击者同时改代码侧白名单）JSON 与代码侧一致")
	require.Error(t, checkAllowedRemovals(t, &evilBaseline, baseFiles, base),
		"代码侧白名单 evidence 与被移除断言无关必须红（不能拿 BASE 任意 token 充数）")

	// 变异⑤b：白名单指向一条仍在当前代码执行的断言（空转豁免）=> 红。
	stillRemoval := assertionRemoval{File: "supplier_earnings_e2e_test.go", Test: "TestSupplierEarningsE2E", Kind: "require"}
	baseSupplier := base.Surfaces["TestSupplierEarningsE2E"]
	for fp, count := range baseSupplier.Fingerprints {
		if count > 0 && clean.Surfaces["TestSupplierEarningsE2E"].Fingerprints[fp] == count {
			texts := base.CallTexts["TestSupplierEarningsE2E"][fp]
			if len(texts) == 0 {
				continue
			}
			stillRemoval.OldFingerprint = fp
			stillRemoval.Evidence = texts[0]
			break
		}
	}
	require.NotEmpty(t, stillRemoval.OldFingerprint, "白名单空转变异需要一条当前仍存在的 BASE 断言（自检）")
	stillBaseline := assertionBaseline{
		SourceCommit:    baseline.SourceCommit,
		Tests:           baseline.Tests,
		AllowedRemovals: append(append([]assertionRemoval{}, baseline.AllowedRemovals...), stillRemoval),
	}
	require.NoError(t, checkAllowedRemovals(t, &stillBaseline, baseFiles, base),
		"白名单空转条目的 evidence 恰好取自该断言 BASE 调用（构造自检）")
	require.Error(t, checkRemovalsActuallyRemoved(&stillBaseline, base, clean),
		"白名单声称移除一条仍在执行的断言必须红")

	// 变异③：新增 harness_evil_test.go 固定端口 => boundary gate 红。
	evilDir := filepath.Join(t.TempDir(), "mutated-boundary")
	copyTree(t, realE2EDir, evilDir)
	evil := "package e2e\n\nimport \"testing\"\n\nconst evilFixedAddr = \"127.0.0.1:18094\"\n\nfunc TestHarnessEvil(t *testing.T) {\n\t_ = evilFixedAddr\n}\n"

	require.NoError(t, os.WriteFile(filepath.Join(evilDir, "harness_evil_test.go"), []byte(evil), 0o644))
	evilFiles := readLocalTestFiles(t, evilDir)
	require.NotEmpty(t, scanBoundaryViolations(evilFiles), "新增固定端口文件必须触发 boundary gate")

	// 变异③b：计算型固定端口（拼接常量）+ 自建监听 => boundary gate 红（spec §13）。
	computedEvil := "package e2e\n\nimport (\n\t\"net\"\n\t\"testing\"\n)\n\n" +
		"const evilHostPart = \"127.0.0.1:\"\nconst evilComputedAddr = evilHostPart + \"18094\"\n\n" +
		"func TestHarnessEvilComputed(t *testing.T) {\n\t_ = evilComputedAddr\n\t_, _ = net.Listen(\"tcp\", \"127.0.0.1:0\")\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(evilDir, "harness_evil_computed_test.go"), []byte(computedEvil), 0o644))
	evilFiles = readLocalTestFiles(t, evilDir)
	joined := formatBoundaryViolations(scanBoundaryViolations(evilFiles))
	require.Contains(t, joined, "计算型固定端口", "拼接常量固定端口必须触发 boundary gate")
	require.Contains(t, joined, "禁止场景自建监听", "场景自建监听必须触发 boundary gate")

	// 对照：httptest 进程内上游（合法）不得触发 boundary gate。
	benignDir := filepath.Join(t.TempDir(), "benign-httptest")
	copyTree(t, realE2EDir, benignDir)
	benign := "package e2e\n\nimport (\n\t\"net/http\"\n\t\"net/http/httptest\"\n\t\"testing\"\n)\n\n" +
		"func TestHarnessBenign(t *testing.T) {\n\tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))\n\tdefer srv.Close()\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(benignDir, "harness_benign_test.go"), []byte(benign), 0o644))
	require.Empty(t, scanBoundaryViolations(readLocalTestFiles(t, benignDir)), "httptest 对照必须绿")

	// 恢复：真实工作区未被变异测试改动（sha256 一致）。
	afterHash := hashTree(t, realE2EDir)
	require.Equal(t, beforeHash, afterHash, "变异测试只在副本内执行，真实工作区 sha256 必须不变")
}
