// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// fakeupstream 模拟 OpenAI chat/completions 上游：支持流式（chunks 个事件 + usage + [DONE]）。
// 用法: go run ./tools/fakeupstream -addr :9100 -chunks 100 -latency 20ms
//
// 扩展（Task 9 验收需要）：
//   - -fail429/-fail500：按上游 key（Authorization: Bearer <key>）注入 429/5xx，
//     用于验证调度器失败转移不产生雪崩（规格 §5.3，brief Step 4）。
//   - /v1/messages：anthropic 官方格式的 SSE 流（event: 行 + message_start/
//     content_block_delta/message_delta/message_stop），SDK 按 event 类型分发，
//     纯 data 事件会被静默跳过（Task 8 修复后的网关同样按官方格式写出）。
//   - 请求体可选字段 "chunks"（整数）：按请求覆盖 -chunks 标志（e2e 需要
//     单个实例同时服务快速请求与长流式请求；缺省用标志值）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// bodyChunks 请求体可选 "chunks" 字段（整数）覆盖全局标志；无则用默认。
func bodyChunks(body map[string]any, def int) int {
	if v, ok := body["chunks"]; ok {
		if f, ok := v.(float64); ok && f >= 1 {
			return int(f)
		}
	}
	return def
}

func main() {
	addr := flag.String("addr", ":9100", "listen addr")
	chunks := flag.Int("chunks", 100, "SSE chunks per stream")
	latency := flag.Duration("latency", 20*time.Millisecond, "per-chunk delay")
	fail429 := flag.String("fail429", "", "comma-separated upstream keys to reject with 429")
	fail500 := flag.String("fail500", "", "comma-separated upstream keys to reject with 500")
	fail400 := flag.String("fail400", "", "comma-separated upstream keys to reject with 400")
	flag.Parse()

	f429 := splitKeys(*fail429)
	f500 := splitKeys(*fail500)
	f400 := splitKeys(*fail400)

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if code := failIfInjected(w, r, f429, f500, f400); code != 0 {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "object": "chat.completion",
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < bodyChunks(body, *chunks); i++ {
			chunk := map[string]any{
				"id": "c1", "object": "chat.completion.chunk",
				"choices": []map[string]any{{"delta": map[string]any{"content": "x"}, "index": 0}},
			}
			if i == bodyChunks(body, *chunks)-1 {
				chunk["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
			time.Sleep(*latency)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})

	// openai responses 格式（Responses API）：非流式 JSON + 流式 SSE
	// （response.output_text.delta → response.completed → [DONE]，多格式压测用）。
	http.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if code := failIfInjected(w, r, f429, f500, f400); code != 0 {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "rsp_1", "object": "response", "status": "completed",
				"output": []any{},
				"usage":  map[string]any{"input_tokens": 10, "output_tokens": 20, "total_tokens": 30},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		writeData := func(v map[string]any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}
		for i := 0; i < bodyChunks(body, *chunks); i++ {
			writeData(map[string]any{"type": "response.output_text.delta", "delta": "x"})
			time.Sleep(*latency)
		}
		writeData(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "rsp_1", "object": "response", "status": "completed",
				"model": "gpt-4o", "output": []any{},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 20, "total_tokens": 30},
			},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})

	// anthropic 官方格式流（event: 行必须带，见文件头注释）。SDK 在
	// message_stop 后结束迭代，故 message_stop 必须是最后一个事件。
	http.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if code := failIfInjected(w, r, f429, f500, f400); code != 0 {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant",
				"content":     []map[string]any{{"type": "text", "text": "hi"}},
				"model":       "claude-3-5-sonnet-20241022",
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 10, "output_tokens": 20},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		writeAnthropic := func(event string, v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\n", event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
		}
		writeAnthropic("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant",
				"model": "claude-3-5-sonnet-20241022", "content": []any{},
				"stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 0},
			},
		})
		writeAnthropic("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for i := 0; i < bodyChunks(body, *chunks); i++ {
			writeAnthropic("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "x"},
			})
			time.Sleep(*latency)
		}
		writeAnthropic("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeAnthropic("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 20},
		})
		writeAnthropic("message_stop", map[string]any{"type": "message_stop"})
	})

	// openai images 格式（generations）：非流式 JSON，data 数组按请求 n 回显——
	// 网关按 data 长度数图计费（ImageCost），size/model 取自请求体不回显校验。
	http.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		if code := failIfInjected(w, r, f429, f500, f400); code != 0 {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		n := 1
		if v, ok := body["n"].(float64); ok && v >= 1 && v <= 10 {
			n = int(v)
		}
		data := make([]map[string]any, n)
		for i := range data {
			data[i] = map[string]any{"url": fmt.Sprintf("https://fake.invalid/img-%d.png", i)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": time.Now().Unix(),
			"data":    data,
		})
	})

	log.Printf("fake upstream on %s (chunks=%d latency=%s fail429=%v fail500=%v fail400=%v)",
		*addr, *chunks, *latency, f429, f500, f400)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func splitKeys(s string) map[string]bool {
	out := make(map[string]bool)
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out[k] = true
		}
	}
	return out
}

// failIfInjected 命中注入 key 则直接写 400/429/500 并返回状态码（0 = 未命中）。
func failIfInjected(w http.ResponseWriter, r *http.Request, f429, f500, f400 map[string]bool) int {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	key = strings.TrimSpace(key)
	switch {
	case f429[key]:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"injected 429","type":"rate_limit_error"}}`))
		return http.StatusTooManyRequests
	case f500[key]:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"injected 500","type":"server_error"}}`))
		return http.StatusInternalServerError
	case f400[key]:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"injected 400","type":"invalid_request_error"}}`))
		return http.StatusBadRequest
	}
	return 0
}
