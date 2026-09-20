// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

type failureRetryTask struct {
	accountID        int64
	fingerprint      string
	identityRevision int64
	reason           string
	deps             FailureDeps
	attempts         int
}

var (
	retryQueue      chan failureRetryTask
	retryCtx        context.Context
	retryCancel     context.CancelFunc
	retryOnce       sync.Once
	retryMu         sync.Mutex
	retryLog        *logx.Logger
	retryDone       <-chan struct{}
	retryBackoff    = 100 * time.Millisecond
	retryMaxBackoff = 5 * time.Second
	retryShutdown   bool
	// retryWG 跟踪在途 backoff 重投 goroutine：Close join 主循环后还要等它
	// 归零（Close ⇒ 无存活 retry goroutine——重投体对 ctx 取消即时响应，
	// 等待近瞬时；预算耗尽按 ctx.Err() 返回由调用方 Warn）。
	retryWG sync.WaitGroup
)

func ensureFailureRetryWorker() {
	retryMu.Lock()
	defer retryMu.Unlock()
	if retryShutdown {
		return
	}
	retryOnce.Do(func() {
		retryQueue = make(chan failureRetryTask, 1024)
		retryCtx, retryCancel = context.WithCancel(context.Background())
		retryDone = worker.GoLoop(retryCtx, "sdk-failure-retry", retryLog, failureRetryLoop)
	})
}

func enqueueFailureRetry(deps FailureDeps, accountID int64, fp string, identityRev int64, reason string) {
	retryMu.Lock()
	if retryShutdown {
		retryMu.Unlock()
		return
	}
	if retryCtx != nil && retryCtx.Err() != nil {
		retryMu.Unlock()
		return
	}
	retryMu.Unlock()
	ensureFailureRetryWorker()
	retryMu.Lock()
	if retryShutdown {
		retryMu.Unlock()
		return
	}
	if retryCtx != nil && retryCtx.Err() != nil {
		retryMu.Unlock()
		return
	}
	q := retryQueue
	retryMu.Unlock()
	if q == nil {
		return
	}
	task := failureRetryTask{accountID: accountID, fingerprint: fp, identityRevision: identityRev, reason: reason, deps: deps, attempts: 0}
	select {
	case q <- task:
	default:
		if deps.Log != nil {
			deps.Log.Warn("sdk failure retry queue full, dropping", logx.Int64("account_id", accountID))
		}
	}
}

func requeueWithBackoff(task failureRetryTask) {
	retryMu.Lock()
	if retryShutdown {
		retryMu.Unlock()
		return
	}
	if retryCtx != nil && retryCtx.Err() != nil {
		retryMu.Unlock()
		return
	}
	q := retryQueue
	ctx := retryCtx
	if q == nil || ctx == nil {
		retryMu.Unlock()
		return
	}
	backoff := backoffForAttempts(task.attempts)
	retryMu.Unlock()
	task.attempts++
	retryWG.Add(1)
	worker.GoRecover("sdk-failure-retry-backoff", retryLog, func() {
		defer retryWG.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		retryMu.Lock()
		if retryShutdown {
			retryMu.Unlock()
			return
		}
		if retryCtx != nil && retryCtx.Err() != nil {
			retryMu.Unlock()
			return
		}
		curQ := retryQueue
		retryMu.Unlock()
		if curQ == nil {
			return
		}
		select {
		case curQ <- task:
		default:
			if task.deps.Log != nil {
				task.deps.Log.Warn("sdk failure retry queue full on requeue, dropping", logx.Int64("account_id", task.accountID))
			}
		}
	})
}

func backoffForAttempts(attempts int) time.Duration {
	d := retryBackoff
	for i := 0; i < attempts; i++ {
		d *= 2
		if d >= retryMaxBackoff {
			return retryMaxBackoff
		}
	}
	if d > retryMaxBackoff {
		return retryMaxBackoff
	}
	return d
}

// failureRetryLoop supervised worker queue pattern: process lifetime until context cancel.
// Fair queue: each task gets single attempt per loop iteration; transient failures requeue with bounded per-item backoff.
func failureRetryLoop(ctx context.Context) {
	retryMu.Lock()
	q := retryQueue
	retryMu.Unlock()
	if q == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-q:
			retryMu.Lock()
			shutdown := retryShutdown
			rCtx := retryCtx
			retryMu.Unlock()
			if shutdown {
				return
			}
			if rCtx != nil && rCtx.Err() != nil {
				return
			}
			if handleRetryOnce(ctx, task) {
				requeueWithBackoff(task)
			}
		}
	}
}

func handleRetryOnce(ctx context.Context, task failureRetryTask) bool {
	cs, ok := task.deps.Store.(casStore)
	if !ok {
		return false
	}
	acct, err := cs.GetAccount(ctx, task.accountID)
	if err != nil {
		return true
	}
	if acct.DeletedAt != nil {
		if task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		return false
	}
	curFp, fpErr := canonicalFingerprint(acct)
	if fpErr != nil {
		if task.deps.Latch != nil && errors.Is(fpErr, ErrMissingCandidateFingerprint) {
			task.deps.Latch.Clear(task.accountID)
		}
		return false
	}
	if curFp != task.fingerprint {
		if task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		return false
	}
	// 围栏维度是 K（身份代际）：失效判决只在"身份未被授权变更"时仍有效。
	if acct.IdentityRevision != task.identityRevision {
		if acct.IdentityRevision > task.identityRevision && task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		return false
	}
	err = cs.FailAccountCAS(ctx, task.accountID, task.identityRevision, "sdk", time.Now(), task.reason)
	if err == nil {
		if task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		if task.deps.Publisher != nil {
			if gg, ok := task.deps.Store.(groupGetter); ok {
				gids, _ := gg.GetAccountGroups(context.WithoutCancel(ctx), task.accountID)
				if len(gids) > 0 {
					task.deps.Publisher.PublishGroups(context.WithoutCancel(ctx), gids)
				}
			}
		}
		return false
	}
	if errors.Is(err, repository.ErrStaleIdentityRevision) {
		if fresh, ferr := cs.GetAccount(ctx, task.accountID); ferr == nil && fresh.IdentityRevision > task.identityRevision && task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		return false
	}
	if !isTransientFailure(err) {
		return false
	}
	return true
}

// ShutdownFailureRetry stops retries for process shutdown; no retry after shutdown.
func ShutdownFailureRetry() {
	retryMu.Lock()
	defer retryMu.Unlock()
	retryShutdown = true
	if retryCancel != nil {
		retryCancel()
	}
}

// FailureRetryWorker adapts the process-lifetime SDK failure retry loop to the
// managed worker contract (Name/Start/Close)：main 注册进 ordered workers，
// Manager 反向排空时 Close 先于 Redis/client 释放执行并 join 循环——修掉
// 旧状态"惰性起循环、永不 join"的脱管生命周期。单次生命周期契约与 worker
// Manager 一致（Close 后 retryShutdown 恒置位，Start 不复活）。
type FailureRetryWorker struct{ log *logx.Logger }

// NewFailureRetryWorker constructs the adapter; log feeds the supervised loop.
func NewFailureRetryWorker(log *logx.Logger) *FailureRetryWorker {
	return &FailureRetryWorker{log: log}
}

// Name satisfies worker.Worker.
func (w *FailureRetryWorker) Name() string { return "sdk-failure-retry" }

// Start eagerly owns the retry loop (same lazy path reused as ensure — idempotent).
func (w *FailureRetryWorker) Start(context.Context) error {
	retryMu.Lock()
	if !retryShutdown {
		retryLog = w.log
	}
	retryMu.Unlock()
	ensureFailureRetryWorker()
	return nil
}

// Close shuts the retry worker down and joins the loop plus all in-flight
// backoff requeue goroutines before returning (bounded by ctx; budget
// exhaustion returns ctx.Err()). 重投体对 retryCancel 即时响应，等待近瞬时：
// Close 返回 ⇒ 无存活 retry goroutine，无 Redis/PG use-after-close 窗口。
func (w *FailureRetryWorker) Close(ctx context.Context) error {
	ShutdownFailureRetry()
	retryMu.Lock()
	done := retryDone
	retryMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wgDone := make(chan struct{})
	worker.GoRecover("sdk-failure-retry-wait", w.log, func() {
		retryWG.Wait()
		close(wgDone)
	})
	select {
	case <-wgDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ResetFailureRetryForTest resets global retry state for tests (single-threaded tests only).
func ResetFailureRetryForTest() {
	var done <-chan struct{}
	retryMu.Lock()
	if retryCancel != nil {
		retryCancel()
	}
	done = retryDone
	retryMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	// backoff 重投 goroutine 对上方 retryCancel 即时响应——排空后再复位状态，
	// 防旧 goroutine 写回新代 queue。
	retryWG.Wait()
	retryMu.Lock()
	defer retryMu.Unlock()
	retryShutdown = false
	retryOnce = sync.Once{}
	retryQueue = nil
	retryCtx = nil
	retryCancel = nil
	retryDone = nil
}
