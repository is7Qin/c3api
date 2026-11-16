// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/is7qin/c3api/pkg/sserelay"
)

// 帧路径**成本结构**基线（2026-11-16 建立，T0 落地时定稿）。用途有三个：
//
//  1. 回答"帧处理还能快多少"——先量，再改，不猜；
//
//  2. 给边界重构立"不得新增"的对照基线（T1–T3 每个提交都要跑它，ns/op 与 allocs/op
//     不得劣化）；
//
//  3. 记录**曾经搞错过的判断**：早期只量了三个扫描，得出"大帧成本几乎全在判死嗅探
//     上"，据此差点给判死嗅探加尺寸闸（有语义损失）。补测后结论反过来：
//
//     每帧动作                            4 KiB      64 KiB     1 MiB     谁在调
//     模型改写（改前）                     7293 ns   112061 ns  1726057 ns  ws_relay.go:302
//     模型改写（T0 后，预筛 miss）            90 ns      772 ns    12696 ns  同上
//     completed memmem 预筛（T0 删除）      52.9 ns    703 ns    11598 ns  ws_relay.go:285
//     completed 精确判定（T0 后唯一路径）    22.8 ns     23.4 ns    23.4 ns  同上
//     判死嗅探 sniffCodexWSDeath           88.8 ns    764 ns    12400 ns  ws_relay.go:249
//     载荷预筛 AsPayloadError              93.4 ns      5.7 ns      5.8 ns  ws_relay.go:258
//     单次 memmem 参照                     87.8 ns    766 ns    12100 ns  —
//
// 结构结论（T0 之后）：**帧路径上已没有全帧扫描的赢利点**。模型改写与 completed
// 都是 O(1) 命中路径（预筛 miss / type 首键），判死嗅探是唯一剩下的全帧 memmem，
// 占 1 MiB 帧端到端（loopback 写出 ~100 µs 起）的 ≤12%，公网更低——所以它**不该**
// 加尺寸闸（那会丢掉"超尺寸错误帧零交付换号重试"）。要再快，得从 syscall 与
// 每帧分配下手，不是从扫描器。
func benchFrame(n int) []byte {
	head := `{"type":"response.output_text.delta","delta":"`
	tail := `"}`
	body := strings.Repeat("x", n-len(head)-len(tail))
	return []byte(head + body + tail)
}

func benchSizes() []int { return []int{4 << 10, 64 << 10, 1 << 20} }

func benchSizeName(n int) string {
	switch n {
	case 4 << 10:
		return "4KiB"
	case 64 << 10:
		return "64KiB"
	case 1 << 20:
		return "1MiB"
	}
	return "?"
}

// BenchmarkFrameScanFatal 判死嗅探（帧路径上唯一的全帧 memmem）。
func BenchmarkFrameScanFatal(b *testing.B) {
	for _, n := range benchSizes() {
		f := benchFrame(n)
		b.Run(benchSizeName(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if sniffCodexWSDeath(f) != nil {
					b.Fatal("内容帧不应判死")
				}
			}
		})
	}
}

// BenchmarkFrameScanPayloadError 流内错误载荷预筛（>4 KiB 早退，O(1)）。
func BenchmarkFrameScanPayloadError(b *testing.B) {
	for _, n := range benchSizes() {
		f := benchFrame(n)
		b.Run(benchSizeName(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if sserelay.AsPayloadError(sserelay.Event{Data: f}) != nil {
					b.Fatal("内容帧不应命中")
				}
			}
		})
	}
}

// BenchmarkFrameScanCompleted response.completed 判定：T0 起只有精确判定一条路径
// （`exact`）。`memmem+exact` 是 T0 删掉的组合，保留在这里当**回归对照**——若有人
// 再把子串预筛加回来，这里会立刻显示它贵了 500×。
func BenchmarkFrameScanCompleted(b *testing.B) {
	for _, n := range benchSizes() {
		f := benchFrame(n)
		b.Run(benchSizeName(n)+"/exact", func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if isResponsesCompleted(f) {
					b.Fatal("内容帧不应命中")
				}
			}
		})
		b.Run(benchSizeName(n)+"/memmem+exact", func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if bytes.Contains(f, completedTypeBytes) && isResponsesCompleted(f) {
					b.Fatal("内容帧不应命中")
				}
			}
		})
	}
}

// BenchmarkFrameScanModelRewrite 模型改写：生产路径（`prefilter-miss`，T0 后）与
// 冻结的改前实现（`old-path`）同场对照。隐式映射模式下每帧都走这里。
func BenchmarkFrameScanModelRewrite(b *testing.B) {
	for _, n := range benchSizes() {
		f := benchFrame(n)
		b.Run(benchSizeName(n)+"/prefilter-miss", func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := rewriteResponseModelJSON(f, "mapped-model"); len(got) != len(f) {
					b.Fatal("内容帧不应被改写")
				}
			}
		})
		b.Run(benchSizeName(n)+"/old-path", func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := rewriteResponseModelJSONPreT0(f, "mapped-model"); len(got) != len(f) {
					b.Fatal("内容帧不应被改写")
				}
			}
		})
	}
}

// BenchmarkFrameScanSingleMemmem 单次 memmem 下界（参照系：任何"合并扫描"方案都
// 不可能优于它）。
func BenchmarkFrameScanSingleMemmem(b *testing.B) {
	for _, n := range benchSizes() {
		f := benchFrame(n)
		b.Run(benchSizeName(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if bytes.Contains(f, []byte(`"error"`)) {
					b.Fatal("内容帧不应命中")
				}
			}
		})
	}
}

// TestFrameScanZeroAlloc 帧路径上的每个判定在内容帧上必须零分配（热路径契约；改
// 边界/改扫描实现时最先破的就是这一条）。
func TestFrameScanZeroAlloc(t *testing.T) {
	for _, n := range benchSizes() {
		f := benchFrame(n)
		if got := testing.AllocsPerRun(100, func() { _ = sniffCodexWSDeath(f) }); got != 0 {
			t.Fatalf("%s 判死嗅探分配 %v 次/op，热路径要求 0", benchSizeName(n), got)
		}
		if got := testing.AllocsPerRun(100, func() {
			_ = sserelay.AsPayloadError(sserelay.Event{Data: f})
		}); got != 0 {
			t.Fatalf("%s 载荷预筛分配 %v 次/op，热路径要求 0", benchSizeName(n), got)
		}
		if got := testing.AllocsPerRun(100, func() { _ = isResponsesCompleted(f) }); got != 0 {
			t.Fatalf("%s completed 判定分配 %v 次/op，热路径要求 0", benchSizeName(n), got)
		}
		if got := testing.AllocsPerRun(100, func() {
			modelRewriteSink = rewriteResponseModelJSON(f, "mapped-model")
		}); got != 0 {
			t.Fatalf("%s 模型改写分配 %v 次/op，热路径要求 0", benchSizeName(n), got)
		}
	}
}
