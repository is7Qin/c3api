// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// benchContinuationRoute 取 5000 账号 fixture 上已编译的 responses 路由决策（真实
// 编译产物，不是手工拼的替身）。
func benchContinuationRoute(tb testing.TB) (*Scheduler, RouteRef) {
	tb.Helper()
	s := schedulerWithAccounts(tb, 5000, domain.ModelMapping{})
	sv := s.View().StaticView()
	if sv == nil {
		tb.Fatal("missing static view")
	}
	c := NewRoutingCompiler()
	dv, err := c.Compile(CompilerInputs{Static: sv})
	if err != nil {
		tb.Fatal(err)
	}
	ref := RouteRefFor(10, string(domain.FormatOpenAIChat), mappingRequestModel)
	if dv.routes[ref] == nil {
		tb.Fatalf("missing compiled route %v (routes=%d)", ref, len(dv.routes))
	}
	return s, ref
}

// BenchmarkPlanContinuationDecision 是 S2 §9 要求的**续接判定微基准**（对应
// attempt_plan_reservation.go 里那条判定被移到构造期之后的成本）。
//
// 它测三件事，都是热路径上真实发生的：
//   - 编译期一次性聚合（对候选车道的能力判定）——只在 reload/compile 上跑；
//   - 构造期把结论拷进计划的只读位——每请求一次字段拷贝，**零查表**；
//   - 数据面读位（plan.ContinuationIncompatible）——每续接请求一次。
//
// 期望：allocs/op = 0（位是 bool，计划是栈值）。
func BenchmarkPlanContinuationDecision(b *testing.B) {
	s, ref := benchContinuationRoute(b)
	dec := s.View().decision.routes[ref]
	if dec == nil {
		b.Fatal("missing published decision")
	}

	b.Run("compile_aggregate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkContinuationNative = planAllCandidatesNativeContinuation(
				dec.Primary, dec.Explore.Ordered, dec.Degraded, capabilityFromTable)
		}
	})

	b.Run("construct_pin", func(b *testing.B) {
		identity := AttemptPlanIdentity{RequestID: "bench-pin"}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			p, err := newSelectSession(identity, dec)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkContinuationIncompatible = p.ContinuationIncompatible()
		}
	})

	b.Run("data_plane_read", func(b *testing.B) {
		p, err := newSelectSession(AttemptPlanIdentity{RequestID: "bench-read"}, dec)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkContinuationIncompatible = p.ContinuationIncompatible()
		}
	})
}

// capabilityFromTable 是能力读投影的生产实现（方法表达式），单独命名只为让基准里
// 的调用与编译器一致。
func capabilityFromTable(t credential.Type) bool { return t.SupportsContinuation() }

var benchmarkContinuationNative bool
var benchmarkContinuationIncompatible bool
