// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// 本文件是 U8 的性能锚点。夹具（A3000_M100、每账号独立 *domain.Template 指针、顺序
// 恶意组）见 build_routes_equiv_test.go 的 routeBenchDomainAccounts——**夹具写错等于
// 验收网反向**：共享指针 + 友好顺序会让"按指针去重"拿 2800×、让"只加 break"拿 52×，
// 换成生产形状后它们分别掉到 9.0× 与 ≈1.0×。
//
// 无 build tag：`go test` 默认不跑基准（`-run '^$' -bench …` 才跑），基准放这里即可。

const (
	// 绝对门（与机器无关，可进 CI）。基线（按账号重算）：7023 allocs/op、36.5 MB/op；
	// 目标（按模板事实去重）：27 allocs/op、99.4 KB/op。
	routeBenchMaxAllocsPerOp = 64
	routeBenchMaxBytesPerOp  = 1 << 20 // 1 MB
	// 时间相对门：momus 在生产形状夹具上实测 1731×，要求 ≥50×（留 30× 余量给机器差异）。
	routeBenchMinSpeedup = 50.0
)

// routeBenchSink 防死代码消除：被基准跑出来的产物必须"被用过"，否则编译器有权把
// 整段计算删掉（于是基准测的是 0）。
var routeBenchSink struct {
	routes map[routeKey]*route
	ref    map[routeKey]*route
	groups map[int64]*groupSnapshot
	facts  map[int64]compilerAccountFacts
}

// BenchmarkBuildRoutesA3000M100 是 buildRoutes 单个切片的 before/after（同机交替：
// -count=5 时两行交替出现，机器漂移对两侧同等作用）。after = 生产实现（按模板事实
// 去重），before = 化石参照（按 model × format × account 重算）。
func BenchmarkBuildRoutesA3000M100(b *testing.B) {
	pool := routeBenchPool(b)
	b.Run("after/buildRoutes", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			routeBenchSink.routes = buildRoutes(pool)
		}
	})
	b.Run("before/buildRoutesReference", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			routeBenchSink.ref = buildRoutesReference(pool)
		}
	})
}

// BenchmarkGroupRebuildA3000M100 是**重建级** before/after：U8 只动 buildRoutes 一个
// 切片，叶构造（newSnapshotStatic，每叶 4.4–5.8 µs）与 attachCompilerFacts（持锁内
// 5.1–5.8 ms）都在它之外 —— 这两个数字不被 U8 降低，所以"重建已 sub-ms"是错的。
//
//	after  = buildSnapshots（含新 buildRoutes）+ attachCompilerFacts
//	before = 同上 + 把 routes 换成化石 buildRoutesReference 的结果（旧算法的重建级
//	         上界，误差 = 新 buildRoutes 自身的 ~0.2 ms，占 before 的 <0.1%）
func BenchmarkGroupRebuildA3000M100(b *testing.B) {
	byGroup := map[int64][]*domain.Account{10: routeBenchDomainAccounts()}
	b.Run("after/buildSnapshots+facts", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			groups, byID, err := buildSnapshots(byGroup, nil, testEgressPool, nil)
			require.NoError(b, err)
			routeBenchSink.groups = groups
			routeBenchSink.facts = attachCompilerFacts(byID)
		}
	})
	b.Run("before/leaf+facts+buildRoutesReference", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			groups, byID, err := buildSnapshots(byGroup, nil, testEgressPool, nil)
			require.NoError(b, err)
			facts := attachCompilerFacts(byID)
			for _, gs := range groups {
				gs.routes = buildRoutesReference(gs.accounts)
			}
			routeBenchSink.groups = groups
			routeBenchSink.facts = facts
		}
	})
}

// TestBuildRoutesPerformanceGates 把三个门做成断言（基准是给人看的，门是给 CI 的）：
// allocs/op ≤ 64、B/op ≤ 1 MB（绝对门，与机器无关）、时间 ≥50×（相对门，同机相邻测量）。
//
// **各门守什么，写清楚免得被当成独立保护**（oracle Important-2）：
//   - allocs/op 守的是"每个 (format, model) 桶都 append 一个切片"这一类**分配病**
//     （旧实现按格子 append，6958 allocs/op）；它抓不住**去重失效（M7）**——实测失效
//     实现仍是 28 allocs/op，因为 `routes`/`models` 早已建好，重复插入只是改写已有键；
//   - B/op 守的是同一类病的字节量（36.5 MB → ~99 KB）；
//   - M7 这类"结果对、只是白干"的退化只有**相对时间门**抓得住（实测 62.3 ms vs 0.20 ms
//     ⇒ 7.35× ≪ 50×）。
//
// 时间门依赖同机相邻测量，机器噪声会让余量变小，故只作 50×（相对 1731× 实测留 30× 余量）。
func TestBuildRoutesPerformanceGates(t *testing.T) {
	pool := routeBenchPool(t)
	after := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			routeBenchSink.routes = buildRoutes(pool)
		}
	})
	before := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			routeBenchSink.ref = buildRoutesReference(pool)
		}
	})

	allocs, bytesPerOp := after.AllocsPerOp(), after.AllocedBytesPerOp()
	t.Logf("buildRoutes: %d ns/op, %d B/op, %d allocs/op", after.NsPerOp(), bytesPerOp, allocs)
	t.Logf("buildRoutesReference: %d ns/op, %d B/op, %d allocs/op",
		before.NsPerOp(), before.AllocedBytesPerOp(), before.AllocsPerOp())

	require.LessOrEqual(t, allocs, int64(routeBenchMaxAllocsPerOp),
		"绝对门：每桶 append 的分配病（旧实现 6958 allocs/op）必须消失；注意它不守去重失效（M7 仍是 ~28）")
	require.LessOrEqual(t, bytesPerOp, int64(routeBenchMaxBytesPerOp),
		"绝对门：按账号 append 的垃圾必须消失（36.5 MB → ~99 KB）")
	require.NotZero(t, before.NsPerOp(), "化石参照必须有可测耗时")
	speedup := float64(before.NsPerOp()) / float64(after.NsPerOp())
	t.Logf("speedup: %.1f×（相对门 ≥%.0f×）", speedup, routeBenchMinSpeedup)
	require.GreaterOrEqual(t, speedup, routeBenchMinSpeedup,
		"相对门：按模板事实去重后应当快两个数量级以上")
}
