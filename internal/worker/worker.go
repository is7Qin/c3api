// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package worker 提供统一后台任务抽象（Global Constraints）：顺序启动、
// 反向排空、panic 捕获（进程不崩）。
package worker

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/is7qin/c3api/pkg/logx"
)

// Worker 是后台任务统一契约：Start 非阻塞启动内部 goroutine，Close 排空/释放。
// 两者必须幂等；Close 在未 Start 时也必须安全。
type Worker interface {
	Name() string
	Start(ctx context.Context) error
	Close(ctx context.Context) error
}

// Manager 统一装配后台任务：顺序启动、反向排空、panic 捕获（进程不崩）。
// 单次生命周期：StartAll/Shutdown 各只允许一次，重启需重建 Manager——复位
// started 是伪修复（worker 层 startOnce 均不复位，热重启是伪需求）。
type Manager struct {
	log     *logx.Logger
	mu      sync.Mutex
	wg      sync.WaitGroup // Go 托管的 goroutine 跟踪（Add 于 Go 内、Done 于 recover 分支后）
	workers []Worker
	started atomic.Bool
}

func New(log *logx.Logger) *Manager { return &Manager{log: log} }

// Register 追加 worker；StartAll 已开始（started 置位）后调用返回错误——
// 否则 worker 永不经 Start 却会在 Shutdown 被 Close。返回值可忽略
// （main.go 调用点仅位于 StartAll 之前，恒成功）。
func (m *Manager) Register(ws ...Worker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.Load() {
		return fmt.Errorf("worker manager: already started")
	}
	m.workers = append(m.workers, ws...)
	return nil
}

// StartAll 按注册顺序启动；任一失败则已启动者与失败者自身（可能已部分启动
// 资源）反向 Close 后返回错误。启动期间不持 Manager 锁：并发 Register 要么
// 进入快照、要么在 started 置位后报错——Start 内回调 Register 不再死锁。
// 不加启动超时：已证实现状无阻塞 Start，超时属行为新增。
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.Lock()
	if m.started.Swap(true) {
		m.mu.Unlock()
		return fmt.Errorf("worker manager: already started")
	}
	ws := make([]Worker, len(m.workers))
	copy(ws, m.workers)
	m.mu.Unlock()

	started := make([]Worker, 0, len(ws))
	for _, w := range ws {
		if err := w.Start(ctx); err != nil {
			started = append(started, w)
			for i := len(started) - 1; i >= 0; i-- {
				if cerr := started[i].Close(ctx); cerr != nil && m.log != nil {
					m.log.Warn("worker close failed on start rollback", logx.String("worker", started[i].Name()), logx.Error(cerr))
				}
			}
			return fmt.Errorf("worker %s start: %w", w.Name(), err)
		}
		started = append(started, w)
	}
	return nil
}

// Shutdown 反向顺序 Close（后注册先关），收集并返回首个错误；随后等待 Go
// 托管的 goroutine 全部退出（复用传入 ctx 限时——预算耗尽 Warn 不阻塞退出；
// 唯一用户 http-server 的退出已由调用方停机链保证，Wait 不引入新阻塞点）。
// 串行持锁排空是固有属性：单 Close 阻塞压缩后续排空预算，可接受。Go 必须在
// Shutdown 之前调用（单次生命周期契约，与 WaitGroup 用法一致）。
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for i := len(m.workers) - 1; i >= 0; i-- {
		w := m.workers[i]
		if err := w.Close(ctx); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("worker %s close: %w", w.Name(), err)
			}
			if m.log != nil {
				m.log.Warn("worker close failed", logx.String("worker", w.Name()), logx.Error(err))
			}
		}
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if m.log != nil {
			m.log.Warn("worker goroutines still running at shutdown deadline", logx.Error(ctx.Err()))
		}
	}
	return firstErr
}

// Go 托管命名 goroutine：panic 捕获 + Warn（含栈），进程不崩。
// 边界（不引入"panic 后回调/重启"抽象）：唯一用户 http-server 的
// ListenAndServe 错误路径已有 fatalf 兜底、handler panic 由 net/http 内置
// recover 兜底返回 500——回调无消费者。m.log==nil 时静默（New 恒注入，
// 现状全调用点恒非 nil）。
func (m *Manager) Go(ctx context.Context, name string, fn func(ctx context.Context)) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done() // defer LIFO：recover 分支先执行，Done 在其后
		defer func() {
			if r := recover(); r != nil && m.log != nil {
				m.log.Warn("worker goroutine panicked",
					logx.String("worker", name),
					logx.Any("panic", r),
					logx.String("stack", string(debug.Stack())),
				)
			}
		}()
		fn(ctx)
	}()
}
