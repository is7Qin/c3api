// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import "testing"

// BenchmarkPayloadDigest 是载荷摘要**编码本身**的成本：它回答"构造期每叶多花多少钱"
// （newSnapshotStatic 每叶调用一次，形状由 payload_digest_test.go 的
// payloadDigestRealShape / payloadDigestOverflowShape 提供，与分配断言同一份输入）。
//
// 三条子基准对应三个真实形状：
//   - real-shape：token 2048B / refresh 256B ⇒ 前像 2336B，落在 4096 的栈上缓冲内
//     （0 alloc，判据见 TestPayloadDigestAllocationShapes）；
//   - overflow-shape：token 8192B ⇒ 前像 8480B，跨过上界 ⇒ 恰好一次 make；
//   - empty：非 codex 账号（ext 三元组全空）⇒ 前像 32B，构造期无条件付的那一份。
func BenchmarkPayloadDigest(b *testing.B) {
	b.Run("real-shape-2336B", func(b *testing.B) {
		token, refresh, expires := payloadDigestRealShape()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			payloadDigestSink = payloadDigestOf(token, refresh, expires)
		}
	})
	b.Run("overflow-shape-8480B", func(b *testing.B) {
		token, refresh, expires := payloadDigestOverflowShape()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			payloadDigestSink = payloadDigestOf(token, refresh, expires)
		}
	})
	b.Run("empty-32B", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			payloadDigestSink = payloadDigestOf("", "", 0)
		}
	})
}
