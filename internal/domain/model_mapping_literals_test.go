// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ─── 本文件钉什么 ──────────────────────────────────────────────────────────
//
// 钉：`internal/modelmap/modelmap.go` 里 model mapping 相关的**每一条** `fmt.Errorf`
// 文案字面量，都必须在 `model_mapping_test.go` 的断言清单里出现。
//
// R2-a 起被扫的文件换了：ModelMapping 系列的 canonical 定义已下沉到中立叶子包
// internal/modelmap（ent 生成代码要引用它，而 ent 不许反向依赖 domain）。本测试
// **留在 package domain**：它用 parser.ParseFile 按路径读源码，不做类型检查，
// 跨包扫描照样成立；搬到 modelmap 里它就变成被测包自己的门禁，失去独立清点的意义。
//
// 为什么需要它：`model_mapping_test.go` 的断言是**手抄清单**，实测漏过一次
// （`modelmap.go:145` 的 `mode %q must be explicit or implicit` 一度零断言，改坏后
// 12 个用例全绿）。手抄清单的失效模式是"加字段/加校验 → 忘了加断言 → 静默绿"，
// 靠人复查抓不住。
//
// 这里把它变成结构性保证：新增一条映射错误文案而没登记断言 ⇒ 本门禁红。
// 反向也成立：清单里登记了文案但源码里找不到 ⇒ 报错（说明文案被删/改写后
// 断言成了空转）。
//
// 刻意**不**造新抽象：复用同包 `formatPolicyRoot` 定位模块根、只解析一个文件，
// 用 `ast.Inspect` 抓 `fmt.Errorf` 的第一个字面量实参。不加接口、不加注册表。

// mappingErrorLiterals 是 internal/modelmap/modelmap.go 里 model mapping 相关错误
// 文案的**完整清单**（行号以该文件为准，重算于 R2-a 搬迁之后）。
// 每一条都必须有对应的断言（断言在哪一类型见 model_mapping_test.go 的注释）。
//
// 维护方式：新增/改写文案时，先改这里，再补断言——顺序刻意如此，这样"忘了补
// 断言"会在本门禁里表现为"清单里有、断言里没有"，而不是静默通过。
var mappingErrorLiterals = []string{
	"mode must not be null",                // :71 Mode.UnmarshalJSON
	"invalid mode %q",                      // :79 Mode.UnmarshalJSON
	"model mapping entry must not be null", // :91 Entry.UnmarshalJSON
	"mapped_model is required",             // :98 Entry.UnmarshalJSON
	"mode is required",                     // :101 Entry.UnmarshalJSON
	"additional properties not allowed",    // :104 Entry.UnmarshalJSON
	"mapped_model must be non-empty without leading/trailing whitespace",    // :112 Entry.UnmarshalJSON
	"model_mapping must not be null",                                        // :122 ModelMapping.UnmarshalJSON
	"alias %q must be non-empty without leading/trailing whitespace",        // :139 ValidateModelMapping
	"mapped_model %q must be non-empty without leading/trailing whitespace", // :142 ValidateModelMapping
	"mode %q must be explicit or implicit",                                  // :145 ValidateModelMapping
	"invalid ModelMappingMode %q",                                           // :61 Mode.MarshalJSON
}

// TestMappingErrorLiteralsHaveAssertions 是清单与源码的双向门禁：
//
//  1. 源码里 `internal/modelmap/modelmap.go` 的每个 `fmt.Errorf` 文案都在
//     `mappingErrorLiterals` 里
//     （新增文案没登记 ⇒ 红）；
//  2. `mappingErrorLiterals` 的每条都在源码里找得到（文案被改/删而清单没跟 ⇒ 红）。
func TestMappingErrorLiteralsHaveAssertions(t *testing.T) {
	root := formatPolicyRoot(t)
	path := filepath.Join(root, "internal", "modelmap", "modelmap.go")
	src, err := os.ReadFile(path)
	require.NoError(t, err, "读 %s 失败", path)

	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	require.NoError(t, err, "解析 %s 失败", path)

	inSource := make(map[string]struct{}, len(mappingErrorLiterals))
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "fmt" || sel.Sel.Name != "Errorf" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		// 只关心 model mapping 的错误文案：其它域文案（BPS/价格/…）不在本门禁范围。
		if !isMappingErrorLiteral(value) {
			return true
		}
		inSource[value] = struct{}{}
		return true
	})

	require.NotEmpty(t, inSource, "AST 扫描没抓到任何映射错误文案：解析/遍历失效（防空转的门禁）")

	registered := make(map[string]struct{}, len(mappingErrorLiterals))
	for _, lit := range mappingErrorLiterals {
		_, dup := registered[lit]
		require.False(t, dup, "清单有重复条目：%q", lit)
		registered[lit] = struct{}{}
		require.Contains(t, inSource, lit,
			"清单里有、internal/modelmap/modelmap.go 里已找不到：%q（文案被删或改写后，对应断言会空转）", lit)
	}
	for lit := range inSource {
		require.Contains(t, registered, lit,
			"internal/modelmap/modelmap.go 里有未登记的映射错误文案：%q——请加进 mappingErrorLiterals 并在 model_mapping_test.go 补断言", lit)
	}
}

// isMappingErrorLiteral 按文案的**词表**判定是否属 model mapping 域。它刻意只看
// 关键词，不看行号：行号会随无关改动漂移，而词表对应的是"这条错误在说哪个概念"。
//
// ⚠ 词表是**保守方向**的：宁可多算（把一条本不属映射域的新文案也算进来，逼人
// 登记），不可漏算。实测过的失效模式：漏了 "mapping" 这个词，于是
// `fmt.Errorf("empty mapping entry not allowed")` 这条新文案被门禁当域外文案
// 静默放过——那正是本门禁要防的事。故词表按"任何与 mapping/mode/model 相关的
// 概念"取宽，而不是按现有 11 条文案反推。
func isMappingErrorLiteral(s string) bool {
	lower := strings.ToLower(s)
	for _, kw := range []string{
		"mapping", "mapped", "mode", "alias", "additional propert",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
