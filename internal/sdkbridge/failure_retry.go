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
}

var (
	retryQueue    chan failureRetryTask
	retryCtx      context.Context
	retryCancel   context.CancelFunc
	retryOnce     sync.Once
	retryMu       sync.Mutex
	retryLog      *logx.Logger
	retryDone     <-chan struct{}
	retryBackoff  = 100 * time.Millisecond
	retryMaxBackoff = 5 * time.Second
	retryShutdown bool
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
	task := failureRetryTask{accountID: accountID, fingerprint: fp, revision: rev, reason: reason, deps: deps}
	select {
	case retryQueue <- task:
	default:
		// queue full: drop to avoid blocking caller; caller already got error
		if deps.Log != nil {
			deps.Log.Warn("sdk failure retry queue full, dropping", logx.Int64("account_id", accountID))
		}
	}
}

// failureRetryLoop supervised worker queue pattern: process lifetime until context cancel.
func failureRetryLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-retryQueue:
			doFailureRetry(ctx, task)
		}
	}
}

func doFailureRetry(ctx context.Context, task failureRetryTask) {
	backoff := retryBackoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-retryCtx.Done():
			return
		default:
		}
		cs, ok := task.deps.Store.(casStore)
		if !ok {
			return
		}
		// Re-read fresh account for fencing
		acct, err := cs.GetAccount(ctx, task.accountID)
		if err != nil {
			// transient read failure -> backoff retry
			if waitRetryBackoff(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		curFp, fpErr := canonicalFingerprint(acct)
		if fpErr != nil {
			// fenced: missing fingerprint
			return
		}
		if curFp != task.fingerprint {
			// stale: fingerprint changed, fenced
			if task.deps.Latch != nil {
				task.deps.Latch.Clear(task.accountID)
			}
			return
		}
		if acct.LifecycleRevision != task.revision {
			if acct.LifecycleRevision > task.revision && task.deps.Latch != nil {
				task.deps.Latch.Clear(task.accountID)
			}
			return
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
			return
		}
		if errors.Is(err, repository.ErrStaleRevision) {
			if fresh, ferr := cs.GetAccount(ctx, task.accountID); ferr == nil && fresh.LifecycleRevision > task.revision && task.deps.Latch != nil {
				task.deps.Latch.Clear(task.accountID)
			}
			return
		}
		if !isTransientFailure(err) {
			return
		}
		if waitRetryBackoff(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func waitRetryBackoff(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-retryCtx.Done():
		return true
	case <-time.After(d):
		return false
	}
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
