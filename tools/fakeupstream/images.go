// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// imagesOpts 封装 imagesHandler 的注入 key 集与逐帧延迟（>3 参数归组）。
type imagesOpts struct {
	latency time.Duration
	f429    map[string]bool
	f500    map[string]bool
	f400    map[string]bool
}

// imageCompletedFrame 网关兼容的 Images SSE completed 帧：type 必须为首键
// （internal/billing.eventTypeIs 锚定 `{"type":"` 前缀判定，键序错 = 计 0 张）；
// 每张图一帧、末帧带 usage（网关按末次 completed 提取 image tokens）。
type imageCompletedFrame struct {
	Type  string           `json:"type"`
	Data  []map[string]any `json:"data"`
	Usage *imageFrameUsage `json:"usage,omitempty"`
}

type imageFrameUsage struct {
	InputTokens        int              `json:"input_tokens"`
	OutputTokens       int              `json:"output_tokens"`
	InputTokensDetails imageTokenDetail `json:"input_tokens_details"`
	OutputTokenDetails imageTokenDetail `json:"output_tokens_details"`
}

type imageTokenDetail struct {
	ImageTokens int `json:"image_tokens"`
}

// imagesHandler openai images generations：非流式回 JSON（data 数组按请求 n
// 回显，网关按长度数图计费）；stream:true 回网关兼容 Images SSE——n 个
// image_generation.completed data 帧（末帧 usage）+ 终帧 [DONE]，逐帧按 latency
// 限速。400/429/500 注入判定先于 stream 分支（注入对两形态同样生效）。
func imagesHandler(w http.ResponseWriter, r *http.Request, o imagesOpts) {
	if code := failIfInjected(w, r, o.f429, o.f500, o.f400); code != 0 {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		audit.add(r, nil, 400)
		w.WriteHeader(400)
		return
	}
	audit.add(r, body, 200)
	n := 1
	if v, ok := body["n"].(float64); ok && v >= 1 && v <= 10 {
		n = int(v)
	}
	stream, _ := body["stream"].(bool)
	if !stream {
		data := make([]map[string]any, n)
		for i := range data {
			data[i] = map[string]any{"url": fmt.Sprintf("https://fake.invalid/img-%d.png", i)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": time.Now().Unix(),
			"data":    data,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	for i := 0; i < n; i++ {
		frame := imageCompletedFrame{
			Type: "image_generation.completed",
			Data: []map[string]any{{"b64_json": fmt.Sprintf("QUJD%03d", i)}},
		}
		if i == n-1 {
			frame.Usage = &imageFrameUsage{
				InputTokens: 10, OutputTokens: 20,
				InputTokensDetails: imageTokenDetail{ImageTokens: 10},
				OutputTokenDetails: imageTokenDetail{ImageTokens: 20},
			}
		}
		data, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
		time.Sleep(o.latency)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}
