// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/is7qin/c3api/internal/domain"
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

// FailureRetryWorker adapts the process-lifetime SDK failure retry loop to the
// managed worker contract (Name/Start/Close)：main 注册进 ordered workers，
// Manager 反向排空时 Close 先于 Redis/client 释放执行并 join 循环——修掉
// 旧状态"惰性起循环、永不 join"的脱管生命周期。单次生命周期契约与 worker
// Manager 一致（Close 后 shutdown 恒置位，Start 不复活）。
//
// 所有可变状态（队列/ctx/取消/once/log/done/退避旋钮/在途组/shutdown）均为
// 实例字段——消除包级可变全局（S8），测试互不串扰。进程内唯一实例经
// NewFailureRetryWorker 装配；HandleFailure 的瞬时失败入队经 defaultRetryWorker
// 路由（构造签名不变）。
type FailureRetryWorker struct {
	mu         sync.Mutex
	once       sync.Once
	queue      chan failureRetryTask
	ctx        context.Context
	cancel     context.CancelFunc
	log        *logx.Logger
	done       <-chan struct{}
	backoff    time.Duration
	maxBackoff time.Duration
	shutdown   bool
	// wg 跟踪在途 backoff 重投 goroutine：Close join 主循环后还要等它归零
	// （Close ⇒ 无存活 retry goroutine——重投体对 ctx 取消即时响应，等待近
	// 瞬时；预算耗尽按 ctx.Err() 返回由调用方 Warn）。
	wg sync.WaitGroup
}

func newRetryWorker(log *logx.Logger) *FailureRetryWorker {
	return &FailureRetryWorker{log: log, backoff: 100 * time.Millisecond, maxBackoff: 5 * time.Second}
}

// defaultRetryWorker 进程生命周期内唯一实例：HandleFailure 的瞬时失败入队经
// 它路由；main 经 NewFailureRetryWorker 装配同一实例。
var defaultRetryWorker = newRetryWorker(nil)

// ensure 惰性起循环（幂等）：首个入队或 Start 触发；shutdown 后不再起。
func (w *FailureRetryWorker) ensure() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.shutdown {
		return
	}
	w.once.Do(func() {
		w.queue = make(chan failureRetryTask, 1024)
		w.ctx, w.cancel = context.WithCancel(context.Background())
		w.done = worker.GoLoop(w.ctx, "sdk-failure-retry", w.log, w.loop)
	})
}

// enqueueFailureRetry 把瞬时失败入队到进程唯一重试实例（HandleFailure 调用）。
func enqueueFailureRetry(deps FailureDeps, accountID int64, fp string, identityRev int64, reason string) {
	defaultRetryWorker.enqueue(deps, accountID, fp, identityRev, reason)
}

func (w *FailureRetryWorker) enqueue(deps FailureDeps, accountID int64, fp string, identityRev int64, reason string) {
	w.mu.Lock()
	if w.shutdown {
		w.mu.Unlock()
		return
	}
	if w.ctx != nil && w.ctx.Err() != nil {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.ensure()
	w.mu.Lock()
	if w.shutdown {
		w.mu.Unlock()
		return
	}
	if w.ctx != nil && w.ctx.Err() != nil {
		w.mu.Unlock()
		return
	}
	q := w.queue
	w.mu.Unlock()
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

func (w *FailureRetryWorker) requeueWithBackoff(task failureRetryTask) {
	w.mu.Lock()
	if w.shutdown {
		w.mu.Unlock()
		return
	}
	if w.ctx != nil && w.ctx.Err() != nil {
		w.mu.Unlock()
		return
	}
	q := w.queue
	ctx := w.ctx
	if q == nil || ctx == nil {
		w.mu.Unlock()
		return
	}
	backoff := w.backoffForAttempts(task.attempts)
	w.mu.Unlock()
	task.attempts++
	w.wg.Add(1)
	worker.GoRecover("sdk-failure-retry-backoff", w.log, func() {
		defer w.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		w.mu.Lock()
		if w.shutdown {
			w.mu.Unlock()
			return
		}
		if w.ctx != nil && w.ctx.Err() != nil {
			w.mu.Unlock()
			return
		}
		curQ := w.queue
		w.mu.Unlock()
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

// backoffForAttempts 指数退避封顶（调用方持 w.mu）。
func (w *FailureRetryWorker) backoffForAttempts(attempts int) time.Duration {
	d := w.backoff
	for i := 0; i < attempts; i++ {
		d *= 2
		if d >= w.maxBackoff {
			return w.maxBackoff
		}
	}
	if d > w.maxBackoff {
		return w.maxBackoff
	}
	return d
}

// loop supervised worker queue pattern: process lifetime until context cancel.
// Fair queue: each task gets single attempt per loop iteration; transient failures requeue with bounded per-item backoff.
func (w *FailureRetryWorker) loop(ctx context.Context) {
	w.mu.Lock()
	q := w.queue
	w.mu.Unlock()
	if q == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-q:
			w.mu.Lock()
			shutdown := w.shutdown
			rCtx := w.ctx
			w.mu.Unlock()
			if shutdown {
				return
			}
			if rCtx != nil && rCtx.Err() != nil {
				return
			}
			if handleRetryOnce(ctx, task) {
				w.requeueWithBackoff(task)
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
	acct, err = ensureTemplate(ctx, cs, acct, task.accountID)
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
	err = cs.FailAccountCAS(ctx, task.accountID, task.identityRevision, domain.FailureSourceSDK, time.Now(), task.reason)
	if err == nil {
		if task.deps.Latch != nil {
			task.deps.Latch.Clear(task.accountID)
		}
		if task.deps.Publisher != nil {
			if gg, ok := task.deps.Store.(groupGetter); ok {
				gids, gerr := gg.GetAccountGroups(context.WithoutCancel(ctx), task.accountID)
				if gerr != nil {
					// 组失效是 best-effort：取组失败不阻断失效链（CAS 已成功），
					// 但不得静默吞错——记一条 Warn 供排查。
					if task.deps.Log != nil {
						task.deps.Log.Warn("account groups lookup failed", logx.Int64("account_id", task.accountID), logx.Error(gerr))
					}
				} else if len(gids) > 0 {
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
	defaultRetryWorker.shutdownRetry()
}

func (w *FailureRetryWorker) shutdownRetry() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.shutdown = true
	if w.cancel != nil {
		w.cancel()
	}
}

// NewFailureRetryWorker constructs the adapter; log feeds the supervised loop.
// 构造签名不变：返回并装配进程生命周期内唯一实例 defaultRetryWorker。
func NewFailureRetryWorker(log *logx.Logger) *FailureRetryWorker {
	w := defaultRetryWorker
	w.mu.Lock()
	w.log = log
	w.mu.Unlock()
	return w
}

// Name satisfies worker.Worker.
func (w *FailureRetryWorker) Name() string { return "sdk-failure-retry" }

// Start eagerly owns the retry loop (same lazy path reused as ensure — idempotent).
func (w *FailureRetryWorker) Start(context.Context) error {
	w.ensure()
	return nil
}

// Close shuts the retry worker down and joins the loop plus all in-flight
// backoff requeue goroutines before returning (bounded by ctx; budget
// exhaustion returns ctx.Err()). 重投体对 cancel 即时响应，等待近瞬时：
// Close 返回 ⇒ 无存活 retry goroutine，无 Redis/PG use-after-close 窗口。
func (w *FailureRetryWorker) Close(ctx context.Context) error {
	w.shutdownRetry()
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wgDone := make(chan struct{})
	worker.GoRecover("sdk-failure-retry-wait", w.log, func() {
		w.wg.Wait()
		close(wgDone)
	})
	select {
	case <-wgDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ResetFailureRetryForTest resets the process retry instance state for tests
// (single-threaded tests only).
func ResetFailureRetryForTest() {
	w := defaultRetryWorker
	var done <-chan struct{}
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	done = w.done
	w.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	// backoff 重投 goroutine 对上方 cancel 即时响应——排空后再复位状态，
	// 防旧 goroutine 写回新代 queue。
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.shutdown = false
	w.once = sync.Once{}
	w.queue = nil
	w.ctx = nil
	w.cancel = nil
	w.done = nil
}
