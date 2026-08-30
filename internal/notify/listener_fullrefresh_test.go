// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

// FullRefresh 初始化闸门回归（设计文档 2026-08-29-notify-full-refresh-retry）：
// consume 的唯一合法入边是同连接 FullRefresh 返回 nil。全部用确定性 channel
// 事件同步（事件通道 + 手动 timer + time.After watchdog），无 require.Eventually /
// time.Sleep / t.Parallel / 真实 PG。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// --- 确定性同步原语 ---

// gateWatchdog 事件等待上限：正向事件超时即判挂死（正常路径永不触发，
// 替代轮询等待的兜底）。
const gateWatchdog = 2 * time.Second

// recvEvent 等待一个 channel 事件；超 gateWatchdog 未到即 Fatalf。
func recvEvent[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(gateWatchdog):
		t.Fatalf("watchdog %v 超时：%s 事件未到", gateWatchdog, what)
	}
	var zero T
	return zero
}

// expectNoEvent 断言无事件。只在 run 可证被阻塞（挂起等退避释放 / 已退出）
// 时调用——此时非阻塞检查是确定性的，不依赖时序。
func expectNoEvent[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s：不该出现事件 %v（循环此刻必被阻塞）", what, v)
	default:
	}
}

// --- gate 替身 ---

// gateConn 假连接：WaitForNotification 进入事件（waiting）、Close 计数
// （closeN）+ 返回事件（closed，携带可配置 closeErr）。
type gateConn struct {
	*fakeConn
	closeErr error // 可配置 Close 返回值（run 必须容忍 Close 出错）
	closeN   atomic.Int32
	closed   chan error
	waiting  chan struct{}
}

func newGateConn() *gateConn {
	return &gateConn{
		fakeConn: newFakeConn(),
		closed:   make(chan error, 4),
		waiting:  make(chan struct{}, 8),
	}
}

func (g *gateConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	select {
	case g.waiting <- struct{}{}: // 进入观测：非阻塞，永不拖住 run
	default:
	}
	return g.fakeConn.WaitForNotification(ctx)
}

func (g *gateConn) Close(ctx context.Context) error {
	g.closeN.Add(1)
	g.closed <- g.closeErr
	return g.closeErr
}

// gateDisp 可编程 FullRefresh：failsLeft>0 失败后转成功；-1 恒失败；block 非空
// 时阻塞至 block 关闭或 ctx 取消（取消感知）。fullIn=调用事件，fullOut=返回
// 事件（携带错误）；applied=Apply 事件。
type gateDisp struct {
	fakeDisp
	failsLeft int
	block     chan struct{}
	fullIn    chan struct{}
	fullOut   chan error
	applied   chan Change
}

func newGateDisp(failsLeft int) *gateDisp {
	return &gateDisp{
		failsLeft: failsLeft,
		fullIn:    make(chan struct{}, 16),
		fullOut:   make(chan error, 16),
		applied:   make(chan Change, 16),
	}
}

func (d *gateDisp) FullRefresh(ctx context.Context) error {
	d.mu.Lock()
	d.full++
	fails := d.failsLeft
	if fails > 0 {
		d.failsLeft--
	}
	d.mu.Unlock()
	d.fullIn <- struct{}{}
	err := error(nil)
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil && fails != 0 {
		err = errors.New("refresh boom")
	}
	d.fullOut <- err
	return err
}

func (d *gateDisp) Apply(ctx context.Context, ch Change) {
	d.fakeDisp.Apply(ctx, ch)
	d.applied <- ch
}

// nextFull 等待下一次 FullRefresh 返回事件并返回其错误。
func (d *gateDisp) nextFull(t *testing.T) error {
	t.Helper()
	return recvEvent(t, d.fullOut, "FullRefresh 返回")
}

// gateRig 手动退避时钟：newTimer 记录请求时长并挂起等待释放；connect 发布
// 连接身份事件（connected）。
type gateRig struct {
	l         *Listener
	disp      *gateDisp
	connectN  atomic.Int32
	connected chan *gateConn
	delays    chan time.Duration
	pending   chan chan time.Time
}

// tweak 可选配置钩子（如收紧 BackoffMax），在 NewListener 前施加。
func newGateRig(t *testing.T, plan []*gateConn, disp *gateDisp, tweak ...func(*ListenerConfig)) *gateRig {
	t.Helper()
	cfg := ListenerConfig{DSN: "postgres://fake", Dispatcher: disp}
	for _, f := range tweak {
		f(&cfg)
	}
	l := NewListener(cfg)
	r := &gateRig{
		l:         l,
		disp:      disp,
		connected: make(chan *gateConn, 16),
		delays:    make(chan time.Duration, 16),
		pending:   make(chan chan time.Time, 16),
	}
	l.connect = func(ctx context.Context, dsn string) (Conn, error) {
		n := int(r.connectN.Add(1)) - 1
		if n >= len(plan) {
			return nil, errors.New("connect: plan exhausted")
		}
		r.connected <- plan[n]
		return plan[n], nil
	}
	l.newTimer = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		r.delays <- d
		r.pending <- ch
		return ch
	}
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, l.Start(ctx))
	t.Cleanup(func() {
		cancel()
		require.NoError(t, l.Close(context.Background()))
	})
	return r
}

// recvConnect 等待下一次成功连接，返回连接身份。
func (r *gateRig) recvConnect(t *testing.T) *gateConn {
	t.Helper()
	return recvEvent(t, r.connected, "连接事件")
}

// waitDelay 断言下一次退避请求时长为 want（未释放前 run 挂起在 sleep）。
func (r *gateRig) waitDelay(t *testing.T, want time.Duration) {
	t.Helper()
	require.Equal(t, want, recvEvent(t, r.delays, "退避请求"))
}

// release 释放最近一次挂起的退避 timer。
func (r *gateRig) release(t *testing.T) {
	t.Helper()
	ch := recvEvent(t, r.pending, "挂起退避 timer")
	ch <- time.Now()
}

// --- 测试 ---

// TestListenerFullRefreshFailureNoConsume FullRefresh 恒失败：连接关闭、通知
// 不消费（WaitForNotification 零进入、Apply=0）、退避释放前不建立下一连接。
func TestListenerFullRefreshFailureNoConsume(t *testing.T) {
	c1, c2 := newGateConn(), newGateConn()
	disp := newGateDisp(-1)
	rig := newGateRig(t, []*gateConn{c1, c2}, disp)

	require.Same(t, c1, rig.recvConnect(t))
	require.Error(t, disp.nextFull(t), "首次刷新必须失败")
	c1.ch <- notif(`{"v":1,"users":true}`) // 失败连接队列里的通知
	recvEvent(t, c1.closed, "c1 Close")    // FullRefresh 失败必须关闭当前连接

	rig.waitDelay(t, time.Second)
	expectNoEvent(t, c1.waiting, "失败连接 WaitForNotification")
	expectNoEvent(t, disp.applied, "Apply") // 初始化失败不得进入 consume
	expectNoEvent(t, rig.connected, "第二连接")

	rig.release(t) // 第二连接同样失败 → 再次关闭、2s 退避
	require.Same(t, c2, rig.recvConnect(t))
	require.Error(t, disp.nextFull(t))
	recvEvent(t, c2.closed, "c2 Close")
	rig.waitDelay(t, 2*time.Second)
	expectNoEvent(t, disp.applied, "Apply")
}

// TestListenerFullRefreshRecoverNextConn 第一连接 FullRefresh 失败、第二连接
// 成功：只有第二连接的通知被 Apply；第一连接通知随 Close 丢弃。
func TestListenerFullRefreshRecoverNextConn(t *testing.T) {
	c1, c2 := newGateConn(), newGateConn()
	disp := newGateDisp(1) // 首次失败，之后成功
	rig := newGateRig(t, []*gateConn{c1, c2}, disp)

	require.Same(t, c1, rig.recvConnect(t))
	require.Error(t, disp.nextFull(t))
	c1.ch <- notif(`{"v":1,"rules":true}`) // 旧连接通知：永不消费
	recvEvent(t, c1.closed, "c1 Close")
	rig.waitDelay(t, time.Second)
	rig.release(t)

	require.Same(t, c2, rig.recvConnect(t))
	require.NoError(t, disp.nextFull(t))
	expectNoEvent(t, c2.closed, "成功连接 Close")
	recvEvent(t, c2.waiting, "c2 consume 进入")
	c2.ch <- notif(`{"v":1,"users":true}`)
	got := recvEvent(t, disp.applied, "Apply")
	require.True(t, got.Users, "只允许 Apply 第二连接的通知")
	require.False(t, got.Rules, "第一连接的通知不得被 Apply")
	expectNoEvent(t, c1.waiting, "旧连接 WaitForNotification")
	require.Len(t, disp.got(), 1)
}

// TestListenerFullRefreshBackoffSequence 连续 FullRefresh 失败共用同一 attempt
// 退避：1s → 2s → 4s，失败期间无 consume、无忙循环。
func TestListenerFullRefreshBackoffSequence(t *testing.T) {
	plan := []*gateConn{newGateConn(), newGateConn(), newGateConn(), newGateConn()}
	disp := newGateDisp(-1)
	rig := newGateRig(t, plan, disp)

	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		require.Same(t, plan[i], rig.recvConnect(t))
		require.Error(t, disp.nextFull(t))
		recvEvent(t, plan[i].closed, "失败连接 Close")
		rig.waitDelay(t, want)
		expectNoEvent(t, disp.applied, "退避期间 Apply")
		rig.release(t)
	}
	require.Same(t, plan[3], rig.recvConnect(t))
	require.Error(t, disp.nextFull(t))
	expectNoEvent(t, disp.applied, "Apply")
}

// TestListenerFullRefreshAttemptResetOnSuccess 前两次失败、第三次成功进入
// consume；此后断线退避从 1s 重新开始（attempt 只在成功后复位）。
func TestListenerFullRefreshAttemptResetOnSuccess(t *testing.T) {
	plan := []*gateConn{newGateConn(), newGateConn(), newGateConn(), newGateConn()}
	disp := newGateDisp(2) // 失败 2 次 → 之后成功
	rig := newGateRig(t, plan, disp)

	require.Same(t, plan[0], rig.recvConnect(t))
	require.Error(t, disp.nextFull(t))
	recvEvent(t, plan[0].closed, "c1 Close")
	rig.waitDelay(t, time.Second)
	rig.release(t)

	require.Same(t, plan[1], rig.recvConnect(t))
	require.Error(t, disp.nextFull(t))
	recvEvent(t, plan[1].closed, "c2 Close")
	rig.waitDelay(t, 2*time.Second)
	rig.release(t)

	require.Same(t, plan[2], rig.recvConnect(t)) // c3 成功 → consume
	require.NoError(t, disp.nextFull(t))
	expectNoEvent(t, plan[2].closed, "成功连接 Close")
	recvEvent(t, plan[2].waiting, "c3 consume 进入")
	plan[2].errs <- errors.New("conn lost") // 消费中断线
	recvEvent(t, plan[2].closed, "断线 Close")

	rig.waitDelay(t, time.Second) // 断线退避从 1s 重新开始（attempt 已在成功时复位）
	rig.release(t)
	require.Same(t, plan[3], rig.recvConnect(t)) // c4 重连 + FullRefresh 成功
	require.NoError(t, disp.nextFull(t))
	expectNoEvent(t, disp.applied, "Apply")
}

// TestListenerFullRefreshCloseErrorTolerated FullRefresh 失败且连接 Close 返回
// 错误：错误经 closed 事件可观测，run 不受影响，照常退避重连并进入消费。
func TestListenerFullRefreshCloseErrorTolerated(t *testing.T) {
	closeBoom := errors.New("close boom")
	c1, c2 := newGateConn(), newGateConn()
	c1.closeErr = closeBoom
	disp := newGateDisp(1) // 首次失败，第二连接成功
	rig := newGateRig(t, []*gateConn{c1, c2}, disp)

	require.Same(t, c1, rig.recvConnect(t))
	require.Error(t, disp.nextFull(t))
	require.ErrorIs(t, recvEvent(t, c1.closed, "c1 Close 返回"), closeBoom)
	require.Equal(t, int32(1), c1.closeN.Load())
	rig.waitDelay(t, time.Second)
	rig.release(t)

	require.Same(t, c2, rig.recvConnect(t))
	require.NoError(t, disp.nextFull(t))
	recvEvent(t, c2.waiting, "c2 consume 进入")
	c2.ch <- notif(`{"v":1,"users":true}`)
	require.True(t, recvEvent(t, disp.applied, "Apply").Users)
}

// TestListenerCancelDuringFullRefresh FullRefresh 阻塞期间取消：以 ctx 错误收
// 场、连接关闭、run 退出，无 Apply、无第二连接。
func TestListenerCancelDuringFullRefresh(t *testing.T) {
	c1 := newGateConn()
	disp := newGateDisp(-1)
	disp.block = make(chan struct{}) // 永不释放，仅响应 ctx
	rig := newGateRig(t, []*gateConn{c1}, disp)

	require.Same(t, c1, rig.recvConnect(t))
	recvEvent(t, disp.fullIn, "FullRefresh 调用") // 此刻必阻塞在刷新中
	require.NoError(t, rig.l.Close(context.Background()))

	require.ErrorIs(t, disp.nextFull(t), context.Canceled)
	recvEvent(t, c1.closed, "取消后 Close")
	expectNoEvent(t, rig.connected, "第二连接")
	expectNoEvent(t, disp.applied, "Apply")
}

// TestListenerCancelDuringFullRefreshBackoff FullRefresh 失败进入退避后取消：
// sleep 立即响应，run 干净退出，不再重连。
func TestListenerCancelDuringFullRefreshBackoff(t *testing.T) {
	c1 := newGateConn()
	disp := newGateDisp(-1)
	rig := newGateRig(t, []*gateConn{c1}, disp)

	require.Same(t, c1, rig.recvConnect(t))
	require.Error(t, disp.nextFull(t))
	recvEvent(t, c1.closed, "c1 Close")
	rig.waitDelay(t, time.Second) // 挂起在退避，不释放

	require.NoError(t, rig.l.Close(context.Background()))
	expectNoEvent(t, rig.connected, "取消后重连")
	expectNoEvent(t, disp.applied, "Apply")
}

// TestListenerFullRefreshPermanentFailureUntilCancel FullRefresh 恒失败且
// BackoffMax=2s：退避请求被封顶为 1s → 2s → 2s，每次只在 timer 释放后重试；
// 每个失败连接都被 Close、全程零 WaitForNotification、零 Apply；第三次退避
// 挂起时取消 → run 干净退出，不再建立第四连接。
func TestListenerFullRefreshPermanentFailureUntilCancel(t *testing.T) {
	plan := []*gateConn{newGateConn(), newGateConn(), newGateConn()}
	disp := newGateDisp(-1)
	rig := newGateRig(t, plan, disp, func(cfg *ListenerConfig) { cfg.BackoffMax = 2 * time.Second })

	for i, want := range []time.Duration{time.Second, 2 * time.Second, 2 * time.Second} {
		require.Same(t, plan[i], rig.recvConnect(t))
		require.Error(t, disp.nextFull(t))
		expectNoEvent(t, plan[i].waiting, "失败连接 WaitForNotification")
		recvEvent(t, plan[i].closed, "失败连接 Close")
		rig.waitDelay(t, want) // 封顶退避：1s → 2s → 2s
		expectNoEvent(t, disp.applied, "退避期间 Apply")
		expectNoEvent(t, rig.connected, "释放前重连")
		if i < 2 {
			rig.release(t) // 第三次不释放：挂起中直接取消
		}
	}

	require.NoError(t, rig.l.Close(context.Background())) // 取消立即解除挂起 sleep
	expectNoEvent(t, rig.connected, "取消后第四连接")
	expectNoEvent(t, disp.applied, "Apply")
}
