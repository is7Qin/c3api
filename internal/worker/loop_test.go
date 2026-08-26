// SPDX-License-Identifier: AGPL-3.0-or-later
package worker

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoop_PanicsAndRestarts(t *testing.T) {
	orig := loopRestartDelay
	loopRestartDelay = 10 * time.Millisecond
	t.Cleanup(func() { loopRestartDelay = orig })

	logger, out := newFileLogger(t, "error")
	ctx := context.Background()

	var calls atomic.Int32
	secondRun := make(chan struct{})
	fn := func(ctx context.Context) {
		n := calls.Add(1)
		if n == 1 {
			panic("boom first")
		}
		if n == 2 {
			close(secondRun)
			return
		}
	}

	GoLoop(ctx, "test-loop-restart", logger, fn)

	select {
	case <-secondRun:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not restart after panic")
	}

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	s := string(b)
	require.Contains(t, s, "worker loop panicked, restarting")
	require.Contains(t, s, "test-loop-restart")
	require.Contains(t, s, "boom first")
	require.GreaterOrEqual(t, calls.Load(), int32(2))
}

func TestLoop_CtxCancelStopsRestarts(t *testing.T) {
	orig := loopRestartDelay
	loopRestartDelay = 50 * time.Millisecond
	t.Cleanup(func() { loopRestartDelay = orig })

	logger, out := newFileLogger(t, "error")
	ctx, cancel := context.WithCancel(context.Background())

	var calls atomic.Int32
	fn := func(ctx context.Context) {
		calls.Add(1)
		panic("always boom")
	}

	GoLoop(ctx, "test-loop-cancel", logger, fn)

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && strings.Contains(string(b), "always boom")
	}, 2*time.Second, 10*time.Millisecond)

	cancel()

	n1 := calls.Load()
	// 静默期断言：等待一个重启周期+余量，确认不再有新调用（absence-proof）
	<-time.After(200 * time.Millisecond)
	n2 := calls.Load()
	require.LessOrEqual(t, n2-n1, int32(1))

	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "test-loop-cancel")
}

func TestLoop_NormalExitNoRestart(t *testing.T) {
	orig := loopRestartDelay
	loopRestartDelay = 10 * time.Millisecond
	t.Cleanup(func() { loopRestartDelay = orig })

	logger, out := newFileLogger(t, "error")
	ctx := context.Background()

	var calls atomic.Int32
	done := make(chan struct{})
	fn := func(ctx context.Context) {
		calls.Add(1)
		close(done)
	}

	GoLoop(ctx, "test-loop-normal", logger, fn)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not run")
	}
	<-time.After(50 * time.Millisecond)
	require.Equal(t, int32(1), calls.Load())
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "worker loop panicked")
}

func TestGoRecover_DoesNotCrash(t *testing.T) {
	logger, out := newFileLogger(t, "error")
	done := make(chan struct{})
	GoRecover("test-recover", logger, func() {
		defer close(done)
		panic("recover boom")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recover goroutine did not complete")
	}
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && strings.Contains(string(b), "recover boom")
	}, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(b), "worker goroutine panicked")
	require.Contains(t, string(b), "test-recover")
}

func TestCatchPanic_OneShot(t *testing.T) {
	logger, out := newFileLogger(t, "error")
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer CatchPanic("test-catch", logger)
		panic("catch boom")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("catch goroutine did not complete")
	}
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && strings.Contains(string(b), "catch boom")
	}, 2*time.Second, 10*time.Millisecond)
}
