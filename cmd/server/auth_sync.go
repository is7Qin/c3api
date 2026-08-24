// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/invalidate"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// authSyncInterval 鉴权快照周期兜底（设计文档 §1.6/§5 #9 / R1）：NOTIFY 丢失/
// 断连期间，key CRUD 与用户变更最长 60s 收敛。现状 auth 无周期 reload——这是
// 60s 兜底缺口的半侧，本 worker 补位。
const authSyncInterval = 60 * time.Second

// authSyncTimeout 单次 Reload per-attempt 超时（B4-2/p2-03）：DB 挂起时循环最长
// 阻塞本时长后必回 select——60s 兜底不因单次挂起永久停摆（此前用生命周期 ctx
// 无超时，DB 挂起 → 循环卡死 → 兜底永久失效）。
const authSyncTimeout = 30 * time.Second

// authSync 周期全量刷新鉴权快照 worker（worker.Worker 契约，Name="auth-sync"）：
// 每 interval 调一次 Auth.Reload。与 invalidate 的 users/keys 事件驱动分支并存
// （事件即时 + 周期兜底双保险）；Reload 内部持锁整体换快照（last-wins），与
// 事件驱动并发调用安全。
type authSync struct {
	auth     invalidate.AuthReloader
	interval time.Duration
	timeout  time.Duration // per-attempt Reload 超时（B4-2：0 兜底 → authSyncTimeout 30s；测试可缩短）
	log      *logx.Logger
	// goFn 托管 goroutine 启动器（B4-3/p2-03：裸 goroutine → worker.Manager.Go
	// 同契约——panic 捕获 + Warn，进程不崩，worker.go:6 承诺）。默认
	// worker.New(log).Go；测试可注入记录/替代实现。
	goFn      func(ctx context.Context, name string, fn func(context.Context))
	startOnce atomic.Bool
	// running/lastReload 观测面（/ops/workers）：循环存活 + 最近一次 Reload 成功
	// 完成时刻（B4-2：失败不前移——成败都记是"正常刷新"可观测性谎言）。
	running    atomic.Bool
	lastReload atomic.Int64
	// failures/lastFailure 失败观测面（B4-2）：Reload 失败累计次数 + 最近失败时刻。
	failures    atomic.Int64
	lastFailure atomic.Int64
}

// newAuthSync 构造周期 worker；interval <= 0 → authSyncInterval（60s）。
func newAuthSync(auth invalidate.AuthReloader, interval time.Duration, log *logx.Logger) *authSync {
	if interval <= 0 {
		interval = authSyncInterval
	}
	w := &authSync{auth: auth, interval: interval, timeout: authSyncTimeout, log: log}
	w.goFn = worker.New(log).Go // B4-3：托管 goroutine（recover 兜底）
	return w
}

// Name 满足 worker.Worker 契约。
func (w *authSync) Name() string { return "auth-sync" }

// Start 启动周期 reload goroutine（幂等：重复 Start 返回错误；worker 契约）。
func (w *authSync) Start(ctx context.Context) error {
	if !w.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("auth-sync: already started")
	}
	// B4-3：裸 goroutine → 托管（Manager.Go 契约的 recover：Reload 链 panic 不崩进程）。
	w.goFn(ctx, w.Name(), func(ctx context.Context) {
		w.running.Store(true) // 观测面：循环存活（退出即复位）
		defer w.running.Store(false)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// B4-2：per-attempt 超时——DB 挂起时 Reload 最长阻塞 w.timeout，
				// 循环必回 select（60s 兜底不因单次挂起永久停摆）。
				rc, cancel := context.WithTimeout(ctx, w.timeout)
				err := w.auth.Reload(rc)
				cancel()
				if err != nil {
					now := time.Now().UnixMilli()
					w.failures.Add(1)
					w.lastFailure.Store(now)
					if w.log != nil {
						w.log.Warn("auth periodic reload failed", logx.Error(err))
					}
				} else {
					w.lastReload.Store(time.Now().UnixMilli()) // 仅成功时前进（B4-2）
				}
			}
		}
	})
	return nil
}

// Close 无操作：循环随 Start 的 ctx 取消退出（worker 契约：幂等、未 Start 也
// 安全）。
func (w *authSync) Close(ctx context.Context) error { return nil }
