// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 图片用量提取纯函数单测（spec §4.1）：真实上游 JSON 形态构造（评审 I-1：
// 用真实上游 JSON 构造单测，不用结构体 marshal 自证）。用例覆盖——
// 非流式：data 数组长度计数 + usage 缺失 → 0（per-image 按张数照算）；
// 流式：completed 事件计数 + partial_image 不计数 + usage 仅末事件。

// —— 非流式（images/generations 响应：data 数组 + usage 可选） ——

func TestImageUsageFromResponse(t *testing.T) {
	// gpt-image-2 官方形态：n=2 → data 2 元素；usage 带 image_tokens 分量
	// （input 8e-06、output 3e-05 价模型下 tokens 为 image token 口径）。
	resp := []byte(`{"created":1589478378,"data":[{"b64_json":"aaa"},{"b64_json":"bbb"}],
"usage":{"input_tokens":100,"input_tokens_details":{"image_tokens":5000},"output_tokens":100,
"output_tokens_details":{"image_tokens":20000},"total_tokens":200}}`)
	it, ot, cnt := ImageUsageFromResponse(resp)
	require.Equal(t, int64(5000), it, "input_tokens_details.image_tokens 直读")
	require.Equal(t, int64(20000), ot, "output_tokens_details.image_tokens 直读")
	require.Equal(t, int64(2), cnt, "data 数组长度 = 张数")

	// usage 缺失（"For gpt-image-1 only"）→ image tokens 0，per-image 按张数照算
	noUsage := []byte(`{"created":1589478378,"data":[{"b64_json":"a"},{"b64_json":"b"},{"b64_json":"c"}]}`)
	it, ot, cnt = ImageUsageFromResponse(noUsage)
	require.Zero(t, it, "usage 缺失 → image input tokens 0")
	require.Zero(t, ot, "usage 缺失 → image output tokens 0")
	require.Equal(t, int64(3), cnt, "usage 缺失不影响张数计数")

	// data 空数组 → 0 张
	empty := []byte(`{"created":1,"data":[]}`)
	_, _, cnt = ImageUsageFromResponse(empty)
	require.Zero(t, cnt, "空 data 数组 → 0 张")

	// 畸形/非 JSON → 全 0（不 panic 不阻塞采集）
	it, ot, cnt = ImageUsageFromResponse([]byte(`not-json`))
	require.Zero(t, it)
	require.Zero(t, ot)
	require.Zero(t, cnt)
	it, ot, cnt = ImageUsageFromResponse(nil)
	require.Zero(t, it)
	require.Zero(t, ot)
	require.Zero(t, cnt)

	// url 形态 data（b64_json 与 url 共存不影响计数）
	urlForm := []byte(`{"data":[{"url":"https://x/y.png"},{"url":"https://x/z.png"},{"url":"https://x/w.png"},{"url":"https://x/v.png"}]}`)
	_, _, cnt = ImageUsageFromResponse(urlForm)
	require.Equal(t, int64(4), cnt, "url 形态同样按 data 长度计数")
}

// —— 流式（SSE：type 事件 + completed 携带 usage） ——

func TestImageStreamEvent(t *testing.T) {
	// image_generation.completed：每完成一张一个事件，携带 usage（image_tokens）
	completed := []byte(`{"type":"image_generation.completed","usage":{"input_tokens":100,
"input_tokens_details":{"image_tokens":2500},"output_tokens":100,
"output_tokens_details":{"image_tokens":10000},"total_tokens":200}}`)
	ok, it, ot := ImageStreamEvent(completed)
	require.True(t, ok, "image_generation.completed 为计费事件")
	require.Equal(t, int64(2500), it)
	require.Equal(t, int64(10000), ot)

	// image_edit.completed 同语义（edits 端点）
	edit := []byte(`{"type":"image_edit.completed","usage":{"input_tokens_details":{"image_tokens":1},"output_tokens_details":{"image_tokens":2}}}`)
	ok, it, ot = ImageStreamEvent(edit)
	require.True(t, ok, "image_edit.completed 为计费事件")
	require.Equal(t, int64(1), it)
	require.Equal(t, int64(2), ot)

	// completed 事件无 usage → 计数照常，tokens 0（usage 可选语义）
	noUsage := []byte(`{"type":"image_generation.completed"}`)
	ok, it, ot = ImageStreamEvent(noUsage)
	require.True(t, ok, "无 usage 的 completed 仍计费（per-image 按张数照算）")
	require.Zero(t, it)
	require.Zero(t, ot)

	// partial_image：不计费不计数
	partial := []byte(`{"type":"partial_image","data":{"b64_json":"xx"}}`)
	ok, it, ot = ImageStreamEvent(partial)
	require.False(t, ok, "partial_image 不计费不计数")
	require.Zero(t, it)
	require.Zero(t, ot)

	// 其余事件（progress/无 type/SSE 结束哨兵）→ 不计费
	for _, ev := range []string{
		`{"type":"image_generation.progress","progress":{"image_index":0,"status":"in_progress"}}`,
		`{"data":{"b64_json":"x"}}`, // 无 type
		`[DONE]`,
	} {
		ok, it, ot = ImageStreamEvent([]byte(ev))
		require.False(t, ok, "非 completed 事件不计费: %s", ev)
		require.Zero(t, it)
		require.Zero(t, ot)
	}

	// 畸形输入 → 不 panic
	ok, it, ot = ImageStreamEvent([]byte(`garbage`))
	require.False(t, ok)
	require.Zero(t, it)
	require.Zero(t, ot)
}

// ——流终计费调用方语义：completed 累加张数 + usage 仅末事件——

func TestImageStreamBillingAccumulation(t *testing.T) {
	// 模拟路由面逐事件处理：3 个 completed + 2 个 partial_image 的事件流，
	// 流终按已收集张数落账——count = 3，usage = 末次 completed 的 tokens
	// （每完成一张一个事件；partial 不计数；中途失败同样按已收集计费）。
	events := [][]byte{
		[]byte(`{"type":"partial_image","data":{"b64_json":"p1"}}`),
		[]byte(`{"type":"image_generation.completed","usage":{"input_tokens_details":{"image_tokens":1000},"output_tokens_details":{"image_tokens":4000}}}`),
		[]byte(`{"type":"partial_image","data":{"b64_json":"p2"}}`),
		[]byte(`{"type":"image_generation.completed","usage":{"input_tokens_details":{"image_tokens":2000},"output_tokens_details":{"image_tokens":8000}}}`),
		[]byte(`{"type":"image_edit.completed","usage":{"input_tokens_details":{"image_tokens":3000},"output_tokens_details":{"image_tokens":12000}}}`),
	}
	var count, lastIt, lastOt int64
	for _, ev := range events {
		if ok, it, ot := ImageStreamEvent(ev); ok {
			count++
			lastIt, lastOt = it, ot
		}
	}
	require.Equal(t, int64(3), count, "completed 事件逐个计数（partial 不计）")
	require.Equal(t, int64(3000), lastIt, "usage 仅取末次 completed 事件")
	require.Equal(t, int64(12000), lastOt)

	// 空流（全 partial / 无事件）→ 0 张 0 usage
	var c2, i2, o2 int64
	for _, ev := range [][]byte{[]byte(`{"type":"partial_image"}`), []byte(`{"type":"image_generation.progress"}`)} {
		if ok, it, ot := ImageStreamEvent(ev); ok {
			c2, i2, o2 = c2+1, i2+it, o2+ot
		}
	}
	require.Zero(t, c2)
	require.Zero(t, i2)
	require.Zero(t, o2)
}

// —— 热路径零分配（性能纪律）：两函数恒零分配 ——

func TestImageUsageZeroAlloc(t *testing.T) {
	resp := []byte(`{"created":1,"data":[{"b64_json":"a"}],"usage":{"input_tokens_details":{"image_tokens":5},"output_tokens_details":{"image_tokens":9}}}`)
	event := []byte(`{"type":"image_generation.completed","usage":{"input_tokens_details":{"image_tokens":5},"output_tokens_details":{"image_tokens":9}}}`)
	// 预热（gjson 惰性无全局缓存；预热排除首次运行的 Go 运行时噪音）
	ImageUsageFromResponse(resp)
	ImageStreamEvent(event)
	require.Zero(t, testing.AllocsPerRun(100, func() {
		ImageUsageFromResponse(resp)
	}), "非流式提取零分配（gjson.GetBytes + data.# 数组长度）")
	require.Zero(t, testing.AllocsPerRun(100, func() {
		ImageStreamEvent(event)
	}), "流式事件判定零分配（type 子串比较 + Int 提取）")
}
