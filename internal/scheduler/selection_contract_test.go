// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/testsupport/racetest"
)

// 控制面侧的契约预算门禁（spec 2026-11-23 P3b §2.4/§2.5 T1/T3）：
//   - 契约尺寸 264B；控制面包装 288B ⇒ 与 P3b 前同一个分配档（0 额外堆字节）；
//   - BenchmarkSelect5000Accounts 的稳态预算 3 allocs/op、328 B/op 原样保持；
//   - AttemptPlan 的 typed-nil 合同：接口里装 nil 具体指针时两个方法都不得 panic。

// TestSelectionWrapperSize 钉住「契约 + 私有状态」的总尺寸：288B 恰好是旧 Selection
// （272B）的分配档上界，所以这次拆分不多占一个堆字节。
func TestSelectionWrapperSize(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64 位布局门禁")
	}
	require.Equal(t, uintptr(264), unsafe.Sizeof(Selection{}), "契约结构尺寸（见 internal/api 的尺寸门禁）")
	require.Equal(t, uintptr(288), unsafe.Sizeof(selection{}),
		"包装尺寸漂移：264(契约) + 24(lease/session/pool) = 288，与旧 272B 同一个分配档；越过就掉进 320B 档")
}

// TestSelectAllocationBudget 是 P2 的绝对门：稳态 Select + 两个纯投影 + Release
// 恰 3 allocs/op、328 B/op。句柄接口化只许换**调用方式**，不许换分配账。
//
// 读数走 testing.Benchmark（与 -benchmem 同源同算法），因此这里钉的数就是
// `go test -bench Select5000Accounts -benchmem` 打印的那两列。
func TestSelectAllocationBudget(t *testing.T) {
	s := schedulerWithAccounts(t, 5000, nil)
	var usageModel, responseModel string
	cost := measureBenchCost(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			sel, err := s.Select(10, domain.FormatOpenAIChat, mappingRequestModel)
			if err != nil {
				b.Fatal(err)
			}
			usageModel = sel.LogMappedModel(mappingRequestModel)
			responseModel = sel.ClientResponseModel(mappingRequestModel)
			sel.Handle.Release()
		}
	})
	require.Equal(t, int64(3), cost.allocsPerOp,
		"分配次数是硬预算：接口化不许引入第二次分配（句柄与 Selection 同属一个堆对象）")
	// 精确字节数在 -race 下不是判据：race 插桩会给部分分配加影子元数据（本用例本机
	// 恰好没漂，但同类断言没有契约保证，见 internal/testsupport/racetest 的纪律）。
	// 分配**次数**与插桩无关，两种模式都断言。
	if racetest.Enabled {
		t.Logf("race 模式：跳过精确 B/op 判据（实测 %d B/op），尺寸档仍由 TestSelectionWrapperSize 的 Sizeof 门禁兜住", cost.bytesPerOp)
	} else {
		require.Equal(t, int64(328), cost.bytesPerOp,
			"B/op 是尺寸档的读数：288(selection 包装) + 40(既有残留)；越过 328 ⇒ 尺寸档已上移")
	}
	require.Empty(t, usageModel)
	require.Empty(t, responseModel)
}

// TestTypedNilAttemptPlan 是 T1 的计划面：数据面把 typed-nil 的具体计划装进
// api.AttemptPlan 时，接口本身**非 nil**（所以 nil 判空救不了），两个方法必须
// 各自保守返回/no-op——否则一次「没有计划」的收尾就是 panic。
func TestTypedNilAttemptPlan(t *testing.T) {
	var plan api.AttemptPlan = (*AttemptPlan)(nil)
	// 用原生 `== nil` 而不是 require.NotNil：testify 会把 typed-nil 判成 nil，
	// 而这里要钉的恰是「接口本身非 nil，所以 nil 判空救不了」。
	if plan == nil {
		t.Fatal("typed-nil 装进接口后接口必须非 nil：这正是 typed-nil 陷阱的入口")
	}
	require.True(t, plan.ContinuationIncompatible(), "无计划 ⇒ fail-closed（必须解析续接）")
	require.NotPanics(t, func() { plan.AbandonLastAttempt() }, "无计划 ⇒ 退款是 no-op，不得 panic")

	// 同一合同的另一种装配：单态断言（编译期证明实现存在，运行期行为由上面钉住）。
	var _ api.AttemptPlan = (*AttemptPlan)(nil)
}
