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

// reportChatOutcome chat 车道的唯一记录点。归因（error_type / status_code /
// error_message / 健康事件）全部来自 f——出口不再自拼类别（旧实现把「上游流内
// 声明失败」和「客户端断开」都写成 abort）；commit 是交付事实（计费资格），由
// 调用方按 Output 的写出事实给出。
func (p *Proxy) reportChatOutcome(ctx context.Context, outcome AttemptOutcome, sel *scheduler.Selection, reqID string, groupID int64, reqModel string, start time.Time, f streamFailure, commit CommitState) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	status, et := http.StatusOK, domain.ErrNone
	var health *AttemptHealthEvent
	switch {
	case outcome.Result == ResultSuccess:
		health = &AttemptHealthEvent{Kind: rule.KindOK}
	case f.ET == domain.ErrAbort:
		// 客户端侧结束（序 0a/0c）：200 + abort，不投递健康事件（不惩罚账号）；
		// 上游可能已消费请求，token 照常落账。
		et = domain.ErrAbort
	default:
		// timeout 仅零业务帧时冷却（§5）：交付事实取 outcome 已确认的
		// BusinessFrameSent（chatOutcomeForAbort 的 sent 形参），不另造计数器。
		status, et, health = f.Status, f.ET, timeoutHealth(f, outcome.BusinessFrameSent)
	}
	u := usageTuple{it: outcome.Usage.InputTokens, ot: outcome.Usage.OutputTokens, tt: outcome.Usage.InputTokens + outcome.Usage.OutputTokens, cr: outcome.Usage.CacheReadTokens, cc: outcome.Usage.CacheCreationTokens}
	if outcome.Timing.TTFTMS != nil {
		ctx = context.WithValue(ctx, ctxKeyTTFT{}, outcome.Timing.TTFTMS)
	}
	l := withCommit(logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIChat, status, et, u, start)), commit)
	// 中止/失败行必须留因：取归因文本（上游信封原文优先）。成功行 f 为零值 ⇒ NULL。
	l.ErrorMessage = f.message()
	// Single owner observation (quality + bounded flow) + health marking; the
	// finish releases the lease exactly once.
	p.observeDispatchOutcome(ctx, outcome, health)
	p.finish(sel, l)
	return nil
}
