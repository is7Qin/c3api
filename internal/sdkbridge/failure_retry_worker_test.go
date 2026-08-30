// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFailureRetryWorkerLifecycle verifies the retry loop runs under the
// managed worker contract: Start eagerly owns the loop, Close cancels and
// joins the worker.GoLoop completion channel before returning (bounded by
// ctx), and post-shutdown enqueues are dropped without panic.
func TestFailureRetryWorkerLifecycle(t *testing.T) {
	ResetFailureRetryForTest()
	defer ResetFailureRetryForTest()

	w := NewFailureRetryWorker(nil)
	require.Equal(t, "sdk-failure-retry", w.Name())

	require.NoError(t, w.Start(context.Background()))
	retryMu.Lock()
	q, ctxLive := retryQueue, retryCtx
	retryMu.Unlock()
	require.NotNil(t, q, "Start must own the retry queue")
	require.NotNil(t, ctxLive, "Start must own the retry context")
	require.NoError(t, ctxLive.Err())

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.Close(closeCtx), "Close must join the retry loop within budget")

	retryMu.Lock()
	done := retryDone
	retryMu.Unlock()
	require.NotNil(t, done)
	select {
	case <-done:
	default:
		require.FailNow(t, "retry loop completion channel not joined by Close")
	}
	require.ErrorIs(t, ctxLive.Err(), context.Canceled)

	// post-shutdown enqueue must be a safe no-op
	require.NotPanics(t, func() {
		enqueueFailureRetry(FailureDeps{}, 1, "fp", 1, "reason")
	})
}

// TestFailureRetryWorkerCloseWithoutStartSafe: Close before Start and repeated
// Close must be safe (worker.Worker contract).
func TestFailureRetryWorkerCloseWithoutStartSafe(t *testing.T) {
	ResetFailureRetryForTest()
	defer ResetFailureRetryForTest()

	w := NewFailureRetryWorker(nil)
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()))

	require.NoError(t, w.Start(context.Background()))
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()))
}

// TestFailureRetryWorkerCloseJoinsBackoffGoroutine（blocker：managed lifecycle
// 的完整语义 = Close 返回 ⇒ 无存活 retry goroutine）：backoff 重投 goroutine
// 曾以裸 `go` 起、Close 只 join 主循环——脱管窗口 + 违反 worker 监督契约。
// 重投体对取消即时响应，时间差不可靠，故用 join 组本身做确定性判据：
// (1) 真实 requeueWithBackoff 必须在 join 组内登记（现实现裸 go 不登记，
// Wait 立即可过——有界 watchdog 反向断言）；
// (2) join 组未排空时 Close 必须阻塞至预算耗尽返回 ctx.Err()（现实现无视
// join 组秒回 nil）。
func TestFailureRetryWorkerCloseJoinsBackoffGoroutine(t *testing.T) {
	ResetFailureRetryForTest()
	defer ResetFailureRetryForTest()

	w := NewFailureRetryWorker(nil)
	require.NoError(t, w.Start(context.Background()))

	// 前序测试会改写全局 backoff 旋钮且不还原——本测试自钉双值（backoff 必须
	// 大于 watchdog 探测窗，max 不得把 backoff 截回去）。
	oldBackoff, oldMax := retryBackoff, retryMaxBackoff
	retryBackoff = 300 * time.Millisecond
	retryMaxBackoff = 5 * time.Second
	t.Cleanup(func() { retryBackoff, retryMaxBackoff = oldBackoff, oldMax })

	requeueWithBackoff(failureRetryTask{accountID: 1, fingerprint: "fp", revision: 1})
	waited := make(chan struct{})
	go func() { retryWG.Wait(); close(waited) }()
	select {
	case <-waited:
		require.FailNow(t, "requeueWithBackoff must track its goroutine in the join group")
	case <-time.After(100 * time.Millisecond):
	}

	// 模拟一条尚未结束的在途重投（与真实 goroutine 同一 join 组）。
	var once sync.Once
	release := func() { once.Do(retryWG.Done) }
	defer release()
	retryWG.Add(1)
	closeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, w.Close(closeCtx), context.DeadlineExceeded,
		"Close must join in-flight backoff requeue goroutines before returning")
	release()
	require.NoError(t, w.Close(context.Background()),
		"Close must complete once the join group drains")
}
