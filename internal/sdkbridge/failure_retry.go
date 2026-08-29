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
	accountID   int64
	fingerprint string
	revision    int64
	reason      string
	deps      FailureDeps
	attempts    int
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
)

func ensureFailureRetryWorker() {
	if retryShutdown {
		return
	}
	retryOnce.Do(func() {
		retryQueue = make(chan failureRetryTask, 1024)
		retryCtx, retryCancel = context.WithCancel(context.Background())
		retryDone = worker.GoLoop(retryCtx, "sdk-failure-retry", retryLog, failureRetryLoop)
	})
}

func enqueueFailureRetry(deps FailureDeps, accountID int64, fp string, rev int64, reason string) {
	if retryShutdown {
		return
	}
	if retryCtx != nil && retryCtx.Err() != nil {
		return
	}
	ensureFailureRetryWorker()
	if retryShutdown {
		return
	}
	if retryCtx != nil && retryCtx.Err() != nil {
		return
	}
	task := failureRetryTask{accountID: accountID, fingerprint: fp, revision: rev, reason: reason, deps: deps, attempts: 0}
	select {
	case retryQueue <- task:
	default:
		if deps.Log != nil {
			deps.Log.Warn("sdk failure retry queue full, dropping", logx.Int64("account_id", accountID))
		}
	}
}

func requeueWithBackoff(task failureRetryTask) {
	if retryShutdown {
		return
	}
	if retryCtx != nil && retryCtx.Err() != nil {
		return
	}
	backoff := backoffForAttempts(task.attempts)
	task.attempts++
	go func(t failureRetryTask, d time.Duration) {
		select {
		case <-retryCtx.Done():
			return
		case <-time.After(d):
		}
		if retryShutdown {
			return
		}
		if retryCtx != nil && retryCtx.Err() != nil {
			return
		}
		select {
		case retryQueue <- t:
		default:
			if t.deps.Log != nil {
				t.deps.Log.Warn("sdk failure retry queue full on requeue, dropping", logx.Int64("account_id", t.accountID))
			}
		}
	}(task, backoff)
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
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-retryQueue:
			if retryCtx != nil && retryCtx.Err() != nil {
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
	if acct.LifecycleRevision != task.revision {
		if acct.LifecycleRevision > task.revision && task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		return false
	}
	err = cs.FailAccountCAS(ctx, task.accountID, task.revision, "sdk", time.Now(), task.reason)
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
	if errors.Is(err, repository.ErrStaleRevision) {
		if fresh, ferr := cs.GetAccount(ctx, task.accountID); ferr == nil && fresh.LifecycleRevision > task.revision && task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		return false
	}
	if !isTransientFailure(err) {
		return false
	}
	return true
}

func nextBackoff(cur time.Duration) time.Duration {
	n := cur * 2
	if n > retryMaxBackoff {
		return retryMaxBackoff
	}
	return n
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

// ResetFailureRetryForTest resets global retry state for tests (single-threaded tests only).
func ResetFailureRetryForTest() {
	retryMu.Lock()
	defer retryMu.Unlock()
	if retryCancel != nil {
		retryCancel()
	}
	retryShutdown = false
	retryOnce = sync.Once{}
	retryQueue = nil
	retryCtx = nil
	retryCancel = nil
	retryDone = nil
}
