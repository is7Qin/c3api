// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestDropsAreVisible spec (p)：丢弃必须可见。
//
// 计数器是包级全局 ⇒ 本测试**必须顶层串行**（禁 t.Parallel），并用 t.Cleanup
// 复位；删掉 noteDrop 调用、或把某类块改成"静默丢"都会让这里红。
func TestDropsAreVisible(t *testing.T) {
	ResetDropCounts()
	t.Cleanup(ResetDropCounts)

	body := `{"model":"m","messages":[
		{"role":"user","content":[
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"UERG"}},
			{"type":"search_result","content":"SR1"},
			{"type":"image","source":{"type":"file","file_id":"f_1"}}
		]},
		{"role":"assistant","content":[
			{"type":"redacted_thinking","data":"RED"},
			{"type":"unknown_block","text":"ZZZ"}
		]}
	]}`
	out, err := ConvertRequest([]byte(body), domain.ProtocolConvertMessToChat)
	require.NoError(t, err)
	// 被丢的内容一个字节都不许出现在产物里（I1：不发明、也不偷渡）。
	for _, gone := range []string{"UERG", "SR1", "f_1", "RED", "ZZZ"} {
		require.NotContains(t, string(out), gone, "被丢弃的内容不得出现在目标报文里：%q", gone)
	}

	got := DropCounts()
	for key, want := range map[string]int64{
		"mess_to_chat|document":          1,
		"mess_to_chat|block_unknown":     2, // search_result 与 unknown_block 都落到 block_unknown
		"mess_to_chat|image_file":        1,
		"mess_to_chat|redacted_thinking": 1,
	} {
		require.EqualValues(t, want, got[key], "丢弃计数 %s 不符，实际 %v", key, got)
	}

	// sink 与计数器同源：装一个出口应能收到同样的事件。
	var seen []string
	SetDropSink(func(_ domain.ProtocolConvert, field string) { seen = append(seen, field) })
	t.Cleanup(func() { SetDropSink(nil) })
	_, err = ConvertRequest([]byte(`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"R2"}]}]}`), domain.ProtocolConvertMessToChat)
	require.NoError(t, err)
	require.Equal(t, []string{"redacted_thinking"}, seen, "sink 必须收到丢弃事件")
	require.EqualValues(t, 2, DropCounts()["mess_to_chat|redacted_thinking"], "sink 安装不得改变计数")
}
