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
	return buildOutcome(CallerChat, "chat_completions", outcomeParams{
		reqID:          AttemptID(reqID + ":1"),
		routeClassID:   "rc1",
		qualityClassID: "qc1",
		fingerprint:    CandidateFingerprint(fp),
		templateID:     sel.TemplateID,
		accountID:      sel.AccountID,
		requestedModel: reqModel,
		mappedModel:    sel.Model,
		timing:         AttemptTiming{LatencyMS: max(time.Since(start).Milliseconds(), 0)},
	})
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

// chatOutcomeForAbort 收敛 chat 流式两条中止支路：客户端取消（clientCancel=true）
// 与上游断流（sent 表示已首帧/已发送）。二者差异只在 Result 与 sent 时的
// commit/terminal 映射，其余（status 0、保留已采集 usage/TTFT）同款。合并前为
// chatOutcomeForClientCancel / chatOutcomeForNetwork 两份。
func chatOutcomeForAbort(base AttemptOutcome, sent bool, u usageTuple, ttft *int64, clientCancel bool) AttemptOutcome {
	o := base
	o.HTTPStatus = 0
	o.Timing.TTFTMS = ttft
	o.Usage = AttemptUsage{InputTokens: u.it, OutputTokens: u.ot, CacheReadTokens: u.cr, CacheCreationTokens: u.cc}
	if clientCancel {
		o.Result = ResultClientCancel
		o.Terminal = true
	} else {
		o.Result = ResultFailed
		o.Terminal = sent
	}
	if sent {
		o.BusinessFrameSent = true
		o.Commit = CommitSentAmbiguous
		if clientCancel {
			o.Commit = CommitResponseStarted
		}
	} else {
		o.Commit = CommitNotSent
		o.BusinessFrameSent = false
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
	// Single owner observation (quality + bounded flow) + health marking; the
	// finish releases the lease exactly once.
	p.observeDispatchOutcome(ctx, outcome, health)
	p.finish(sel, l)
	return nil
}
