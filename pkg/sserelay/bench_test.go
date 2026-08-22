// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 100 个 chunk 的典型高频流（对应压测 fakeupstream 100 × 20ms 的流形态）
func sseStream100() string {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"x"},"index":0}]}`)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func BenchmarkRelay100Chunks(b *testing.B) {
	src := sseStream100()
	var sink bytes.Buffer
	cfg := Config{FlushBytes: 4096, FlushInterval: time.Millisecond}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink.Reset()
		if err := Relay(context.Background(), &benchWriter{&sink}, strings.NewReader(src), cfg); err != nil {
			b.Fatal(err)
		}
	}
}

// benchWriter 刻意不实现 http.Flusher：无 Flush syscall，衡量纯 relay 成本。
type benchWriter struct{ *bytes.Buffer }

func (w *benchWriter) Header() http.Header { return http.Header{} }
func (w *benchWriter) WriteHeader(int)     {}

// benchFlusher 实现 http.Flusher（no-op Flush）：计入 fl.Flush() 调用链与
// 首事件立即 flush 的完整开销（对齐生产 dst 形态——net/http ResponseWriter 恒
// 可 Flusher）。
type benchFlusher struct {
	buf bytes.Buffer
	hdr http.Header
}

func (w *benchFlusher) Header() http.Header         { return w.hdr }
func (w *benchFlusher) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *benchFlusher) WriteHeader(int)             {}
func (w *benchFlusher) Flush()                      {}

// sseFrames 拼 n 个约 size 字节的 data 帧 + [DONE] 终止帧。
func sseFrames(n, size int) []byte {
	payload := strings.Repeat("x", size)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(`data: {"id":"c1","choices":[{"delta":{"content":"`)
		b.WriteString(payload)
		b.WriteString(`"}]}]}` + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

// BenchmarkRelayShortStream 短流形态（fakeup chunks=1 / 快速单事件流）：单帧
// 即 EOF，走首事件立即 flush 路径。衡量每流固定开销（池化 bufio 取还、timer
// 创建+goroutine 启停、deadline watcher、锁、首 flush 全链）。
// dst 跨迭代复用（Reset 保容量）：sink 增长分配不计入 relay 成本。
func BenchmarkRelayShortStream(b *testing.B) {
	src := sseFrames(1, 200)
	dst := &benchFlusher{hdr: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst.buf.Reset()
		if err := Relay(context.Background(), dst, bytes.NewReader(src), Config{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRelayTokenStream 生产长流形态：500 × ~230B token 帧持续到达，
// 4KB 阈值批量 + 1ms timer 批量生效区间。
func BenchmarkRelayTokenStream(b *testing.B) {
	src := sseFrames(500, 180)
	dst := &benchFlusher{hdr: http.Header{}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst.buf.Reset()
		if err := Relay(context.Background(), dst, bytes.NewReader(src), Config{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRelayBulk16K 大帧直写形态：8 × 16KB 帧，越过 8KB bufio 缓冲触发
// bufio 直写路径（绕过 bw 缓冲拷贝）。
func BenchmarkRelayBulk16K(b *testing.B) {
	src := sseFrames(8, 16000)
	dst := &benchFlusher{hdr: http.Header{}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst.buf.Reset()
		if err := Relay(context.Background(), dst, bytes.NewReader(src), Config{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIOCopyFloor 同负载 io.Copy 下限：理想 dumb pipe 的 CPU/alloc 基线，
// 用于计算 Relay 相对裸拷贝的开销倍数（含帧解析/Observer 视图/timer 机制）。
func BenchmarkIOCopyFloor(b *testing.B) {
	src := sseFrames(500, 180)
	sink := &bytes.Buffer{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink.Reset()
		if _, err := io.Copy(sink, bytes.NewReader(src)); err != nil {
			b.Fatal(err)
		}
	}
}
