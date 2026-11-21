// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/stretchr/testify/require"
)

// policyGolden 是 6 类型 × 6 格式的**手抄**期望（规范序）。刻意不从 allowedFormats
// 反推：表整体改错时反推会一起错，测试照绿；手抄的 golden 与表是两种形状，diff
// 落在两处，review 才看得见。
//
// 规范序：chat / responses / responses-ws / images / search / anthropic。
var policyGolden = map[credential.Type][]RequestFormat{
	credential.TypeAPIKey: {
		FormatOpenAIChat,
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
		FormatAnthropic,
	},
	credential.TypeCustom: {
		FormatOpenAIChat,
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
		FormatAnthropic,
	},
	credential.TypeResponsesSpecial: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
	},
	credential.TypeCodexOAuth: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
	},
	credential.TypeCodexPAT: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
		FormatOpenAIImages,
		FormatOpenAISearch,
	},
	credential.TypeBPS: {
		FormatOpenAIResponses,
		FormatOpenAIResponsesWS,
	},
}

// allRequestFormats 是 RequestFormat 的全集（规范序）。
var allRequestFormats = []RequestFormat{
	FormatOpenAIChat,
	FormatOpenAIResponses,
	FormatOpenAIResponsesWS,
	FormatOpenAIImages,
	FormatOpenAISearch,
	FormatAnthropic,
}

// TestFormatPolicyMatrix 逐格断言 6 类型 × 6 格式：期望值全部来自 policyGolden，
// 不看 allowedFormats。类型集合与格式集合也必须与 golden 完全对齐——加一行类型
// 或一个格式而漏改 golden → 红。
func TestFormatPolicyMatrix(t *testing.T) {
	require.Len(t, allowedFormats, len(policyGolden), "allowedFormats 与 golden 的类型集合必须一致")
	for typ, want := range policyGolden {
		for _, f := range want {
			require.True(t, slicesContains(allRequestFormats, f), "%q 的 golden 含矩阵外的格式 %q", typ, f)
		}
		for _, f := range allRequestFormats {
			require.Equal(t, slicesContains(want, f), FormatAllowedFor(typ, f), "%q × %q", typ, f)
		}
		got, ok := FormatsAllowedFor(typ)
		require.True(t, ok, "%q 必须在表内", typ)
		require.Equal(t, want, got, "%q 的允许集合（含规范序）", typ)
	}
}

// TestFormatPolicyUnknownType 表外类型：FormatsAllowedFor 必须 ok=false 且返回
// nil，FormatAllowedFor 对任意格式必须 false。末尾钉住副本语义——调用方原地改写
// 返回切片后，下一次调用必须仍是权威值。
func TestFormatPolicyUnknownType(t *testing.T) {
	for _, typ := range []credential.Type{"", "bogus", "codex_oauth"} {
		got, ok := FormatsAllowedFor(typ)
		require.False(t, ok, "%q 不在表内 → ok=false", typ)
		require.Nil(t, got, "%q 不在表内 → 返回 nil（不是空切片）", typ)
		for _, f := range allRequestFormats {
			require.False(t, FormatAllowedFor(typ, f), "%q × %q：未知类型恒 false", typ, f)
		}
	}
	first, ok := FormatsAllowedFor(credential.TypeBPS)
	require.True(t, ok)
	require.NotEmpty(t, first)
	authoritative := first[0]
	first[0] = FormatAnthropic // 原地改写副本
	require.NotEqual(t, authoritative, first[0], "夹具必须真的改写首元素，否则下面的副本断言是空断言")
	second, ok := FormatsAllowedFor(credential.TypeBPS)
	require.True(t, ok)
	require.NotEmpty(t, second)
	// 只断言「调用方改不动权威表」，不比对行内容：行内容归 TestFormatPolicyMatrix。
	require.Equal(t, authoritative, second[0], "FormatsAllowedFor 必须返回副本：调用方改不动权威表")
}

// formatGateSites 是 BPS 格式门的**调用点**全集（仓库相对路径 → 期望调用数）。
// 站点 6 在 domain 包内是裸标识符调用（types.go），3/4/5 是 domain.BPSFormatsOnly
// 选择器调用；四处都读同一张 allowedFormats 的 BPS 行，删掉任一处都能静默放行
// 不该放行的格式，故必须逐个在位。
var formatGateSites = map[string]int{
	"internal/domain/types.go":                       1,
	"internal/repository/template_repo.go":           1,
	"internal/repository/batch.go":                   1,
	"internal/scheduler/routing_compiler_support.go": 1,
}

// TestBPSFormatGateSites 存在性门禁：扫**仓库全部非测试 .go**，`BPSFormatsOnly`
// 的调用文件集合必须**恰好**等于 formatGateSites，每文件恰好 1 处调用；扫到 0 处
// 必须失败（空转的门禁比没有门禁更危险）。
//
// 为什么是存在性而非否定式：把整块调用删掉不产生任何格式字面量，否定式 AST 拦
// 不住；「调用数恰好为 1」既能抓删除，也能抓「改写成字面量」（调用消失）。
func TestBPSFormatGateSites(t *testing.T) {
	root := formatPolicyRoot(t)
	got := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
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
		n := countBPSFormatsOnlyCalls(f)
		if n == 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		got[filepath.ToSlash(rel)] = n
		return nil
	})
	require.NoError(t, err, "扫描仓库源码失败")
	require.NotEmpty(t, got, "BPSFormatsOnly 调用点扫到 0 个：门禁在空转")
	require.Equal(t, formatGateSites, got, "BPS 格式门的调用点集合必须恰好是四处，每处恰好 1 次调用")
}

// countBPSFormatsOnlyCalls 统计一个文件里对 BPSFormatsOnly 的**调用**数：同时识别
// 选择器形式（domain.BPSFormatsOnly(...)）与包内裸标识符形式（BPSFormatsOnly(...)）。
// 函数声明不是 CallExpr，自然不计入。
//
// 裸标识符分支必须限定在 domain 包内（f.Name.Name == "domain"）：生产里只有
// internal/domain/types.go 用裸调。若不加包名限制，别的包定义一个同名 no-op
// 再加一处裸调用，就能让门禁误以为调用在位，而真正的 BPS 门已被替换——这正是
// 整支对抗评审实测到的绕过路径。
func countBPSFormatsOnlyCalls(f *ast.File) int {
	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if f.Name.Name == "domain" && fn.Name == "BPSFormatsOnly" {
				count++
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name != "BPSFormatsOnly" {
				return true
			}
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "domain" {
				count++
			}
		}
		return true
	})
	return count
}

// formatPolicyRoot 定位模块根：本文件在 <root>/internal/domain/ 下，向上找到含
// go.mod 的目录。
func formatPolicyRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller 必须可用")
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "向上未找到 go.mod：模块根定位失败")
		dir = parent
	}
}

// slicesContains 是本测试文件的自有小工具：意图是「golden 里有没有这个格式」，
// 用包内 slices.Contains 也行，但这里显式写出来，避免把生产实现的细节（比如它
// 是否用 slices）打进 golden 的比较面。
func slicesContains(haystack []RequestFormat, needle RequestFormat) bool {
	for _, f := range haystack {
		if f == needle {
			return true
		}
	}
	return false
}

// TestBPSFormatsOnlySubsetAlgebra 钉住 BPSFormatsOnly 作为**切片子集判定**的语义。
//
// 为什么在 TestBPSFormatsOnlyMatchesPolicy 被删掉之后本条仍需存在：那条 golden
// 副本断言的是「BPS 行恰好是这两个格式」，那件事已经由 TestFormatPolicyMatrix
// 逐格覆盖；但 BPSFormatsOnly 的**调用形状**是切片，不是单格式谓词。生产里目前
// 只有 Template.FormatSupports 用**单元素**切片调它（domain/types.go:382），
// 多元素与空切片的语义没有任何别的读者——删掉旧测试后就是零覆盖。这正是 S11
// 「循环从未进第 2 次迭代」的同类缺陷面：实现退化成只看 fs[0]、或退化成「任一
// 元素允许即 true」、或干脆恒 true/恒 false，现有的矩阵与门禁全部照绿。
//
// 三组断言都**与表行内容无关**：
//  1. 空切片恒 true（∅ ⊆ 任何集合）；
//  2. 单元素 ⟺ FormatAllowedFor(TypeBPS, f)（两个视图同源，同一个数据源）；
//  3. 双元素 ⟺ 逐元素都允许。因为对照的是 FormatAllowedFor 的逐元素合取，而不是
//     手抄的格式白名单，所以改 allowedFormats 的 BPS 行不会让它变红——它不是锁，
//     只是代数性质。
//
// 双元素的**有序**全组合（含 a==b）同时覆盖两种病态：允许 + 不允许（首元素语义
// 会误报 true）与 不允许 + 允许（任一允许语义会误报 true）。
func TestBPSFormatsOnlySubsetAlgebra(t *testing.T) {
	// 1) 空切片：空集是任何集合的子集。nil 与空切片两种字面形状都必须真。
	require.True(t, BPSFormatsOnly(nil), "nil 切片必须恒 true")
	require.True(t, BPSFormatsOnly([]RequestFormat{}), "空切片必须恒 true")

	// 2) 单元素：与单格式视图 FormatAllowedFor 必须逐格同源。
	for _, a := range allRequestFormats {
		require.Equal(t,
			FormatAllowedFor(credential.TypeBPS, a),
			BPSFormatsOnly([]RequestFormat{a}),
			"单元素 %q：BPSFormatsOnly 必须与 FormatAllowedFor(TypeBPS, ·) 同源", a,
		)
	}

	// 3) 双元素：全集 × 全集的有序组合，期望值 = 逐元素允许的合取。
	for _, a := range allRequestFormats {
		for _, b := range allRequestFormats {
			want := FormatAllowedFor(credential.TypeBPS, a) && FormatAllowedFor(credential.TypeBPS, b)
			require.Equal(t, want, BPSFormatsOnly([]RequestFormat{a, b}),
				"双元素 [%q, %q]：必须是逐元素合取，不能只看首元素也不能任一即真", a, b)
		}
	}

	// 长度 > 2 的同一性质（一次全量，防止实现只在 len<=2 时正确）。
	wantAll := true
	for _, f := range allRequestFormats {
		wantAll = wantAll && FormatAllowedFor(credential.TypeBPS, f)
	}
	require.Equal(t, wantAll, BPSFormatsOnly(allRequestFormats),
		"6 个格式的全量切片：必须是逐元素合取")
}
