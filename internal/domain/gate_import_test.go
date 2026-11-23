// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// ─── 本文件钉什么 ──────────────────────────────────────────────────────────
//
// 本文件是两条仓库级 AST 门禁（`internal/domain/bps_derived_sites_test.go` 的
// countBPSSymbolUses 与 `internal/domain/format_policy_test.go` 的
// countFormatGateCalls）**共用的 import 感知层**，以及它的自测。
//
// 它存在的理由是一次对抗评审实测到的静默绿：两个计数器原先都硬编码「选择器前缀
// 的名字恰好是 `domain`」，于是
//
//	import . "github.com/is7qin/c3api/internal/domain"   // 点导入
//	var _ = BPSDefaultEndpoint                            // 裸 Ident，包名不是 domain
//
// 与
//
//	import d "github.com/is7qin/c3api/internal/domain"    // 别名导入
//	var _ = d.BPSProfileHash                              // 前缀是 d，不是 domain
//
// 两种写法都能在门禁全绿的前提下新增派生站点。修法不是"再猜几个名字"，而是把
// 「这个文件里哪个本地名绑定到了 internal/domain」从 ImportSpec 里解析出来。

// domainImportPath 是这两个扫描器唯一关心的 import path。其它 import（含
// `github.com/.../internal/domain/xxx` 之类的子路径）一律忽略：门禁的判据是
// 「对 domain 包符号的引用」，换成别的 path 就是另一件事，不在本层职责内。
const domainImportPath = "github.com/is7qin/c3api/internal/domain"

// domainImportNames 返回 file 里**绑定到 domainImportPath 的本地名集合**：
//
//   - `import "…/internal/domain"`（无别名）→ {"domain"}；
//   - `import d "…/internal/domain"`（别名）→ {"d"}。**不**额外登记 "domain"：
//     本文件里的 `domain` 若另有含义（别的同名包），把它算进来就是假红，而假红
//     会逼着下一个人放宽判据；
//   - `import _ "…/internal/domain"`（空白导入）→ 不绑定任何名字，不影响集合；
//   - 同一个 path 出现多次（合成夹具）→ 取并集；
//   - 文件里**没有**任何指向该 path 的 ImportSpec → 回落 {"domain"}。合成夹具与
//     `package domain` 自身的文件都不写 import 语句，而回落的代价只是"多算一个
//     前缀"——方向是变红，不是漏绿。真实的跨包引用必须写 import，Go 编译器已经
//     替我们保证了这一点。
//
// 点导入（`import . "…/internal/domain"`）**直接返回错误**，由调用方 t.Fatalf：
// 点导入把 `domain.X` 洗成裸 `X`，而裸 Ident 分支只对 `package domain` 的文件
// 生效，于是另一个包里的点导入会让两个计数器同时失明——这正是要修的那条静默绿
// 路径。拒绝这种写法比"猜哪个裸 Ident 来自点导入"可靠得多（这一层没有类型信息，
// 猜不出）。
func domainImportNames(f *ast.File) (map[string]struct{}, error) {
	names := make(map[string]struct{}, 1)
	imported := false
	for _, spec := range f.Imports {
		if spec == nil || spec.Path == nil {
			continue
		}
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != domainImportPath {
			// 其它 import 一律忽略；无法解析的字面量交给编译器报错，不在这里猜。
			continue
		}
		if spec.Name == nil {
			imported = true
			names["domain"] = struct{}{}
			continue
		}
		switch spec.Name.Name {
		case ".":
			return nil, fmt.Errorf("禁点导入 %s：它把符号洗成裸 Ident，计数会失明", domainImportPath)
		case "_":
			// 空白导入不绑定名字，对计数无影响。
		default:
			imported = true
			names[spec.Name.Name] = struct{}{}
		}
	}
	if !imported {
		names["domain"] = struct{}{}
	}
	return names, nil
}

// parseSyntheticFile 是两条门禁自测共用的合成源码解析器：只解析、不做类型检查
// （合成源码里的 `package scheduler` 引用 domain 符号本就不该能编译）。
func parseSyntheticFile(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, parser.SkipObjectResolution)
	require.NoError(t, err, "合成源码必须可解析：\n%s", src)
	return f
}

// TestDomainImportNamesResolvesLocalBindings 钉住助手本身的三条绑定语义。它不经过
// 两个计数器，因此计数器写坏时这条仍然是独立证据。
func TestDomainImportNamesResolvesLocalBindings(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "默认导入绑定 domain",
			src:  "package scheduler\n\nimport \"github.com/is7qin/c3api/internal/domain\"\n",
			want: []string{"domain"},
		},
		{
			name: "别名导入只绑定别名",
			src:  "package scheduler\n\nimport d \"github.com/is7qin/c3api/internal/domain\"\n",
			want: []string{"d"},
		},
		{
			name: "空白导入不绑定名字（回落默认名）",
			src:  "package scheduler\n\nimport _ \"github.com/is7qin/c3api/internal/domain\"\n",
			want: []string{"domain"},
		},
		{
			name: "没有 import 语句时回落默认名",
			src:  "package domain\n\nvar _ = BPSDefaultEndpoint\n",
			want: []string{"domain"},
		},
		{
			name: "其它 import 不参与（含同名前缀的另一个包）",
			src: "package scheduler\n\nimport (\n\t\"github.com/is7qin/c3api/internal/domain\"\n" +
				"\t\"github.com/is7qin/c3api/internal/other\"\n)\n",
			want: []string{"domain"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domainImportNames(parseSyntheticFile(t, tc.src))
			require.NoError(t, err)
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			require.ElementsMatch(t, tc.want, keys)
		})
	}

	// 点导入：必须返回错误，且错误文案要指向"计数会失明"这个后果。
	_, err := domainImportNames(parseSyntheticFile(t,
		"package scheduler\n\nimport . \"github.com/is7qin/c3api/internal/domain\"\n\nvar _ = BPSDefaultEndpoint\n"))
	require.Error(t, err, "点导入必须判违规")
	require.Contains(t, err.Error(), "禁点导入", "失败信息必须点名禁的是哪种写法")
	require.Contains(t, err.Error(), "计数会失明", "失败信息必须说清后果")
}

// TestBPSDerivedScannerRejectsDotImport 是本单的主证据：点导入 + 裸符号原先在
// S0 门禁里全绿（裸 Ident 分支只看 `package domain`），现在必须变红。
func TestBPSDerivedScannerRejectsDotImport(t *testing.T) {
	f := parseSyntheticFile(t, "package scheduler\n\nimport . \"github.com/is7qin/c3api/internal/domain\"\n\nvar _ = BPSDefaultEndpoint\n")
	n, err := countBPSSymbolUses(f, "BPSDefaultEndpoint")
	require.Error(t, err, "点导入必须判违规（否则裸 Ident 计数失明）")
	require.Zero(t, n, "判违规时不返回任何计数，避免调用方拿半成品数字继续比对")
	require.Contains(t, err.Error(), "禁点导入")
}

// TestBPSDerivedScannerCountsAliasImport 别名导入必须被数到：`d.BPSProfileHash`
// 是一处真实的派生站点，别名不得让它隐形。
func TestBPSDerivedScannerCountsAliasImport(t *testing.T) {
	f := parseSyntheticFile(t, "package scheduler\n\nimport d \"github.com/is7qin/c3api/internal/domain\"\n\nvar _ = d.BPSProfileHash\n")
	n, err := countBPSSymbolUses(f, "BPSProfileHash")
	require.NoError(t, err)
	require.Equal(t, 1, n, "别名导入 + 选择器必须计 1")
}

// TestBPSDerivedScannerCountsDefaultImport 回归：默认导入的原有语义不许被本轮
// 改造改动。
func TestBPSDerivedScannerCountsDefaultImport(t *testing.T) {
	f := parseSyntheticFile(t, "package scheduler\n\nimport \"github.com/is7qin/c3api/internal/domain\"\n\nvar _ = domain.BPSProfileHash\n")
	n, err := countBPSSymbolUses(f, "BPSProfileHash")
	require.NoError(t, err)
	require.Equal(t, 1, n, "默认导入 + 选择器必须计 1")
}

// TestBPSDerivedScannerSkipsDeclaredFields 结构体字段名是**声明**，不是使用。
//
// 夹具刻意用 `package domain`：裸 Ident 分支在这个包里生效，若 *ast.Field 的
// Names 没有登记进 declared，这里的计数会变成 1——方向是假红（新增一个同名字段
// 就把门禁顶红），比漏检更早磨损判据的可信度。
func TestBPSDerivedScannerSkipsDeclaredFields(t *testing.T) {
	f := parseSyntheticFile(t, "package domain\n\ntype zzz struct {\n\tBPSProfileHash string\n}\n")
	n, err := countBPSSymbolUses(f, "BPSProfileHash")
	require.NoError(t, err)
	require.Zero(t, n, "结构体字段名是声明，不能计为使用")

	// 函数参数/结果同理（*ast.Field 的另一半）：入参名与结果名都不是使用。
	f = parseSyntheticFile(t, "package domain\n\nfunc zzz(BPSDefaultEndpoint string) (BPSProfileHash string) {\n\treturn \"\"\n}\n")
	got, err := countBPSSymbolUses(f, "BPSDefaultEndpoint")
	require.NoError(t, err)
	require.Zero(t, got, "函数参数名是声明，不能计为使用")
	got, err = countBPSSymbolUses(f, "BPSProfileHash")
	require.NoError(t, err)
	require.Zero(t, got, "函数结果名是声明，不能计为使用")
}

// TestFormatGateScannerIsImportAware 是第二条门禁（运行时格式门）的同款证据：
// 点导入判违规，别名调用计 1。两条门禁共用同一层绑定解析，这里钉的是"真的接上了"。
func TestFormatGateScannerIsImportAware(t *testing.T) {
	dot := parseSyntheticFile(t, "package scheduler\n\nimport . \"github.com/is7qin/c3api/internal/domain\"\n\nvar _ = BPSFormatsOnly(nil)\n")
	_, err := countFormatGateCalls(dot, "BPSFormatsOnly")
	require.Error(t, err, "点导入必须判违规（否则裸调用计数失明）")

	alias := parseSyntheticFile(t, "package repository\n\nimport d \"github.com/is7qin/c3api/internal/domain\"\n\nvar _ = d.FormatAllowedFor(\"\", \"\")\n")
	n, err := countFormatGateCalls(alias, "FormatAllowedFor")
	require.NoError(t, err)
	require.Equal(t, 1, n, "别名导入 + 选择器调用必须计 1")

	// 回归：默认导入的既有语义不变。
	plain := parseSyntheticFile(t, "package repository\n\nimport \"github.com/is7qin/c3api/internal/domain\"\n\nvar _ = domain.BPSFormatsOnly(nil)\n")
	n, err = countFormatGateCalls(plain, "BPSFormatsOnly")
	require.NoError(t, err)
	require.Equal(t, 1, n, "默认导入 + 选择器调用必须计 1")
}
