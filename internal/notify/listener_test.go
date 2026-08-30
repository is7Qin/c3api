// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// --- fake 连接 / fake dispatcher / rig ---

// fakeConn 可编程监听连接：ch 投递通知、errs 投递断开错误（WaitForNotification
// 按序消费两者）。
type fakeConn struct {
	ch   chan *pgconn.Notification
	errs chan error
}

func newFakeConn() *fakeConn {
	return &fakeConn{ch: make(chan *pgconn.Notification, 8), errs: make(chan error, 8)}
}

func (f *fakeConn) Listen(ctx context.Context, channel string) error { return nil }
func (f *fakeConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	select {
	case err := <-f.errs:
		return nil, err
	case n := <-f.ch:
		return n, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeConn) Close(ctx context.Context) error { return nil }

// fakeDisp 记录 Apply 收到的变更与 FullRefresh 次数。
type fakeDisp struct {
	mu      sync.Mutex
	applied []Change
	full    int
}

func (f *fakeDisp) Apply(ctx context.Context, ch Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, ch)
}
func (f *fakeDisp) FullRefresh(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.full++
	return nil
}
func (f *fakeDisp) applyCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.applied) }
func (f *fakeDisp) fullCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.full }
func (f *fakeDisp) got() []Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Change(nil), f.applied...)
}

// newLRig 构造监听器 rig：plan 为连接序列（nil = 连接失败）；fake 时钟即时
// 到期（跳过真实退避等待）。
func newLRig(t *testing.T, src string, plan []Conn, disp *fakeDisp) *lrig {
	t.Helper()
	if disp == nil {
		disp = &fakeDisp{}
	}
	l := NewListener(ListenerConfig{DSN: "postgres://fake", Src: src, Dispatcher: disp})
	var idx atomic.Int32
	l.connect = func(ctx context.Context, dsn string) (Conn, error) {
		n := int(idx.Add(1)) - 1
		if n < len(plan) {
			if plan[n] == nil {
				return nil, errors.New("connect refused")
			}
			return plan[n], nil
		}
		if len(plan) == 0 {
			return nil, errors.New("connect: no plan")
		}
		return plan[len(plan)-1], nil
	}
	l.newTimer = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, l.Start(ctx))
	r := &lrig{t: t, l: l, disp: disp, ctx: ctx, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		require.NoError(t, l.Close(context.Background()))
	})
	return r
}

type lrig struct {
	t      *testing.T
	l      *Listener
	disp   *fakeDisp
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *lrig) waitFor(fn func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatal("timeout waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}

func notif(payload string) *pgconn.Notification {
	return &pgconn.Notification{Channel: Channel, Payload: payload}
}

// --- 测试 ---

// TestListenerApplyAndSkipSelf 收到通知 → Apply 调用断言（解析出的 Change）；
// 自播（Src == 本实例 ID）跳过；空 Src 不跳过。
func TestListenerApplyAndSkipSelf(t *testing.T) {
	c1 := newFakeConn()
	r := newLRig(t, "i-1", []Conn{c1}, nil)
	r.waitFor(func() bool { return r.disp.fullCount() == 1 }) // 首连成功 → 全量刷新

	c1.ch <- notif(`{"v":1,"users":true,"src":"i-2"}`) // 他实例 → Apply
	r.waitFor(func() bool { return r.disp.applyCount() == 1 })
	c1.ch <- notif(`{"v":1,"keys":true,"src":"i-1"}`) // 自播 → 跳过
	c1.ch <- notif(`{"v":1,"settings":true}`)         // 空 Src → 不跳过
	r.waitFor(func() bool { return r.disp.applyCount() == 2 })

	got := r.disp.got()
	require.Equal(t, 2, len(got), "自播 NOTIFY 必须被跳过")
	require.True(t, got[0].Users)
	require.True(t, got[1].Settings)
	require.False(t, got[1].Keys, "skip 通知不得被解析应用")
}

// TestListenerFullRefreshOnStart 启动首连成功 → 立即一次全量刷新；后续通知
// 只走 Apply（不再 FullRefresh）。
func TestListenerFullRefreshOnStart(t *testing.T) {
	c1 := newFakeConn()
	r := newLRig(t, "", []Conn{c1}, nil)
	r.waitFor(func() bool { return r.disp.fullCount() == 1 })
	c1.ch <- notif(`{"v":1,"users":true}`)
	r.waitFor(func() bool { return r.disp.applyCount() == 1 })
	if got := r.disp.fullCount(); got != 1 {
		t.Fatalf("通知消费不应再触发全量刷新, full=%d", got)
	}
}

// TestListenerReconnect 连接失败退避重试 → 成功后 FullRefresh；消费中断线 →
// 退避重连 → 再次 FullRefresh（覆盖断连期间 NOTIFY 丢失）。
func TestListenerReconnect(t *testing.T) {
	c1, c2 := newFakeConn(), newFakeConn()
	r := newLRig(t, "", []Conn{nil, nil, c1, c2}, nil) // 2 次连接失败 → c1 → 断线 → c2
	r.waitFor(func() bool { return r.disp.fullCount() == 1 })

	c1.ch <- notif(`{"v":1,"users":true}`)
	r.waitFor(func() bool { return r.disp.applyCount() == 1 })

	c1.errs <- errors.New("conn lost") // 模拟断线
	r.waitFor(func() bool { return r.disp.fullCount() == 2 })

	c2.ch <- notif(`{"v":1,"rules":true}`) // 重连后的连接正常消费
	r.waitFor(func() bool { return r.disp.applyCount() == 2 })
}

// TestListenerParseErrorTolerated 非法载荷 → 告警跳过，循环继续。
func TestListenerParseErrorTolerated(t *testing.T) {
	c1 := newFakeConn()
	r := newLRig(t, "", []Conn{c1}, nil)
	r.waitFor(func() bool { return r.disp.fullCount() == 1 })

	c1.ch <- notif(`not-json`) // 解析失败：忽略继续
	c1.ch <- notif(`{"v":1,"users":true}`)
	r.waitFor(func() bool { return r.disp.applyCount() == 1 })
}

// TestListenerFullRefreshError FullRefresh 失败 = 初始化未完成：不消费、关闭
// 当前连接、退避释放前不重连；释放后仅当下一连接 FullRefresh 成功才允许 Apply。
func TestListenerFullRefreshError(t *testing.T) {
	c1, c2 := newGateConn(), newGateConn()
	disp := newGateDisp(1) // 首次失败，第二连接成功
	rig := newGateRig(t, []*gateConn{c1, c2}, disp)

	require.Same(t, c1, rig.recvConnect(t))
	require.Error(t, disp.nextFull(t), "首次刷新必须失败")
	c1.ch <- notif(`{"v":1,"users":true}`) // 失败连接的通知：永不消费
	recvEvent(t, c1.closed, "c1 Close")    // FullRefresh 失败必须关闭当前连接

	rig.waitDelay(t, time.Second) // run 挂起等退避释放，以下非阻塞检查皆确定性
	expectNoEvent(t, c1.waiting, "失败连接 WaitForNotification")
	expectNoEvent(t, disp.applied, "Apply")
	expectNoEvent(t, rig.connected, "第二连接")

	rig.release(t)
	require.Same(t, c2, rig.recvConnect(t))
	require.NoError(t, disp.nextFull(t))
	recvEvent(t, c2.waiting, "c2 consume 进入")
	c2.ch <- notif(`{"v":1,"users":true}`)
	got := recvEvent(t, disp.applied, "Apply")
	require.True(t, got.Users, "下一连接 FullRefresh 成功后才允许 Apply")
	expectNoEvent(t, c1.waiting, "旧连接 WaitForNotification")
	require.Len(t, disp.got(), 1)
}

// TestListenerCloseBeforeStart 未 Start 的 Close 安全（worker 契约）。
func TestListenerCloseBeforeStart(t *testing.T) {
	l := NewListener(ListenerConfig{DSN: "postgres://fake", Dispatcher: &fakeDisp{}})
	require.NoError(t, l.Close(context.Background()))
}

// TestListenerStartTwice 重复 Start 返回错误（worker 契约）。
func TestListenerStartTwice(t *testing.T) {
	r := newLRig(t, "", []Conn{newFakeConn()}, nil)
	require.Error(t, r.l.Start(r.ctx), "重复 Start 必须报错")
}

// TestListenerNilDispatcher Dispatcher 未注入 → Start 显式错误。
func TestListenerNilDispatcher(t *testing.T) {
	l := NewListener(ListenerConfig{DSN: "postgres://fake"})
	require.Error(t, l.Start(context.Background()))
}

// TestListenerDefaults NewListener 默认值：channel/退避/连接工厂。
func TestListenerDefaults(t *testing.T) {
	l := NewListener(ListenerConfig{DSN: "postgres://fake", Dispatcher: &fakeDisp{}})
	require.Equal(t, Channel, l.channel)
	require.Equal(t, time.Second, l.cfg.BackoffBase)
	require.Equal(t, 30*time.Second, l.cfg.BackoffMax)
	require.NotNil(t, l.connect)
}

// TestBackoffDelay 指数退避 1s→30s 封顶（1s,2s,4s,8s,16s,30s,30s…）。
func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second},
		{7, 30 * time.Second},
		{100, 30 * time.Second}, // 大 attempt 无溢出、封顶
	}
	for _, c := range cases {
		if got := backoffDelay(c.attempt, time.Second, 30*time.Second); got != c.want {
			t.Fatalf("backoffDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}
