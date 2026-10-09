// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// safeWriter 是并发安全的 ResponseWriter+Flusher：心跳 goroutine 与测试线程
// 并发读写（-race 下同步）。
type safeWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
	hdr     http.Header
	writeN  int
}

func (w *safeWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *safeWriter) WriteHeader(int) {}

func (w *safeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeN++
	return w.buf.Write(p)
}

func (w *safeWriter) Flush() {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
}

func (w *safeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *safeWriter) Flushes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushes
}

func (w *safeWriter) WriteCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeN
}

// TestOutputHeartbeatShapeAndCadence：静默 ≥3 周期均发 `: keepalive\n`（单换行，
// 恒形态）；心跳置 committed 但不置 businessSent、不产业务帧。
func TestOutputHeartbeatShapeAndCadence(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 15*time.Millisecond, OutputOptions{})
	defer o.Release()
	require.Eventually(t, func() bool {
		return strings.Count(sw.String(), heartbeatFrame) >= 3
	}, 2*time.Second, 5*time.Millisecond, "静默期必须每周期发心跳")
	body := sw.String()
	require.Equal(t, strings.Repeat(heartbeatFrame, strings.Count(body, heartbeatFrame)), body,
		"心跳字节恒为单换行形态，无额外字节日志")
	require.True(t, o.Committed(), "心跳是真实可见输出 → committed")
	require.False(t, o.BusinessFrameSent(), "心跳不算业务帧")
	require.False(t, o.WriteFailed())
}

// TestOutputVisibleOutputResetsHeartbeat：成功可见输出（业务帧真实下行）重置
// 保活调度——持续下行期间无心跳。间隔取远大于逐帧写入节律（消除调度抖动），
// 窗口覆盖多个间隔周期，若未重置则必现心跳。
func TestOutputVisibleOutputResetsHeartbeat(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 250*time.Millisecond, OutputOptions{})
	defer o.Release()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := o.WriteFrame([]byte("data: x\n\n")); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		_ = o.DrainFlush() // 模拟 relay flush-on-drain：帧真实可见
		time.Sleep(2 * time.Millisecond)
	}
	require.NotContains(t, sw.String(), "keepalive", "持续可见下行必须持续重置调度")
	require.True(t, o.BusinessFrameSent())
}

// TestOutputLargeFrameAtomicNoHeartbeatInsert：>4KB 完整帧串行写出——心跳不得
// 插入帧内（同锁整帧）。
func TestOutputLargeFrameAtomicNoHeartbeatInsert(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 15*time.Millisecond, OutputOptions{})
	defer o.Release()
	big := "data: " + strings.Repeat("x", 5000) + "\n\n"
	if _, err := o.WriteFrame([]byte(big)); err != nil {
		t.Fatal(err)
	}
	_ = o.DrainFlush()
	time.Sleep(60 * time.Millisecond) // 给心跳充分机会
	require.Contains(t, sw.String(), big, "大帧必须连续（心跳不得插帧）")
}

// TestOutputStopTimerAllowsTerminalWrite：StopTimer 汇合在途回调后仍允许终态
// 错误帧（须已提交）。
func TestOutputStopTimerAllowsTerminalWrite(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 10*time.Millisecond, OutputOptions{})
	defer o.Release()
	require.NoError(t, o.Commit())
	o.StopTimer()
	before := strings.Count(sw.String(), heartbeatFrame)
	time.Sleep(40 * time.Millisecond)
	require.Equal(t, before, strings.Count(sw.String(), heartbeatFrame), "StopTimer 后无残留心跳")
	require.NoError(t, o.WriteError([]byte("event: error\ndata: {\"message\":\"x\"}\n\n")))
	require.Contains(t, sw.String(), "event: error", "终态错误帧在 StopTimer 后仍可写")
}

// TestOutputWriteFailureCancelsAndGates：真实写失败 → 置 writeFailed、取消上游
// ctx、禁终态补写。
func TestOutputWriteFailureCancelsAndGates(t *testing.T) {
	werr := errors.New("client gone")
	fw := &errorOnceWriter{err: werr}
	var canceled atomic.Bool
	o := NewOutput(fw, 0, OutputOptions{Cancel: func() { canceled.Store(true) }})
	defer o.Release()
	_, err := o.WriteFrame([]byte("data: x\n\n"))
	require.ErrorIs(t, err, werr)
	require.True(t, o.WriteFailed())
	require.ErrorIs(t, o.IOErr(), werr)
	require.True(t, canceled.Load(), "写失败必须取消上游 ctx")
	// 写失败后禁补写：WriteError 为 no-op，不再触发底层写。
	require.NoError(t, o.WriteError([]byte("event: error\n\n")))
	require.Equal(t, 1, fw.calls, "写失败后不得再补写")
}

// TestOutputFinishDisablesAllWrites：Finish 后禁一切写。
func TestOutputFinishDisablesAllWrites(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{})
	defer o.Release()
	require.NoError(t, o.Commit())
	o.Finish()
	wrote, err := o.WriteFrame([]byte("data: x\n\n"))
	require.False(t, wrote)
	require.NoError(t, err)
	require.NoError(t, o.WriteError([]byte("event: error\n\n")))
	require.NotContains(t, sw.String(), "data:")
	require.NotContains(t, sw.String(), "event: error")
}

// TestRelayOnEventFiresForDroppedFrame pin 统一写出前 seam 顺序：组帧 → Mapper
// （可 drop）→ OnEvent（原始 Event，含被 drop 的帧）→ 写出（drop 则不写）。
func TestRelayOnEventFiresForDroppedFrame(t *testing.T) {
	var datas []string
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, "data: drop-me\n\ndata: keep\n\n", Config{
		Mapper: func(e Event) ([]byte, bool) {
			if string(e.Data) == "drop-me" {
				return nil, true
			}
			return e.Raw, false
		},
		OnEvent: func(e Event) { datas = append(datas, string(e.Data)) },
	}))
	require.Equal(t, []string{"drop-me", "keep"}, datas, "被 drop 的帧也必须触发 OnEvent（写出前 seam）")
	require.Equal(t, "data: keep\n\n", rec.Body.String(), "drop 帧不得写出字节")
}

// TestOutputCommitNoEventStillCommitsAndFlushes：无事件成功也提交并 flush（images
// 零事件路径）；Output 不采 TTFT（不涉计时）。
func TestOutputCommitNoEventStillCommitsAndFlushes(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{})
	defer o.Release()
	require.NoError(t, o.Commit())
	require.True(t, o.Committed())
	require.False(t, o.BusinessFrameSent())
	require.Equal(t, 1, sw.Flushes(), "Commit 须 flush 一次")
	require.Equal(t, "text/event-stream", sw.Header().Get("Content-Type"))
	require.Equal(t, "no-cache", sw.Header().Get("Cache-Control"))
	require.Equal(t, "no", sw.Header().Get("X-Accel-Buffering"))
}

// TestRelayHeartbeatDuringUpstreamSilence：Relay 经 Output 在读写间隙发心跳
// （上游静默），且不打扰业务帧。
func TestRelayHeartbeatDuringUpstreamSilence(t *testing.T) {
	sw := &safeWriter{}
	src := &stagedReader{chunks: [][]byte{[]byte("data: x\n\n")}, block: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Relay(context.Background(), sw, src, Config{Interval: 10 * time.Millisecond}) }()
	require.Eventually(t, func() bool {
		return strings.Count(sw.String(), heartbeatFrame) >= 2
	}, 2*time.Second, 5*time.Millisecond, "上游静默期必须发心跳")
	close(src.block)
	require.NoError(t, <-done)
}

// TestRelayHeartbeatOffNoTimer：Interval<=0 关闭通用定时心跳——静默不发注释。
func TestRelayHeartbeatOffNoTimer(t *testing.T) {
	sw := &safeWriter{}
	src := &stagedReader{chunks: [][]byte{[]byte("data: x\n\n")}, block: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Relay(context.Background(), sw, src, Config{Interval: 0}) }()
	time.Sleep(50 * time.Millisecond)
	require.NotContains(t, sw.String(), "keepalive", "Interval=0 不得发通用定时心跳")
	close(src.block)
	require.NoError(t, <-done)
}

// TestOutputCallerOwnedNotReleased：调用方持有的 Output 经 Config.Output 传入时
// Relay 不 Release（可继续读状态/补写）。
func TestOutputCallerOwnedNotReleased(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{FlushBytes: 4096})
	defer o.Release()
	require.NoError(t, Relay(context.Background(), sw, strings.NewReader("data: x\n\n"), Config{Output: o}))
	require.Equal(t, "data: x\n\n", sw.String())
	require.True(t, o.Committed())
	require.True(t, o.BusinessFrameSent())
}

// errorOnceWriter 底层写恒失败且计数写调用。
type errorOnceWriter struct {
	err   error
	calls int
}

func (w *errorOnceWriter) Header() http.Header         { return http.Header{} }
func (w *errorOnceWriter) WriteHeader(int)             {}
func (w *errorOnceWriter) Write(p []byte) (int, error) { w.calls++; return 0, w.err }

// flushErrWriter 写成功但 flush 失败：探测链命中的是 FlushError（可返错 flush）。
type flushErrWriter struct {
	err   error
	calls int
}

func (w *flushErrWriter) Header() http.Header         { return http.Header{} }
func (w *flushErrWriter) WriteHeader(int)             {}
func (w *flushErrWriter) Write(p []byte) (int, error) { w.calls++; return len(p), nil }
func (w *flushErrWriter) FlushError() error           { return w.err }

// partialErrWriter 大帧直写时部分写出（n>0）+ 错误。
type partialErrWriter struct{ err error }

func (w *partialErrWriter) Header() http.Header { return http.Header{} }
func (w *partialErrWriter) WriteHeader(int)     {}
func (w *partialErrWriter) Write(p []byte) (int, error) {
	return len(p) / 2, w.err
}

// TestOutputFlushErrorFailsAndGates：flush 可返错——Write 成功但 FlushError 失败
// → writeFailed + 取消上游 ctx、禁写；字节虽已过底层写边界（businessSent=true）
// 但不得推进可见调度（nextHeartbeat 不更新）。
func TestOutputFlushErrorFailsAndGates(t *testing.T) {
	boom := errors.New("flush boom")
	fw := &flushErrWriter{err: boom}
	var canceled atomic.Bool
	o := NewOutput(fw, time.Hour, OutputOptions{Cancel: func() { canceled.Store(true) }})
	defer o.Release()
	_, err := o.WriteFrame([]byte("data: x\n\n"))
	require.ErrorIs(t, err, boom)
	require.True(t, o.WriteFailed())
	require.ErrorIs(t, o.IOErr(), boom)
	require.True(t, canceled.Load(), "flush 失败必须取消上游 ctx")
	require.True(t, o.BusinessFrameSent(), "字节已过底层写边界（Write 成功）→ 真实下行")
	require.True(t, o.nextHeartbeat.IsZero(), "flush 失败不得推进可见调度（nextHeartbeat）")
	wrote, err := o.WriteFrame([]byte("data: y\n\n"))
	require.False(t, wrote)
	require.NoError(t, err)
	require.Equal(t, 1, fw.calls, "失败后禁再写底层")
}

// TestOutputLargeFramePartialWriteFailure：>4KB 首帧直写底层前即置提交态；
// 部分写出（n>0 + err）的业务字节已过底层写边界 → 计入真实下行（businessSent），
// 不再计未下行（pendingBusiness）。
func TestOutputLargeFramePartialWriteFailure(t *testing.T) {
	boom := errors.New("big frame write failed")
	fw := &partialErrWriter{err: boom}
	var canceled atomic.Bool
	o := NewOutput(fw, 0, OutputOptions{Cancel: func() { canceled.Store(true) }})
	defer o.Release()
	big := []byte("data: " + strings.Repeat("x", 6000) + "\n\n")
	_, err := o.WriteFrame(big)
	require.ErrorIs(t, err, boom)
	require.True(t, o.WriteFailed())
	require.True(t, o.Committed(), "大帧直写底层前即置提交态")
	require.Equal(t, 0, o.pendingBusiness, "部分写出的字节已过底层写边界，不再计未下行")
	require.True(t, o.BusinessFrameSent(), "部分写失败仍保留真实下行事实")
	require.True(t, canceled.Load())
}

// TestOutputBusinessBoundaryAccounting：小帧仅入缓冲（未下行）→ businessSent 不置位、
// pendingBusiness 计未下行；成功 flush 后才计真实下行并清零未下行。
func TestOutputBusinessBoundaryAccounting(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{FlushBytes: 1 << 20})
	defer o.Release()
	frame := []byte("data: x\n\n")
	if _, err := o.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	// 首帧即时 flush → 已下行；再写一小帧仅入缓冲（阈值远大于 pending）。
	if _, err := o.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, len(frame), o.pendingBusiness, "小帧未下行 → 计入 pendingBusiness")
	require.True(t, o.BusinessFrameSent(), "首帧已下行")
	require.NoError(t, o.DrainFlush())
	require.Equal(t, 0, o.pendingBusiness, "成功 flush 后未下行清零")
	require.True(t, o.BusinessFrameSent())
}

// TestOutputFinishDisablesDrainFlush：首帧 flush → 缓冲小帧 → Finish →
// DrainFlush 不再写（DrainFlush 亦须检查 finished）。
func TestOutputFinishDisablesDrainFlush(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{FlushBytes: 1 << 20})
	defer o.Release()
	if _, err := o.WriteFrame([]byte("data: a\n\n")); err != nil {
		t.Fatal(err)
	}
	calls := sw.WriteCalls() // 首帧即时 flush 后的底层写次数
	if _, err := o.WriteFrame([]byte("data: b\n\n")); err != nil {
		t.Fatal(err)
	}
	o.Finish()
	require.NoError(t, o.DrainFlush())
	require.Equal(t, calls, sw.WriteCalls(), "Finish 后 DrainFlush 不得再写底层")
}

// TestOutputWriteErrorClosesLifecycle：错误帧写成功后关闭写生命周期——此后
// 业务帧/心跳/DrainFlush 均被拒；缓冲业务状态结算。
func TestOutputWriteErrorClosesLifecycle(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{})
	defer o.Release()
	require.NoError(t, o.Commit())
	require.NoError(t, o.WriteError([]byte("event: error\ndata: {\"message\":\"x\"}\n\n")))
	require.Contains(t, sw.String(), "event: error")
	before := sw.String()
	wrote, err := o.WriteFrame([]byte("data: later\n\n"))
	require.False(t, wrote)
	require.NoError(t, err)
	require.NoError(t, o.Heartbeat())
	require.NoError(t, o.DrainFlush())
	require.Equal(t, before, sw.String(), "错误帧写成功后不得再写任何字节")
}

// TestOutputReleaseIdempotent：Release 真幂等——重复调用不再二次 Put。
func TestOutputReleaseIdempotent(t *testing.T) {
	sw := &safeWriter{}
	o := NewOutput(sw, 0, OutputOptions{})
	o.Release()
	require.NotPanics(t, func() { o.Release() }, "重复 Release 不得 panic/二次入池")
	o2 := NewOutput(sw, 0, OutputOptions{})
	defer o2.Release()
	_, err := o2.WriteFrame([]byte("data: x\n\n"))
	require.NoError(t, err)
	require.True(t, o2.BusinessFrameSent(), "池复用后 Output 仍可用")
}

// TestOutputClientCancelBeforeWriteFailure：客户端先取消（ctx 已取消）后写失败
// → 不计 selfCanceled（避免把客户端取消误记为写失败出口）。
func TestOutputClientCancelBeforeWriteFailure(t *testing.T) {
	bctx, bcancel := context.WithCancel(context.Background())
	bcancel() // 客户端先取消
	boom := errors.New("client gone")
	fw := &errorOnceWriter{err: boom}
	var selfCanceledCalled atomic.Bool
	o := NewOutput(fw, 0, OutputOptions{Ctx: bctx, Cancel: func() { selfCanceledCalled.Store(true) }})
	defer o.Release()
	_, err := o.WriteFrame([]byte("data: x\n\n"))
	require.ErrorIs(t, err, boom)
	require.True(t, o.WriteFailed())
	require.False(t, o.SelfCanceled(), "客户端先取消 → 不得标记 selfCanceled")
	require.True(t, selfCanceledCalled.Load(), "仍尝试取消上游（幂等 no-op）")
}
