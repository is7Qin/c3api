// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bufio"
	"context"
	"net/http"
	"sync"
	"time"
)

// heartbeatFrame 是网关双侧保活注释帧的恒定字节（单换行）。openai-go 共享
// ssestream 上层 Stream.Next() 对 `\n\n` 形态会报错，故恒 `: keepalive\n`。
const heartbeatFrame = ": keepalive\n"

// SetSSEHeaders 写入 SSE 响应头三件套（text/event-stream）——单一 SSE 头助手，
// Output.Commit 与 proxy 侧共用（proxy.writeSSEHeaders 委托本函数）。仅设置
// 头、不提交状态码。
func SetSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
}

// OutputOptions 构造 Output 的可选项。
type OutputOptions struct {
	// Cancel 是实际上游请求 ctx 的 cancel（须在请求发出前建立）。心跳写/flush
	// 失败时 Output 调用它解除在读的上游请求；nil = 不联动。
	Cancel context.CancelFunc
	// FlushBytes 缓冲阈值；<=0 时默认 4096。
	FlushBytes int
}

// Output 是统一下行 owner：持真实 wire、唯一串行锁与自有写缓冲，负责完整帧
// 的写出/批量 flush、静默保活、提交态与终态错误帧。读缓冲留在 relay（组帧仍
// 依赖读缓冲耗尽判断）。所有帧级操作持 mu。
//
// 生命周期：NewOutput（可复用池取）→ WriteFrame/Heartbeat/Commit/WriteError/
// DrainFlush → StopTimer（汇合在途心跳）→ 读取提交态 → Finish（禁写）。调用方
// 持有 Output 时须在读取状态后调用 Release 归还池（Relay 内部自建的 Output 由
// Relay 自行 Release）。
type Output struct {
	w  http.ResponseWriter
	fl http.Flusher
	bw *bufio.Writer

	mu              sync.Mutex
	pending         int  // 累计写入字节；批量 flush/drain/结束残余后归零（首事件 flush 不归零）
	pendingBusiness int  // 已进 Output 未下行的业务字节
	firstFlushed    bool // 首帧已即时 flush
	committed       bool // 实际底层已写头/字节
	businessSent    bool // 业务帧真实下行
	writeFailed     bool // 真实 I/O 失败
	finished        bool // 禁一切写
	lastErr         error

	flushBytes    int
	interval      time.Duration
	cancel        context.CancelFunc
	lastVisible   time.Time
	nextHeartbeat time.Time

	stopCh  chan struct{}
	timerWG sync.WaitGroup
	started bool
}

// outputPool 池化 Output 本体（含写缓冲与状态），使 Interval<=0 路径相对迁移前零新增
// 分配。归还前 StopTimer 已汇合、缓冲引用已解除。
var outputPool = sync.Pool{New: func() any { return &Output{bw: bufio.NewWriterSize(nil, 4096)} }}

// NewOutput 构造一个下行 Output（复用池）。w 为真实 wire；interval<=0 关闭通用
// 定时保活（零新增分配——不建 timer/goroutine）。opts.Cancel 见 OutputOptions。
func NewOutput(w http.ResponseWriter, interval time.Duration, opts OutputOptions) *Output {
	o := outputPool.Get().(*Output)
	o.w = w
	o.fl = nil
	if f, ok := w.(http.Flusher); ok {
		o.fl = f
	}
	o.bw.Reset(w)
	o.pending = 0
	o.pendingBusiness = 0
	o.firstFlushed = false
	o.committed = false
	o.businessSent = false
	o.writeFailed = false
	o.finished = false
	o.lastErr = nil
	o.flushBytes = opts.FlushBytes
	if o.flushBytes <= 0 {
		o.flushBytes = 4096
	}
	o.interval = interval
	o.cancel = opts.Cancel
	o.lastVisible = time.Time{}
	o.nextHeartbeat = time.Time{}
	o.stopCh = nil
	o.started = false
	if interval > 0 {
		o.stopCh = make(chan struct{})
		o.started = true
		o.timerWG.Add(1)
		go o.heartbeatLoop(o.stopCh)
	}
	return o
}

// heartbeatLoop 定时保活：每到点持 mu 复核（now>=nextHeartbeat 且未写失败/未
// finish）才写。stopCh 关闭即退出（StopTimer 汇合）。
func (o *Output) heartbeatLoop(stop chan struct{}) {
	defer o.timerWG.Done()
	t := time.NewTimer(o.interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			o.fireHeartbeat()
			t.Reset(o.interval)
		}
	}
}

// fireHeartbeat 到点持 mu 复核后写心跳。
func (o *Output) fireHeartbeat() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed {
		return
	}
	if time.Now().Before(o.nextHeartbeat) {
		return
	}
	o.writeHeartbeatLocked()
}

// WriteFrame 完整帧同步消费/复制进自有写缓冲（不必已可见）。返回是否写入及
// 错误。映射帧复用切片由 Mapper 返回前复制（见 relay）。
func (o *Output) WriteFrame(frame []byte) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return false, nil
	}
	if len(frame) == 0 {
		return false, nil
	}
	if _, err := o.bw.Write(frame); err != nil {
		o.failLocked(err)
		return false, err
	}
	o.pending += len(frame)
	o.pendingBusiness += len(frame)
	if !o.firstFlushed {
		o.firstFlushed = true
		// 首事件立即 flush，保证首字节延迟；不重置 pending——首事件字节仍计入
		// 阈值，后续小事件可叠加触发一次批量 flush。
		if err := o.flushNoResetLocked(); err != nil {
			return false, err
		}
		return true, nil
	}
	if o.pending >= o.flushBytes {
		if err := o.flushLocked(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Heartbeat 写 `: keepalive\n`；成功可见后更新 lastVisible/nextHeartbeat。到点
// 外的显式调用（images SDK 驱动）也走此路径。
func (o *Output) Heartbeat() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed {
		return nil
	}
	return o.writeHeartbeatLocked()
}

// writeHeartbeatLocked 独立心跳写：写注释帧 → flush → 结算 pending（含顺带
// flush 已缓冲业务字节：置 businessSent、清 pendingBusiness）→ 重置
// lastVisible/nextHeartbeat。须持 mu。
func (o *Output) writeHeartbeatLocked() error {
	if _, err := o.bw.WriteString(heartbeatFrame); err != nil {
		o.failLocked(err)
		return err
	}
	o.committed = true
	if err := o.bw.Flush(); err != nil {
		o.failLocked(err)
		return err
	}
	if o.fl != nil {
		o.fl.Flush()
	}
	if o.pendingBusiness > 0 {
		o.businessSent = true
		o.pendingBusiness = 0
	}
	o.pending = 0
	now := time.Now()
	o.lastVisible = now
	o.nextHeartbeat = now.Add(o.interval)
	return nil
}

// Commit 写 SSE 头三件套 + WriteHeader(200)；committed 在实际上层回调前置位。
func (o *Output) Commit() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.committed {
		return nil
	}
	SetSSEHeaders(o.w.Header())
	o.committed = true
	o.w.WriteHeader(http.StatusOK)
	if o.fl != nil {
		o.fl.Flush()
	}
	now := time.Now()
	o.lastVisible = now
	if o.interval > 0 {
		o.nextHeartbeat = now.Add(o.interval)
	}
	return nil
}

// WriteError 写终态错误帧（须已提交且未写失败、未 finish）。
func (o *Output) WriteError(frame []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed || !o.committed {
		return nil
	}
	if _, err := o.bw.Write(frame); err != nil {
		o.failLocked(err)
		return err
	}
	if err := o.bw.Flush(); err != nil {
		o.failLocked(err)
		return err
	}
	if o.fl != nil {
		o.fl.Flush()
	}
	return nil
}

// DrainFlush flush-on-drain：读缓冲耗尽、即将阻塞等新数据前调用，flush 已写入
// 的完整帧（同一读批多帧 = 一次写系统调用）。空 pending 为 no-op。
func (o *Output) DrainFlush() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending <= 0 {
		return nil
	}
	if o.bw.Buffered() == 0 {
		// 字节已由首帧即时 flush 写出，仅清账不再触发空 Flush。
		o.pending = 0
		return nil
	}
	return o.flushLocked()
}

// StopTimer 停定时器并在 mu 外汇合在途回调（幂等；之后仍允许终态写）。
func (o *Output) StopTimer() {
	o.mu.Lock()
	ch := o.stopCh
	o.stopCh = nil
	o.mu.Unlock()
	if ch == nil {
		return
	}
	close(ch)
	o.timerWG.Wait()
}

// Finish 禁一切写（先确保定时器已汇合）。
func (o *Output) Finish() {
	o.StopTimer()
	o.mu.Lock()
	o.finished = true
	o.mu.Unlock()
}

// Release 归还 Output 到池（先 StopTimer/Finish 语义；解除 wire/缓冲引用）。
func (o *Output) Release() {
	o.StopTimer()
	o.mu.Lock()
	o.finished = true
	o.bw.Reset(nil)
	o.w = nil
	o.fl = nil
	o.cancel = nil
	o.pending = 0
	o.pendingBusiness = 0
	o.mu.Unlock()
	outputPool.Put(o)
}

// Committed 报告响应头/字节是否已实际下行。
func (o *Output) Committed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.committed
}

// BusinessFrameSent 报告业务帧是否已真实下行（心跳不计）。
func (o *Output) BusinessFrameSent() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.businessSent
}

// WriteFailed 报告是否已发生真实 I/O 失败。
func (o *Output) WriteFailed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writeFailed
}

// IOErr 返回首个真实 I/O 错误（若有）。
func (o *Output) IOErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastErr
}

// failLocked 记录首个 I/O 错误、置 writeFailed 并取消上游 ctx。须持 mu。
func (o *Output) failLocked(err error) {
	if o.writeFailed {
		return
	}
	o.writeFailed = true
	if o.lastErr == nil {
		o.lastErr = err
	}
	if o.cancel != nil {
		o.cancel()
	}
}

// flushLocked 批量 flush（阈值/drain/结束残余触发）：pending>0 时 flush +
// fl.Flush，结算 pending 与业务下行标记，并重置 lastVisible/nextHeartbeat。
func (o *Output) flushLocked() error {
	if o.pending <= 0 {
		return nil
	}
	o.committed = true
	if err := o.bw.Flush(); err != nil {
		o.failLocked(err)
		return err
	}
	if o.fl != nil {
		o.fl.Flush()
	}
	if o.pendingBusiness > 0 {
		o.businessSent = true
		o.pendingBusiness = 0
	}
	now := time.Now()
	o.lastVisible = now
	if o.interval > 0 {
		o.nextHeartbeat = now.Add(o.interval)
	}
	o.pending = 0
	return nil
}

// flushNoResetLocked 只 flush 不重置 pending：首事件 latency flush 专用。
func (o *Output) flushNoResetLocked() error {
	o.committed = true
	if err := o.bw.Flush(); err != nil {
		o.failLocked(err)
		return err
	}
	if o.fl != nil {
		o.fl.Flush()
	}
	if o.pendingBusiness > 0 {
		o.businessSent = true
		o.pendingBusiness = 0
	}
	now := time.Now()
	o.lastVisible = now
	if o.interval > 0 {
		o.nextHeartbeat = now.Add(o.interval)
	}
	return nil
}
