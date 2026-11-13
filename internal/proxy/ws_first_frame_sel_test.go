// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/scheduler"
)

// TestWSFirstFrameForSel 首帧模型改写在调用方按当次 sel 预处理（relayWS 原样写出）：
// 映射不同 → 改写 model；未映射/同名 → 返回原切片（零拷贝）。
func TestWSFirstFrameForSel(t *testing.T) {
	first := []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)

	got := wsFirstFrameForSel(first, &scheduler.Selection{Model: "gpt-5.6"}, "gpt-4o")
	require.Equal(t, `{"type":"response.create","model":"gpt-5.6","input":"hi"}`, string(got))
	require.Equal(t, `{"type":"response.create","model":"gpt-4o","input":"hi"}`, string(first), "不改写入参")

	same := wsFirstFrameForSel(first, &scheduler.Selection{Model: "gpt-4o"}, "gpt-4o")
	require.Same(t, &first[0], &same[0], "同名不改写：原切片")
	none := wsFirstFrameForSel(first, &scheduler.Selection{}, "gpt-4o")
	require.Same(t, &first[0], &none[0], "未映射不改写：原切片")
}
