// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ─── 本文件钉什么、不钉什么 ────────────────────────────────────────────────
//
// 钉：`internal/scheduler`（控制面）里对 `domain.BPSDefaultEndpoint` 与
// `domain.BPSProfileHash` 的**使用点**，逐文件精确计数，只许变短。
//
// 为什么不钉全仓：`internal/domain`（定义与规范化）、`internal/sdkbridge`（BPS SDK
// 适配）、`internal/repository`（存储过滤）都合法持有这两个符号，钉进来就是第二份
// 会漂移的名单，且挡不住任何真实缺陷。
//
// 为什么不钉 `domain.BPSFormatsOnly`：它已经被本包 repo-wide 的
// TestBPSFormatGateSites（format_policy_test.go:127-132 的 formatGateSites 注册表）
// 钉死在 `internal/scheduler/routing_compiler_support.go` 的 1 处调用上——同一个符号
// 两份注册表，改一处漏一处就是名单漂移。本文件因此**只看**上面两个符号，
// scheduler 里那处 BPSFormatsOnly 调用是有意不在此处的。
//
// 为什么目标是归零（注册表最终为空）：S2 会把 control-plane 的这两处分别改成
// `ChannelOf(t) == ChannelBPS` 与表驱动 `FormatsAllowedFor`。届时 scheduler 不再
// 派生任何 `domain.BPS*`，本条门禁就退化成与 P1 的 `pkg/* ↛ internal/*` 同款的
// 「Scheduler 零 domain.BPS*」硬规则——此后新增任何一处派生都会被判红。
//
// 为什么是计数而不是否定式：把整块赋值/调用删掉不产生任何字面量，否定式 AST 拦不住
// （对抗评审实测：照抄 credential_channel_gate_test.go 的否定式断言，命中 14 / 漏 20，
// 漏掉的全是 domain.BPS* 派生）。

// bpsDerivedSymbols 是本门禁的符号全集，也是 scheduler 里 `domain.BPS*` 派生点的
// 目标集合（`domain.BPSFormatsOnly` 见文件头，另有门禁）。
//
// 刻意不写成"扫到的任意 `domain.BPS*` 都算"：那会把 domain 包对自身符号的引用
// （routing.go 的裸标识符）也算进控制面，让注册表凭空多出行。
//
// 这份清单同时是自检锚点的输入：锚点要求 `internal/domain/bps.go` 里
// `BPSDefaultEndpoint` 的使用 ≥1。谁把符号从这份清单里删掉，锚点立刻读到 0 而变红。
var bpsDerivedSymbols = []string{"BPSDefaultEndpoint", "BPSProfileHash"}

// bpsSiteKey 是注册表的键：(仓库相对路径, 符号名)。同一个文件里两个符号各算各的。
type bpsSiteKey struct {
	file   string
	symbol string
}

// bpsDerivedSite 是注册表的一行：文件、符号、该文件里的**使用**次数。
type bpsDerivedSite struct {
	file   string
	symbol string
	count  int
}

// bpsDerivedSites 是 `internal/scheduler` 内 `domain.BPS*` 派生点的**完整**注册表
// （非测试、非注释；声明不计，见 countBPSSymbolUses）。复核基准 7a486270：
//
//	internal/scheduler/static_key.go:249  av.bpsProfileDigest = domain.BPSProfileHash(...)
//	internal/scheduler/static_key.go:257  k.baseURL = domain.BPSDefaultEndpoint
//
// 只许变短：删掉一行必须同时把 rowsMax 改小（或归零时一并删掉本表），否则
// TestBPSDerivedSitesInScheduler 会红。新增一行等于承认控制面又多了一处"加类型时
// 忘了改就静默走错分支"的派生点，需要在评审里说明理由。
var bpsDerivedSites = []bpsDerivedSite{
	{file: "internal/scheduler/static_key.go", symbol: "BPSDefaultEndpoint", count: 1},
	{file: "internal/scheduler/static_key.go", symbol: "BPSProfileHash", count: 1},
}

// rowsMax 是注册表的行数上限，与 len(bpsDerivedSites) 精确相等。它存在的唯一目的是
// 把"删一行"变成一次必须显式改小的编辑：只删 bpsDerivedSites 的行而忘了改 rowsMax，
// 测试红；反过来，想加行就得先把 rowsMax 抬高，diff 里看得见。
//
// 只许改小。目标值是 0（S2 完成后本门禁变成"Scheduler 零 domain.BPS*"硬规则）。
const rowsMax = 2

// TestBPSDerivedSitesInScheduler 是控制面派生点的精确计数门禁：
// `internal/scheduler` 内每个符号的「文件 → 次数」必须逐项等于 bpsDerivedSites。
// 多一处（新增派生）红，少一处（删掉赋值 / 改写成字面量）红。
//
// 扫描面复用同包的 formatPolicyRoot(t) 与 gateScanSkipTopDirs：首段剪枝语义与
// TestBPSFormatGateSites 完全一致（深层同名目录照扫，`internal/scheduler/tmp` 之类
// 藏不住）。刻意不另造第二份剪枝名单——两份名单迟早会分叉，而分叉的那一份会把
// 站点藏起来还显示绿色。
func TestBPSDerivedSitesInScheduler(t *testing.T) {
	// 注册表自身的结构约束（先钉住注册表的形状，再看仓库）。
	require.Len(t, bpsDerivedSites, rowsMax, "注册表只许变短：删行必须把 rowsMax 同步改小")
	want := make(map[bpsSiteKey]int, len(bpsDerivedSites))
	for _, row := range bpsDerivedSites {
		require.True(t, strings.HasPrefix(row.file, "internal/scheduler/"),
			"%s × %s：本门禁只钉控制面（internal/scheduler/），domain/sdkbridge/repository 的合法使用不进注册表",
			row.file, row.symbol)
		require.Contains(t, bpsDerivedSymbols, row.symbol,
			"%s × %s：符号不在 bpsDerivedSymbols 里，扫描面数不到它", row.file, row.symbol)
		require.Greater(t, row.count, 0, "%s × %s：0 次的行没有意义，删掉它并改小 rowsMax", row.file, row.symbol)
		key := bpsSiteKey{file: row.file, symbol: row.symbol}
		_, dup := want[key]
		require.False(t, dup, "%s × %s：注册表有重复行", row.file, row.symbol)
		want[key] = row.count
	}

	root := formatPolicyRoot(t)
	all, err := scanBPSDerivedSites(root)
	require.NoError(t, err, "扫描仓库源码失败")

	// 自检锚点（防"空转的门禁"）：计数器必须在 domain/bps.go 里数到 BPSDefaultEndpoint
	// 的**使用**（bps.go:28 的 `raw == BPSDefaultEndpoint`；bps.go:22 的 const 声明不算）。
	// 这条断言与注册表内容无关，因此在 S2 把注册表清空之后仍然成立，是"归零"阶段的
	// 唯一防退化锚点：解析器/遍历失效、symbol 清单被清空，都会让它读到 0。
	anchor := all[bpsSiteKey{file: "internal/domain/bps.go", symbol: "BPSDefaultEndpoint"}]
	require.GreaterOrEqual(t, anchor, 1,
		"计数器在空转：internal/domain/bps.go 里 BPSDefaultEndpoint 的使用数为 %d，说明解析/遍历/符号清单有一处失效", anchor)

	// 只把控制面的行投影出来比较：domain/sdkbridge/repository 的合法使用不参与断言。
	got := make(map[bpsSiteKey]int, len(all))
	for key, n := range all {
		if strings.HasPrefix(key.file, "internal/scheduler/") {
			got[key] = n
		}
	}
	require.Equal(t, want, got,
		"internal/scheduler 内 domain.BPS* 的使用点必须恰好等于注册表：多一处是新增派生，少一处是被删除或改写成了字面量")
}

// TestBPSDerivedUseCountsUsesNotDeclarations 用合成 *ast.File 钉住计数器本身的两个
// 关键语义，避免门禁因为"数错"而空转或误红：
//
//  1. 声明不算使用——`func BPSProfileHash(...)` 的 FuncDecl.Name、`const
//     BPSDefaultEndpoint = ...` 的 ValueSpec 名、`type X` 的 TypeSpec 名都必须跳过。
//     生产里的实例是 internal/domain/bps.go:206，数进去就会凭空多 1；
//  2. 选择器形态（`domain.BPSProfileHash`）只计 1 次，且它的 `Sel` 不能在同一文件是
//     package domain 时被裸标识符分支再数一遍（否则 bps.go 这类文件恒多 1）；
//  3. 裸标识符只在 package domain 的文件里算——别的包定义一个同名 no-op 再加一处裸调用，
//     不得伪装成控制面的派生点。
func TestBPSDerivedUseCountsUsesNotDeclarations(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		symbol string
		want   int
	}{
		{
			name:   "只有函数声明时计 0",
			src:    "package domain\n\nfunc BPSProfileHash(raw []byte) string { return \"\" }\n",
			symbol: "BPSProfileHash",
			want:   0,
		},
		{
			name:   "函数声明 + 一处引用只计 1（反例：声明被数进去会变成 2）",
			src:    "package domain\n\nfunc BPSProfileHash(raw []byte) string { return \"\" }\n\nvar _ = BPSProfileHash\n",
			symbol: "BPSProfileHash",
			want:   1,
		},
		{
			name:   "const 声明不计，只有一处引用时计 1",
			src:    "package domain\n\nconst BPSDefaultEndpoint = \"x\"\n\nvar _ = BPSDefaultEndpoint\n",
			symbol: "BPSDefaultEndpoint",
			want:   1,
		},
		{
			name:   "type 声明不计",
			src:    "package domain\n\ntype BPSDefaultEndpoint struct{}\n",
			symbol: "BPSDefaultEndpoint",
			want:   0,
		},
		{
			name:   "domain 包内的选择器形态只计 1（Sel 不被裸标识符分支重复计）",
			src:    "package domain\n\nvar _ = domain.BPSProfileHash\n",
			symbol: "BPSProfileHash",
			want:   1,
		},
		{
			name:   "domain 包外的选择器形态计 1",
			src:    "package scheduler\n\nvar _ = domain.BPSDefaultEndpoint\n",
			symbol: "BPSDefaultEndpoint",
			want:   1,
		},
		{
			name:   "domain 包外的裸标识符计 0（同名 no-op 不算派生点）",
			src:    "package scheduler\n\nvar _ = BPSProfileHash\n",
			symbol: "BPSProfileHash",
			want:   0,
		},
		{
			name:   "注释与字符串里的名字不计",
			src:    "package domain\n\n// BPSProfileHash 是注释\nvar _ = \"BPSProfileHash\"\n",
			symbol: "BPSProfileHash",
			want:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", tc.src, parser.SkipObjectResolution)
			require.NoError(t, err, "合成源码必须可解析")
			got, err := countBPSSymbolUses(f, tc.symbol)
			require.NoError(t, err, "本组夹具都不含点导入")
			require.Equal(t, tc.want, got)
		})
	}
}

// scanBPSDerivedSites 扫**仓库全部非测试 .go**，返回每个 (文件, 符号) 的使用次数，
// 0 次的键不落表。走法与 TestBPSFormatGateSites 逐行对齐：同一个 formatPolicyRoot(t)
// 定位的模块根、同一份 gateScanSkipTopDirs 首段剪枝、同样的解析失败点名（仓库相对
// 路径 + 解析器的行列号）。
//
// 每个文件只解析一次，两个符号在同一趟里数完——这是本节唯一的全仓 walk，别把它拆成
// 每符号一趟。
func scanBPSDerivedSites(root string) (map[bpsSiteKey]int, error) {
	got := map[bpsSiteKey]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// 只剪**顶层**目录（语义见 gateScanSkipTopDirs 注释）：root 自身 rel == "."，
			// 首段 "." 命中不了名单；`internal/scheduler/tmp` 的首段是 internal，照扫。
			if top, _, _ := strings.Cut(rel, "/"); slices.Contains(gateScanSkipTopDirs, top) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("解析 %s 失败：%w", rel, perr)
		}
		for _, symbol := range bpsDerivedSymbols {
			n, cerr := countBPSSymbolUses(f, symbol)
			if cerr != nil {
				// 点导入：点名文件，让下一个人不用猜是哪一处让整条门禁失明。
				return fmt.Errorf("%s: %w", rel, cerr)
			}
			if n > 0 {
				got[bpsSiteKey{file: rel, symbol: symbol}] = n
			}
		}
		return nil
	})
	return got, err
}

// countBPSSymbolUses 统计一个文件里对 symbol 的**使用**次数：选择器形态
// （domain.BPSDefaultEndpoint / d.BPSProfileHash(...)）与包内裸标识符形态
// （bps.go 的 `raw == BPSDefaultEndpoint`）。裸标识符分支只在 f.Name.Name == "domain"
// 的文件里生效，理由与 countBPSFormatsOnlyCalls 相同：不加包名限制，别的包定义一个
// 同名 no-op 再裸调一处，就能让门禁误以为派生点还在位。
//
// 选择器的前缀**不是**硬编码的 "domain"，而是 domainImportNames 从 ImportSpec 里
// 解析出的本地名集合（默认名 / 别名）。点导入让两个分支同时失明，故由那个助手直接
// 返回错误、这里原样上抛，调用方 t.Fatalf——宁可整条门禁红，也不要静默绿。
//
// 声明不是使用，必须跳过四类 Ident：
//   - FuncDecl.Name（`func BPSProfileHash(...)`，生产实例 internal/domain/bps.go:206）；
//   - ValueSpec.Names（`const BPSDefaultEndpoint = ...`，bps.go:22）；
//   - TypeSpec.Name；
//   - Field.Names（结构体字段 / 函数参数 / 结果 / 嵌入字段 / 接口方法名）。漏了这条
//     时，新增一个同名结构体字段就会凭空多算 1 次——方向是假红。
//
// 选择器的 Sel（`domain.BPSProfileHash` 里那个 BPSProfileHash）也要跳过：它已经在
// SelectorExpr 分支计过，若再进裸标识符分支，package domain 的文件会平白多 1。
func countBPSSymbolUses(f *ast.File, symbol string) (int, error) {
	// 先解析本地名：点导入在这里直接失败，不产生任何"半成品计数"。
	domainPkgs, err := domainImportNames(f)
	if err != nil {
		return 0, err
	}
	declared := map[*ast.Ident]struct{}{}
	selected := map[*ast.Ident]struct{}{}
	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			// 父节点先于子节点访问：这里登记的 Name 在自己被访问时一定已在集合里。
			if node.Name != nil {
				declared[node.Name] = struct{}{}
			}
		case *ast.TypeSpec:
			if node.Name != nil {
				declared[node.Name] = struct{}{}
			}
		case *ast.ValueSpec:
			for _, name := range node.Names {
				if name != nil {
					declared[name] = struct{}{}
				}
			}
		case *ast.Field:
			for _, name := range node.Names {
				if name != nil {
					declared[name] = struct{}{}
				}
			}
		case *ast.SelectorExpr:
			if node.Sel == nil {
				return true
			}
			selected[node.Sel] = struct{}{}
			if node.Sel.Name != symbol {
				return true
			}
			if pkg, ok := node.X.(*ast.Ident); ok {
				if _, isDomain := domainPkgs[pkg.Name]; isDomain {
					count++
				}
			}
		case *ast.Ident:
			if node.Name != symbol || f.Name.Name != "domain" {
				return true
			}
			if _, isDecl := declared[node]; isDecl {
				return true
			}
			if _, isSel := selected[node]; isSel {
				return true
			}
			count++
		}
		return true
	})
	return count, nil
}
