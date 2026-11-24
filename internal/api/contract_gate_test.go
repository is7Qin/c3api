// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// 契约门禁（spec 2026-11-23 P3b §2.2/§2.4/§2.5）。四条判据全部可执行：
//   - F1：SelectionHandle 方法集恰为 4、AttemptPlan 方法集恰为 2（双向：多/少都红）；
//   - F2：本包不 import internal/scheduler（AST + import 字面量，含测试文件）；
//   - 尺寸：unsafe.Sizeof(Selection{}) == 264 且 <= 288（分配档 ≤288，0 额外堆字节）；
//   - 形状：Selection 是具体结构、只读事实全是字段（实测 20 个）、只多一个 Handle 字段；
//     方法集只有 3 个纯投影 + 1 个句柄取值口（没有字段访问器、没有 Facts()）；
//   - 分离语义：nil Selection / nil Handle 经 HandleOrDetached 退化成 no-op 句柄
//     （P3b 前 `(*Selection)` 的 nil receiver 早退，逐条等价）。
//   - 语义：分离句柄的 4 个方法 no-op/零值；真实句柄的 4 个方法透传到控制面。

// selectionFacts 是只读事实的**精确名单**（实测 20 个：brief 记 19，漏了 ProxyID）（顺序 = 字段声明顺序）。
// 少一个 ⇒ 数据面 238 处直读里有断点；多一个 ⇒ 契约面悄悄变宽。
var selectionFacts = []string{
	"AccountID", "TemplateID", "BaseURL", "Format", "UpstreamKey",
	"CredentialType", "Model", "StripImageTools", "SkipContinuation",
	"UpstreamPaths", "CodexResponsesTransport", "Ext", "CandidateFingerprint",
	"Conversation", "CacheDomain", "InstallationID", "ModelMappingMode",
	"SupplierFinance", "Egress", "ProxyID",
}

// TestSelectionShapeIsConcreteFacts 钉住形状 A：具体结构 + 20 个事实字段 + 1 个 Handle，
// 没有访问器、没有按值返回的 Facts()（那会是每次调用 +288B 分配的红线）。
func TestSelectionShapeIsConcreteFacts(t *testing.T) {
	typ := reflect.TypeOf(Selection{})
	require.Equal(t, reflect.Struct, typ.Kind(), "形状 A：Selection 必须是具体结构体，不是接口/别名到访问器")
	require.Equal(t, len(selectionFacts)+1, typ.NumField(), "字段数 = 20 个只读事实 + Handle")

	for i, want := range selectionFacts {
		f := typ.Field(i)
		require.Equal(t, want, f.Name, "第 %d 个字段必须是 %s（顺序参与内存布局与热路径偏移）", i, want)
		require.True(t, f.IsExported(), "%s 必须是导出字段：数据面直读，不做访问器", want)
	}
	handle, ok := typ.FieldByName("Handle")
	require.True(t, ok, "私有句柄只能经唯一的 Handle 字段暴露")
	require.Equal(t, reflect.TypeOf((*SelectionHandle)(nil)).Elem(), handle.Type,
		"Handle 必须是 SelectionHandle（形状 A：4 方法句柄）")

	// 方法集：只有 3 个纯数据投影。Release/Thread/StepIdentity/Credential 在句柄上，
	// 不在 Selection 上（否则 proxy 可以绕开契约、M2 变异不会落在 proxy）。
	var methods []string
	for i := 0; i < reflect.TypeOf((*Selection)(nil)).NumMethod(); i++ {
		methods = append(methods, reflect.TypeOf((*Selection)(nil)).Method(i).Name)
	}
	sort.Strings(methods)
	require.Equal(t, []string{"ClientResponseModel", "HandleOrDetached", "LogMappedModel", "PriceModel"}, methods,
		"Selection 上只许三个纯数据投影 + 一个句柄取值口；字段访问器/Facts() 一律拒绝（直读才是零成本）")
}

// TestSelectionSizeBudget 钉住尺寸：264B 具体结构（272 - 三个私有指针 24B + 句柄 16B），
// 上界 288 —— 与 P3b 前同一个 288B 分配档，每次预留 0 额外堆字节。
func TestSelectionSizeBudget(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("64 位布局门禁")
	}
	got := unsafe.Sizeof(Selection{})
	require.Equal(t, uintptr(264), got,
		"Selection 尺寸漂移：272(旧) - 24(三个私有指针) + 16(句柄接口) = 264；改字段必须重算并登记")
	require.LessOrEqual(t, got, uintptr(288),
		"尺寸预算 <=288：越过 288 就掉进 320B 分配档（B/op +32，基准红线）")
	require.Equal(t, uintptr(16), unsafe.Sizeof(Selection{}.Handle), "句柄是接口 = 两个机器字")
}

// TestSelectionHandleIsExactlyFourMethods 是 F1：句柄恰好 4 个方法，逐名相等。
// 双向：多一个（例如此前草案里的 LogMappedModel/ClientResponseModel/PriceModel）
// 红；少一个（Release 被误搬走）也红。
func TestSelectionHandleIsExactlyFourMethods(t *testing.T) {
	typ := reflect.TypeOf((*SelectionHandle)(nil)).Elem()
	require.Equal(t, reflect.Interface, typ.Kind(), "SelectionHandle 必须是接口")

	require.Equal(t, 4, typ.NumMethod(), "F1：句柄恰好 4 个方法（Release/Thread/StepIdentity/Credential）")
	// reflect 对接口类型返回的方法签名**不含 receiver**，所以 NumIn 就是实参个数。
	want := map[string]struct {
		in  int
		out []string
	}{
		"Release":      {in: 0, out: []string{}},
		"Thread":       {in: 0, out: []string{"sdkbridge.ThreadHandle"}},
		"StepIdentity": {in: 0, out: []string{}},
		"Credential":   {in: 0, out: []string{"domain.AccountCredential"}},
	}
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		got = append(got, m.Name)
		w, ok := want[m.Name]
		require.True(t, ok, "F1：句柄出现未登记的方法 %s（多一个就是冻结面白扩）", m.Name)
		if !ok {
			continue
		}
		require.Equal(t, w.in, m.Type.NumIn(), "%s 的参数个数是契约的一部分", m.Name)
		require.Equal(t, len(w.out), m.Type.NumOut(), "%s 的返回个数是契约的一部分", m.Name)
		for j, out := range w.out {
			require.Equal(t, out, m.Type.Out(j).String(), "%s 的返回类型是数据面契约的一部分", m.Name)
		}
	}
	require.Equal(t, []string{"Credential", "Release", "StepIdentity", "Thread"}, got,
		"F1：方法集逐名相等（reflect 按字典序返回）")
}

// TestAttemptPlanIsExactlyTwoMethods 是 AttemptPlan 窄接口的姊妹断言：
// ReserveAttempt 属控制面编排，不许塞进计划接口（那会把冻结面从 2 个签名扩成整个预留机制）。
func TestAttemptPlanIsExactlyTwoMethods(t *testing.T) {
	typ := reflect.TypeOf((*AttemptPlan)(nil)).Elem()
	require.Equal(t, reflect.Interface, typ.Kind(), "AttemptPlan 必须是窄接口")
	require.Equal(t, 2, typ.NumMethod(), "AttemptPlan 恰好 2 个方法")
	// reflect 按字典序：AbandonLastAttempt < ContinuationIncompatible。
	abandon := typ.Method(0)
	require.Equal(t, "AbandonLastAttempt", abandon.Name)
	require.Equal(t, 0, abandon.Type.NumIn(), "AbandonLastAttempt 不得带参数")
	require.Equal(t, 0, abandon.Type.NumOut(), "AbandonLastAttempt 不得有返回")
	incompat := typ.Method(1)
	require.Equal(t, "ContinuationIncompatible", incompat.Name)
	require.Equal(t, 0, incompat.Type.NumIn(), "ContinuationIncompatible 不得带参数")
	require.Equal(t, 1, incompat.Type.NumOut())
	require.Equal(t, reflect.Bool, incompat.Type.Out(0).Kind())
}

// TestAPIPackageDoesNotImportScheduler 是 F2：api 是中立包，依赖方向必须单向。
// 扫描面 = 本目录**全部** .go（含测试）——测试文件 import scheduler 也会把依赖
// 方向带回来，断言同一条规则。
func TestAPIPackageDoesNotImportScheduler(t *testing.T) {
	dir := apiPackageDir(t)
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "api 包不得为空：F2 在空扫描面上恒绿")

	fset := token.NewFileSet()
	for _, path := range paths {
		af, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		require.NoError(t, err, "解析 %s", path)
		for _, spec := range af.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err)
			require.NotEqual(t, "github.com/is7qin/c3api/internal/scheduler", imp,
				"F2：%s import 了控制面（api -> scheduler 边会构成契约环）", filepath.Base(path))
			require.False(t, strings.HasPrefix(imp, "github.com/is7qin/c3api/internal/") &&
				imp != "github.com/is7qin/c3api/internal/domain" &&
				imp != "github.com/is7qin/c3api/internal/credential" &&
				imp != "github.com/is7qin/c3api/internal/sdkbridge",
				"F2：api 只许依赖中立值类型（domain/credential/sdkbridge）+ pkg/*，发现 %s", imp)
		}
	}
	// 反向 tripwire：扫描面必须真的看到了契约文件（F2 不是扫了个空目录）。
	var sawSelection bool
	for _, path := range paths {
		if strings.HasSuffix(path, "selection.go") {
			sawSelection = true
		}
	}
	require.True(t, sawSelection, "F2 扫描面缺 selection.go：计数为 0 的绿是假绿")
}

// apiPackageDir 给出本包目录（门禁的扫描根）。
func apiPackageDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) 失败：拿不到扫描根")
	return filepath.Dir(self)
}

// TestNoFactsByValue 是一条**反向**断言：包内不许出现名为 Facts 的方法/函数，
// 也不许出现按值返回 Selection 的函数（+264B 拷贝 ⇒ 逃逸 ⇒ 每次调用多一次分配）。
// 用 AST 扫描包内全部非测试源码；今天只有 0 处，将来加一个立刻红。
func TestNoFactsByValue(t *testing.T) {
	dir := apiPackageDir(t)
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "解析 %s", path)
		ast.Inspect(af, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			require.NotEqual(t, "Facts", fn.Name.Name, "红线：Facts() 按值返回 = +288B/alloc")
			if fn.Type.Results != nil {
				for _, res := range fn.Type.Results.List {
					if id, ok := res.Type.(*ast.Ident); ok && id.Name == "Selection" {
						require.Failf(t, "按值返回 Selection", "%s 按值返回 Selection：接口化后每次调用都会拷 264B 并逃逸",
							fset.Position(fn.Pos()))
					}
				}
			}
			return true
		})
	}
}

// TestHandleOrDetachedPreservesNilReceiverSemantics 钉住 P3b 前 `(*Selection)` 的
// nil receiver 语义：`sel == nil` 与零值 Selection 上取句柄都必须拿到 no-op 句柄，
// 因此数据面的 17 处 `sel == nil` 早退与「没有预留也走同一条收尾」都不变。
func TestHandleOrDetachedPreservesNilReceiverSemantics(t *testing.T) {
	var nilSel *Selection
	for _, sel := range []*Selection{nilSel, {}, {Handle: detachedHandle{}}} {
		h := sel.HandleOrDetached()
		if h == nil {
			t.Fatal("HandleOrDetached 恒非 nil：nil 句柄必须退化成分离句柄")
		}
		require.NotPanics(t, func() { h.Release() }, "分离句柄的 Release 是 no-op")
		require.NotPanics(t, func() { h.StepIdentity() }, "分离句柄的 StepIdentity 是 no-op")
		require.Equal(t, sdkbridge.ThreadHandle{}, h.Thread(), "分离句柄的 Thread 是零值")
		require.Equal(t, domain.AccountCredential{}, h.Credential(), "分离句柄的 Credential 是零值")
	}

	real := &fakeHandle{cred: domain.AccountCredential{AccountID: 7}}
	sel := &Selection{Handle: real}
	require.Equal(t, real, sel.HandleOrDetached(), "真实句柄原样返回（不包装、不复制）")
	sel.HandleOrDetached().Release()
	sel.HandleOrDetached().StepIdentity()
	require.Equal(t, sdkbridge.ThreadHandle{}, sel.HandleOrDetached().Thread())
	require.Equal(t, int64(7), sel.HandleOrDetached().Credential().AccountID)
	require.Equal(t, []string{"release", "step", "thread", "cred"}, real.calls)
}

// fakeHandle 记录调用顺序：证明 HandleOrDetached 没有把调用吞在数据面。
type fakeHandle struct {
	calls []string
	cred  domain.AccountCredential
}

func (f *fakeHandle) Release()      { f.calls = append(f.calls, "release") }
func (f *fakeHandle) StepIdentity() { f.calls = append(f.calls, "step") }
func (f *fakeHandle) Thread() sdkbridge.ThreadHandle {
	f.calls = append(f.calls, "thread")
	return sdkbridge.ThreadHandle{}
}
func (f *fakeHandle) Credential() domain.AccountCredential {
	f.calls = append(f.calls, "cred")
	return f.cred
}

var _ SelectionHandle = (*fakeHandle)(nil)
