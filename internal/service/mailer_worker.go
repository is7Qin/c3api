// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package service

import (
	"context"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	mail "github.com/wneessen/go-mail"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// MailSendTask 邮件发送任务（明文 code 仅瞬态内存+通道，不落日志/不落库）。
type MailSendTask struct {
	To      string
	Purpose domain.EmailTemplatePurpose
	Code    string
	TTLMin  int
}

const mailQueueCap = 256

var (
	mailSendTimeout  = 15 * time.Second
	mailRetryBackoff = []time.Duration{2 * time.Second, 8 * time.Second}
)

// MailWorker 专用邮件发送后台 worker（D-W1..W7）。
type MailWorker struct {
	svc       *Service
	ch        chan MailSendTask
	quit      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	sent      atomic.Int64
	failed    atomic.Int64
	retried   atomic.Int64
	dropped   atomic.Int64
	lastErr   atomic.Pointer[string]
}

// NewMailWorker 构造邮件 worker（svc 复用 RenderTemplate/mailConfig 私有面）。
func NewMailWorker(svc *Service) *MailWorker {
	return &MailWorker{
		svc:  svc,
		ch:   make(chan MailSendTask, mailQueueCap),
		quit: make(chan struct{}),
	}
}

// Name 实现 worker.Worker + handler.StatsProvider。
func (w *MailWorker) Name() string { return "email" }

// Start 启动单条 sender goroutine（panic 自捕，对齐 worker.Manager.Go 模式）。
func (w *MailWorker) Start(ctx context.Context) error {
	var err error
	w.startOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil && w.svc != nil && w.svc.log != nil {
					w.svc.log.Warn("mail worker panicked",
						logx.Any("panic", r),
						logx.String("stack", string(debug.Stack())),
					)
				}
			}()
			w.loop(ctx)
		}()
	})
	return err
}

// Close 关闭接收并限时排空已在队任务（反序排空中段关闭，D-W4）。
func (w *MailWorker) Close(ctx context.Context) error {
	w.closeOnce.Do(func() { close(w.quit) })
	// 限时排空：ctx 预算内继续消费，超时丢弃计数。
	w.drainRemaining(ctx)
	return nil
}

// Enqueue 入队（有界 256；满或已关闭 → dropped++ + Warn + ErrMailQueueFull；永不 close ch）。
func (w *MailWorker) Enqueue(t MailSendTask) error {
	// 已关闭 → 丢弃
	select {
	case <-w.quit:
		w.dropped.Add(1)
		return ErrMailQueueFull
	default:
	}
	select {
	case w.ch <- t:
		return nil
	case <-w.quit:
		w.dropped.Add(1)
		return ErrMailQueueFull
	default:
		w.dropped.Add(1)
		if w.svc != nil && w.svc.log != nil {
			w.svc.log.Warn("mail queue full, dropped",
				logx.String("email", t.To),
				logx.String("purpose", string(t.Purpose)),
			)
		}
		return ErrMailQueueFull
	}
}

// mailStats 观测结构（json 小写对齐 ops.go 契约）。
type mailStats struct {
	Queued       int    `json:"queued"`
	QueueCap     int    `json:"queue_cap"`
	SentTotal    int64  `json:"sent_total"`
	FailedTotal  int64  `json:"failed_total"`
	RetryTotal   int64  `json:"retry_total"`
	DroppedTotal int64  `json:"dropped_total"`
	LastError    string `json:"last_error"`
}

// Stats 实现 handler.StatsProvider。
func (w *MailWorker) Stats() any {
	var last string
	if p := w.lastErr.Load(); p != nil {
		last = *p
	}
	return mailStats{
		Queued:       len(w.ch),
		QueueCap:     mailQueueCap,
		SentTotal:    w.sent.Load(),
		FailedTotal:  w.failed.Load(),
		RetryTotal:   w.retried.Load(),
		DroppedTotal: w.dropped.Load(),
		LastError:    last,
	}
}

func (w *MailWorker) loop(ctx context.Context) {
	for {
		select {
		case <-w.quit:
			return
		case <-ctx.Done():
			return
		case t := <-w.ch:
			w.process(ctx, t)
		}
	}
}

func (w *MailWorker) drainRemaining(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// 预算耗尽 → 剩余全量丢弃计数
			remaining := len(w.ch)
			if remaining > 0 {
				w.dropped.Add(int64(remaining))
				for i := 0; i < remaining; i++ {
					<-w.ch
				}
				if w.svc != nil && w.svc.log != nil {
					w.svc.log.Warn("mail drain budget exhausted, dropped remaining",
						logx.Int64("dropped", w.dropped.Load()),
					)
				}
			}
			return
		default:
		}
		select {
		case t := <-w.ch:
			w.process(ctx, t)
		default:
			return
		}
	}
}

func (w *MailWorker) process(ctx context.Context, t MailSendTask) {
	// 每任务最多 3 次尝试：attempt1 立即，retry 间隙 2s/8s。
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			w.retried.Add(1)
			backoff := mailRetryBackoff[attempt-1]
			if backoff > 0 {
				timer := time.NewTimer(backoff)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					w.failed.Add(1)
					if lastErr != nil {
						s := lastErr.Error()
						w.lastErr.Store(&s)
					}
					return
				case <-w.quit:
					timer.Stop()
					w.failed.Add(1)
					if lastErr != nil {
						s := lastErr.Error()
						w.lastErr.Store(&s)
					}
					return
				}
			}
		}
		err := w.deliver(ctx, t)
		if err == nil {
			w.sent.Add(1)
			return
		}
		lastErr = err
		if w.svc != nil && w.svc.log != nil {
			w.svc.log.Error("mail deliver failed",
				logx.String("email", t.To),
				logx.String("purpose", string(t.Purpose)),
				logx.Int("attempt", attempt+1),
				logx.Error(err),
			)
		}
	}
	w.failed.Add(1)
	if lastErr != nil {
		s := lastErr.Error()
		w.lastErr.Store(&s)
	}
}

// deliver 渲染并投递单次（无重试）。
func (w *MailWorker) deliver(ctx context.Context, t MailSendTask) error {
	host, port, username, password, fromAddr, tlsPolicy, ok := w.svc.mailConfig()
	if !ok {
		return ErrMailNotConfigured
	}
	subj, body, err := w.svc.RenderTemplate(ctx, t.Purpose, map[string]string{
		"code":        t.Code,
		"ttl_minutes": strconv.Itoa(t.TTLMin),
		"app_name":    domain.AppName,
	})
	if err != nil {
		return err
	}
	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithTimeout(mailSendTimeout),
	}
	switch tlsPolicy {
	case "implicit":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthPlain), mail.WithUsername(username), mail.WithPassword(password))
	}
	client, err := mail.NewClient(host, opts...)
	if err != nil {
		return fmt.Errorf("mail client: %w", err)
	}
	msg := mail.NewMsg()
	if err := msg.From(fromAddr); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := msg.To(t.To); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	msg.Subject(subj)
	msg.SetBodyString(mail.TypeTextPlain, body)
	sendCtx, cancel := context.WithTimeout(ctx, mailSendTimeout)
	defer cancel()
	if err := client.DialAndSendWithContext(sendCtx, msg); err != nil {
		return err
	}
	if w.svc != nil && w.svc.log != nil {
		w.svc.log.Info("mail sent", logx.String("email", t.To), logx.String("purpose", string(t.Purpose)))
	}
	return nil
}
