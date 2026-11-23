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
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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

// TestFormatPolicyUnknownType 表外类型上的**两个视图必须分离**：
//
//   - FormatsAllowedFor（管理面：这个类型允许哪些格式）→ ok=false 且 nil，
//     调用方必须显式拒绝；
//   - FormatAllowedFor（运行时门：这个类型是否额外禁止某格式）→ **任意格式放行**。
//     表外类型没有政策行 ⇒ 没有可施加的额外限制。这条是既有语义的冻结：
//     BPS 门落地之前，运行时门就是 `IsBPS()`（表外 false ⇒ 不拦）。把 !ok 写成
//     拒绝会静默拒掉那些没设 credential_type 的历史模板（夹具 tpl(8,…) 那类）。
//
// 末尾钉住副本语义——调用方原地改写返回切片后，下一次调用必须仍是权威值。
func TestFormatPolicyUnknownType(t *testing.T) {
	for _, typ := range []credential.Type{"", "bogus", "codex_oauth"} {
		got, ok := FormatsAllowedFor(typ)
		require.False(t, ok, "%q 不在表内 → ok=false", typ)
		require.Nil(t, got, "%q 不在表内 → 返回 nil（不是空切片）", typ)
		for _, f := range allRequestFormats {
			require.True(t, FormatAllowedFor(typ, f), "%q × %q：表外类型放行（无政策行 = 无额外限制）", typ, f)
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

// formatPolicyRowViolations 是"能力表 ↔ 格式政策表"跨表完整性的**唯一判据**
// （空 = 合规）。两个方向都要报：
//   - 已知类型缺 allowedFormats 行：新类型只登记了 credential.caps 行 ⇒
//     FormatAllowedFor 走 `!ok ⇒ 放行` ⇒ **运行时格式门对该类型全放行**（BPS 拿到
//     anthropic、codex 拿到 chat 都不再被拦），而两张表各自的 golden 全绿；
//   - allowedFormats 有行但类型不在能力表：删类型时漏删的残留行，会让已经被拒绝的
//     类型在格式门上"看起来有政策"。
//
// 判据写成纯函数并接受 known/rows 两个参数，是为了让自测能喂合成输入（证明它不是
// 恒空转）——真实调用点只传 credential.KnownTypes() 与 allowedFormats。
func formatPolicyRowViolations(known []credential.Type, rows map[credential.Type][]RequestFormat) []string {
	var out []string
	inKnown := make(map[credential.Type]bool, len(known))
	for _, typ := range known {
		inKnown[typ] = true
		if _, ok := rows[typ]; !ok {
			out = append(out, fmt.Sprintf("新类型必须同时登记 domain.allowedFormats：%q 只有 credential.caps 行，运行时格式门会对它**全放行**（FormatAllowedFor 的 !ok 分支）", typ))
		}
	}
	for typ := range rows {
		if !inKnown[typ] {
			out = append(out, fmt.Sprintf("%q 在 domain.allowedFormats 有政策行，但不在 credential.caps 能力表内（残留行）", typ))
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryKnownTypeHasFormatPolicyRow 跨表完整性：**每个** credential 已知类型都必须在
// domain.allowedFormats 里有政策行，且两边键集合恰好相等。
//
// 这是 `FormatAllowedFor` 的 `!ok ⇒ 放行` 语义的**唯一机械兜底**：放行分支保留历史
// 兼容（表外类型不施加额外限制），代价是"新类型忘记登记 allowedFormats"会静默放行
// 全部格式，而这个缺口在两张表各自的矩阵 golden 里都看不见。
func TestEveryKnownTypeHasFormatPolicyRow(t *testing.T) {
	known := credential.KnownTypes()
	require.NotEmpty(t, known, "已知类型枚举不得为空（空转的完整性检查 = 没有检查）")
	require.Empty(t, formatPolicyRowViolations(known, allowedFormats),
		"能力表与格式政策表必须逐类型对齐")
	require.Len(t, allowedFormats, len(known),
		"allowedFormats 与 credential.KnownTypes() 的键集合必须相等（多一行=残留，少一行=漏登记）")
}

// TestFormatPolicyRowViolationsSelfTest 负例：证明上面的判据**真的会拒**——只加能力表
// 行（新类型）与只留格式政策行（残留行）两个方向都要被点名。
func TestFormatPolicyRowViolationsSelfTest(t *testing.T) {
	rows := map[credential.Type][]RequestFormat{
		credential.TypeAPIKey: {FormatOpenAIChat},
	}
	// 合规对照：两边一致 → 空。
	require.Empty(t, formatPolicyRowViolations([]credential.Type{credential.TypeAPIKey}, rows))

	// 方向 1：caps 有行、allowedFormats 无行（oracle 点名的静默风险）。
	got := formatPolicyRowViolations([]credential.Type{credential.TypeAPIKey, credential.Type("future-only-in-caps")}, rows)
	require.Len(t, got, 1, "必须恰好报一条：%v", got)
	require.Contains(t, got[0], "future-only-in-caps")
	require.Contains(t, got[0], "全放行", "失败信息必须说清后果")

	// 方向 2：allowedFormats 有行、能力表无该类型（残留行）。
	got = formatPolicyRowViolations(nil, rows)
	require.Len(t, got, 1, "残留行必须被点名：%v", got)
	require.Contains(t, got[0], "残留行")
}

// TestFormatAllowedForZeroAlloc 运行时格式门在 buildRoutes 的三层循环里被逐账号 ×
// 模型 × 格式调用，故必须是纯查表 + slices.Contains：**零分配**。表外类型走的是同一
// 条路径（`!ok` 直接返回 true），不得因为"要报告未知类型"而构造任何值。
func TestFormatAllowedForZeroAlloc(t *testing.T) {
	cases := []credential.Type{credential.TypeBPS, credential.TypeAPIKey, "", "bogus"}
	for _, typ := range cases {
		allocs := testing.AllocsPerRun(1000, func() {
			_ = FormatAllowedFor(typ, FormatAnthropic)
		})
		require.Zero(t, allocs, "%q：运行时格式门必须零分配（allocs=%v）", typ, allocs)
	}
}

// formatGateSites 是**运行时格式门**的调用点全集（仓库相对路径 → 期望调用数）。
//
// 迁移史（S2，谓词→通道）：BPS 门此前有四个站点——domain/types.go 与
// scheduler/routing_compiler_support.go 各一处 `IsBPS() && !BPSFormatsOnly(...)`，
// repository/template_repo.go 与 repository/batch.go 各一处 `BPSFormatsOnly(...)`。
// 前两处改成逐 Type 单格判定 `domain.FormatAllowedFor(t, f)` 之后，`BPSFormatsOnly`
// 就只剩仓库侧两个站点；调度/领域侧的 2 个站点**没有消失**，它们迁移成
// `formatAllowedSites`（见下）——删除注册表等于把这两个站点从门禁里摘掉，那是削弱
// 测试，不是迁移。
var formatGateSites = map[string]int{
	"internal/repository/template_repo.go": 1,
	"internal/repository/batch.go":         1,
}

// formatAllowedSites 是 `FormatAllowedFor`（逐 Type × 逐格式的单格视图）的调用点
// 全集。它承接了上面迁移过来的两个站点：这两处是**运行时**格式门，删掉任一处都能
// 静默放行不该放行的格式（BPS 拿到 anthropic、responses-special 拿到 chat），所以
// 必须逐个在位、每处恰好一次。
//
// 为什么两族分开计数而不是合成一张表：判据是**调用形状**（切片子集 vs 单格），
// 混在一起时"把格判定改写成切片判定"会让总数不变、门禁照绿。
var formatAllowedSites = map[string]int{
	"internal/domain/types.go":                       1, // Template.FormatSupports ①
	"internal/scheduler/routing_compiler_support.go": 1, // tplSupportsFormat（blanket 桶）
}

// gateScanSkipTopDirs 是门禁遍历的排除项，按**模块根下的顶层路径**匹配：相对路径的
// 首段 ∈ 名单才剪枝（`<root>/tmp` 剪，`<root>/internal/scheduler/tmp` 照扫）。
//
// 为什么不是按目录名匹配：按名字匹配时，任意深度的同名目录都被跳过——在
// `internal/scheduler/tmp/` 下藏第 5 个调用点，门禁仍然绿。跳过名单承认的是"这个
// **顶层**目录不是产品源码树"，它不对深层同名目录作任何断言；把匹配面收紧到首段，
// 深层目录就回到扫描面内，藏不住。
//
// 模块根自身**在结构上**不可能被跳过：它的相对路径是 "."，首段 "." 不在名单里，
// 所以"检出目录恰好叫 tmp"这种事故也不会把整棵树剪空。
//
// 放进这个名单 = 承认门禁不覆盖该顶层目录；加目录必须说明它为什么不是产品源码。
//
//   - .git：版本库内部对象，没有源码语义；
//   - .omo：本地的计划/报告/一次性探针（gitignore）——留在里面的 0 字节 .go 曾让整个
//     internal/domain 红，而"这台机器上还留着什么"不该决定门禁的红绿；
//   - tmp：构建/调试产物（根级 gitignore）；
//   - node_modules / vendor：第三方依赖副本，出现即工具产物（本仓不是 vendor 布局）。
//
// 刻意**不**收录 tools/ 与 web/：它们是仓库真实的产品/工具源码树（tools/ 是可直接
// `go build` 的 Go 包），BPS 格式门在那里出现调用点同样必须被数到。
var gateScanSkipTopDirs = []string{".git", ".omo", "tmp", "node_modules", "vendor"}

// TestBPSFormatGateSites 存在性门禁（运行时格式门的第一族）：扫**仓库全部非测试
// .go**，`BPSFormatsOnly` 的调用文件集合必须**恰好**等于 formatGateSites，每文件
// 恰好 1 处调用；扫到 0 处必须失败（空转的门禁比没有门禁更危险）。
//
// 扫描面 = 模块根减去 gateScanSkipTopDirs 里的顶层非源码树（首段匹配，深层同名目录
// 照扫；名单理由见该变量注释）；解析失败必须**点名文件**（仓库相对路径 + 解析器的
// 行列号），不让定位成本落在下一个人身上。
//
// 为什么是存在性而非否定式：把整块调用删掉不产生任何格式字面量，否定式 AST 拦
// 不住；「调用数恰好为 1」既能抓删除，也能抓「改写成字面量」（调用消失）。
func TestBPSFormatGateSites(t *testing.T) {
	bps, _ := scanFormatGateSites(t)
	require.NotEmpty(t, bps, "BPSFormatsOnly 调用点扫到 0 个：门禁在空转")
	require.Equal(t, formatGateSites, bps, "BPSFormatsOnly 的调用点集合必须恰好是仓库侧两处，每处恰好 1 次调用")
}

// TestFormatAllowedGateSites 存在性门禁（运行时格式门的第二族，S2 从
// TestBPSFormatGateSites 迁移过来）：`FormatAllowedFor` 的调用点集合必须**恰好**
// 等于 formatAllowedSites。
//
// 为什么迁移而不删除：这两个站点原来写的是 `IsBPS() && !BPSFormatsOnly(...)`，
// 改法只是把判定换成政策表的单格视图——站点本身的职责（运行时拦住不该放的格式）
// 一点没变。删掉注册表 = 这两个站点从门禁里消失，而"删掉一处调用"正是这条门禁
// 存在的理由。
func TestFormatAllowedGateSites(t *testing.T) {
	_, allowed := scanFormatGateSites(t)
	require.NotEmpty(t, allowed, "FormatAllowedFor 调用点扫到 0 个：门禁在空转")
	require.Equal(t, formatAllowedSites, allowed, "FormatAllowedFor 的调用点集合必须恰好是运行时两个站点，每处恰好 1 次调用")
}

// scanFormatGateSites 一次遍历仓库源码，分别数出两族运行时格式门的逐文件调用数。
// 两族共用同一次解析（一次 IO、一个 AST），但**分开返回**：调用形状不同（切片子集
// vs 单格），合成一张表会让"把格判定改写成切片判定"在总数上不可见。
func scanFormatGateSites(t *testing.T) (bps, allowed map[string]int) {
	t.Helper()
	root := formatPolicyRoot(t)
	bps, allowed = map[string]int{}, map[string]int{}
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
			// 只剪**顶层**目录：相对路径首段（root 自身 rel == "."，首段 "." 命中不了
			// 名单）∈ 名单才 SkipDir。深层同名目录（internal/scheduler/tmp 之类）
			// 的首段是 internal，照扫。
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
		if n := countFormatGateCalls(f, "BPSFormatsOnly"); n > 0 {
			bps[rel] = n
		}
		if n := countFormatGateCalls(f, "FormatAllowedFor"); n > 0 {
			allowed[rel] = n
		}
		return nil
	})
	require.NoError(t, err, "扫描仓库源码失败")
	return bps, allowed
}

// countFormatGateCalls 统计一个文件里对 name 的**调用**数：同时识别选择器形式
// （domain.FormatAllowedFor(...)）与包内裸标识符形式（FormatAllowedFor(...)）。
// 函数声明不是 CallExpr，自然不计入。
//
// 裸标识符分支必须限定在 domain 包内（f.Name.Name == "domain"）：生产里只有
// internal/domain 的两个文件用裸调。若不加包名限制，别的包定义一个同名 no-op
// 再加一处裸调用，就能让门禁误以为调用在位，而真正的格式门已被替换——这正是
// 整支对抗评审实测到的绕过路径。
func countFormatGateCalls(f *ast.File, name string) int {
	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if f.Name.Name == "domain" && fn.Name == name {
				count++
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name != name {
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
