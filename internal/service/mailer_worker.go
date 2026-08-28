// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package service

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// MailSendTask 邮件发送任务（明文 code 仅瞬态内存+通道，不落日志/不落库）。
type MailSendTask struct {
	To              string
	Purpose         domain.EmailTemplatePurpose
	Code            string
	TTLMin          int
	BalanceMillis   int64
	ThresholdMillis int64
}

// BalanceWarningMailTask is the low-priority mail-worker envelope.
type BalanceWarningMailTask struct {
	Event      domain.BalanceWarningEvent
	OnComplete func(error)
}

const (
	mailQueueCap        = 256
	mailWarningQueueCap = 256
)

var (
	mailSendTimeout  = 15 * time.Second
	mailRetryBackoff = []time.Duration{2 * time.Second, 8 * time.Second}
)

// MailWorker serializes auth and warning mail through one sender. Auth work has
// strict priority until warning delivery enters SMTP, where it is non-preemptive.
type MailWorker struct {
	svc       *Service
	ch        chan MailSendTask
	warningCh chan BalanceWarningMailTask

	mu            sync.Mutex
	started       bool
	closed        bool
	cancel        context.CancelFunc
	senderDone    chan struct{}
	drainStarted  bool
	drainComplete bool
	drainDone     chan struct{}
	drainErr      error

	sent    atomic.Int64
	failed  atomic.Int64
	retried atomic.Int64
	dropped atomic.Int64
	lastErr atomic.Pointer[string]

	testWarningSelected func()
	testDrainStarted    func()
}

func NewMailWorker(svc *Service) *MailWorker {
	return &MailWorker{
		svc:        svc,
		ch:         make(chan MailSendTask, mailQueueCap),
		warningCh:  make(chan BalanceWarningMailTask, mailWarningQueueCap),
		senderDone: make(chan struct{}),
		drainDone:  make(chan struct{}),
	}
}

// EnqueueBalanceWarning admits a warning without competing for auth capacity.
func (w *MailWorker) EnqueueBalanceWarning(event domain.BalanceWarningEvent, onComplete func(error)) error {
	if onComplete != nil {
		callback := onComplete
		var callbackOnce sync.Once
		onComplete = func(err error) {
			callbackOnce.Do(func() { callback(err) })
		}
	}
	task := BalanceWarningMailTask{Event: event, OnComplete: onComplete}
	w.mu.Lock()
	var err error
	if w.closed {
		err = ErrMailQueueFull
	} else {
		select {
		case w.warningCh <- task:
		default:
			err = ErrMailQueueFull
		}
	}
	if err != nil {
		w.dropped.Add(1)
	}
	w.mu.Unlock()
	if err != nil {
		w.completeWarning(task, err)
	}
	return err
}

// Enqueue admits auth mail independently of warning capacity.
func (w *MailWorker) Enqueue(task MailSendTask) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.dropped.Add(1)
		return ErrMailQueueFull
	}
	select {
	case w.ch <- task:
		return nil
	default:
		w.dropped.Add(1)
		return ErrMailQueueFull
	}
}

func (w *MailWorker) Name() string { return "email" }

type mailStats struct {
	Queued          int    `json:"queued"`
	QueueCap        int    `json:"queue_cap"`
	WarningQueued   int    `json:"warning_queued"`
	WarningQueueCap int    `json:"warning_queue_cap"`
	SentTotal       int64  `json:"sent_total"`
	FailedTotal     int64  `json:"failed_total"`
	RetryTotal      int64  `json:"retry_total"`
	DroppedTotal    int64  `json:"dropped_total"`
	LastError       string `json:"last_error"`
}

func (w *MailWorker) Stats() any {
	var last string
	if value := w.lastErr.Load(); value != nil {
		last = *value
	}
	return mailStats{
		Queued:          len(w.ch),
		QueueCap:        mailQueueCap,
		WarningQueued:   len(w.warningCh),
		WarningQueueCap: mailWarningQueueCap,
		SentTotal:       w.sent.Load(),
		FailedTotal:     w.failed.Load(),
		RetryTotal:      w.retried.Load(),
		DroppedTotal:    w.dropped.Load(),
		LastError:       last,
	}
}
