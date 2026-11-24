// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import "testing"

// BenchmarkFactsDigest 测**最坏合法形状**（前像 285B，见 factsDigestWorstLegal）的
// 编码成本。硬判据是 `0 allocs/op ∧ 0 B/op`——编码走栈上 inline 缓冲 +
// sha256.Sum256，不进堆。
//
// 为什么必须用最坏**合法**形状而不是随机小输入：小输入下任何实现都零分配，基准会
// 变成"我写了个函数"的证明。这一形状的每一维都取真实字段长度上界（含 128 字符
// 上游键），它零分配，比它更小的形状自然零分配。
func BenchmarkFactsDigest(b *testing.B) {
	f, upstream, pat := factsDigestWorstLegal()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		factsDigestSink = FactsDigest(f, upstream, pat)
	}
}

// BenchmarkFactsDigestOverflow 测跨过 512B 上界的形状（前像 547B，见
// factsDigestOverflowShape）：**恰好一次 make**，ns 不看齐栈上分支。
//
// 硬判据只有 ≤1 alloc；ns 上界（600）只为防退化。v3 订正：inlineCap=512 下 534B
// 输入实测 465–561 ns/op、1134B 为 984–1009 ns/op ⇒ 把上界压到 250 是不可达的。
func BenchmarkFactsDigestOverflow(b *testing.B) {
	f, upstream, pat := factsDigestOverflowShape()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		factsDigestSink = FactsDigest(f, upstream, pat)
	}
}
