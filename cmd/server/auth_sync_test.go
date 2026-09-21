// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestInstanceSrcUniqueAcrossInstances：同 hostname+pid 的两实例
// Src 必须不同（容器化多实例 pid 各自为 1——纯 hostname-pid 碰撞会互把对方
// NOTIFY 当自播跳过 → 失效静默全灭；随机 nonce 保证唯一）。
func TestInstanceSrcUniqueAcrossInstances(t *testing.T) {
	a, err := instanceSrc("srv-1", 1)
	require.NoError(t, err)
	b, err := instanceSrc("srv-1", 1)
	require.NoError(t, err)
	require.NotEqual(t, a, b, "同 hostname+pid 的两实例 Src 必须不同")
	require.Contains(t, a, "srv-1-1-", "Src 保留 hostname-pid 前缀（可读性/排障归因）")
	require.Equal(t, len(a), len(b), "同前缀下长度一致")
}

// recAuthN 记录 Reload 调用次数（auth-sync 周期测试目标）。
type recAuthN struct {
	mu sync.Mutex
	n  int
}

func (r *recAuthN) Reload(ctx context.Context) error { r.mu.Lock(); r.n++; r.mu.Unlock(); return nil }
func (r *recAuthN) calls() int                       { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// TestAuthSyncPeriodicReload 短周期（5ms）实测：周期到点调用 Auth.Reload
// （≥2 次）；Close 后不再增长。
func TestAuthSyncPeriodicReload(t *testing.T) {
	auth := &recAuthN{}
	w := newAuthSync(auth, 5*time.Millisecond, nil)
	require.Equal(t, "auth-sync", w.Name())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, w.Start(ctx))
	require.Error(t, w.Start(ctx), "重复 Start 返回错误（worker 契约）")

	require.Eventually(t, func() bool { return auth.calls() >= 2 }, time.Second, 10*time.Millisecond,
		"周期到点应触发 ≥2 次 Reload")

	require.NoError(t, w.Close(ctx))
	// 循环随 Start ctx 取消退出（Close no-op，与 invalidate/scheduler 同契约）
	cancel()
	time.Sleep(50 * time.Millisecond) // 等循环退出（select 上 ctx.Done 立即返回）
	before := auth.calls()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, before, auth.calls(), "ctx 取消后循环退出，不再 reload")
}

// TestAuthSyncNotStartedCloseSafe 未 Start 的 Close 安全（worker 契约）。
func TestAuthSyncNotStartedCloseSafe(t *testing.T) {
	w := newAuthSync(&recAuthN{}, 0, nil) // interval 0 → 默认 60s
	require.NoError(t, w.Close(context.Background()))
}

// recAuthErr 记录 Reload 调用次数并恒返回错误（失败路径注入）。
type recAuthErr struct {
	mu sync.Mutex
	n  int
}

func (r *recAuthErr) Reload(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return errors.New("reload boom")
}
func (r *recAuthErr) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// TestAuthSyncFailureObservable：Reload 失败注入——Warn（log 可空）
// + 循环不中断 + last_success（最近成功时刻）不前移 + Stats 失败计数/失败时刻。
// 此前 recAuthN 恒 nil、失败路径零覆盖。
func TestAuthSyncFailureObservable(t *testing.T) {
	auth := &recAuthErr{}
	w := newAuthSync(auth, 5*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, w.Start(ctx))

	require.Eventually(t, func() bool { return auth.calls() >= 2 }, time.Second, 10*time.Millisecond,
		"Reload 失败后循环必须继续（不中断）")
	st := w.Stats().(authSyncStats)
	require.GreaterOrEqual(t, st.Failures, int64(2), "每次失败都应累计计数")
	require.Zero(t, st.LastReloadUnixMs, "从未成功 → last_reload（最近成功时刻）不前移")
	require.Greater(t, st.LastFailureUnixMs, int64(0), "最近失败时刻应被记录")
}

// hangAuthN：Reload 阻塞直到 ctx 取消（模拟 DB 挂起），返回 ctx 错误。
type hangAuthN struct {
	mu sync.Mutex
	n  int
}

func (r *hangAuthN) Reload(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}
func (r *hangAuthN) calls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.n }

// TestAuthSyncReloadTimeoutUnblocksLoop：DB 挂起时 per-attempt
// 超时（生产 30s）后循环必须回 select 继续触发——60s 兜底不因单次挂起永久停摆
// （此前用生命周期 ctx 无超时，循环卡死即永久失效）。
func TestAuthSyncReloadTimeoutUnblocksLoop(t *testing.T) {
	auth := &hangAuthN{}
	w := newAuthSync(auth, 5*time.Millisecond, nil)
	w.timeout = 50 * time.Millisecond // 缩短 per-attempt 超时（生产 authSyncTimeout=30s）
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, w.Start(ctx))

	// calls 在 Reload 入口自增、失败在超时后才记录——直接轮询 Stats 的失败计数
	// （≥3 = 已发生 ≥3 次完整超时周期，循环确实回到 select 继续触发）。
	require.Eventually(t, func() bool { return w.Stats().(authSyncStats).Failures >= 3 }, 2*time.Second, 20*time.Millisecond,
		"DB 挂起时 per-attempt 超时（50ms）后循环必须回到 select 继续触发")
	st := w.Stats().(authSyncStats)
	require.GreaterOrEqual(t, st.Failures, int64(3), "每次超时都应计失败")
	require.Zero(t, st.LastReloadUnixMs, "挂起超时全部失败 → last_reload 不前移")
	require.Greater(t, st.LastFailureUnixMs, int64(0), "最近失败时刻应被记录")
}

// panicAuthN：Reload 恒 panic（注入）。
type panicAuthN struct{}

func (panicAuthN) Reload(ctx context.Context) error { panic("reload boom") }

// TestAuthSyncPanicRecovered：Reload panic → 托管 recover 捕获 →
// 进程不崩（无托管时本测试二进制直接崩溃即失败）；panic 后循环退出（running
// 复位，观测面一致）。
func TestAuthSyncPanicRecovered(t *testing.T) {
	w := newAuthSync(panicAuthN{}, 5*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, w.Start(ctx))
	time.Sleep(100 * time.Millisecond) // 让首个 tick 触发 panic 并经托管 recover 捕获
	require.False(t, w.running.Load(), "panic 后循环退出（running 复位）")
}
