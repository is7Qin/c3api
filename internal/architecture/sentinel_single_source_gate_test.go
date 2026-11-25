// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 哨兵单一来源门禁（P5-3-B 留着的那条尾巴）：**全仓每个哨兵恰好一处声明，且只在
// 它该在的包**。
//
// # 判据
//
// 对下面 8 个导出名，在 internal/ + cmd/ + pkg/ 的**全部非测试** .go 文件里找包级
// var/const 声明（一条 ValueSpec 里的每个名字各算一处），要求：
//
//   - canonical 恰好 8 处，右值是 `errors.New(...)`，落点严格等于
//     sentinelCanonicalTable（internal/identityfence ×3、internal/api ×5）；
//   - 别名恰好 8 处，右值是**单一**选择器 `X.ErrName`，X 必须按 **import 路径**
//     解析到该行的 canonical 包（不是按拼写：`fence "…/identityfence"` 同样算，
//     判据是"值从哪个包来"），且 Sel 与声明的名字同名；落点严格等于
//     sentinelAliasTable（internal/sdkbridge ×3、internal/scheduler ×3）；
//   - 除这 16 处之外全仓零命中：第三种形状——引用局部变量、字面量、别的构造函数、
//     只有类型没有右值——必须为 0；
//   - 防空转：扫到的非测试 .go 文件数不低于 sentinelFileFloor，三个扫描根各自都
//     扫到了文件，8 个名字全部被找到。
//
// 比对按「包目录 + 名字」，不钉行号：行号会随任何无关编辑漂移，而"谁在这个包里声明了
// 这个名字"才是判据。失败信息一律带 file:line，能直接跳到那一行。
//
// # 为什么值/文案断言不够
//
// 现存的等价断言（internal/proxy/error_mapping_test.go 直接拿 api.ErrX 喂数据面映射、
// internal/identityfence/ownership_gate_test.go 的"只许别名 + 转发形状"）全部是**按引用跟随**的：它们写在两个具体位置上，两端指的
// 是同一个符号，所以"别处又声明了一份"根本不进视线。更糟的是失效方向恰好是最危险的
// 那个：第二份声明出现时，只要没人用错那一份，所有等价/文案断言继续绿——而一旦有人
// 用错，errors.Is 静默失配，代码早就跑偏了，那时才发现已经晚了。
//
// 现有那几道门禁还有结构性的盲区：ownership_gate_test.go 只钉 internal/identityfence、
// internal/scheduler/candidate_identity.go、internal/sdkbridge/failure.go 三个**具体
// 位置**；proxy/runtime_sentinel_scope_gate_test.go 只钉"数据面不引用 scheduler.ErrX"，
// 且五条运行期哨兵自 P5 收口后 scheduler 侧已无别名、它扫生产代码只剩一致性证据。没有
// 任何一条覆盖"第四个包又写了一份"——而"别处再声明一份"正是值断言看不见的那一格。
//
// `fmt.Errorf("%w", api.ErrNoAvailable)` 是这条路径上最隐蔽的形态：双向 errors.Is 都
// 成立、端到端断言全绿，但 `==` 与 `%v` 文案都变了。本门禁的别名断言按**形状**判，
// 它既不是单一选择器、也不是 errors.New，于是同时让别名计数掉一条、并在"第三种形状"
// 上报出 file:line。
//
// 一句话：值断言证明"这两处相等"，本门禁证明"全仓只有这 16 处"。前者是必要条件，
// 后者才是单一来源的充分表达。
//
// # 三类断言缺一不可（各抓一个不同的失效方向）
//
//  1. 落点表（8 canonical / 8 别名）：抓"搬走"与"分裂"。少一处 ⇒ 哨兵被删或搬到了
//     表外；多一处 ⇒ 第二份声明。两者都让「包目录 + 名字」集合与表不等。
//  2. 形状（canonical = errors.New；别名 = 单一选择器 + 主语解析到该行的 canonical
//     包 + 同名）：抓"照抄文案的等值副本"。文案一字不差也会红，因为判据是**谁造的
//     值**，不是字符串；`fmt.Errorf("%w", …)` 这种"errors.Is 成立但值身份变了"的
//     写法也在这里被挡住。
//  3. 16 处之外零命中：前两条按形状分桶，而形状之外的声明**从不属于任何一桶**——只
//     数两桶条数会把 `var ErrX error` / `var ErrX = pickOne()` / 表外目录里的
//     `var ErrX = api.ErrX` 全部静默漏掉。这条把"总数 16"与"第三种形状为 0"分别钉住，
//     其中 16 是手抄常量而不是 len(表)+len(表)：后者恒真，等于没有断言。
//
// # 加一个新哨兵时必须同时改哪张表
//
//   - canonical：往 sentinelCanonicalTable 加一行（包目录 + 名字）；
//   - 每个别名：往 sentinelAliasTable 加一行（包目录 + 名字 + 该行 canonical 包路径）；
//   - sentinelTotalDecls 加 1；//     从"别名"变成"包内 var = api.ErrX"，总数仍是 16；操作恰好相反时它们会被当成第三种形状报出）；
//
// 诚实边界：表是常量，有意同时改表与代码的改动在机械上完全合法。门禁的作用是让这类
// 改动**不可静默**——它必须让 8 / 8 / 16 三个数在 diff 里同时出现，评审看得见理由，
// 而不是不可发生。这与 importgraph_test.go 的 allow 指纹是同一类 tripwire。
//
// # 扫描口径
//
// 文件系统遍历，只走 internal/ + cmd/ + pkg/ 三棵运行期树（本单指定口径）；
// testdata / vendor / tmp / node_modules 与隐藏目录在任意深度都不深入，跳过名单直接
// 复用 importgraph_test.go 的 opaqueDirNames——同一份口径不抄第二遍，否则两份名单早晚
// 会漂移。tools/ 与 demo/ 是开发期消费者，不是哨兵的 owner：它们要判等只会 import
// canonical，不会自己声明。
//
// 本文件只读源码：不生成、不修复、不写盘，生产代码零改动。

const (
	// sentinelFileFloor 是防空转下限。基 commit d4f8e68d 实测 735 个非测试 .go
	// （internal 695 / cmd 22 / pkg 18）；取 500 ≈ 实测的 68%，留出合包与删文件的
	// 空间，但不允许扫描面塌掉（遍历被写死成只扫一个根、跳过名单写过头吃掉整棵
	// internal 子树、模块根定位错位）。真正的防漏是下面三条互补的断言：三个根各自
	// 扫到文件、8 个名字全部命中、16 处严格相等。
	sentinelFileFloor = 500

	// sentinelTotalDecls = 8 处 canonical + 8 处别名。故意手抄成常量，不写成
	// len(sentinelCanonicalTable) + len(sentinelAliasTable)：后者恒真，等于没有断言。
	sentinelTotalDecls = 16

	sentinelErrorsImportPath = "errors"
	sentinelAPIImportPath    = modulePath + "internal/api"
	sentinelFenceImportPath  = modulePath + "internal/identityfence"
)

// 声明形状：只有这三种取值。
const (
	// sentinelKindCanonical：右值是 stdlib 的 errors.New(...)。
	sentinelKindCanonical = "canonical"
	// sentinelKindAlias：右值是单一 SelectorExpr（`pkg.ErrName`）。
	sentinelKindAlias = "alias"
	// sentinelKindOther：其余一切形状（调用、字面量、引用局部变量、无右值……）。
	sentinelKindOther = "other"
)

// sentinelNames 是本门禁看守的 8 个名字。逐个写死而不是从表里推导：名字表是"扫描器
// 看什么"的输入，表是"应该在哪儿"的期望，两者拼错任何一个都要能互相发现。
var sentinelNames = []string{
	"ErrMissingCandidateFingerprint",
	"ErrCandidateFingerprintMismatch",
	"ErrMissingExpectedRevision",
	"ErrGroupNotFound",
	"ErrFormatUnavailable",
	"ErrPlanNotReady",
	"ErrNoAvailable",
	"ErrAttemptsExhausted",
}

var sentinelNameSet = func() map[string]bool {
	set := make(map[string]bool, len(sentinelNames))
	for _, name := range sentinelNames {
		set[name] = true
	}
	return set
}()

// sentinelScanRoots 是扫描面。go list 口径里 tools/、demo/ 也在图上，但哨兵的 owner
// 与别名只可能住在这三棵树：开发期消费者要判等会 import canonical，不需要自己的声明。
var sentinelScanRoots = []string{"internal", "cmd", "pkg"}

// sentinelRow 是一行期望。
type sentinelRow struct {
	dir  string // 包目录，相对仓库根，斜杠分隔
	name string // 哨兵名
	// pkg 只对别名行有意义：右值必须**按 import 路径**解析到这个包。canonical 行留空，
	// 因为它的形状已经限定为 stdlib errors.New。
	pkg string
}

// sentinelCanonicalTable：canonical 的 8 个落点。基 commit d4f8e68d 实测即这 8 行。
var sentinelCanonicalTable = []sentinelRow{
	{dir: "internal/identityfence", name: "ErrMissingCandidateFingerprint"},
	{dir: "internal/identityfence", name: "ErrCandidateFingerprintMismatch"},
	{dir: "internal/identityfence", name: "ErrMissingExpectedRevision"},
	{dir: "internal/api", name: "ErrGroupNotFound"},
	{dir: "internal/api", name: "ErrFormatUnavailable"},
	{dir: "internal/api", name: "ErrPlanNotReady"},
	{dir: "internal/api", name: "ErrNoAvailable"},
	{dir: "internal/api", name: "ErrAttemptsExhausted"},
}

// sentinelAliasTable：别名的 8 个落点。右值必须是单一选择器、同名、且主语解析到
// pkg 指定的 canonical 包——三者同时成立才算"同一 error 值"。
var sentinelAliasTable = []sentinelRow{
	// internal/sdkbridge/failure.go 的三条身份哨兵（P3c 起只许别名）。
	{dir: "internal/sdkbridge", name: "ErrMissingCandidateFingerprint", pkg: sentinelFenceImportPath},
	{dir: "internal/sdkbridge", name: "ErrCandidateFingerprintMismatch", pkg: sentinelFenceImportPath},
	{dir: "internal/sdkbridge", name: "ErrMissingExpectedRevision", pkg: sentinelFenceImportPath},

	// internal/scheduler/candidate_identity.go 的三条身份哨兵自 P5 收口起**不再有别名**（迁移期兼容层已删）：
	// scheduler 包内与数据面一律直接引用 `identityfence.ErrX`。它们再出现包级声明
	// 就是表外命中（下面的"16 处之外零命中"会当场报出）。

	// 五条运行期哨兵（ErrGroupNotFound 一族）的 canonical 只住在 internal/api。
	// 下面 5 处是 internal/scheduler 的**包内拼写**（scheduler 是这五条的唯一生产方：
	// attempt_plan_reservation.go / attempt_plan_exec.go），右值是单一选择器 `api.ErrX`，
	// 与 api canonical 是同一 error 值。
	//
	// 它们**不是**迁移期兼容层：数据面的生产代码与测试已一律直指 `api.ErrX`
	// （由 internal/proxy/runtime_sentinel_scope_gate_test.go 守）。这 5 处之所以还在，
	// 是因为 scheduler 包内 45 处调用点用短名；任何**新增**的包级别名都是表外声明，
	// 本条门禁会当场报出——包括有人把别名加回 internal/sdkbridge 或 internal/proxy。
	{dir: "internal/scheduler", name: "ErrGroupNotFound", pkg: sentinelAPIImportPath},
	{dir: "internal/scheduler", name: "ErrFormatUnavailable", pkg: sentinelAPIImportPath},
	{dir: "internal/scheduler", name: "ErrPlanNotReady", pkg: sentinelAPIImportPath},
	{dir: "internal/scheduler", name: "ErrNoAvailable", pkg: sentinelAPIImportPath},
	{dir: "internal/scheduler", name: "ErrAttemptsExhausted", pkg: sentinelAPIImportPath},
}

// sentinelKey 是「包目录 + 名字」。行号不参与比较（见文件头：判据是归属，不是位置）。
type sentinelKey struct {
	dir  string
	name string
}

// sentinelDecl 是扫到的一处包级声明。
type sentinelDecl struct {
	dir    string // 包目录，相对仓库根
	name   string
	file   string // 文件，相对仓库根
	line   int
	kind   string
	rhs    string // 人读的右值形状，失败信息直接用
	srcPkg string // 右值是选择器时，主语按 import 路径解析出的包（解析不到为 ""）
	sel    string // 右值是选择器时的 Sel 名字
	note   string // 形状不合规的具体原因（kind == other 时非空）
}

// TestSentinelSingleSource 是这道门禁的入口：四条断言各自一个子测试，缺一条都会让
// 另一条失效方向漏网（见文件头"三类断言缺一不可"）。
func TestSentinelSingleSource(t *testing.T) {
	root := sentinelModuleRoot(t)
	decls, files, perRoot := sentinelScan(t, root)
	t.Logf("扫描：%d 个非测试 .go（internal %d / cmd %d / pkg %d），命中 %d 处包级声明",
		files, perRoot["internal"], perRoot["cmd"], perRoot["pkg"], len(decls))

	// ① 防空转：扫描面与名字表都必须真的覆盖到，否则下面三条会在空集合上恒绿。
	t.Run("防空转_扫描面与名字表都非空", func(t *testing.T) {
		require.GreaterOrEqualf(t, files, sentinelFileFloor,
			"只扫到 %d 个非测试 .go，低于下限 %d：扫描面塌了（根写错 / 跳过名单写过头 / 模块根定位错位）。"+
				"若确因重构变少，请在同一提交里改小 sentinelFileFloor 并写明理由", files, sentinelFileFloor)
		for _, top := range sentinelScanRoots {
			require.NotZerof(t, perRoot[top], "扫描根 %s 一个非测试 .go 都没扫到：路径写死或遍历提前返回", top)
		}
		seen := make(map[string]int, len(sentinelNames))
		for _, d := range decls {
			seen[d.name]++
		}
		for _, name := range sentinelNames {
			require.NotZerof(t, seen[name],
				"%s 全仓零声明：名字表与扫描器脱节（拼写不一致），或哨兵被整体删除。"+
					"8 个名字必须全部被找到，否则本门禁对该名字恒真", name)
		}
	})

	// ② canonical：恰好 8 处 errors.New，落点严格等于表。
	t.Run("canonical_恰好8处errorsNew且落在表内", func(t *testing.T) {
		canonical := sentinelByKind(decls, sentinelKindCanonical)
		require.Equalf(t, len(sentinelCanonicalTable), len(canonical),
			"canonical 必须恰好 %d 处（一个哨兵一处 errors.New），实测 %d 处。\n"+
				"  期望（包目录 名字）：\n%s\n  实测：\n%s",
			len(sentinelCanonicalTable), len(canonical),
			sentinelWantList(sentinelCanonicalTable), sentinelDescribe(canonical))

		extra, missing := sentinelDiff(sentinelCanonicalTable, canonical)
		require.Emptyf(t, extra,
			"canonical 出现**表外**声明——这就是第二份哨兵，必须删掉（要迁 owner 就删旧行、加新行，一次说清）：\n  %s",
			strings.Join(extra, "\n  "))
		require.Emptyf(t, missing,
			"canonical 的**表内**声明缺失——哨兵被删 / 被搬走 / 改成别名了；"+
				"若是搬家，请同时更新 sentinelCanonicalTable 与 sentinelAliasTable：%s",
			strings.Join(missing, "\n  "))
	})

	// ③ 别名：恰好 8 处单一选择器，同名，主语解析到该行的 canonical 包。
	t.Run("别名_恰好8处单一选择器且指向canonical包", func(t *testing.T) {
		aliases := sentinelByKind(decls, sentinelKindAlias)
		require.Equalf(t, len(sentinelAliasTable), len(aliases),
			"别名必须恰好 %d 处（`var ErrX = canonicalPkg.ErrX`），实测 %d 处。\n"+
				"  期望（包目录 名字）：\n%s\n  实测：\n%s",
			len(sentinelAliasTable), len(aliases),
			sentinelWantList(sentinelAliasTable), sentinelDescribe(aliases))

		extra, missing := sentinelDiff(sentinelAliasTable, aliases)
		require.Emptyf(t, extra,
			"别名出现**表外**声明（第二份声明，或落点没登记）：\n  %s", strings.Join(extra, "\n  "))
		require.Emptyf(t, missing,
			"别名的**表内**声明缺失（别名被删 / 被搬走 / 被改成调用或字面量）：%s", strings.Join(missing, "\n  "))

		want := sentinelRowIndex(sentinelAliasTable)
		for _, d := range aliases {
			row, ok := want[sentinelKey{dir: d.dir, name: d.name}]
			if !ok {
				continue // 表外命中已由上面的 extra 断言报出，不重复刷屏
			}
			require.Equalf(t, d.name, d.sel,
				"%s:%d 包 %s 的 %s 右值是 %s：选择器必须指向**同名**哨兵（%s.%s）。"+
					"换名意味着两枚不同的 error 值，errors.Is 会静默失配",
				d.file, d.line, d.dir, d.name, d.rhs, row.pkg, d.name)
			require.Equalf(t, row.pkg, d.srcPkg,
				"%s:%d 包 %s 的 %s 右值是 %s：主语必须按 import 路径解析到 %s（同一 error 值）。"+
					"不许改成调用 / 包装 / 别的包：`fmt.Errorf(\"%%w\", %s.%s)` 这种写法 errors.Is 成立但 == 与文案都变了",
				d.file, d.line, d.dir, d.name, d.rhs, row.pkg, row.pkg, d.name)
		}
	})

	// ④ 16 处之外零命中：总数手抄常量 + 第三种形状为 0。
	t.Run("16处之外全仓零命中", func(t *testing.T) {
		require.Equalf(t, sentinelTotalDecls, len(decls),
			"这 8 个名字全仓只许有 %d 处包级声明（8 canonical + 8 别名），实测 %d 处。\n"+
				"  全部命中：\n%s", sentinelTotalDecls, len(decls), sentinelDescribe(decls))

		var others []string
		for _, d := range decls {
			if d.kind != sentinelKindOther {
				continue
			}
			others = append(others, fmt.Sprintf("%s:%d 包 %s 的 %s = %s（%s）",
				d.file, d.line, d.dir, d.name, d.rhs, d.note))
		}
		require.Emptyf(t, others,
			"哨兵只有两种合法形状（canonical 的 errors.New / 别名的单一选择器），"+
				"以下声明哪种都不是，因而不可能属于这 %d 处：\n  %s",
			sentinelTotalDecls, strings.Join(others, "\n  "))
	})
}

// ---------------------------------------------------------------------------
// 扫描器
// ---------------------------------------------------------------------------

// sentinelScan 遍历三棵运行期树，返回全部命中、扫到的非测试 .go 文件数、每个根的
// 文件数（后者证明三个根都真的走到了）。
func sentinelScan(t *testing.T, root string) ([]sentinelDecl, int, map[string]int) {
	t.Helper()
	fset := token.NewFileSet()
	var decls []sentinelDecl
	files := 0
	perRoot := make(map[string]int, len(sentinelScanRoots))

	for _, top := range sentinelScanRoots {
		base := filepath.Join(root, filepath.FromSlash(top))
		info, err := os.Stat(base)
		require.NoErrorf(t, err, "扫描根必须存在：%s", top)
		require.Truef(t, info.IsDir(), "扫描根必须是目录：%s", top)

		walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if path == base {
					return nil // 根目录本身要进去
				}
				name := d.Name()
				if strings.HasPrefix(name, ".") {
					return fs.SkipDir
				}
				if _, opaque := opaqueDirNames[name]; opaque {
					return fs.SkipDir // 任意深度的 testdata / vendor / tmp / node_modules
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			f, parseErr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if parseErr != nil {
				return fmt.Errorf("解析失败（扫描面必须全是可解析的 Go 源码）：%s：%w", path, parseErr)
			}
			files++
			perRoot[top]++
			decls = append(decls, sentinelCollect(t, fset, root, f)...)
			return nil
		})
		require.NoErrorf(t, walkErr, "遍历 %s 失败", top)
	}

	sort.Slice(decls, func(i, j int) bool {
		if decls[i].file != decls[j].file {
			return decls[i].file < decls[j].file
		}
		return decls[i].line < decls[j].line
	})
	return decls, files, perRoot
}

// sentinelCollect 从单个文件的**包级** var/const 里收出哨兵声明。
func sentinelCollect(t *testing.T, fset *token.FileSet, root string, f *ast.File) []sentinelDecl {
	t.Helper()
	imports := sentinelImports(t, f)
	var out []sentinelDecl
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || (gd.Tok != token.VAR && gd.Tok != token.CONST) {
			continue // 函数内的局部变量不是"声明面"：只有包级才算
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if !sentinelNameSet[id.Name] {
					continue
				}
				pos := fset.Position(id.Pos())
				rel, err := filepath.Rel(root, pos.Filename)
				require.NoErrorf(t, err, "无法相对化路径：%s", pos.Filename)
				dir, err := filepath.Rel(root, filepath.Dir(pos.Filename))
				require.NoErrorf(t, err, "无法相对化目录：%s", pos.Filename)
				decl := sentinelDecl{
					dir:  filepath.ToSlash(dir),
					name: id.Name,
					file: filepath.ToSlash(rel),
					line: pos.Line,
				}
				sentinelClassify(&decl, vs, i, imports)
				out = append(out, decl)
			}
		}
	}
	return out
}

// sentinelClassify 判定一处声明的形状。这是本门禁的核心判据：**形状**决定它是
// canonical 还是别名，而不是它写在哪个文件里——落点由表单独裁决，两者交叉验证。
func sentinelClassify(decl *sentinelDecl, vs *ast.ValueSpec, idx int, imports map[string]string) {
	val := sentinelValueFor(vs, idx)
	if val == nil {
		decl.kind = sentinelKindOther
		decl.rhs = "无右值（只声明了类型）"
		decl.note = "包级哨兵必须有右值：只声明类型等于把值的来源交给每个使用者"
		return
	}
	decl.rhs = sentinelExprString(val)

	if sentinelIsErrorsNew(val, imports) {
		decl.kind = sentinelKindCanonical
		decl.srcPkg = sentinelErrorsImportPath
		return
	}

	sel, ok := val.(*ast.SelectorExpr)
	if !ok {
		decl.kind = sentinelKindOther
		decl.note = "右值不是单一选择器也不是 errors.New：不许调用、不许字面量、不许引用局部变量"
		return
	}
	decl.kind = sentinelKindAlias
	decl.sel = sel.Sel.Name
	if id, ok := sel.X.(*ast.Ident); ok {
		decl.srcPkg = imports[id.Name]
	}
	if decl.srcPkg == "" {
		decl.note = "选择器主语不是本文件 import 进来的包（局部变量 / 字段 / 嵌套表达式），别名必须指向 canonical 包"
	}
}

// sentinelValueFor 取出第 idx 个名字的右值。一条 ValueSpec 可以声明多个名字
// （`var A, B = f()`，两个名字共享同一个右值表达式），也可以没有右值（`var X error`）。
func sentinelValueFor(vs *ast.ValueSpec, idx int) ast.Expr {
	switch {
	case len(vs.Values) == len(vs.Names):
		return vs.Values[idx]
	case len(vs.Values) == 1:
		return vs.Values[0]
	default:
		return nil
	}
}

// sentinelIsErrorsNew 判断右值是不是 stdlib 的 errors.New(...)。主语按 import 路径
// 解析：`errors "errors"` 之类的重命名照样认，`errors` 指向别的包则不认。
func sentinelIsErrorsNew(e ast.Expr, imports map[string]string) bool {
	ce, ok := e.(*ast.CallExpr)
	if !ok || len(ce.Args) != 1 {
		return false
	}
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "New" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && imports[id.Name] == sentinelErrorsImportPath
}

// sentinelExprString 把右值渲染成人读形状（只用于失败信息，不求可编译）。
func sentinelExprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.CallExpr:
		return sentinelExprString(x.Fun) + "(...)"
	case *ast.SelectorExpr:
		return sentinelExprString(x.X) + "." + x.Sel.Name
	case *ast.Ident:
		return x.Name
	case *ast.BasicLit:
		return "字面量 " + x.Kind.String()
	case *ast.UnaryExpr:
		return x.Op.String() + sentinelExprString(x.X)
	default:
		return fmt.Sprintf("%T", e)
	}
}

// sentinelImports 返回 局部名 -> import 路径。局部名缺省取路径最后一段，显式重命名
// 按重命名算：判据是"值从哪个包来"，不是标识符拼写。
func sentinelImports(t *testing.T, f *ast.File) map[string]string {
	t.Helper()
	out := make(map[string]string, len(f.Imports))
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		require.NoErrorf(t, err, "import 字面量必须可解引号：%s", spec.Path.Value)
		local := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			local = p[i+1:]
		}
		if spec.Name != nil {
			local = spec.Name.Name
		}
		out[local] = p
	}
	return out
}

// sentinelModuleRoot 从本文件向上找含 go.mod 的目录。找不到即 FailNow（不猜路径）。
func sentinelModuleRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) 必须可用")
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "向上未找到 go.mod：模块根定位失败")
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// 断言辅助（纯函数：失败信息必须能直接定位到 file:line）
// ---------------------------------------------------------------------------

func sentinelByKind(decls []sentinelDecl, kind string) []sentinelDecl {
	out := make([]sentinelDecl, 0, len(decls))
	for _, d := range decls {
		if d.kind == kind {
			out = append(out, d)
		}
	}
	return out
}

func sentinelRowIndex(rows []sentinelRow) map[sentinelKey]sentinelRow {
	out := make(map[sentinelKey]sentinelRow, len(rows))
	for _, r := range rows {
		out[sentinelKey{dir: r.dir, name: r.name}] = r
	}
	return out
}

// sentinelDiff 返回「表外命中」与「表内缺失」。表外命中带 file:line——只报一个计数
// 会让人从头找，这道门禁的价值一半在定位。
func sentinelDiff(rows []sentinelRow, hits []sentinelDecl) (extra, missing []string) {
	want := sentinelRowIndex(rows)
	got := make(map[sentinelKey]bool, len(hits))
	for _, d := range hits {
		got[sentinelKey{dir: d.dir, name: d.name}] = true
	}
	for _, d := range hits {
		if _, ok := want[sentinelKey{dir: d.dir, name: d.name}]; !ok {
			extra = append(extra, fmt.Sprintf("%s:%d 包 %s 的 %s = %s", d.file, d.line, d.dir, d.name, d.rhs))
		}
	}
	for _, r := range rows {
		if !got[sentinelKey{dir: r.dir, name: r.name}] {
			missing = append(missing, fmt.Sprintf("%s %s", r.dir, r.name))
		}
	}
	return extra, missing
}

// sentinelDescribe 把命中逐条写成 `file:line 包 dir 的 Name = 右值`。
func sentinelDescribe(hits []sentinelDecl) string {
	if len(hits) == 0 {
		return "    （无：0 处命中）"
	}
	var b strings.Builder
	for i, d := range hits {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "    %s:%d 包 %s 的 %s = %s [%s]", d.file, d.line, d.dir, d.name, d.rhs, d.kind)
		if d.note != "" {
			fmt.Fprintf(&b, "（%s）", d.note)
		}
	}
	return b.String()
}

// sentinelWantList 渲染期望表，和 sentinelDescribe 的实测列表并排看。
func sentinelWantList(rows []sentinelRow) string {
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		if r.pkg == "" {
			fmt.Fprintf(&b, "    %s %s", r.dir, r.name)
			continue
		}
		fmt.Fprintf(&b, "    %s %s = %s.%s", r.dir, r.name, r.pkg, r.name)
	}
	return b.String()
}
