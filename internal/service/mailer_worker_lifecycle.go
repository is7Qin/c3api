// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/worker"
)

func (w *MailWorker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("mail worker: already closed")
	}
	if w.started {
		return fmt.Errorf("mail worker: already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.started = true
	w.cancel = cancel
	worker.GoRecover("email", w.svc.log, func() {
		defer close(w.senderDone)
		worker.Loop(runCtx, "email", w.svc.log, w.loop)
	})
	return nil
}

func (w *MailWorker) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		if w.cancel != nil {
			w.cancel()
		}
	}
	started := w.started
	senderDone := w.senderDone
	w.mu.Unlock()

	if started {
		select {
		case <-senderDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return w.drainOnce(ctx)
}

func (w *MailWorker) drainOnce(ctx context.Context) (err error) {
	w.mu.Lock()
	if w.drainComplete {
		err := w.drainErr
		w.mu.Unlock()
		return err
	}
	if w.drainStarted {
		done := w.drainDone
		w.mu.Unlock()
		select {
		case <-done:
			w.mu.Lock()
			err := w.drainErr
			w.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.drainStarted = true
	hook := w.testDrainStarted
	w.mu.Unlock()
	defer func() {
		if recover() != nil {
			err = errMailWorkerPanicked
			w.storeMailFailure(err)
		}
		w.mu.Lock()
		w.drainErr = err
		w.drainComplete = true
		close(w.drainDone)
		w.mu.Unlock()
	}()

	if hook != nil {
		hook()
	}
	err = w.drainRemaining(ctx)
	return err
}

func (w *MailWorker) loop(ctx context.Context) {
	var pending BalanceWarningMailTask
	hasPending := false
	defer func() {
		if !hasPending {
			return
		}
		err := ctx.Err()
		if err == nil {
			err = errMailWorkerPanicked
		}
		w.failed.Add(1)
		w.storeMailFailure(err)
		w.completeWarning(pending, err)
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case task := <-w.ch:
			w.process(ctx, task)
			continue
		default:
		}
		if hasPending {
			task := pending
			hasPending = false
			w.processWarning(ctx, task)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case task := <-w.ch:
			w.process(ctx, task)
		case task := <-w.warningCh:
			pending = task
			hasPending = true
			if hook := w.testWarningSelected; hook != nil {
				hook()
			}
		}
	}
}

func (w *MailWorker) drainRemaining(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			w.dropRemaining(err)
			return err
		}
		select {
		case task := <-w.ch:
			w.process(ctx, task)
			continue
		default:
		}
		select {
		case task := <-w.warningCh:
			w.processWarning(ctx, task)
		default:
			return nil
		}
	}
}

func (w *MailWorker) dropRemaining(err error) {
	for {
		select {
		case <-w.ch:
			w.dropped.Add(1)
		default:
			goto warnings
		}
	}

warnings:
	for {
		select {
		case task := <-w.warningCh:
			w.dropped.Add(1)
			w.completeWarning(task, err)
		default:
			return
		}
	}
}
