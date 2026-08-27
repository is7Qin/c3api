// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	mail "github.com/wneessen/go-mail"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

const (
	mailFailureDelivery      = "mail_delivery_failed"
	mailFailureNotConfigured = "mail_not_configured"
	mailFailureCanceled      = "mail_delivery_canceled"
	mailFailurePanicked      = "mail_worker_panicked"
)

var errMailWorkerPanicked = errors.New("mail worker panicked")

func (w *MailWorker) process(ctx context.Context, t MailSendTask) {
	var lastErr error
	for attempt := range 3 {
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
					return
				}
			}
		}
		if err := w.deliver(ctx, t); err == nil {
			w.sent.Add(1)
			return
		} else {
			lastErr = err
			if w.svc != nil && w.svc.log != nil {
				w.svc.log.Error("mail deliver failed",
					logx.String("purpose", string(t.Purpose)),
					logx.Int("attempt", attempt+1),
					logx.String("failure_category", mailFailureCategory(err)),
				)
			}
		}
	}
	w.failed.Add(1)
	if lastErr != nil {
		w.storeMailFailure(lastErr)
	}
}

func (w *MailWorker) processWarning(ctx context.Context, task BalanceWarningMailTask) {
	terminalRecorded := false
	defer func() {
		if recover() == nil {
			return
		}
		if terminalRecorded {
			w.storeMailFailure(errMailWorkerPanicked)
		} else {
			w.warningFailure(errMailWorkerPanicked)
		}
		w.completeWarning(task, errMailWorkerPanicked)
		panic(errMailWorkerPanicked)
	}()
	err := w.processWarningResult(ctx, task.Event)
	terminalRecorded = true
	w.completeWarning(task, err)
}

func (w *MailWorker) completeWarning(task BalanceWarningMailTask, err error) {
	if task.OnComplete != nil {
		defer func() {
			if recover() != nil {
				w.storeMailFailure(errMailWorkerPanicked)
				panic(errMailWorkerPanicked)
			}
		}()
		task.OnComplete(err)
	}
}

func (w *MailWorker) processWarningResult(ctx context.Context, event domain.BalanceWarningEvent) error {
	var lastErr error
	for attempt := range 3 {
		if err := ctx.Err(); err != nil {
			return w.warningFailure(err)
		}
		if attempt > 0 {
			w.retried.Add(1)
			backoff := mailRetryBackoff[attempt-1]
			if backoff > 0 {
				timer := time.NewTimer(backoff)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return w.warningFailure(ctx.Err())
				}
			}
		}
		lastErr = w.deliver(ctx, MailSendTask{To: event.Email, Purpose: domain.EmailTemplateBalanceWarning, BalanceMillis: event.BalanceMillis, ThresholdMillis: event.ThresholdMillis})
		if lastErr == nil {
			w.sent.Add(1)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return w.warningFailure(err)
		}
	}
	return w.warningFailure(lastErr)
}

func (w *MailWorker) warningFailure(err error) error {
	w.failed.Add(1)
	if err != nil {
		w.storeMailFailure(err)
	}
	return err
}

func mailFailureCategory(err error) string {
	switch {
	case errors.Is(err, errMailWorkerPanicked):
		return mailFailurePanicked
	case errors.Is(err, ErrMailNotConfigured):
		return mailFailureNotConfigured
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return mailFailureCanceled
	default:
		return mailFailureDelivery
	}
}

func (w *MailWorker) storeMailFailure(err error) {
	category := mailFailureCategory(err)
	w.lastErr.Store(&category)
}

func (w *MailWorker) deliver(ctx context.Context, t MailSendTask) error {
	host, port, username, password, fromAddr, tlsPolicy, ok := w.svc.mailConfig()
	if !ok {
		return ErrMailNotConfigured
	}
	vars := map[string]string{"code": t.Code, "ttl_minutes": strconv.Itoa(t.TTLMin), "app_name": domain.AppName}
	if t.Purpose == domain.EmailTemplateBalanceWarning {
		vars["balance"] = strconv.FormatFloat(float64(t.BalanceMillis)/1e5, 'f', 2, 64)
		vars["threshold"] = strconv.FormatFloat(float64(t.ThresholdMillis)/1e5, 'f', 2, 64)
	}
	subj, body, err := w.svc.RenderTemplate(ctx, t.Purpose, vars)
	if err != nil {
		return err
	}
	opts := []mail.Option{mail.WithPort(port), mail.WithTimeout(mailSendTimeout)}
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
		w.svc.log.Info("mail sent", logx.String("purpose", string(t.Purpose)))
	}
	return nil
}
