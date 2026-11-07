// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestDropsAreVisible spec (p)：丢弃必须可见。
//
// 计数器是包级全局 ⇒ 本测试**必须顶层串行**（禁 t.Parallel），并用 t.Cleanup
// 复位；删掉 noteReqDrop/noteRespDrop 调用、或把某类块改成"静默丢"都会让这里红。
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
		"mess_to_chat|req|document":          1,
		"mess_to_chat|req|block_unknown":     2, // search_result 与 unknown_block 都落到 block_unknown
		"mess_to_chat|req|image_file":        1,
		"mess_to_chat|req|redacted_thinking": 1,
	} {
		require.EqualValues(t, want, got[key], "丢弃计数 %s 不符，实际 %v", key, got)
	}

	// sink 与计数器同源：装一个出口应能收到同样的事件。
	var seen []string
	SetDropSink(func(e DropEvent) { seen = append(seen, e.Face.String()+":"+e.Field) })
	t.Cleanup(func() { SetDropSink(nil) })
	_, err = ConvertRequest([]byte(`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"R2"}]}]}`), domain.ProtocolConvertMessToChat)
	require.NoError(t, err)
	require.Equal(t, []string{"req:redacted_thinking"}, seen, "sink 必须收到丢弃事件（带面）")
	require.EqualValues(t, 2, DropCounts()["mess_to_chat|req|redacted_thinking"], "sink 安装不得改变计数")
}

// TestNoteDropConcurrentFirstHit 并发首次命中同一个新键（I3）：计数器的创建必须
// 走 LoadOrStore。旧写法（Load miss → new → Store）在两个协程同时首次命中时会
// 互相覆盖，各自从 1 起算 ⇒ 最终计数小于实际次数。
func TestNoteDropConcurrentFirstHit(t *testing.T) {
	ResetDropCounts()
	t.Cleanup(ResetDropCounts)

	const n = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			<-start // 一起放行：把"同时首次命中"的窗口开到最大
			noteReqDrop(domain.ProtocolConvertMessToChat, "concurrent_probe")
		}()
	}
	close(start)
	wg.Wait()

	require.EqualValues(t, n, DropCounts()["mess_to_chat|req|concurrent_probe"],
		"并发首次命中同一个键不得丢计数")
}

// TestRespChatInputFileDropCounted resp 方向 input_file 的独占夹具（C12）：丢弃计数
// 精确 +1，且被丢字段的字节一个都不许出现在产物里。
func TestRespChatInputFileDropCounted(t *testing.T) {
	ResetDropCounts()
	t.Cleanup(ResetDropCounts)

	out, err := ConvertRequest([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"look"},
		{"type":"input_file","file_id":"file_SECRET","filename":"secret.pdf"}
	]}]}`), domain.ProtocolConvertRespToChat)
	require.NoError(t, err)
	require.EqualValues(t, 1, DropCounts()["resp_to_chat|req|input_file"],
		"resp 方向的 input_file 丢弃必须计数（chat 无文件载体）")
	for _, gone := range []string{"file_SECRET", "secret.pdf", "input_file"} {
		require.NotContains(t, string(out), gone, "被丢弃的文件字段不得出现在产物里：%q", gone)
	}
	require.Contains(t, string(out), `"text":"look"`, "同一条消息里的文本必须保留")
}
