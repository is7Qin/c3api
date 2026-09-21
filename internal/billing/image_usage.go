// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"bytes"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// 图片用量提取纯函数（spec §4.1 官方文档实证）：images 端点响应（非流式
// data 数组 + 流式 completed 事件）→ image 分量计数，供路由面（直连 /
//  codex 路径）接入后走 ImageCost 计费 + UsageLog 图片列落账。
// 零分配（热路径）：gjson.GetBytes 输入字节直读（unsafe 无拷贝）、`data.#`
// 数组长度不物化数组、type 判定为输入字节子串比较（见 eventTypeIs——gjson
// 对字符串型结果会物化 Str 分配 32B，故 type 走字节扫描）。负/异常输入恒
// 返回零值（不 panic 不阻塞采集，对齐 chat 提取路径的缺失→0 语义）。

// ImageUsageFromResponse 非流式 images 响应用量提取：imageCount = 响应
// data 数组长度（每张图一个元素——落账 call_count）；image_input/output_tokens
// = usage.input/output_tokens_details.image_tokens（**usage 可选**——"For
// gpt-image-1 only"：缺失 → 0，per-image 按张数照算）。
func ImageUsageFromResponse(data []byte) (imageInputTokens, imageOutputTokens, imageCount int64) {
	return gjson.GetBytes(data, "usage.input_tokens_details.image_tokens").Int(),
		gjson.GetBytes(data, "usage.output_tokens_details.image_tokens").Int(),
		gjson.GetBytes(data, "data.#").Int()
}

// eventTypePrefix `{"type":"` 帧首顶层锚定（上游 SSE data 帧恒为该形态开头；
// type 值恒为无转义 ASCII——值区间直接字节比较）。锚定后嵌套 `"type":"` 先
// 出现的帧不误判——接线后本函数为每帧计费热路径，宁漏勿错。
const eventTypePrefix = `{"type":"`

// eventTypeIs 零分配判定 data 的 type 字段值 == want：帧首 `{"type":"` 锚定 +
// 值字节逐个比较 + 闭引号边界。want 为编译期常量（循环逐字节索引无拷贝）。
// 非首键/转义/空白变体不匹配 → 不计费（防御方向 = 不误计，宁漏勿错——上游
// 事件机器生成恒该形态）。gjson 对字符串型结果会物化 Str（实测 32B/次），
// 热路径零分配红线故不用 gjson 取 type。
func eventTypeIs(data []byte, want string) bool {
	if !bytes.HasPrefix(data, []byte(eventTypePrefix)) {
		return false
	}
	v := len(eventTypePrefix)
	if v+len(want) >= len(data) || data[v+len(want)] != '"' {
		return false
	}
	for j := 0; j < len(want); j++ {
		if data[v+j] != want[j] {
			return false
		}
	}
	return true
}

// ImageStreamEvent 流式（SSE）images 事件判定：type ∈ {image_generation.
// completed, image_edit.completed}（domain 类型化常量——wire 事件名收敛，
// → completed=true（每完成一张一个事件），并返回该事件携带的
// usage image tokens（input/output_tokens_details.image_tokens；事件无
// usage → 0）；其余事件（partial_image 等）→ completed=false 不计费不计数。
// 流终计费：调用方按 completed 累加张数（落账 call_count），usage 取**末次**
// completed 事件的 tokens；流中途失败/ErrAbort → 已收集 usage 照常计费。
func ImageStreamEvent(data []byte) (completed bool, imageInputTokens, imageOutputTokens int64) {
	if !(eventTypeIs(data, string(domain.ImageStreamEventCompleted)) || eventTypeIs(data, string(domain.ImageStreamEventEditCompleted))) {
		return false, 0, 0
	}
	return true,
		gjson.GetBytes(data, "usage.input_tokens_details.image_tokens").Int(),
		gjson.GetBytes(data, "usage.output_tokens_details.image_tokens").Int()
}
