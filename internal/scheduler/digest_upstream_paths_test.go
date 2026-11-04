// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDigestUpstreamPaths 摘要必须无歧义：长度前缀编码让 {"ab":"c"} 与
// {"a":"bc"} 不同（裸拼接/分隔符拼接会撞车 → 改路径后旧计划继续生效）。
func TestDigestUpstreamPaths(t *testing.T) {
	require.Equal(t, digestUpstreamPaths(nil), digestUpstreamPaths(map[string]string{}), "nil 与空等价")

	require.NotEqual(t, digestUpstreamPaths(map[string]string{"ab": "c"}), digestUpstreamPaths(map[string]string{"a": "bc"}),
		"键值边界不可省（拼接歧义）")
	require.NotEqual(t, digestUpstreamPaths(map[string]string{"a": "b"}), digestUpstreamPaths(map[string]string{"a": "c"}),
		"值变化必须改摘要")
	require.NotEqual(t, digestUpstreamPaths(map[string]string{"a": "b"}), digestUpstreamPaths(map[string]string{"d": "b"}),
		"键变化必须改摘要")
	require.Equal(t,
		digestUpstreamPaths(map[string]string{"a": "b", "c": "d"}),
		digestUpstreamPaths(map[string]string{"c": "d", "a": "b"}),
		"map 遍历序无关")
	require.NotEqual(t, digestUpstreamPaths(map[string]string{"a": "b"}), digestUpstreamPaths(nil), "配置与未配置必须不同")
}
