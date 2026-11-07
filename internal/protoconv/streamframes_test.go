// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 流式 golden 的**硬件不变量**（spec §8(b)）：帧数写死、首末帧类型固定、
// 终止帧恰好一次、每帧都带 `data: ` 前缀。
//
// 与 `TestGoldenOutputs` 互补：逐字节断言只证明"字节没变"，这里证明"结构性
// 契约仍然成立"——把 `[DONE]` 收尾删掉、把终止帧发两遍、或漏掉 message_start，
// 逐字节更新 golden 后可以照样绿，但这里会红。

func TestStreamGoldenFrameInvariants(t *testing.T) {
	// 帧数**写死**（spec §8(b)：N 不用变量派生）。改实现导致帧数变化时必须
	// 同时改这里，且要能说出为什么变。
	want := map[string]struct {
		frames int
		first  string // 首帧必须含的字面量
		last   string // 末帧必须含的字面量
	}{
		"chat_to_mess_stream_text":  {3, `"type":"message_start"`, `"type":"message_stop"`},
		"chat_to_mess_stream_tools": {4, `"type":"message_start"`, `"type":"message_stop"`},
		"chat_to_mess_stream_eof":   {2, `"type":"message_start"`, `"type":"message_stop"`},
		"chat_to_resp_stream_text":  {3, `"type":"response.created"`, `"type":"response.completed"`},
		"chat_to_resp_stream_tools": {4, `"type":"response.created"`, `"type":"response.completed"`},
		"chat_to_resp_stream_length": {2, `"type":"response.created"`,
			`"status":"incomplete"`},
	}

	live := goldenCases()
	seen := map[string]bool{}
	for _, c := range live {
		if !c.stream {
			continue
		}
		w, ok := want[c.name]
		if !ok {
			continue // 其它方向的流式用例不归本门禁管
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			got := c.run(t)
			require.NotEmpty(t, got, "帧序列不得为空")

			frames := strings.Split(got, "\n---\n")
			require.Len(t, frames, w.frames, "帧数必须与写死的值一致（%s）", c.name)
			for i, fr := range frames {
				require.Contains(t, fr, "data: ", "第 %d 帧缺少 data: 前缀", i)
				// 每帧的每个 data: 载荷都必须是合法 JSON（`[DONE]` 除外）。
				for _, line := range strings.Split(fr, "\n") {
					payload, ok := strings.CutPrefix(line, "data: ")
					if !ok || payload == "[DONE]" {
						continue
					}
					require.True(t, gjson.Valid(payload), "第 %d 帧的载荷不是合法 JSON：%s", i, payload)
				}
			}
			require.Contains(t, frames[0], w.first, "首帧类型不对")
			require.Contains(t, frames[len(frames)-1], w.last, "末帧类型不对")

			// 终止帧恰好一次（双终止是历史 bug 的回归点）。
			var n int
			for _, fr := range frames {
				if strings.Contains(fr, w.last) {
					n++
				}
			}
			require.Equal(t, 1, n, "终止标记必须恰好出现一次")
		})
	}
	require.Equal(t, len(want), len(seen), "写死的用例名必须都能在 goldenCases() 里找到")
}
