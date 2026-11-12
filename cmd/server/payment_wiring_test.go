// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

// payment_wiring_test.go P53 的**装配面五层门禁**（spec v2 §13 P53 / §15-17 / N59+N68）。
//
// 规范表述：**具体协议构造只能出现在 `internal/paymentregistry` 包**。五层各自独立
// 断言，任一层单独失效都会被其它层补齐——但每层都必须能**自己**说明问题：
//
//	① cmd/server 的 internal import 集合**取等号**（逐条列出；新增依赖必须改表）；
//	② 仓级：非测试文件 import `internal/service/epay` 的文件全集 == {internal/paymentregistry/*}；
//	③ `go list -deps`：service 不得依赖 epay（否则"换协议"要动 service），
//	   cmd/server 必须依赖 paymentregistry（否则门禁在空转）；
//	④ main.go 的 AST 形状：无 []domain.PaymentProvider 字面量、Deps 里
//	   PaymentRegistry 恰 1 次 / PaymentProviders 0 次、paymentregistry.Registry
//	   恰 1 次、**且不得出现 domain.NewProviderRegistry**（空注册表合法 ⇒ 必须单独禁）；
//	⑤ 协议名**自派生**：真调 `paymentregistry.Registry(nil).All()` 收 Name()，
//	   断言这些字符串不出现在 main.go 的字符串字面量里（只看 `ast.BasicLit`，
//	   不查注释——main 里的中文注释提到协议名是合法的）。
//
// 为什么 ④ 要单独禁 `domain.NewProviderRegistry`：它接受 nil，`NewProviderRegistry(nil)`
// 是**合法**的空注册表，于是 main 可以绕开"协议清单的唯一事实来源"直接装一个空表
// 并通过其余所有断言——启动后一个协议都没有，"支付能卖"这个前提静默消失。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/paymentregistry"
)

const (
	wiringModulePath      = "github.com/is7qin/c3api"
	wiringEpayPkg         = wiringModulePath + "/internal/service/epay"
	wiringRegistryPkg     = wiringModulePath + "/internal/paymentregistry"
	wiringDomainPkg       = wiringModulePath + "/internal/domain"
	wiringServicePkg      = wiringModulePath + "/internal/service"
	wiringRegistryRelGlob = "internal/paymentregistry/"
)

// cmdServerInternalImports 层 ① 的等号右侧：`cmd/server` 的**非测试** `.go` 文件
// import 的 internal 包全集（本次快照）。
//
// 这张表就是判据：新增任何 internal 依赖都必须显式改这里，从而在 review 里被看见。
// 尤其**不得**出现 `internal/service/epay` 或任何其它协议实现包。
var cmdServerInternalImports = []string{
	wiringModulePath + "/internal/auth",
	wiringModulePath + "/internal/billing",
	wiringModulePath + "/internal/config",
	wiringModulePath + "/internal/continuation",
	wiringModulePath + "/internal/credential",
	wiringModulePath + "/internal/discovery",
	wiringModulePath + "/internal/domain",
	wiringModulePath + "/internal/handler",
	wiringModulePath + "/internal/handler/payment",
	wiringModulePath + "/internal/handler/paymentadmin",
	wiringModulePath + "/internal/handler/user",
	wiringModulePath + "/internal/invalidate",
	wiringModulePath + "/internal/latch",
	wiringModulePath + "/internal/notification",
	wiringModulePath + "/internal/notify",
	wiringModulePath + "/internal/paymentregistry",
	wiringModulePath + "/internal/pricing",
	wiringModulePath + "/internal/probe",
	wiringModulePath + "/internal/protoconv",
	wiringModulePath + "/internal/proxy",
	wiringModulePath + "/internal/quality",
	wiringModulePath + "/internal/repository",
	wiringModulePath + "/internal/rule",
	wiringModulePath + "/internal/scheduler",
	wiringModulePath + "/internal/sdkbridge",
	wiringModulePath + "/internal/server",
	wiringModulePath + "/internal/service",
	wiringModulePath + "/internal/settingssnap",
	wiringModulePath + "/internal/snapshot",
	wiringModulePath + "/internal/supplier",
	wiringModulePath + "/internal/usage",
	wiringModulePath + "/internal/verification",
	wiringModulePath + "/internal/worker",
}

// wiringRepoRoot 定位模块根（含 go.mod）。找不到即 FailNow：门禁不能在"路径猜错"
// 时静默通过。
func wiringRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "找不到模块根（go.mod）")
		dir = parent
	}
}

// wiringParse 解析一个 Go 源文件（不带注释、不做对象解析：门禁只读 import 与声
// 明形状，省掉这部分开销）。
func wiringParse(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, "解析 %s", path)
	return fset, file
}

// wiringImportPath 把 import spec 的字面量还原成路径。
func wiringImportPath(t *testing.T, spec *ast.ImportSpec) string {
	t.Helper()
	path, err := strconv.Unquote(spec.Path.Value)
	require.NoError(t, err, "import 字面量 %s", spec.Path.Value)
	return path
}

// wiringGoFileNames 列出目录下**非测试**的 `.go` 文件名（排序即稳定）。
func wiringGoFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "读目录 %s", dir)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestPaymentWiringGate1CmdServerInternalImports 层 ①：import 集合取等号。
//
// 取等号（而不是"不含 epay"）的收益：协议实现可以经**中间包**二次导出（`x.New(hc)`
// 在 cmd/server 里看起来毫无协议痕迹），而只要它是 internal 包就会被这张表抓到。
func TestPaymentWiringGate1CmdServerInternalImports(t *testing.T) {
	dir := filepath.Join(wiringRepoRoot(t), "cmd", "server")
	names := wiringGoFileNames(t, dir)
	require.Greater(t, len(names), 5, "cmd/server 的非测试源文件数异常（扫面是空转？）")

	got := make(map[string]bool, len(cmdServerInternalImports))
	for _, name := range names {
		_, file := wiringParse(t, filepath.Join(dir, name))
		for _, spec := range file.Imports {
			path := wiringImportPath(t, spec)
			if strings.HasPrefix(path, wiringModulePath+"/internal/") {
				got[path] = true
			}
		}
	}
	gotList := make([]string, 0, len(got))
	for path := range got {
		gotList = append(gotList, path)
	}
	sort.Strings(gotList)
	want := append([]string(nil), cmdServerInternalImports...)
	sort.Strings(want)

	require.Equal(t, want, gotList,
		"cmd/server 的 internal import 集合必须与 cmdServerInternalImports 逐条相等（新增 internal 依赖请改表）")
	require.False(t, got[wiringEpayPkg], "协议实现包不得出现在装配根（构造只能发生在 internal/paymentregistry）")
}

// TestPaymentWiringGate2EpayImportedOnlyByRegistry 层 ②：仓级文件全集相等。
//
// 扫 `internal/` + `cmd/` 的**非测试**文件（递归）：测试文件可以用 epay（表驱动、
// 真实 PEM 等都要它），生产代码只有清单包可以。等号是双向的——把 paymentregistry
// 改成不 import epay 也会红（那意味着清单包空了）。
func TestPaymentWiringGate2EpayImportedOnlyByRegistry(t *testing.T) {
	root := wiringRepoRoot(t)
	var importers []string
	scanned := 0
	for _, base := range []string{"internal", "cmd"} {
		basePath := filepath.Join(root, base)
		walkErr := filepath.WalkDir(basePath, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "dist" || entry.Name() == "web" {
					return filepath.SkipDir
				}
				return nil
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			_, file := wiringParse(t, path)
			scanned++
			for _, spec := range file.Imports {
				if wiringImportPath(t, spec) == wiringEpayPkg {
					rel, relErr := filepath.Rel(root, path)
					if relErr != nil {
						return relErr
					}
					importers = append(importers, filepath.ToSlash(rel))
					break
				}
			}
			return nil
		})
		require.NoError(t, walkErr, "扫面 %s", base)
	}
	require.Greater(t, scanned, 500, "扫面文件数异常（实际 %d）：门禁在空转", scanned)
	sort.Strings(importers)

	require.NotEmpty(t, importers, "清单包必须 import 协议实现（否则协议清单是空的）")
	require.True(t, slices.ContainsFunc(importers, func(p string) bool {
		return strings.HasPrefix(p, wiringRegistryRelGlob)
	}), "internal/paymentregistry 必须出现在 epay 的 import 方集合里，实际：%v", importers)
	for _, importer := range importers {
		require.True(t, strings.HasPrefix(importer, wiringRegistryRelGlob),
			"%s import 了 %s：具体协议构造只能出现在 internal/paymentregistry", importer, wiringEpayPkg)
	}
}

// TestPaymentWiringGate3DependencyGraph 层 ③：依赖图上验一次（门禁非空转）。
//
// 本层用 `os/exec` 跑真 `go list`：找不到 go 工具链就 `t.Skip` 并**在输出里说明**
// ——`skip ≠ green`，报告里必须写清楚这一层是没跑而不是通过。
func TestPaymentWiringGate3DependencyGraph(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("跳过（不是通过）：找不到 go 工具链，无法验证依赖图：%v", err)
	}
	root := wiringRepoRoot(t)

	listDeps := func(pkg string) []string {
		cmd := exec.Command(goBin, "list", "-deps", pkg)
		cmd.Dir = root
		out, runErr := cmd.Output()
		require.NoError(t, runErr, "go list -deps %s", pkg)
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}

	serviceDeps := listDeps("./internal/service")
	require.True(t, slices.Contains(serviceDeps, wiringModulePath+"/internal/domain"),
		"/internal/service 的依赖图里没有 domain？go list 结果可疑（门禁空转）")
	require.False(t, slices.Contains(serviceDeps, wiringEpayPkg),
		"/internal/service 不得依赖 %s：service 只认 domain.PaymentProvider（否则换协议要动 service）", wiringEpayPkg)

	serverDeps := listDeps("./cmd/server")
	require.True(t, slices.Contains(serverDeps, wiringRegistryPkg),
		"/cmd/server 必须依赖 %s：否则本门禁与层 ② 都在对一棵不含清单的依赖树说话", wiringRegistryPkg)
}

// TestPaymentWiringGate4MainGoAssemblyShape 层 ④：main.go 的装配形状。
//
// 四条 AST 断言（都按 import 别名解析，别名 import 绕不过去）：
//   - 无 `[]domain.PaymentProvider` composite literal（协议集合不得是装配根里的字面量）；
//   - `service.Deps` 字面量里 `PaymentRegistry` 恰 1 次、`PaymentProviders` 0 次；
//   - `paymentregistry.Registry` 调用恰 1 次（清单的唯一入口）；
//   - `domain.NewProviderRegistry` 调用 0 次（空注册表合法 ⇒ 必须单独禁）。
func TestPaymentWiringGate4MainGoAssemblyShape(t *testing.T) {
	_, file := parseMainGo(t)
	alias := wiringImportAliases(t, file)

	// 4a. 不得出现 []domain.PaymentProvider 字面量。
	var sliceLits []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		arrayType, ok := lit.Type.(*ast.ArrayType)
		if !ok {
			return true
		}
		if sel, ok := arrayType.Elt.(*ast.SelectorExpr); ok &&
			wiringSelectorPath(alias, sel) == wiringDomainPkg+".PaymentProvider" {
			sliceLits = append(sliceLits, wiringPosition(alias, n))
		}
		return true
	})
	require.Empty(t, sliceLits, "main.go 不得出现 []domain.PaymentProvider 字面量（协议集合只能来自 paymentregistry）")

	// 4b. service.Deps 字面量里的两个字段计数。
	registryFields, legacyFields := 0, 0
	depsLits := 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := lit.Type.(*ast.SelectorExpr); !ok || wiringSelectorPath(alias, sel) != wiringServicePkg+".Deps" {
			return true
		}
		depsLits++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "PaymentRegistry":
				registryFields++
			case "PaymentProviders":
				legacyFields++
			}
		}
		return true
	})
	require.Equal(t, 1, depsLits, "main.go 必须恰好构造一次 service.Deps")
	require.Equal(t, 1, registryFields, "service.Deps 里 PaymentRegistry 必须恰 1 次（装配唯一入口）")
	require.Equal(t, 0, legacyFields, "service.Deps 不得再出现 PaymentProviders（弱契约已删除）")

	// 4c. paymentregistry.Registry 调用恰 1 次；domain.NewProviderRegistry 0 次。
	registryCalls := 0
	newRegistryCalls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch wiringSelectorPath(alias, sel) {
		case wiringRegistryPkg + ".Registry":
			registryCalls++
		case wiringDomainPkg + ".NewProviderRegistry":
			newRegistryCalls++
		}
		return true
	})
	require.Equal(t, 1, registryCalls, "paymentregistry.Registry 必须恰 1 次（协议清单的唯一事实来源）")
	require.Equal(t, 0, newRegistryCalls,
		"main 不得调用 domain.NewProviderRegistry：空的空注册表是合法的，绕开清单包也能启动（此后一个协议都没有）")
}

// TestPaymentWiringGate5ProtocolNamesAbsentFromMainGo 层 ⑤：协议名自派生。
//
// 名字从**运行时注册表**取（`Registry(nil).All()` → `Name()`），不是抄一份常量表：
// 新增协议自动被本层覆盖。只查 `ast.BasicLit`（字符串字面量），**不查注释**——
// main 里的中文注释提到协议名是合法的（层 ② 已经把 import 封死）。
func TestPaymentWiringGate5ProtocolNamesAbsentFromMainGo(t *testing.T) {
	registry, err := paymentregistry.Registry(nil)
	require.NoError(t, err, "Registry(nil) 必须成功：nil client 是协议构造函数的契约（N67）")

	names := make([]string, 0, 4)
	for _, p := range registry.All() {
		names = append(names, string(p.Name()))
	}
	require.NotEmpty(t, names, "注册表为空时本层会空转：至少一个协议必须注册")
	require.True(t, slices.IsSorted(names),
		"All() 是唯一排序点（name 升序），实际 %v：调用方不得再排序，注册表也不得退化成插入序", names)

	_, file := parseMainGo(t)
	var literals []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, unquoteErr := strconv.Unquote(lit.Value)
		if unquoteErr != nil {
			value = lit.Value
		}
		literals = append(literals, value)
		return true
	})
	require.NotEmpty(t, literals, "main.go 的字符串字面量扫面是空转")

	for _, name := range names {
		require.NotContains(t, literals, name,
			"main.go 出现了协议名字面量 %q：协议名只能来自 paymentregistry（构造与命名都不得内联）", name)
	}
}

// wiringImportAliases 返回本文件里"本地名 → import 路径"的映射（别名 import 也解析）。
func wiringImportAliases(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	alias := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		path := wiringImportPath(t, spec)
		local := ""
		if spec.Name != nil {
			local = spec.Name.Name
		} else {
			local = path[strings.LastIndex(path, "/")+1:]
		}
		alias[local] = path
	}
	return alias
}

// wiringSelectorPath 把 `x.Sel` 还原成 "import 路径.Sel"（别名也解析）；解析不了 → 空串。
func wiringSelectorPath(alias map[string]string, sel *ast.SelectorExpr) string {
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	path, ok := alias[ident.Name]
	if !ok {
		return ""
	}
	return path + "." + sel.Sel.Name
}

// wiringPosition 给人看的定位串（失败信息里指出是哪一处字面量）。
func wiringPosition(_ map[string]string, n ast.Node) string {
	return "composite literal at offset " + strconv.Itoa(int(n.Pos()))
}
