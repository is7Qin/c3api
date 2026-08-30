// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

// FullRefresh 初始化闸门回归（设计文档 2026-08-29-notify-full-refresh-retry）：
// consume 的唯一合法入边是同连接 FullRefresh 返回 nil。全部用 channel 屏障 +
// 手动 timer，无 time.Sleep / t.Parallel / 真实 PG。

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- gate 替身 ---

// gateConn 带 Listen/Close 计数的假连接（通知/断开面复用 fakeConn）。
type gateConn struct {
	*fakeConn
	listenN atomic.Int32
	closeN  atomic.Int32
}

func newGateConn() *gateConn { return &gateConn{fakeConn: newFakeConn()} }

func (g *gateConn) Listen(ctx context.Context, channel string) error {
	g.listenN.Add(1)
	return nil
}
func (g *gateConn) Close(ctx context.Context) error {
	g.closeN.Add(1)
	return nil
}

// gateDisp 可编程 FullRefresh：failsLeft>0 失败后转成功；-1 恒失败；block 非空
// 时 FullRefresh 阻塞至 block 关闭或 ctx 取消（取消感知）。
type gateDisp struct {
	fakeDisp
	failsLeft int
	block     chan struct{}
}

func newGateDisp(failsLeft int) *gateDisp { return &gateDisp{failsLeft: failsLeft} }

func (d *gateDisp) FullRefresh(ctx context.Context) error {
	d.mu.Lock()
	d.full++
	fails := d.failsLeft
	if fails > 0 {
		d.failsLeft--
	}
	d.mu.Unlock()
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fails != 0 {
		return errors.New("refresh boom")
	}
	return nil
}

func (d *gateDisp) waitFull(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return d.fullCount() >= n },
		2*time.Second, 5*time.Millisecond, "FullRefresh 调用次数未达 %d", n)
}

// gateRig 手动退避时钟：newTimer 记录请求时长并挂起等待释放。
type gateRig struct {
	l        *Listener
	disp     *gateDisp
	connectN atomic.Int32
	delays   chan time.Duration  // 退避请求（时长）
	pending  chan chan time.Time // 等待释放的 timer
}

func newGateRig(t *testing.T, plan []*gateConn, disp *gateDisp) *gateRig {
	t.Helper()
	l := NewListener(ListenerConfig{DSN: "postgres://fake", Dispatcher: disp})
	r := &gateRig{
		l:       l,
		disp:    disp,
		delays:  make(chan time.Duration, 16),
		pending: make(chan chan time.Time, 16),
	}
	l.connect = func(ctx context.Context, dsn string) (Conn, error) {
		n := int(r.connectN.Add(1)) - 1
		if n >= len(plan) {
			return nil, errors.New("connect: plan exhausted")
		}
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

// waitDelay 断言下一次退避请求时长为 want（未释放前 run 挂起在 sleep）。
func (r *gateRig) waitDelay(t *testing.T, want time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		select {
		case d := <-r.delays:
			if d == want {
				return true
			}
			select {
			case r.delays <- d:
			default:
			}
			return false
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "未观察到 %v 退避", want)
}

// release 释放最近一次挂起的退避 timer。
func (r *gateRig) release(t *testing.T) {
	t.Helper()
	var ch chan time.Time
	require.Eventually(t, func() bool {
		select {
		case ch = <-r.pending:
			return true
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "无挂起退避可释放")
	ch <- time.Now()
}

// --- 测试 ---

// TestListenerFullRefreshFailureNoConsume FullRefresh 恒失败：连接关闭、通知
// 不消费（Apply=0）、退避释放前不建立下一连接。
func TestListenerFullRefreshFailureNoConsume(t *testing.T) {
	c1, c2 := newGateConn(), newGateConn()
	disp := newGateDisp(-1)
	rig := newGateRig(t, []*gateConn{c1, c2}, disp)

	disp.waitFull(t, 1)
	c1.ch <- notif(`{"v":1,"users":true}`) // 失败连接队列里的通知

	require.Eventually(t, func() bool { return c1.closeN.Load() == 1 },
		2*time.Second, 5*time.Millisecond, "FullRefresh 失败必须关闭当前连接")
	rig.waitDelay(t, time.Second)
	require.Zero(t, disp.applyCount(), "初始化失败不得进入 consume")
	require.Equal(t, int32(1), rig.connectN.Load(), "退避未释放前不得重连")

	rig.release(t) // 第二连接同样失败 → 再次关闭、2s 退避
	disp.waitFull(t, 2)
	require.Eventually(t, func() bool { return c2.closeN.Load() == 1 },
		2*time.Second, 5*time.Millisecond)
	rig.waitDelay(t, 2*time.Second)
	require.Zero(t, disp.applyCount())
}

// TestListenerFullRefreshRecoverNextConn 第一连接 FullRefresh 失败、第二连接
// 成功：只有第二连接的通知被 Apply；第一连接通知随 Close 丢弃。
func TestListenerFullRefreshRecoverNextConn(t *testing.T) {
	c1, c2 := newGateConn(), newGateConn()
	disp := newGateDisp(1) // 首次失败，之后成功
	rig := newGateRig(t, []*gateConn{c1, c2}, disp)

	disp.waitFull(t, 1)
	c1.ch <- notif(`{"v":1,"rules":true}`) // 旧连接通知：永不消费
	require.Eventually(t, func() bool { return c1.closeN.Load() == 1 },
		2*time.Second, 5*time.Millisecond)
	rig.waitDelay(t, time.Second)
	rig.release(t)

	disp.waitFull(t, 2)
	require.Zero(t, c2.closeN.Load(), "成功连接不得被关闭")
	c2.ch <- notif(`{"v":1,"users":true}`)
	require.Eventually(t, func() bool { return disp.applyCount() == 1 },
		2*time.Second, 5*time.Millisecond, "第二连接 FullRefresh 成功后必须消费")
	got := disp.got()
	require.Len(t, got, 1)
	require.True(t, got[0].Users, "只允许 Apply 第二连接的通知")
	require.False(t, got[0].Rules, "第一连接的通知不得被 Apply")
}

// TestListenerFullRefreshBackoffSequence 连续 FullRefresh 失败共用同一 attempt
// 退避：1s → 2s → 4s，失败期间无 consume、无忙循环。
func TestListenerFullRefreshBackoffSequence(t *testing.T) {
	plan := []*gateConn{newGateConn(), newGateConn(), newGateConn(), newGateConn()}
	disp := newGateDisp(-1)
	rig := newGateRig(t, plan, disp)

	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		disp.waitFull(t, i+1)
		require.Eventually(t, func() bool { return plan[i].closeN.Load() == 1 },
			2*time.Second, 5*time.Millisecond)
		rig.waitDelay(t, want)
		require.Zero(t, disp.applyCount(), "退避期间不得消费")
		rig.release(t)
	}
	disp.waitFull(t, 4)
	require.Zero(t, disp.applyCount())
}

// TestListenerFullRefreshAttemptResetOnSuccess 前两次失败、第三次成功进入
// consume；此后断线退避从 1s 重新开始（attempt 只在成功后复位）。
func TestListenerFullRefreshAttemptResetOnSuccess(t *testing.T) {
	plan := []*gateConn{newGateConn(), newGateConn(), newGateConn(), newGateConn()}
	disp := newGateDisp(2) // 失败 2 次 → 之后成功
	rig := newGateRig(t, plan, disp)

	disp.waitFull(t, 1)
	rig.waitDelay(t, time.Second)
	rig.release(t)
	disp.waitFull(t, 2)
	rig.waitDelay(t, 2*time.Second)
	rig.release(t)

	disp.waitFull(t, 3) // c3 成功 → consume
	require.Zero(t, plan[2].closeN.Load(), "成功连接不得被关闭")
	plan[2].errs <- errors.New("conn lost") // 消费中断线

	rig.waitDelay(t, time.Second) // 断线退避从 1s 重新开始（attempt 已在成功时复位）
	rig.release(t)
	disp.waitFull(t, 4) // c4 重连 + FullRefresh
	require.Zero(t, disp.applyCount(), "失败连接的通知从未消费")
}

// TestListenerCancelDuringFullRefresh FullRefresh 阻塞期间取消：以 ctx 错误收
// 场、连接关闭、run 退出，无 Apply、无第二连接。
func TestListenerCancelDuringFullRefresh(t *testing.T) {
	c1 := newGateConn()
	disp := newGateDisp(-1)
	disp.block = make(chan struct{}) // 永不释放，仅响应 ctx
	rig := newGateRig(t, []*gateConn{c1}, disp)

	disp.waitFull(t, 1)
	require.NoError(t, rig.l.Close(context.Background())) // 取消 + 等 run 退出

	require.Equal(t, int32(1), c1.closeN.Load(), "取消后连接必须关闭")
	require.Equal(t, int32(1), rig.connectN.Load(), "不得建立第二连接")
	require.Zero(t, disp.applyCount())
}

// TestListenerCancelDuringFullRefreshBackoff FullRefresh 失败进入退避后取消：
// sleep 立即响应，run 干净退出，不再重连。
func TestListenerCancelDuringFullRefreshBackoff(t *testing.T) {
	c1 := newGateConn()
	disp := newGateDisp(-1)
	rig := newGateRig(t, []*gateConn{c1}, disp)

	disp.waitFull(t, 1)
	require.Eventually(t, func() bool { return c1.closeN.Load() == 1 },
		2*time.Second, 5*time.Millisecond)
	rig.waitDelay(t, time.Second) // 挂起在退避，不释放

	require.NoError(t, rig.l.Close(context.Background()))
	require.Equal(t, int32(1), rig.connectN.Load(), "取消后不得重连")
	require.Zero(t, disp.applyCount())
}
