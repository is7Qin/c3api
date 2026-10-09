// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bufio"
	"context"
	"errors"
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

// FlushWriter 触发真实 wire 的 flush 并返回可诊断错误：按 FlushError →
// Flusher → Unwrap 顺序探测（与 http.ResponseController 一致；Go 1.26 的
// *http.response 实现 FlushError，故真实响应可返错）。探测链全缺（无 Flusher
// 的旧兼容 writer）→ http.ErrNotSupported，调用方按「无需 flush」的旧行为处理、
// 不算失败。零分配（仅接口类型断言）。internal/server 的 statusWriter 转发
// FlushError 复用本函数（单一探测链实现）。
func FlushWriter(w http.ResponseWriter) error {
	if fe, ok := w.(interface{ FlushError() error }); ok {
		return fe.FlushError()
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		return nil
	}
	if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
		return FlushWriter(u.Unwrap())
	}
	return http.ErrNotSupported
}

// OutputOptions 构造 Output 的可选项。
type OutputOptions struct {
	// Ctx 是实际上游请求 ctx（须在请求发出前建立）。用于区分「Output 自身因
	// 写失败取消上游」与「客户端先取消」，并作为写 deadline 已失效（取消/超时）
	// 时禁补写的门禁。nil = 不联动。
	Ctx context.Context
	// Cancel 是实际上游请求 ctx 的 cancel（须在请求发出前建立）。心跳写/flush
	// 失败时 Output 调用它解除在读的上游请求。**建议必填**：为 nil 时 Output
	// 无法解除在途上游读，仅能记录 writeFailed（Relay 收尾仍按 writeFailed/
	// IOErr 传播 I/O 错，但阻塞在写的在途 goroutine 需依赖请求取消/流总超时
	// 解除）。
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
	bw *bufio.Writer

	mu              sync.Mutex
	pending         int  // 累计写入字节；批量 flush/drain/结束残余后归零（首事件 flush 不归零）
	pendingBusiness int  // 已进 Output 未下行的业务字节
	firstFlushed    bool // 首帧已即时 flush
	committed       bool // 实际底层已写头/字节
	businessSent    bool // 业务帧真实下行
	writeFailed     bool // 真实 I/O 失败
	selfCanceled    bool // writeFailed 由 Output 自身取消上游 ctx 引发（非客户端先取消）
	finished        bool // 禁一切写
	released        bool // 已归还池（Release 幂等）
	lastErr         error

	flushBytes    int
	interval      time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
	nextHeartbeat time.Time

	stopCh  chan struct{}
	timerWG sync.WaitGroup
}

// outputPool 池化 Output 本体（含写缓冲与状态），使 Interval<=0 路径相对迁移前零新增
// 分配。归还前 StopTimer 已汇合、缓冲引用已解除。
var outputPool = sync.Pool{New: func() any { return &Output{bw: bufio.NewWriterSize(nil, 4096)} }}

// NewOutput 构造一个下行 Output（复用池）。w 为真实 wire；interval<=0 关闭通用
// 定时保活（零新增分配——不建 timer/goroutine）。opts.Ctx/Cancel 见 OutputOptions。
func NewOutput(w http.ResponseWriter, interval time.Duration, opts OutputOptions) *Output {
	o := outputPool.Get().(*Output)
	o.w = w
	o.bw.Reset(w)
	o.pending = 0
	o.pendingBusiness = 0
	o.firstFlushed = false
	o.committed = false
	o.businessSent = false
	o.writeFailed = false
	o.selfCanceled = false
	o.finished = false
	o.released = false
	o.lastErr = nil
	o.flushBytes = opts.FlushBytes
	if o.flushBytes <= 0 {
		o.flushBytes = 4096
	}
	o.interval = interval
	o.ctx = opts.Ctx
	o.cancel = opts.Cancel
	o.nextHeartbeat = time.Time{}
	o.stopCh = nil
	if interval > 0 {
		o.stopCh = make(chan struct{})
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
// 错误。映射帧复用切片由 Mapper 返回前复制（见 relay）。finished/writeFailed
// 后拒绝写。
func (o *Output) WriteFrame(frame []byte) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed {
		return false, nil
	}
	if len(frame) == 0 {
		return false, nil
	}
	// 提交态在真实底层写前置位：帧超过缓冲余量时 bufio 会经底层 Write 直写
	//（首帧即隐式提交 200）。业务字节按「真正到达底层写边界」核算——不得把
	// bufio.Write 的 n 当作已下行（大帧可能直写底层，小帧仅入缓冲）。
	buffered := o.bw.Buffered()
	if len(frame) > o.bw.Available() {
		o.committed = true
	}
	n, err := o.bw.Write(frame)
	if n > 0 {
		o.pending += n
	}
	// 任何返回路径先结算：缓冲余量不足时 bufio 会先隐式 flush 既有业务字节再接受
	// 本次字节，该隐式 flush 可能已部分下行并返错——无论 n 是否为零、err 是否非
	// nil，都须按「操作前缓冲 + 本次接受字节 − 操作后缓冲」结清真实下行业务字节。
	// 写缓冲在此调用点恒为业务字节，故 business == wire == buffered + n。
	o.settleWireLocked(buffered+n, buffered+n, o.bw.Buffered())
	if err != nil {
		o.failLocked(err)
		return false, err
	}
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

// Heartbeat 写 `: keepalive\n`；成功可见后更新 nextHeartbeat。到点外的显式
// 调用（images SDK 驱动）也走此路径。finished/writeFailed 后拒绝写。
func (o *Output) Heartbeat() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed {
		return nil
	}
	return o.writeHeartbeatLocked()
}

// writeHeartbeatLocked 独立心跳写：先排空残余业务（使心跳字节不与业务字节混批）
// → 写注释帧 → flush → 结算（businessBefore 恒为 0，心跳字节不计业务）→ 重置
// nextHeartbeat。flush 失败不得视为可见成功（不更新 nextHeartbeat）。须持 mu。
func (o *Output) writeHeartbeatLocked() error {
	// 心跳字节不计业务：先排空残余业务（若有），使本次心跳写入即便触发隐式 flush
	// 也不含业务字节，业务/心跳字节在结算时界限分明。
	if o.bw.Buffered() > 0 {
		if err := o.flushLocked(); err != nil {
			return err // flushLocked 内部已 failLocked
		}
	}
	// 排空后缓冲应为空；仍以实测 businessBefore（FIFO 前端）结算，防御性正确。
	businessBefore := o.bw.Buffered()
	n, err := o.bw.WriteString(heartbeatFrame)
	o.settleWireLocked(businessBefore, businessBefore+n, o.bw.Buffered())
	if err != nil {
		o.failLocked(err)
		return err
	}
	o.committed = true
	err = o.bw.Flush()
	o.settleWireLocked(businessBefore, businessBefore+n, o.bw.Buffered())
	if err != nil {
		o.failLocked(err)
		return err
	}
	if err := o.flushWireLocked(); err != nil {
		o.failLocked(err)
		return err
	}
	o.pending = 0
	o.nextHeartbeat = time.Now().Add(o.interval)
	return nil
}

// Commit 写 SSE 头三件套 + WriteHeader(200)；committed 在实际上层回调前置位；
// flush 失败即失败（不更新可见性）。
func (o *Output) Commit() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed || o.committed {
		return nil
	}
	SetSSEHeaders(o.w.Header())
	o.committed = true
	o.w.WriteHeader(http.StatusOK)
	if err := o.flushWireLocked(); err != nil {
		o.failLocked(err)
		return err
	}
	if o.interval > 0 {
		o.nextHeartbeat = time.Now().Add(o.interval)
	}
	return nil
}

// WriteError 写终态错误帧（须已提交且未写失败/未 finish）。写成功后关闭写
// 生命周期（此后业务帧/心跳不得再写）、结算缓冲业务状态。写 deadline 已失效
// （ctx 已取消/超时）时禁补写。
func (o *Output) WriteError(frame []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed || !o.committed {
		return nil
	}
	if o.ctx != nil && o.ctx.Err() != nil {
		// 写 deadline 已失效（取消/总超时后 relay 设立即过期写 deadline）：
		// 不承诺 SSE error 必达，禁补写。
		return nil
	}
	// 错误帧字节不计业务：先排空残余业务（若有），使错误帧写入即便触发隐式 flush
	// 也不含业务字节。
	if o.bw.Buffered() > 0 {
		if err := o.flushLocked(); err != nil {
			return err // flushLocked 内部已 failLocked
		}
	}
	businessBefore := o.bw.Buffered() // 排空后应为 0；防御性实测
	n, werr := o.bw.Write(frame)
	o.settleWireLocked(businessBefore, businessBefore+n, o.bw.Buffered())
	if werr != nil {
		o.failLocked(werr)
		return werr
	}
	o.committed = true
	err := o.bw.Flush()
	o.settleWireLocked(businessBefore, businessBefore+n, o.bw.Buffered())
	if err != nil {
		o.failLocked(err)
		return err
	}
	if err := o.flushWireLocked(); err != nil {
		o.failLocked(err)
		return err
	}
	o.pending = 0
	o.finished = true // 终态：关闭写生命周期
	return nil
}

// DrainFlush flush-on-drain：读缓冲耗尽、即将阻塞等新数据前调用，flush 已写入
// 的完整帧（同一读批多帧 = 一次写系统调用）。空 pending / 已 finish / 写失败为
// no-op（禁写一致）。
func (o *Output) DrainFlush() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished || o.writeFailed {
		return nil
	}
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
// 幂等：重复调用不再二次 Put（防同对象双入池并发复用）。
func (o *Output) Release() {
	o.StopTimer()
	o.mu.Lock()
	if o.released {
		o.mu.Unlock()
		return
	}
	o.released = true
	o.finished = true
	o.bw.Reset(nil)
	o.w = nil
	o.ctx = nil
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

// SelfCanceled 报告 writeFailed 是否由 Output 自身取消上游 ctx 引发（区别于
// 客户端先取消）。Relay 据此优先返回保存的 I/O 错、出口判定据此归写失败。
func (o *Output) SelfCanceled() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.selfCanceled
}

// IOErr 返回首个真实 I/O 错误（若有）。
func (o *Output) IOErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastErr
}

// failLocked 记录首个 I/O 错误、置 writeFailed 并取消上游 ctx。selfCanceled
// 语义不依赖 cancel 是否非 nil：只要写失败发生在 ctx 仍存活（或 ctx 未知）时，
// 即视为「Output 自身引发的写失败」（区别于客户端先取消），据此 Relay 可安全
// 优先返回保存的 I/O 错而无需等待 cancel 回灌。须持 mu。
func (o *Output) failLocked(err error) {
	if o.writeFailed {
		return
	}
	o.writeFailed = true
	if o.lastErr == nil {
		o.lastErr = err
	}
	if o.ctx == nil || o.ctx.Err() == nil {
		o.selfCanceled = true
	}
	if o.cancel != nil {
		o.cancel()
	}
}

// flushWireLocked 触发真实 wire flush；无 Flusher 的旧兼容 writer 的
// ErrNotSupported 视为无需 flush（不算失败）。须持 mu。
func (o *Output) flushWireLocked() error {
	err := FlushWriter(o.w)
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// settleWireLocked 于底层写边界核算业务下行：businessBuffered 为本次 flush 前
// 缓冲内的业务字节数（缓冲恒以业务字节打头），wireBuffered 为缓冲总字节数
// （业务 + 本次追加的心跳/错误帧），after 为 flush 后剩余缓冲字节数。真正下行
// 字节 = wireBuffered − after；其中业务部分 = min(wired, businessBuffered)
// （FIFO——业务字节先于尾部的心跳/错误帧下行）。据此更新 businessSent（真实
// 下行事实，部分写失败也保留）与 pendingBusiness（未下行字节）。须持 mu。
func (o *Output) settleWireLocked(businessBuffered, wireBuffered, after int) {
	wired := wireBuffered - after
	businessWired := wired
	if businessWired > businessBuffered {
		businessWired = businessBuffered
	}
	if businessWired > 0 {
		o.businessSent = true
	}
	remain := businessBuffered - businessWired
	if remain < 0 {
		remain = 0
	}
	o.pendingBusiness = remain
}

// flushLocked 批量 flush（阈值/drain/结束残余触发）：pending>0 时 flush +
// wire flush，按底层写边界结算业务下行并重置 nextHeartbeat。flush 失败不得
// 视为可见成功（不更新 nextHeartbeat）。
func (o *Output) flushLocked() error {
	if o.pending <= 0 {
		return nil
	}
	o.committed = true
	buffered := o.bw.Buffered()
	err := o.bw.Flush()
	o.settleWireLocked(buffered, buffered, o.bw.Buffered())
	if err != nil {
		o.failLocked(err)
		return err
	}
	if err := o.flushWireLocked(); err != nil {
		o.failLocked(err)
		return err
	}
	if o.interval > 0 {
		o.nextHeartbeat = time.Now().Add(o.interval)
	}
	o.pending = 0
	return nil
}

// flushNoResetLocked 只 flush 不重置 pending：首事件 latency flush 专用。
func (o *Output) flushNoResetLocked() error {
	o.committed = true
	buffered := o.bw.Buffered()
	err := o.bw.Flush()
	o.settleWireLocked(buffered, buffered, o.bw.Buffered())
	if err != nil {
		o.failLocked(err)
		return err
	}
	if err := o.flushWireLocked(); err != nil {
		o.failLocked(err)
		return err
	}
	if o.interval > 0 {
		o.nextHeartbeat = time.Now().Add(o.interval)
	}
	return nil
}
