// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/logx"
)

type fakeWorker struct {
	name    string
	events  *[]string
	mu      *sync.Mutex
	startFn func() error
	closeFn func() error
}

func (f *fakeWorker) Name() string { return f.name }
func (f *fakeWorker) Start(context.Context) error {
	// events/mu 可为 nil（如 TestStartAllTwice 只关心双启动报错，不关心事件序列）
	if f.mu != nil {
		f.mu.Lock()
		*f.events = append(*f.events, "start:"+f.name)
		f.mu.Unlock()
	}
	if f.startFn != nil {
		return f.startFn()
	}
	return nil
}
func (f *fakeWorker) Close(context.Context) error {
	if f.mu != nil {
		f.mu.Lock()
		*f.events = append(*f.events, "close:"+f.name)
		f.mu.Unlock()
	}
	if f.closeFn != nil {
		return f.closeFn()
	}
	return nil
}

// startRegistersWorker：Start 内回调 Register——旧实现 StartAll 持锁 → 死锁；
// 新实现 started 已置位 → Register 返回错误而非死锁，StartAll 经回滚报错。
type startRegistersWorker struct {
	name   string
	m      *Manager
	added  *fakeWorker
	events *[]string
	mu     *sync.Mutex
}

func (w *startRegistersWorker) Name() string { return w.name }
func (w *startRegistersWorker) Start(context.Context) error {
	w.mu.Lock()
	*w.events = append(*w.events, "start:"+w.name)
	w.mu.Unlock()
	return w.m.Register(w.added)
}
func (w *startRegistersWorker) Close(context.Context) error {
	w.mu.Lock()
	*w.events = append(*w.events, "close:"+w.name)
	w.mu.Unlock()
	return nil
}

// newFileLogger 与 logx 包测试同构：JSON 行写入临时文件。
func newFileLogger(t *testing.T, level string) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "worker-test-")
	require.NoError(t, err)
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New(level, out)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return logger, out
}

// waitLog 轮询等待日志行落盘（日志在子 goroutine 的 recover 分支写入，
// 与测试 goroutine 之间无显式同步点）。
func waitLog(t *testing.T, out string, needle string) {
	t.Helper()
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && strings.Contains(string(b), needle)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestStartAllShutdownOrder(t *testing.T) {
	var events []string
	var mu sync.Mutex
	m := New(nil)
	m.Register(&fakeWorker{name: "a", events: &events, mu: &mu})
	m.Register(&fakeWorker{name: "b", events: &events, mu: &mu})
	require.NoError(t, m.StartAll(context.Background()))
	require.Equal(t, []string{"start:a", "start:b"}, events)
	require.NoError(t, m.Shutdown(context.Background()))
	require.Equal(t, []string{"start:a", "start:b", "close:b", "close:a"}, events)
}

func TestStartAllRollback(t *testing.T) {
	var events []string
	var mu sync.Mutex
	m := New(nil)
	m.Register(&fakeWorker{name: "a", events: &events, mu: &mu})
	m.Register(&fakeWorker{name: "b", events: &events, mu: &mu, startFn: func() error { return context.DeadlineExceeded }})
	err := m.StartAll(context.Background())
	require.Error(t, err)
	// 回滚含失败者自身 b（其可能已部分启动资源）：反向 close:b → close:a
	require.Equal(t, []string{"start:a", "start:b", "close:b", "close:a"}, events)
}

func TestShutdownContinuesAfterWorkerCloseError(t *testing.T) {
	// Given
	var events []string
	var mu sync.Mutex
	m := New(nil)
	require.NoError(t, m.Register(
		&fakeWorker{name: "later", events: &events, mu: &mu},
		&fakeWorker{name: "failing", events: &events, mu: &mu, closeFn: func() error { return context.Canceled }},
	))

	// When
	err := m.Shutdown(context.Background())

	// Then
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"close:failing", "close:later"}, events)
}

func TestStartAllTwice(t *testing.T) {
	m := New(nil)
	m.Register(&fakeWorker{name: "a"})
	require.NoError(t, m.StartAll(context.Background()))
	require.Error(t, m.StartAll(context.Background()))
}

func TestRegisterAfterStartAll(t *testing.T) {
	m := New(nil)
	require.NoError(t, m.StartAll(context.Background()))
	require.ErrorContains(t, m.Register(&fakeWorker{name: "x"}), "already started")
	// 未注册成功：不会出现在 Shutdown 排空里
	require.NoError(t, m.Shutdown(context.Background()))
}

// 并发 Register + StartAll 交错（-race）：Register 要么在快照内被启动、
// 要么在 started 置位后报 "already started"，无竞态无死锁。
func TestRegisterConcurrentWithStartAll(t *testing.T) {
	m := New(nil)
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- m.StartAll(context.Background()) }()
	var wg sync.WaitGroup
	var errs []error
	var errMu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := m.Register(&fakeWorker{name: fmt.Sprintf("c%d", n)}); err != nil {
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	select {
	case err := <-startErrCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("StartAll did not return")
	}
	errMu.Lock()
	defer errMu.Unlock()
	for _, err := range errs {
		require.ErrorContains(t, err, "already started")
	}
	require.NoError(t, m.Shutdown(context.Background()))
}

// Start 内回调 Register 不死锁回归：StartAll 返回错误（Register 在 started
// 置位后报错）而非死锁；回滚 Close 失败者自身；a-added 未注册成功不进排空。
func TestStartAllStartRegistersNoDeadlock(t *testing.T) {
	var events []string
	var mu sync.Mutex
	m := New(nil)
	m.Register(&startRegistersWorker{name: "a", m: m, added: &fakeWorker{name: "a-added"}, events: &events, mu: &mu})
	var startErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		startErr = m.StartAll(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartAll deadlocked on Register inside Start")
	}
	require.ErrorContains(t, startErr, "already started")
	require.Equal(t, []string{"start:a", "close:a"}, events)
	require.NoError(t, m.Shutdown(context.Background()))
}

func TestGoCatchesPanic(t *testing.T) {
	m := New(nil)
	done := make(chan struct{})
	m.Go(context.Background(), "boom", func(context.Context) {
		defer close(done)
		panic("test panic")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not run")
	}
}

// panic 注入 → 日志含 stack（E1-1）：Warn 行带 "stack" 字段与真实栈。
func TestGoPanicLogsStack(t *testing.T) {
	logger, out := newFileLogger(t, "warn")
	m := New(logger)
	done := make(chan struct{})
	m.Go(context.Background(), "boom", func(context.Context) {
		defer close(done)
		panic("test panic")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not run")
	}
	// panic 在 close(done) 之后才展开到 recover，日志在 recover 分支写——轮询落盘
	waitLog(t, out, "worker goroutine panicked")
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	s := string(b)
	require.Contains(t, s, `"stack":"goroutine `)
	require.Contains(t, s, "runtime/debug.Stack")
	require.Contains(t, s, `"panic":"test panic"`)
}

// 正常退出路径不受影响：无 panic 无日志。
func TestGoNormalExitNoLog(t *testing.T) {
	logger, out := newFileLogger(t, "warn")
	m := New(logger)
	done := make(chan struct{})
	m.Go(context.Background(), "fine", func(context.Context) { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not run")
	}
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "worker goroutine panicked")
}

// Shutdown 等待 Go 托管的慢 goroutine 退出（E1-2）：release 前 Shutdown
// 不返回，release 后随 goroutine 退出而返回。
func TestShutdownWaitsForGo(t *testing.T) {
	m := New(nil)
	release := make(chan struct{})
	exited := make(chan struct{})
	m.Go(context.Background(), "slow", func(context.Context) {
		<-release
		close(exited)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- m.Shutdown(ctx) }()
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned before Go goroutine exited")
	case <-time.After(100 * time.Millisecond):
		// 预期：Shutdown 在等慢 goroutine
	}
	close(release)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("Go goroutine did not exit")
	}
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after Go goroutine exit")
	}
}

// ctx 预算耗尽 → Warn + 不阻塞返回（E1-2）。
func TestShutdownWaitTimeoutWarnsNotBlock(t *testing.T) {
	logger, out := newFileLogger(t, "warn")
	m := New(logger)
	never := make(chan struct{})
	m.Go(context.Background(), "stuck", func(context.Context) { <-never })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Shutdown(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked past ctx deadline")
	}
	require.NoError(t, logger.Sync())
	waitLog(t, out, "worker goroutines still running at shutdown deadline")
}
