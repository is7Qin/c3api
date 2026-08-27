// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

func (w *Worker) finishFailure(event domain.BalanceWarningEvent, token, category string) {
	w.failed.Add(1)
	w.storeFailure(category)
	if token != "" {
		w.releaseClaim(event, token)
	}
	if hook := w.failedHook; hook != nil {
		hook()
	}
	if hook := w.processedHook; hook != nil {
		hook()
	}
}

func (w *Worker) storeFailure(category string) {
	w.lastErr.Store(&category)
}

func (w *Worker) finishSuppressed() {
	w.suppressed.Add(1)
	if hook := w.processedHook; hook != nil {
		hook()
	}
}

type notificationStats struct {
	Queued             int    `json:"queued"`
	QueueCap           int    `json:"queue_cap"`
	Evaluated          int64  `json:"evaluated"`
	Admitted           int64  `json:"admitted"`
	Suppressed         int64  `json:"suppressed"`
	CooldownSuppressed int64  `json:"cooldown_suppressed"`
	ClaimFailedTotal   int64  `json:"claim_failed_total"`
	ReleaseFailedTotal int64  `json:"release_failed_total"`
	DroppedTotal       int64  `json:"dropped_total"`
	SentTotal          int64  `json:"sent_total"`
	FailedTotal        int64  `json:"failed_total"`
	LastError          string `json:"last_error"`
}

func (w *Worker) Stats() any {
	var last string
	if p := w.lastErr.Load(); p != nil {
		last = *p
	}
	return notificationStats{
		Queued:             len(w.ch),
		QueueCap:           queueCap,
		Evaluated:          w.evaluated.Load(),
		Admitted:           w.admitted.Load(),
		Suppressed:         w.suppressed.Load(),
		CooldownSuppressed: w.cooldownSuppressed.Load(),
		ClaimFailedTotal:   w.claimFailed.Load(),
		ReleaseFailedTotal: w.releaseFailed.Load(),
		DroppedTotal:       w.dropped.Load(),
		SentTotal:          w.sent.Load(),
		FailedTotal:        w.failed.Load(),
		LastError:          last,
	}
}

func (w *Worker) drainWithBudget(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			remaining := len(w.ch)
			if remaining > 0 {
				n := 0
				for range remaining {
					select {
					case <-w.ch:
						n++
					default:
					}
				}
				w.dropped.Add(int64(n))
				if w.log != nil {
					w.log.Warn("notification drain budget exhausted, dropped remaining",
						logx.Int64("dropped", w.dropped.Load()),
					)
				}
			}
			return ctx.Err()
		default:
		}
		select {
		case ev := <-w.ch:
			w.handleEvent(ctx, ev)
		default:
			return nil
		}
	}
}
