// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// build50ToolBody 构造 50-tool 边界体（压测同形态：1 image_generation_tool +
// 49 function，~3.4KB，tool_choice 悬挂）。spec 测试节：压测 A/B 对照基准。
func build50ToolBody() []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","tools":[`)
	b.WriteString(`{"type":"image_generation_tool","namespace":"image_gen","description":"Generate an image"},`)
	for i := 1; i <= 49; i++ {
		fmt.Fprintf(&b, `{"type":"function","name":"f_%02d","parameters":{"type":"object"}}`, i)
		if i < 49 {
			b.WriteByte(',')
		}
	}
	b.WriteString(`],"tool_choice":{"type":"image_generation_tool"}}`)
	return []byte(b.String())
}

// BenchmarkStripImageTools50Tools 50-tool 边界体剥除基准（压测 档 A/B 对照：
// 现状 +166µs / 23 allocs → 目标 ~+20-40µs / ≤4 allocs，实测记录）。
func BenchmarkStripImageTools50Tools(b *testing.B) {
	body := build50ToolBody()
	b.ReportAllocs()
	for b.Loop() {
		out := stripImageTools(body)
		_ = out
	}
}

// BenchmarkStripImageTools2Tools 2-tool 常态基准（压测 2-tool 常态不劣化对照）。
func BenchmarkStripImageTools2Tools(b *testing.B) {
	body := []byte(`{"model":"m","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"image_generation_tool","namespace":"image_gen"}],"tool_choice":{"type":"image_generation_tool"}}`)
	b.ReportAllocs()
	for b.Loop() {
		out := stripImageTools(body)
		_ = out
	}
}
