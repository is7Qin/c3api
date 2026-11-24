// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/scheduler"
)

// 数据面 → 控制面调用面门禁（spec 2026-11-23 P2.5）。
//
// 边界：数据面（internal/proxy 生产代码）只许经 5 个基线方法触碰控制面
// （*scheduler.Scheduler），或对 p.sched 做 nil 判空。一旦出现别名、方法值、返回、
// 传参、读内部字段，边界就只是"大家记得别这么写"的约定——本门禁把它变成红。
//
// 判据与断言同住测试文件（与 internal/domain 的 S0/S3b 门禁同款）：只有一个
// checkControlPlaneSurface，自测矩阵与实仓扫描都调它——单一真值来源靠"只有一份实现"
// 保证，与文件落点无关；测试里不许另写一套"看起来等价"的规则，那会让判据改坏时
// 两边一起绿。
//
// 测试面跨层调用不代表生产面契约放宽：本包同包测试大量直接操作 p.sched 的全部方法
// 造夹具（Select/Runtime/FlushRules/InvalidateAllSync…），那是测试的便利，不是数据面
// 可以效仿的用法；门禁只扫非测试源码。

// ---------- 判据（单一真值来源） ----------

const (
	// schedField 是 Proxy 上控制面依赖的字段名（见 forward.go 的 Proxy.sched）。
	schedField = "sched"
	// extField 是 Selection 上的快照扩展对象字段名。
	extField = "Ext"
)

// schedBoundaryDoc / extBoundaryDoc 是白名单失败信息的两条尾巴（spec §1②/§1④）。
const (
	schedBoundaryDoc = "数据面只许经 `p.sched.<M>`（M ∈ 允许集）或 nil 判空访问控制面；" +
		"别名/返回/传参/读内部字段会让边界失效"
	extBoundaryDoc = "`Ext` 指向快照内对象；除 nil 判空外一律不许触碰（写、读字段、别名、传参都算）"
)

// allowedSchedMethods 是数据面经 p.sched 访问控制面的**精确基线**：只有这 5 个方法
// 可以直接调用，且每条都带上放行理由（理由是评审的依据，不是注释装饰）。
//
// 纪律：删除方法必须同提交收紧 rowsMax；新增方法（含 P3 的 Select 合并）**不得自动
// 放行**——必须单独提交、附调用清单与理由，并在评审里一起改 rowsMax。门禁的默认
// 回答是"不"。
var allowedSchedMethods = map[string]string{
	"NewAttemptPlan": "预留计划构造的唯一入口（数据面不自行组计划）",
	"ReserveAttempt": "占槽 + 租约；并发控制唯一入口",
	"MarkResult":     "上报尝试结果（健康/规则事件）",
	"Classify":       "读规则引擎的处置建议（只读）",
	"GroupModels":    "读组可用模型列表（只读）",
}

// rowsMax 是允许集的行数上限，与 len(allowedSchedMethods) 精确相等：只删行时忘了
// 改小它，测试红；想加行必须先把 rowsMax 抬高，diff 里看得见。
const rowsMax = 5

// controlPlaneScan 是一次控制面访问面扫描的完整结果。Violations 非空即失败；其余
// 计数是 tripwire 的锚点（扫描面静默缩小 / 符号改名会让计数掉到 0，而不是"0 违规"）。
type controlPlaneScan struct {
	Violations []string
	// SchedRefs 是 `.sched` 选择器总数；SchedNil + sum(SchedCalls) == SchedRefs
	// 当且仅当所有引用都落在允许的分支上。
	SchedRefs  int
	SchedNil   int            // 合法的 `p.sched == nil` / `!= nil`
	SchedCalls map[string]int // 允许方法名 → 合规的直接调用点数
	// ExtRefs 是 `.Ext` 选择器总数；ExtNil 是其中合法的 nil 判空数。
	ExtRefs int
	ExtNil  int
}

// checkControlPlaneSurface 是「数据面 → 控制面调用面」的唯一判据：对 files 逐个做
// 单遍 AST 遍历，返回全部违规与计数。fset 只用于把位置格式化成 file:line:col。
//
// 判据只做 AST 判定，不读文件、不解析：I/O 与 parser 在用例侧（合成源码与实仓源码
// 都经 parser.ParseFile 进来）。
//
// 已知残余（诚实标注）：反射 / unsafe 可以绕过 AST 判据；字符串字面量 `"sched"`
// 禁令把 `FieldByName("sched")` 这条最常见的反射绕过堵死，但 `Field(1)` 一类按序号的
// 访问只能靠 review。
func checkControlPlaneSurface(fset *token.FileSet, files []*ast.File) controlPlaneScan {
	out := controlPlaneScan{SchedCalls: make(map[string]int, len(allowedSchedMethods))}
	for _, file := range files {
		scanControlPlaneFile(fset, file, &out)
	}
	return out
}

// scanControlPlaneFile 单遍遍历一个文件，用 stack 自然携带父节点链：
// ast.Inspect 在子树结束时回调 f(nil)，此时弹栈；f 返回 false 的节点不入栈。
func scanControlPlaneFile(fset *token.FileSet, file *ast.File, out *controlPlaneScan) {
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			switch x.Sel.Name {
			case schedField:
				out.schedRef(fset, x, stack)
			case extField:
				out.extRef(fset, x, stack)
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				out.stringLit(fset, x)
			}
		}
		stack = append(stack, n)
		return true
	})
}

// schedRef 判定一处 `.sched` 引用。允许两种父形态，其余全红：
//  1. 直接调用 `p.sched.<M>`（M ∈ 允许集）——父是 SelectorExpr，再上一层必须是
//     CallExpr 且 CallExpr.Fun 就是该 SelectorExpr。**只多验"Sel ∈ 允许集"会放行
//     方法值 `f := p.sched.MarkResult`**，那正是这条判据存在的理由。
//  2. nil 判空 `p.sched ==/!= nil`——父是 BinaryExpr 且另一侧是 nil 标识符；
//     不认任意 BinaryExpr（`p.sched == other` 同样是拿走了句柄）。
func (sc *controlPlaneScan) schedRef(fset *token.FileSet, ref *ast.SelectorExpr, stack []ast.Node) {
	sc.SchedRefs++
	parent := stack[len(stack)-1]
	if nilCompared(ref, parent) {
		sc.SchedNil++
		return
	}
	if outer, ok := parent.(*ast.SelectorExpr); ok {
		method := outer.Sel.Name
		if _, allowed := allowedSchedMethods[method]; !allowed {
			sc.violate(fset, ref.Pos(), schedBoundaryDoc,
				"`p.%s.%s` 不在允许集（M 只许是 %d 个基线方法之一）", schedField, method, rowsMax)
			return
		}
		var grand ast.Node
		if len(stack) >= 2 {
			grand = stack[len(stack)-2]
		}
		if call, ok := grand.(*ast.CallExpr); ok && call.Fun == ast.Expr(outer) {
			sc.SchedCalls[method]++
			return
		}
		sc.violate(fset, ref.Pos(), schedBoundaryDoc,
			"`p.%s.%s` 不是直接调用（方法值/赋值/复合字面量都会把控制面句柄带出边界）",
			schedField, method)
		return
	}
	sc.violate(fset, ref.Pos(), schedBoundaryDoc,
		"`p.%s` 只许直接调用允许方法，或与 nil 判空", schedField)
}

// extRef 判定一处 `.Ext` 引用：只认 nil 判空的操作数形态。写点、字段读、别名、
// 传参、delete、自增都由同一条谓词封死（父节点不是 `==/!= nil` 的 BinaryExpr 即红）。
//
// 谓词按**字段名**而不是变量名（不是"只认 sel.Ext"）：把 `sel` 改名不能削弱门禁。
func (sc *controlPlaneScan) extRef(fset *token.FileSet, ref *ast.SelectorExpr, stack []ast.Node) {
	sc.ExtRefs++
	if nilCompared(ref, stack[len(stack)-1]) {
		sc.ExtNil++
		return
	}
	sc.violate(fset, ref.Pos(), extBoundaryDoc,
		"以非 nil 判空的方式访问 `.%s`", extField)
}

// stringLit 封死 reflect.FieldByName("sched") 这条无选择器的绕过路径。只比精确值：
// 文案里出现 sched 无害，裸的字段名才是绕过。
func (sc *controlPlaneScan) stringLit(fset *token.FileSet, lit *ast.BasicLit) {
	s, err := strconv.Unquote(lit.Value)
	if err != nil || s != schedField {
		return
	}
	sc.violate(fset, lit.Pos(), schedBoundaryDoc,
		"出现字符串字面量 `\"sched\"`——反射按名取字段等于绕过选择器判据")
}

// nilCompared 判断 expr 的一次出现是否恰好是 `expr ==/!= nil` 或 `nil ==/!= expr`
// 的操作数。ref 与父节点的 X/Y 按指针相等比较，文本相同的另一个节点不算。
func nilCompared(expr ast.Expr, parent ast.Node) bool {
	bin, ok := parent.(*ast.BinaryExpr)
	if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
		return false
	}
	var other ast.Expr
	switch {
	case bin.X == expr:
		other = bin.Y
	case bin.Y == expr:
		other = bin.X
	default:
		return false
	}
	id, ok := other.(*ast.Ident)
	return ok && id.Name == "nil"
}

func (sc *controlPlaneScan) violate(fset *token.FileSet, pos token.Pos, doc, format string, args ...any) {
	sc.Violations = append(sc.Violations,
		fmt.Sprintf("%s：%s（%s）", fset.Position(pos), fmt.Sprintf(format, args...), doc))
}

// ---------- 断言 ----------

// TestControlPlaneSurfaceAllowedSetIsExact 钉住允许集：5 条、每条理由逐字符串相等。
//
// 双向：多一条红（新增方法不得自动放行），少一条也红（删方法必须同提交收紧 rowsMax
// 并在评审里说明），理由被清空/改写同样红——理由是放行依据，不是注释装饰。
func TestControlPlaneSurfaceAllowedSetIsExact(t *testing.T) {
	want := map[string]string{
		"NewAttemptPlan": "预留计划构造的唯一入口（数据面不自行组计划）",
		"ReserveAttempt": "占槽 + 租约；并发控制唯一入口",
		"MarkResult":     "上报尝试结果（健康/规则事件）",
		"Classify":       "读规则引擎的处置建议（只读）",
		"GroupModels":    "读组可用模型列表（只读）",
	}
	require.Len(t, allowedSchedMethods, rowsMax,
		"允许集只许变短：删方法必须同提交改小 rowsMax；新增方法必须单独评审并同步抬高 rowsMax")
	require.Equal(t, want, allowedSchedMethods,
		"数据面→控制面允许集必须与本基线逐条相等（方法名 + 理由）；新增（含 P3 的 Select 合并）不得自动放行")
	for method, reason := range allowedSchedMethods {
		require.NotEmpty(t, strings.TrimSpace(reason), "%s：放行理由是评审依据，不许留空", method)
	}
}

// TestControlPlaneSurfaceRepo 把实仓非测试源码喂给判据：0 违规，且计数锚点不许
// 静默缩小（扫描面缩到看不见时，0 违规是最危险的绿）。
func TestControlPlaneSurfaceRepo(t *testing.T) {
	fset, files := controlPlaneParseRepo(t)
	scan := checkControlPlaneSurface(fset, files)

	require.Empty(t, scan.Violations,
		"数据面→控制面调用面违例（共 %d 条）：\n%s",
		len(scan.Violations), strings.Join(scan.Violations, "\n"))
	// 锚点：今天 16 处 .sched = 13 处允许调用 + 3 处 nil 判空；4 处 .Ext 全是 nil 判空。
	// 调用点增删必须同提交更新锚点——否则"门禁扫了个寂寞"永远不会有症状。
	require.Equal(t, 16, scan.SchedRefs, "实仓 `.sched` 引用基线漂移：确认扫描面（非测试 .go）没有缩小")
	require.Equal(t, 3, scan.SchedNil, "实仓 `p.sched` nil 判空基线漂移")
	require.Equal(t, 4, scan.ExtRefs, "实仓 `.Ext` 引用基线漂移：确认扫描面没有缩小")
	require.Equal(t, 4, scan.ExtNil, "实仓 `.Ext` 只许 nil 判空：今天 4 处全在此列")
	require.Equal(t, scan.SchedRefs, scan.SchedNil+schedCallTotal(scan),
		"每个 .sched 引用都必须落在「nil 判空」或「允许方法直接调用」上")
	require.Positive(t, schedCallTotal(scan))
	for method := range allowedSchedMethods {
		require.Positive(t, scan.SchedCalls[method],
			"允许方法 %s 实仓调用计数为 0：扫描面缩小或符号改名让门禁空转", method)
	}
	t.Logf("实仓计数：.sched=%d（允许调用 %d + nil 判空 %d），.Ext=%d（nil 判空 %d），分方法 %v",
		scan.SchedRefs, schedCallTotal(scan), scan.SchedNil, scan.ExtRefs, scan.ExtNil, scan.SchedCalls)
}

// TestControlPlaneSurfaceProxyTripwire 钉住 Proxy 的持有形态与扫描根：
//   - canonical 类型 *scheduler.Scheduler 的字段恰好 1 个且叫 sched（另存一个字段、
//     或接口化后自存具体类型，都会让 p.sched 白名单失明）；
//   - internal/proxy 无子包（本门禁只扫单目录，出现子包就必须改成递归 + 目录白名单，
//     不许静默缩小扫描面）。
func TestControlPlaneSurfaceProxyTripwire(t *testing.T) {
	typ := reflect.TypeOf((*Proxy)(nil)).Elem()
	schedType := reflect.TypeOf((*scheduler.Scheduler)(nil))
	var holders []string
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type == schedType {
			holders = append(holders, typ.Field(i).Name)
		}
	}
	require.Equal(t, []string{schedField}, holders,
		"Proxy 中 *scheduler.Scheduler 字段必须恰好 1 个且名为 %q：另存字段=绕过 p.sched 白名单，改为接口=门禁失明；改名必须同改门禁", schedField)

	requireNoControlPlaneSubPackage(t, controlPlanePackageDir(t))
}

// TestControlPlaneSurfaceSelfTests 自测矩阵：所有用例都喂 checkControlPlaneSurface
// （唯一判据），不复制任何判据。方法值那两条是本轮的关键——只验"父是 SelectorExpr
// 且 Sel ∈ 允许集"会把 `f := p.sched.MarkResult` 放成假绿。
func TestControlPlaneSurfaceSelfTests(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantErr  int
		contains string         // wantErr > 0 时，首条违规必须含有的关键词
		calls    map[string]int // 期望的允许方法调用计数（nil = 一个都不许记）
		schedNil int
		extNil   int
	}{
		{
			name: "允许调用全部基线方法通过",
			src: syntheticProxySource(`
		p.sched.NewAttemptPlan(identity, route)
		p.sched.ReserveAttempt(plan)
		p.sched.MarkResult(id, kind, nil, status, msg, model)
		p.sched.Classify(event)
		p.sched.GroupModels(groupID)`),
			calls: map[string]int{
				"NewAttemptPlan": 1,
				"ReserveAttempt": 1,
				"MarkResult":     1,
				"Classify":       1,
				"GroupModels":    1,
			},
		},
		{
			name:     "非允许方法调用红",
			src:      syntheticProxySource(`p.sched.InvalidateAll()`),
			wantErr:  1,
			contains: "不在允许集",
		},
		{
			name:     "允许方法的方法值红",
			src:      syntheticProxySource(`f := p.sched.MarkResult`),
			wantErr:  1,
			contains: "不是直接调用",
		},
		{
			name:     "非允许方法的方法值红",
			src:      syntheticProxySource(`f := p.sched.InvalidateAll`),
			wantErr:  1,
			contains: "不在允许集",
		},
		{
			name:     "别名红",
			src:      syntheticProxySource(`x := p.sched`),
			wantErr:  1,
			contains: "只许直接调用允许方法",
		},
		{
			name:     "return 红",
			src:      syntheticProxySource(`return p.sched`),
			wantErr:  1,
			contains: "只许直接调用允许方法",
		},
		{
			name:     "读控制面内部字段红",
			src:      syntheticProxySource(`_ = p.sched.view`),
			wantErr:  1,
			contains: "不在允许集",
		},
		{
			name:     "作为参数红",
			src:      syntheticProxySource(`use(p.sched)`),
			wantErr:  1,
			contains: "只许直接调用允许方法",
		},
		{
			name:     "nil 判等通过",
			src:      syntheticProxySource(`if p.sched == nil { return }`),
			schedNil: 1,
		},
		{
			name:     "nil 判不等通过",
			src:      syntheticProxySource(`if p.sched != nil { use(p) }`),
			schedNil: 1,
		},
		{
			name:     "与 nil 无关的 BinaryExpr 红",
			src:      syntheticProxySource(`_ = p.sched == other`),
			wantErr:  1,
			contains: "只许直接调用允许方法",
		},
		{
			name:     "Ext 赋值红",
			src:      syntheticProxySource(`sel.Ext = &AccountExt{}`),
			wantErr:  1,
			contains: "Ext",
		},
		{
			name:     "Ext 字段赋值红",
			src:      syntheticProxySource(`sel.Ext.F = 1`),
			wantErr:  1,
			contains: "Ext",
		},
		{
			name:     "Ext 别名红",
			src:      syntheticProxySource(`ext := sel.Ext`),
			wantErr:  1,
			contains: "Ext",
		},
		{
			name:     "Ext 传参红",
			src:      syntheticProxySource(`mut(sel.Ext)`),
			wantErr:  1,
			contains: "Ext",
		},
		{
			name:     "Ext 自增红",
			src:      syntheticProxySource(`sel.Ext.N++`),
			wantErr:  1,
			contains: "Ext",
		},
		{
			name:   "Ext nil 判等通过",
			src:    syntheticProxySource(`if sel.Ext == nil { return }`),
			extNil: 1,
		},
		{
			name:   "Ext nil 判不等通过",
			src:    syntheticProxySource(`if sel.Ext != nil { use(sel) }`),
			extNil: 1,
		},
		{
			name:     "字符串字面量 sched 红",
			src:      syntheticProxySource(`_ = "sched"`),
			wantErr:  1,
			contains: "字符串字面量",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			af, err := parser.ParseFile(fset, "synthetic.go", tc.src, 0)
			require.NoError(t, err, "合成源码必须可解析")
			scan := checkControlPlaneSurface(fset, []*ast.File{af})

			require.Len(t, scan.Violations, tc.wantErr, "违规集合不符：%v", scan.Violations)
			if tc.contains != "" {
				require.Contains(t, scan.Violations[0], tc.contains, "违规信息必须指出具体形态")
			}
			wantCalls := tc.calls
			if wantCalls == nil {
				wantCalls = map[string]int{}
			}
			require.Equal(t, wantCalls, scan.SchedCalls, "允许调用计数（方法值不得计入）")
			require.Equal(t, tc.schedNil, scan.SchedNil, "p.sched nil 判空计数")
			require.Equal(t, tc.extNil, scan.ExtNil, "Ext nil 判空计数")
		})
	}
}

// syntheticProxySource 包一层函数体，让用例只写被判定的那几行。只解析不类型检查，
// 所以未声明的标识符（Proxy/Selection/use）不影响判据。
func syntheticProxySource(body string) string {
	return "package synthetic\n\nfunc f(p *Proxy, sel *Selection) {\n" + body + "\n}\n"
}

// controlPlaneParseRepo 解析本包全部非测试源。文件清单复用 proxySourceFiles
// （credential_channel_gate_test.go）：两个门禁扫描面必须一致，各写一份迟早会漂移。
func controlPlaneParseRepo(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths := proxySourceFiles(t)
	require.NotEmpty(t, paths, "internal/proxy 非测试源为 0：门禁在空转")
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(paths))
	for _, path := range paths {
		af, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "解析 %s", path)
		files = append(files, af)
	}
	return fset, files
}

// controlPlanePackageDir 给出本包目录（门禁的扫描根）。
func controlPlanePackageDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) 失败：拿不到扫描根")
	return filepath.Dir(self)
}

// requireNoControlPlaneSubPackage 断言扫描根下没有子包。子包里的 .sched 访问不会被
// 单目录扫描看见——出现子包就必须把门禁改成递归 + 目录白名单，而不是让扫描面静默
// 缩小；所以这里直接红，并给出改法。
func requireNoControlPlaneSubPackage(t *testing.T, root string) {
	t.Helper()
	var subPackages []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Dir(path) == root || !strings.HasSuffix(path, ".go") {
			return nil
		}
		subPackages = append(subPackages, path)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, subPackages,
		"internal/proxy 出现子包 %v：本门禁只扫单目录，必须改成递归 + 目录白名单（不许静默缩小扫描面）", subPackages)
}

func schedCallTotal(scan controlPlaneScan) int {
	total := 0
	for _, n := range scan.SchedCalls {
		total += n
	}
	return total
}
