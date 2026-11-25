// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egressapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkNew 钉 New 的分配预算：它是热路径上**唯一**的规格构造点（每次调度派生
// 出口规格都会走它），allocs/op 只允许 0——Spec 是 [5]string + int 的值类型，
// 任何一次堆分配都说明有人在里面偷偷建了 map/切片/错误。
func BenchmarkNew(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		spec := New("socks5", "[::1]", 1080, "u", "p")
		if spec.Host != "::1" || spec.Scheme != "socks5" {
			b.Fatal("normalization lost")
		}
	}
}

// TestNewNormalizes：New 是 Spec 字段的**唯一**规范化入口（scheme 小写、host 去
// IPv6 方括号），任何"自己拼 Spec"的调用点都会产出不同摘要 ⇒ 静态键抖动。
func TestNewNormalizes(t *testing.T) {
	spec := New("SOCKS5", "[::1]", 1080, "u", "p")
	require.Equal(t, "socks5", spec.Scheme)
	require.Equal(t, "::1", spec.Host, "方括号必须去掉：裸地址才是规范形态")
	require.Equal(t, 1080, spec.Port)
	require.Equal(t, "u", spec.Username)
	require.Equal(t, "p", spec.Password)
	require.Equal(t, "[::1]:1080", spec.Addr(), "Addr 负责把方括号补回来")

	bare := New("SOCKS5", "::1", 1080, "u", "p")
	require.Equal(t, bare.Key(), spec.Key(), "[::1] 与 ::1 是同一条线")
}
