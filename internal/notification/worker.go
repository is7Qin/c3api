// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

const queueCap = 256

type WarningMailEnqueue func(domain.BalanceWarningEvent, func(error)) error

type Worker struct {
	cooldown *Cooldown
	enabled  func() bool
	enqueue  WarningMailEnqueue
	log      *logx.Logger

	ch     chan domain.BalanceWarningEvent
	quit   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc

	mu      sync.Mutex
	started bool
	closed  bool

	evaluated          atomic.Int64
	admitted           atomic.Int64
	suppressed         atomic.Int64
	cooldownSuppressed atomic.Int64
	claimFailed        atomic.Int64
	releaseFailed      atomic.Int64
	dropped            atomic.Int64
	sent               atomic.Int64
	failed             atomic.Int64
	lastErr            atomic.Pointer[string]

	drainMu   sync.Mutex
	drainDone bool
	drainErr  error

	sentHook, failedHook, processedHook func()
	testPreDrainHook                    func()
}

func New(cooldown *Cooldown, enabled func() bool, enqueue WarningMailEnqueue, log *logx.Logger) *Worker {
	if cooldown == nil {
		panic("notification: New(nil cooldown)")
	}
	return &Worker{
		cooldown: cooldown,
		enabled:  enabled,
		enqueue:  enqueue,
		log:      log,
		ch:       make(chan domain.BalanceWarningEvent, queueCap),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (w *Worker) Name() string { return "notification" }

func (w *Worker) TrySubmit(event domain.BalanceWarningEvent) bool {
	w.evaluated.Add(1)
	if event.EntityID == 0 || event.ThresholdMillis <= 0 || event.Email == "" {
		w.suppressed.Add(1)
		return false
	}
	if event.EventType != domain.NotificationBalanceWarningCrossed || event.EntityType != domain.NotificationUser {
		w.suppressed.Add(1)
		return false
	}
	if event.BalanceMillis > event.ThresholdMillis {
		w.suppressed.Add(1)
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.dropped.Add(1)
		return false
	}
	select {
	case w.ch <- event:
		w.admitted.Add(1)
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("notification worker: already closed")
	}
	if w.started {
		return fmt.Errorf("notification worker: already started")
	}
	w.started = true
	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	done := w.done
	worker.GoRecover("notification", w.log, func() {
		defer close(done)
		worker.Loop(ctx, "notification", w.log, w.loop)
	})
	return nil
}

func (w *Worker) Close(ctx context.Context) (err error) {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.quit)
		if w.cancel != nil {
			w.cancel()
		}
	}
	started := w.started
	done := w.done
	w.mu.Unlock()
	if started {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if hook := w.testPreDrainHook; hook != nil {
		hook()
	}
	w.drainMu.Lock()
	defer w.drainMu.Unlock()
	if w.drainDone {
		return w.drainErr
	}
	defer func() {
		if recover() != nil {
			w.storeFailure(failureNotificationDrain)
			w.drainErr = errNotificationDrainPanicked
		}
		w.drainDone = true
		err = w.drainErr
	}()
	w.drainErr = w.drainWithBudget(ctx)
	return w.drainErr
}

func (w *Worker) loop(ctx context.Context) {
	for {
		select {
		case <-w.quit:
			return
		case <-ctx.Done():
			return
		case ev := <-w.ch:
			w.handleEvent(ctx, ev)
		}
	}
}

func (w *Worker) handleEvent(ctx context.Context, ev domain.BalanceWarningEvent) {
	if w.enqueue == nil || w.enabled == nil || !w.enabled() {
		w.finishSuppressed()
		return
	}
	token, claimed, err := w.cooldown.TryClaim(ctx, ev)
	if err != nil {
		w.claimFailed.Add(1)
		w.finishFailure(ev, "", failureCooldownClaim)
		return
	}
	if !claimed {
		w.cooldownSuppressed.Add(1)
		w.processed()
		return
	}
	var once sync.Once
	complete := func(deliveryErr error) {
		once.Do(func() {
			if deliveryErr == nil {
				w.sent.Add(1)
				if hook := w.sentHook; hook != nil {
					hook()
				}
				w.processed()
				return
			}
			w.finishFailure(ev, token, failureMailDelivery)
		})
	}
	func() {
		defer func() {
			if recover() == nil {
				return
			}
			once.Do(func() {
				w.finishFailure(ev, token, failureMailEnqueuePanic)
			})
			panic(errMailEnqueuePanicked)
		}()
		if err := w.enqueue(ev, complete); err != nil {
			once.Do(func() {
				w.finishFailure(ev, token, failureMailEnqueue)
			})
		}
	}()
}

func (w *Worker) processed() {
	if hook := w.processedHook; hook != nil {
		hook()
	}
}

func (w *Worker) releaseClaim(ev domain.BalanceWarningEvent, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := w.cooldown.Release(ctx, ev, token); err != nil {
		w.releaseFailed.Add(1)
		w.storeFailure(failureCooldownRelease)
	}
}
