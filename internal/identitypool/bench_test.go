// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package identitypool

// 池级预算（P5b v2 阻塞 A）：基准 + **按上限断言**。
//
// 为什么不能拿 `BenchmarkSelect5000Accounts` 当搬包判据：它的夹具是
// `credential.TypeAPIKey` 账号 ⇒ `ChannelOf != ChannelCodex` ⇒ 不建池、`claim`
// 一次不跑——对池的分配覆盖率是 **0**。那个基准只保留为 Select 侧 tripwire。
//
// 为什么断言上界而不是等值：分配数会随 runtime 版本漂；上界取基线 c6cb3250 的
// 实测值（k=8 = 58 allocs·3360 B·4.5 µs；k=1024 = 7170 allocs·419 KB·478 µs）。
// 三列与 `-benchmem` 输出同源（testing.Benchmark 的 AllocsPerOp/BytesPerOp/NsPerOp），
// 不用 MemStats 手工差分——后者会把基准框架自身的分配算进分子（实测 k=8 多 6 次）。
//
// 分工：`Benchmark*` 只报数（`-bench .` 的读数就是评审要贴的那三列），
// `TestPoolBuildAllocCeilings` 才拦人（在基准函数体内嵌套 testing.Benchmark 会
// 与 b.run1 的互斥量死锁，这也是这条分工的技术原因）。

import (
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/testsupport/racetest"
)

// buildSpec 建池夹具（生产形态：账号 7 / installation "inst"）。
func buildSpec(k int) Spec {
	return Spec{AccountID: 7, K: k, InstallationID: "inst", Policy: testPolicy(), Retire: func(int64, string) {}}
}

// benchPoolBuild 是 `-bench` 侧的纯读数（不改写 b.N，只做 b.N 次建池）。
func benchPoolBuild(b *testing.B, k int) {
	b.Helper()
	spec := buildSpec(k)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Resize(nil, spec, nil)
	}
}

// BenchmarkResizeBuildK8 账号默认并发数（K=8）的建池成本。
func BenchmarkResizeBuildK8(b *testing.B) { benchPoolBuild(b, 8) }

// BenchmarkResizeBuildK1024 上界容量（MaxSlots）的建池成本：一次 reload 在
// publisher.mu 内开 1024 条线程，也是"误用每请求建池"那条路的成本量级。
func BenchmarkResizeBuildK1024(b *testing.B) { benchPoolBuild(b, 1024) }

// measureBuildNS 计时：关掉 GC + 一个时间预算内多轮平均。
//
// 为什么计时不带 GC：每 op 419 KB 垃圾的回收成本属于环境而不是池的分配形态
// （同一份工作量在 GOGC=off 下 512–606 µs，默认 GOGC 下 650–856 µs）。计时判据只
// 量池自己的工作，且用预算平均而不是单次采样（单次采样的抖动会盖过判据本身）。
func measureBuildNS(spec Spec, budget time.Duration) int64 {
	prev := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(prev)
	start := time.Now()
	var n int64
	for time.Since(start) < budget {
		_ = Resize(nil, spec, nil)
		n++
	}
	return time.Since(start).Nanoseconds() / n
}

// TestPoolBuildAllocCeilings 是建池的**上限判据**：分配形态一旦被改坏（每会话多
// 一次分配、缓冲换档、锁内 UUID 放大），这里立刻红。
//
// 分配/字节两列与基线**逐一相等**（58/3360、7170/419168）；耗时上界取基线的 2×
// ——基线是单次采样（k=8 4.5 µs、k=1024 478 µs），本机重复采样得到 4.3–5.9 µs
// 与 513–707 µs（k=1024 的每会话成本比基线那一次高约 20–45%，见报告；同一份工作
// 量、GOGC=off、预算平均），所以耗时只做数量级 tripwire，不做等值契约。
//
// **`-race` 下不当判据**：race 插桩把计时放大近一个数量级（实测 k=8 4.3 µs→26 µs、
// k=1024 0.55 ms→2.8 ms），并会给部分分配加影子元数据而改变 B/op（本用例恰好没漂，
// 但那是运气、不是契约）⇒ 精确字节数与耗时的上界只在非 race 模式成立。分配**次数**
// 与插桩无关，两种模式都可断言；race 模式下另留功能断言（池建出来了、可认领）。
func TestPoolBuildAllocCeilings(t *testing.T) {
	cases := []struct {
		name     string
		k        int
		maxAlloc int64
		maxBytes int64
		maxNs    int64
	}{
		{"k8", 8, 58, 3360, 9000},
		{"k1024", 1024, 7170, 419 * 1024, 956000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := buildSpec(tc.k)
			res := testing.Benchmark(func(rb *testing.B) {
				rb.ReportAllocs()
				for i := 0; i < rb.N; i++ {
					_ = Resize(nil, spec, nil)
				}
			})
			allocs, bytes := res.AllocsPerOp(), res.AllocedBytesPerOp()
			ns := measureBuildNS(spec, 50*time.Millisecond)
			t.Logf("k=%d：%d allocs/op，%d B/op，%d ns/op（race=%v）", tc.k, allocs, bytes, ns, racetest.Enabled)

			// 功能断言：两种模式都必须成立（池真的建出来了、且能按 K 认领）。
			built := Resize(nil, spec, nil)
			require.Equal(t, tc.k, built.Len(), "建池必须给出 K 个槽位")
			require.NotNil(t, built.Claim(), "建出来的池必须可认领")

			if racetest.Enabled {
				// race 插桩改变分配计数与耗时 ⇒ 字节/耗时上界只在非 race 模式是判据。
				// 分配**次数**不随插桩变化，仍可断言（它才是"每会话 7 allocs"那条形态）。
				require.LessOrEqual(t, allocs, tc.maxAlloc,
					"分配次数与 -race 无关：每会话 7 allocs + 池/槽位切片 2 是基线形态")
				return
			}
			require.LessOrEqual(t, allocs, tc.maxAlloc,
				"每会话 7 allocs + 池/槽位切片 2 是基线形态")
			require.LessOrEqual(t, bytes, tc.maxBytes, "每会话约 420 B")
			require.LessOrEqual(t, ns, tc.maxNs, "锁内 UUID 分配必须留在常数级")
		})
	}
}

// BenchmarkClaimRelease 请求热路径：认领 + 归还必须 0 alloc。
func BenchmarkClaimRelease(b *testing.B) {
	pool := newTestPool(8, testPolicy())
	b.ReportAllocs()
	var miss int
	for i := 0; i < b.N; i++ {
		s := pool.Claim()
		if s == nil {
			miss++
			continue
		}
		s.Release()
	}
	b.StopTimer()
	if miss != 0 {
		b.Fatalf("单 goroutine 独占 claim 失败 %d 次（池被别处占着？）", miss)
	}
	if allocs := testing.AllocsPerRun(1000, func() { pool.Claim().Release() }); allocs != 0 {
		b.Fatalf("claim+release 必须 0 alloc，实测 %v", allocs)
	}
}

// BenchmarkSyncUnchanged 无关重载的池路径：容量/安装/策略未变 ⇒ 原样复用，
// **0 alloc**。退化成 Resize 会变成每 reload 一次 2 alloc（池 + 槽位切片）并丢弃
// 在途 busy 与会话身份——这条断言就是那个回退的捕手。
func BenchmarkSyncUnchanged(b *testing.B) {
	spec := buildSpec(8)
	pool := Resize(nil, spec, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := Sync(pool, spec, nil); got != pool {
			b.Fatalf("未变的池必须原样复用（拿到 %p，原 %p）", got, pool)
		}
	}
	b.StopTimer()
	if allocs := testing.AllocsPerRun(1000, func() { _ = Sync(pool, spec, nil) }); allocs != 0 {
		b.Fatalf("sync 未变必须 0 alloc（退化成 resize 会变 2），实测 %v", allocs)
	}
}

// BenchmarkResizeSameSpec 对照：同样的 K 但走 resize（重建池对象 + 槽位切片、
// 迁移会话指针）——与上面的 0 alloc 形成"复用的价值"读数。
func BenchmarkResizeSameSpec(b *testing.B) {
	spec := buildSpec(8)
	pool := Resize(nil, spec, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Resize(pool, spec, nil)
	}
	b.StopTimer()
	allocs := testing.AllocsPerRun(1000, func() { _ = Resize(pool, spec, nil) })
	b.ReportMetric(allocs, "pool-allocs/op")
	if allocs > 4 {
		b.Fatalf("同参 resize 只应分配池对象 + 槽位切片（≤4），实测 %v", allocs)
	}
}
