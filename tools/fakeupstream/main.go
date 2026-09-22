// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// fakeupstream 模拟 OpenAI chat/completions 上游：支持流式（chunks 个事件 + usage + [DONE]）。
// 用法: go run ./tools/fakeupstream -addr :9100 -chunks 100 -latency 20ms
//
// 扩展：
//   - -fail429/-fail500：按上游 key（Authorization: Bearer <key>）注入 429/5xx，
//     用于验证调度器失败转移不产生雪崩（规格 §5.3，brief）。
//   - /v1/messages：anthropic 官方格式的 SSE 流（event: 行 + message_start/
//     content_block_delta/message_delta/message_stop），SDK 按 event 类型分发，
//     纯 data 事件会被静默跳过（修复后的网关同样按官方格式写出）。
//   - /v1/images/generations：stream:true 时回网关兼容 Images SSE（每张图一个
//     image_generation.completed data 帧 + 末帧 usage + [DONE] 终帧）。
//   - 请求体可选字段 "chunks"（整数）：按请求覆盖 -chunks 标志（e2e 需要
//     单个实例同时服务快速请求与长流式请求；缺省用标志值）。
//   - 请求审计面 GET /_audit：环形缓冲记录每请求的（path、上游 key、model、
//     prompt_cache_key、终态码、时刻），注入失败同样如实入账——intelligent-
//     routing e2e 据此归因"哪个账号/哪条路由真实处理了请求"。key 均为测试
//     合成值，非真实凭据。
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

	http.HandleFunc("/_audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(audit.snapshot())
	})
	http.HandleFunc("/v1/models", modelsHandler)

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if code := failIfInjected(w, r, f429, f500, f400); code != 0 {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			audit.add(r, nil, 400)
			w.WriteHeader(400)
			return
		}
		audit.add(r, body, 200)
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
		writeChatStream(w, fl, *latency, bodyChunks(body, *chunks))
	})

	// openai responses 格式（Responses API）：非流式 JSON + 流式 SSE
	// （response.output_text.delta → response.completed → [DONE]，多格式压测用）。
	http.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if code := failIfInjected(w, r, f429, f500, f400); code != 0 {
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			audit.add(r, nil, 400)
			w.WriteHeader(400)
			return
		}
		audit.add(r, body, 200)
		responseID := nextResponseID()
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": responseID, "object": "response", "status": "completed",
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
			time.Sleep(*latency)
			writeData(map[string]any{"type": "response.output_text.delta", "delta": "x"})
		}
		writeData(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": responseID, "object": "response", "status": "completed",
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
			audit.add(r, nil, 400)
			w.WriteHeader(400)
			return
		}
		audit.add(r, body, 200)
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
		// 首事件同样限速（TTFB 现实化，缺陷 C 同因）。
		time.Sleep(*latency)
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
			time.Sleep(*latency)
			writeAnthropic("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "x"},
			})
		}
		writeAnthropic("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeAnthropic("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 20},
		})
		writeAnthropic("message_stop", map[string]any{"type": "message_stop"})
	})

	// openai images 格式（generations）：非流式 JSON + 流式 SSE（见 imagesHandler）。
	http.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		imagesHandler(w, r, imagesOpts{latency: *latency, f429: f429, f500: f500, f400: f400})
	})

	log.Printf("fake upstream on %s (chunks=%d latency=%s fail429=%v fail500=%v fail400=%v)",
		*addr, *chunks, *latency, f429, f500, f400)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// writeChatStream chat 流式 SSE：n 个 chunk（末块带 usage）+ [DONE] 终帧。
// 逐帧按 latency 限速——限速在写出前（含首帧）：首帧延迟即上游 TTFB，生产
// TTFT 按此采集；写后限速会使首帧恒 ~0ms，经毫秒截断 + log(1)=0 后 durable
// TTFT 和恒零、frontier 全员谎报 1ms（缺陷 C）。
func writeChatStream(w http.ResponseWriter, fl http.Flusher, latency time.Duration, n int) {
	// 帧预编码：100 帧里 99 帧字节恒定，仅末帧带 usage —— 逐帧 map+Marshal 在
	// 10k 流压测下是上游模拟器的主要 CPU 成本（实测 1.5-3.2 核）。按请求编码
	// 2 次、逐帧单写，线格式与旧实现逐字节一致。
	base := map[string]any{
		"id": "c1", "object": "chat.completion.chunk",
		"choices": []map[string]any{{"delta": map[string]any{"content": "x"}, "index": 0}},
	}
	raw, _ := json.Marshal(base)
	frame := append(append([]byte("data: "), raw...), '\n', '\n')
	base["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
	rawLast, _ := json.Marshal(base)
	last := append(append([]byte("data: "), rawLast...), '\n', '\n')
	for i := 0; i < n; i++ {
		time.Sleep(latency)
		data := frame
		if i == n-1 {
			data = last
		}
		_, _ = w.Write(data)
		fl.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	audit.add(r, nil, http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
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
// 注入拒绝同样入审计（真实账目：该请求确实到达了本上游、由该 key 归因）。
func failIfInjected(w http.ResponseWriter, r *http.Request, f429, f500, f400 map[string]bool) int {
	key := upstreamKey(r)
	switch {
	case f429[key]:
		audit.add(r, nil, http.StatusTooManyRequests)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"injected 429","type":"rate_limit_error"}}`))
		return http.StatusTooManyRequests
	case f500[key]:
		audit.add(r, nil, http.StatusInternalServerError)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"injected 500","type":"server_error"}}`))
		return http.StatusInternalServerError
	case f400[key]:
		audit.add(r, nil, http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"injected 400","type":"invalid_request_error"}}`))
		return http.StatusBadRequest
	}
	return 0
}
