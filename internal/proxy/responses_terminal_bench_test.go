// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"testing"

	"github.com/is7qin/c3api/pkg/sserelay"
)

// BenchmarkIsResponsesTerminal 终态嗅探的回归锁（spec §5.5，不进 CI）。
//
// 判据从 json.Valid（O(帧体)）换成 ev.Raw 的空行后缀（O(1)）后，成本不再随帧
// 大小走：改动前 1 MB 终态帧 ~0.97 ms/op，改动后 1 MB 与 4 KB 同为 5 ns 量级。
// 回归锁靠 ns/op（三个数量级差），不是 allocs/op——go1.27 的 json.Valid 在 1 MB
// 载荷上同样 0 allocs/op，零分配断言对整帧校验没有鉴别力；语义守卫在
// TestIsResponsesTerminal 的判据边界用例（残帧 / CRLF / 两来源）。
func BenchmarkIsResponsesTerminal(b *testing.B) {
	cases := []struct {
		name string
		ev   sserelay.Event
	}{
		// 常规帧：event 名不是终态 ⇒ 早退，不碰 Raw。
		{"delta", frameEvent("response.output_text.delta", respTermDelta, tailComplete)},
		{"terminal-4KiB", frameEvent("response.completed", terminalPayload(4<<10), tailComplete)},
		{"terminal-1MiB", frameEvent("response.completed", terminalPayload(1<<20), tailComplete)},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			// 不报 SetBytes：O(1) 判据上它会显示成 1.9e8 MB/s 之类的假吞吐
			// （除数是被跳过的帧体大小），只留 ns/op 与 allocs/op 两个真指标。
			for b.Loop() {
				_ = isResponsesTerminal(c.ev)
			}
		})
	}
}
