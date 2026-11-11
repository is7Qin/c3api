// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodexTerminalBlockFieldWhitespace A4：终态检测与 probe.validateCodexSSE
// 必须采用同一字段解析规则——行首空白的 ` data:` 也参与 data 合并，不能被
// 一边忽略、另一边当内容。
func TestCodexTerminalBlockFieldWhitespace(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"output":[{"type":"message"}]}}`

	// 行首空白的后一行 data 污染多行事件：合并后 JSON 非法 → 不是终态。
	block := []byte("event: response.completed\ndata: " + completed + "\n data: garbage\n\n")
	require.False(t, codexTerminalBlock(block), "行首空白的 data 行必须参与合并，污染后不是终态")

	// 行首空白本身不影响字段识别：前置空白的 event/data 仍可成功。
	block = []byte(" event: response.completed\n data: " + completed + "\n\n")
	require.True(t, codexTerminalBlock(block), "行首空白不影响 event/data 字段识别")

	// 事件名失败优先于 data.type 的成功信号：error / response.failed 事件必须
	// 立即停止读取（具体成功/失败分类由 probe.validateCodexSSE 做）。
	require.True(t, codexTerminalBlock([]byte("event: error\ndata: "+completed+"\n\n")))
	require.True(t, codexTerminalBlock([]byte("event: response.failed\ndata: {\"type\":\"foo\"}\n\n")))
}
