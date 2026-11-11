//go:build perf

package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 性能门禁（ns/op）。为什么是构建标签而不是普通测试：
//
//   - 这个门禁用 testing.Benchmark 驱动真实计时，而 Go 不允许在 benchmark 循环内
//     再开 benchmark（实测：testing.(*B).runN 卡在全局锁上死锁），所以它只能是 Test；
//   - 而 ns/op 只在空闲机器上有意义，与其余包并行跑会读到 2.7× 的噪声（同一二进制
//     单跑 0.9–1.0×），因此它不应该进入默认测试那一遍；
//   - 所以用 `//go:build perf` 把它从 `go test ./...` 里摘出去——不需要 -skip、也不需要
//     “跳过再串行补跑”这种脚本约定。脚本与 CI 用：
//
//     go test -tags perf -p 1 -run '^TestPerfGateToolSchemaHotPath$' ./internal/protoconv
//
// 分配（allocs/op）与机器负载无关，留在默认测试 TestToolSchemaAllocsGate 里跑。

// bestOf 跑 n 次 bench 取最优（并发测试环境下单次读数受调度噪声影响大）。
func bestOf(n int, f func() testing.BenchmarkResult) testing.BenchmarkResult {
	best := f()
	for i := 1; i < n; i++ {
		cur := f()
		if cur.NsPerOp() < best.NsPerOp() {
			best = cur
		}
	}
	return best
}

// TestPerfGateToolSchemaHotPath 真跑 bench 并与 M 记录表比对 ns/op（(g)：数值断言，
// 不是文件子串）。**只断言时间**：分配在 TestToolSchemaAllocsGate。
func TestPerfGateToolSchemaHotPath(t *testing.T) {
	targets := loadBenchTargets(t)

	// Scan 必须跑的是生产热路径 pInFast；亚微秒基准的相对噪声远大于微秒级，
	// 所以这里给 3x（其余 2.5x，见 spec L25）。
	scan := bestOf(3, func() testing.BenchmarkResult { return testing.Benchmark(BenchmarkMessSchemaScan) })
	if !raceEnabled {
		require.LessOrEqual(t, scan.NsPerOp(), targets["BenchmarkMessSchemaScan"].ns*3,
			"BenchmarkMessSchemaScan 的 ns/op 相对 M 记录超限（%d ns）", scan.NsPerOp())
	}

	check := func(name string, r testing.BenchmarkResult) {
		tgt, ok := targets[name]
		require.True(t, ok, "bench_targets.tsv 缺 %s", name)
		if raceEnabled {
			return // -race 下 ns 放大数倍，不可比（L25）
		}
		// 相对阈值：同机允许抖动与并发干扰（3 次取最优），但数量级变差必须红。
		require.LessOrEqual(t, r.NsPerOp(), tgt.ns*5/2, "%s 的 ns/op 相对 M 记录超限（%d ns）", name, r.NsPerOp())
	}
	if testing.Short() {
		return
	}
	check("BenchmarkMessSchemaFlatten", bestOf(3, func() testing.BenchmarkResult {
		return testing.Benchmark(BenchmarkMessSchemaFlatten)
	}))
	check("BenchmarkConvertRequestRespToMessTools20", bestOf(3, func() testing.BenchmarkResult {
		return testing.Benchmark(BenchmarkConvertRequestRespToMessTools20)
	}))
	// spec §8(o)①：两个新方向的 benchmark 必须真的被门禁跑到，否则
	// bench_targets.tsv 里那两行是死数据。
	check("BenchmarkConvertRequestMessToChatTools20", bestOf(3, func() testing.BenchmarkResult {
		return testing.Benchmark(BenchmarkConvertRequestMessToChatTools20)
	}))
	check("BenchmarkConvertRequestRespToChatTools20", bestOf(3, func() testing.BenchmarkResult {
		return testing.Benchmark(BenchmarkConvertRequestRespToChatTools20)
	}))
}
