// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

func chatDispatchedBase(sel *scheduler.Selection, reqID string, reqModel string, start time.Time) AttemptOutcome {
	fp := string(sel.CandidateFingerprint)
	if fp == "" {
		fp = "fp-placeholder"
	}
	lat := time.Since(start).Milliseconds()
	if lat < 0 {
		lat = 0
	}
	return AttemptOutcome{
		ID:                AttemptID(reqID + ":1"),
		RouteClassID:      "rc1",
		QualityClassID:    "qc1",
		Fingerprint:       CandidateFingerprint(fp),
		TemplateID:        sel.TemplateID,
		AccountID:         sel.AccountID,
		RequestedModel:    reqModel,
		MappedModel:       sel.Model,
		CallerCategory:    CallerChat,
		OperationTag:      "chat_completions",
		Ordinal:           1,
		LifecycleRevision: 1,
		Lane:              LanePrimary,
		Generation:        1,
		Timing:            AttemptTiming{LatencyMS: lat},
	}
}

func chatOutcomeForSuccess(base AttemptOutcome, ttft *int64, u usageTuple) AttemptOutcome {
	o := base
	o.Commit = CommitResponseStarted
	o.Result = ResultSuccess
	o.HTTPStatus = 200
	o.Terminal = true
	o.BusinessFrameSent = true
	o.Timing.TTFTMS = ttft
	o.Usage = AttemptUsage{InputTokens: u.it, OutputTokens: u.ot, CacheReadTokens: u.cr, CacheCreationTokens: u.cc}
	return o
}

func chatOutcomeForClientCancel(base AttemptOutcome, sent bool, u usageTuple, ttft *int64) AttemptOutcome {
	o := base
	o.Result = ResultClientCancel
	o.HTTPStatus = 0
	o.Terminal = true
	o.Timing.TTFTMS = ttft
	o.Usage = AttemptUsage{InputTokens: u.it, OutputTokens: u.ot, CacheReadTokens: u.cr, CacheCreationTokens: u.cc}
	if sent {
		o.Commit = CommitResponseStarted
		o.BusinessFrameSent = true
	} else {
		o.Commit = CommitNotSent
		o.BusinessFrameSent = false
	}
	return o
}

func chatOutcomeForNetwork(base AttemptOutcome, sent bool, u usageTuple, ttft *int64) AttemptOutcome {
	o := base
	o.Result = ResultFailed
	o.HTTPStatus = 0
	o.Timing.TTFTMS = ttft
	o.Usage = AttemptUsage{InputTokens: u.it, OutputTokens: u.ot, CacheReadTokens: u.cr, CacheCreationTokens: u.cc}
	if sent {
		o.Commit = CommitSentAmbiguous
		o.BusinessFrameSent = true
		o.Terminal = true
	} else {
		o.Commit = CommitNotSent
		o.BusinessFrameSent = false
		o.Terminal = false
	}
	return o
}

func (p *Proxy) reportChatOutcome(ctx context.Context, outcome AttemptOutcome, sel *scheduler.Selection, reqID string, groupID int64, reqModel string, start time.Time) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	var status int
	var et domain.ErrorType
	var health *AttemptHealthEvent
	switch outcome.Result {
	case ResultSuccess:
		status = http.StatusOK
		et = domain.ErrNone
		health = &AttemptHealthEvent{Kind: rule.KindOK}
	case ResultClientCancel:
		status = http.StatusOK
		et = domain.ErrAbort
		health = nil
	case ResultFailed:
		status = http.StatusOK
		et = domain.ErrAbort
		health = &AttemptHealthEvent{Kind: rule.KindNetwork}
	default:
		status = http.StatusOK
		et = domain.ErrAbort
		health = &AttemptHealthEvent{Kind: rule.KindNetwork}
	}
	u := usageTuple{it: outcome.Usage.InputTokens, ot: outcome.Usage.OutputTokens, tt: outcome.Usage.InputTokens + outcome.Usage.OutputTokens, cr: outcome.Usage.CacheReadTokens, cc: outcome.Usage.CacheCreationTokens}
	if outcome.Timing.TTFTMS != nil {
		ctx = context.WithValue(ctx, ctxKeyTTFT{}, outcome.Timing.TTFTMS)
	}
	l := logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIChat, status, et, u, start))
	p.applyBilling(l)
	p.auth.DeductQuota(l.KeyID, l.TotalTokens)
	if p.cfg.UsageCapture {
		p.routeLog(l)
	}
	observer := NewAttemptObserver(nil,
		func(o AttemptOutcome, e AttemptHealthEvent) {
			p.sched.MarkResult(o.AccountID, e.Kind, nil, int(o.HTTPStatus), "", o.MappedModel)
		},
		nil,
		func() { sel.Release() },
	)
	if outcome.Result == ResultClientCancel {
		return observer.Cancel(outcome)
	}
	return observer.Complete(outcome, health)
}
