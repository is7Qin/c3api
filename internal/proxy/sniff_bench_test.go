// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"testing"
)

// 每帧判死嗅探的性能面（relay 热路径，一帧一次）。
//
// 实测（本机 Ryzen 7 7840H / go1.27 / -benchtime 300ms ×3，改造前后同机同 fixture）：
//
//	正常帧（内容帧，不含未转义的 "error" 键）—— 99.9% 的帧：
//	  128B   45.8ns  →  45.7ns     0
//	  4KB    86.4ns  →  85.6ns     0
//	  64KB   739ns   →  736ns      0
//	  1MB    11.3µs  →  11.1µs     0（噪声内）
//	  分配：0 → 0（未命中零分配契约）
//
//	错误承载帧（帧里带未转义的 "error" 键——按定义就是错误帧，冷路径）：
//	  4KB    1.29µs  →  2.03µs     +0.74µs
//	  64KB   19.5µs  →  29.6µs     +10µs
//	  1MB    307µs   →  462µs      +155µs
//
// 结论：常规帧零变化（新针更短，仍是 SIMD memmem，吞吐 ~90GB/s）；多出来的
// 一跳只落在错误帧上——"判死裁决要在 SDK 里再看一眼形态与 status"。真实上游
// 错误体是几百字节量级，绝对增量 <0.1µs。
//
// 首版没有 status 短针时，1MB 内容帧凡正文出现 error 就要付一次完整 gjson 解析
// （实测 ~500µs/帧），已由 codex-sdk 3d884f4 修掉；本文件同时钉住回归（零分配）。
//
// 绝对量级参照：同一帧在 relay 里还要过一次 sserelay.AsPayloadError 与 completed
// 预筛（各一次全帧扫描）外加 client.Write；判死嗅探不是主项。每连接另加两项：
// 首帧预读多一次嗅探（~46ns）、缓冲传输每帧多一次接口转发（1.1ns → 2.7ns）。
func sniffBenchFrames() map[string][]byte {
	out := map[string][]byte{
		"content-100B": []byte(`{"type":"response.output_text.delta","delta":"hello world","item_id":"msg_1","output_index":0}`),
		"fatal-402":    []byte(`{"type":"error","status":402,"error":{"message":"Payment Required","code":"deactivated_workspace"}}`),
	}
	out["content-64KB"] = contentFrame(64<<10, false)
	out["errorword-4KB"] = contentFrame(4<<10, true)
	out["errorword-64KB"] = contentFrame(64<<10, true)
	out["errorword-1MB"] = contentFrame(1<<20, true)
	return out
}

// contentFrame 造一个 text delta 帧；withErrorKey 让帧里带一个**未转义的**
// `"error"` 键（最坏情况：外层短针命中，判死裁决要再看一眼形态 + 找 status）。
//
// 注意不能用正文里的 "error: " 来造：delta 文本里的引号是转义的，字节序列里
// 没有 `"error"`（前后引号），外层预筛根本不会命中——那样测的是无关路径。
func contentFrame(n int, withErrorKey bool) []byte {
	b := make([]byte, 0, n+64)
	if withErrorKey {
		b = append(b, `{"type":"response.output_text.delta","error":{"code":"server_error"},"delta":"`...)
	} else {
		b = append(b, `{"type":"response.output_text.delta","delta":"`...)
	}
	for len(b) < n {
		b = append(b, "lorem ipsum dolor sit amet consectetur adipiscing elit "...)
	}
	return append(b, `"}`...)
}

func BenchmarkSniffCodexWSDeath(b *testing.B) {
	for name, f := range sniffBenchFrames() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = sniffCodexWSDeath(f)
			}
		})
	}
}

// TestSniffCodexWSDeathZeroAllocOnMiss 未命中零分配（热路径契约；ns 级预算由
// BenchmarkSniffCodexWSDeath 给出，测试只钉可移植的那一半）。
func TestSniffCodexWSDeathZeroAllocOnMiss(t *testing.T) {
	misses := [][]byte{
		contentFrame(100, false),
		contentFrame(64<<10, false),
		contentFrame(4<<10, true), // 正文含 error：形态判定后仍不判死
		[]byte(`{"type":"response.created","response":{"id":"r"}}`),
		nil,
	}
	for _, f := range misses {
		if got := sniffCodexWSDeath(f); got != nil {
			t.Fatalf("%q 不应判死, got %T", f, got)
		}
		allocs := testing.AllocsPerRun(200, func() {
			_ = sniffCodexWSDeath(f)
		})
		if allocs != 0 {
			t.Fatalf("未命中零分配契约破坏（len=%d）: allocs=%.1f", len(f), allocs)
		}
	}
}
