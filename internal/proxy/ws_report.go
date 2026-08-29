// SPDX-License-Identifier: AGPL-3.0-or-later
// ws_report 聚合 WS 观测：Outcome 为单次尝试的可重试与计费视图，BusinessFrameSent
// 标记业务帧是否已见，决定是否可迁移；Commit 区分 not-sent/已响应/已开始响应。

package proxy

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

func wsCallerCategory(sel *scheduler.Selection) CallerCategory {
	if isCodexCredentialType(sel.CredentialType) {
		return CallerCodexWS
	}
	return CallerResponsesWS
}

func wsDispatchedBase(sel *scheduler.Selection, reqModel string, start time.Time) AttemptOutcome {
	lat := time.Since(start).Milliseconds()
	if lat < 0 {
		lat = 0
	}
	mapped := sel.Model
	if mapped == "" {
		mapped = reqModel
	}
	if mapped == "" {
		mapped = "unknown"
	}
	req := reqModel
	if req == "" {
		req = mapped
	}
	cat := wsCallerCategory(sel)
	fp := CandidateFingerprint(sel.CandidateFingerprint)
	if fp == "" {
		fp = CandidateFingerprint("fp-ws")
	}
	rc := RouteClassID("rc-ws")
	qc := QualityClassID("qc-ws")
	if sel.CandidateFingerprint != "" {
		if len(sel.CandidateFingerprint) > 4 {
			rc = RouteClassID("rc-" + sel.CandidateFingerprint[:4])
		} else {
			rc = RouteClassID("rc-" + sel.CandidateFingerprint)
		}
		qc = QualityClassID("qc-" + sel.CandidateFingerprint[:1])
		if qc == "" {
			qc = QualityClassID("qc-ws")
		}
	}
	return AttemptOutcome{
		ID:                AttemptID("attempt-ws-1"),
		RouteClassID:      rc,
		QualityClassID:    qc,
		Fingerprint:       fp,
		TemplateID:        sel.TemplateID,
		AccountID:         sel.AccountID,
		RequestedModel:    req,
		MappedModel:       mapped,
		CallerCategory:    cat,
		OperationTag:      OperationTag(string(domain.OpResponsesWS)),
		Ordinal:           1,
		LifecycleRevision: 1,
		Lane:              LanePrimary,
		Generation:        1,
		Timing:            AttemptTiming{LatencyMS: lat},
	}
}

func wsUsageFromTuple(u usageTuple) AttemptUsage {
	return AttemptUsage{
		InputTokens:         u.it,
		OutputTokens:        u.ot,
		CacheReadTokens:     u.cr,
		CacheCreationTokens: u.cc,
		CallCount:           u.calls,
	}
}

func wsOutcomeForSuccess(base AttemptOutcome, ttft *int64, u usageTuple) AttemptOutcome {
	o := base
	o.Commit = CommitResponseStarted
	o.Result = ResultSuccess
	o.HTTPStatus = 200
	o.Terminal = true
	o.BusinessFrameSent = true // 业务帧已见，成功且不可重试
	o.Timing.TTFTMS = ttft
	o.Usage = wsUsageFromTuple(u)
	return o
}

func wsOutcomeForClientAbort(base AttemptOutcome, u usageTuple, ttft *int64) AttemptOutcome {
	o := base
	o.Result = ResultClientCancel
	o.HTTPStatus = 0
	o.Terminal = true
	o.Commit = CommitResponseStarted
	o.BusinessFrameSent = true // 客户端断开前已见业务帧，不计冷却
	o.Timing.TTFTMS = ttft
	o.Usage = wsUsageFromTuple(u)
	return o
}

func wsOutcomeForUpstreamError(base AttemptOutcome, u usageTuple, ttft *int64) AttemptOutcome {
	o := base
	o.Result = ResultFailed
	o.HTTPStatus = 0
	o.Commit = CommitSentAmbiguous
	o.BusinessFrameSent = true // 已见业务帧但上游异常，需冷却
	o.Terminal = true
	o.Timing.TTFTMS = ttft
	o.Usage = wsUsageFromTuple(u)
	return o
}

func wsOutcomeForNotSentNetwork(base AttemptOutcome, u usageTuple) AttemptOutcome {
	o := base
	o.Result = ResultFailed
	o.HTTPStatus = 0
	o.Commit = CommitNotSent
	o.BusinessFrameSent = false // 首帧未送达未见业务帧，可重试
	o.Terminal = false
	o.Usage = wsUsageFromTuple(u)
	return o
}

func wsOutcomeForUpstreamStatus(base AttemptOutcome, status int, u usageTuple, ttft *int64) AttemptOutcome {
	o := base
	o.Usage = wsUsageFromTuple(u)
	o.Timing.TTFTMS = ttft
	if status == 429 {
		// 429 已响应未见业务帧，可重试限流
		o.Commit = CommitUpstreamResponded
		o.Result = ResultFailed
		o.HTTPStatus = 429
		o.Terminal = false
		o.BusinessFrameSent = false
		return o
	}
	if status >= 400 && status < 500 {
		// 4xx 已响应未见业务帧，确定性拒绝不重试
		o.Commit = CommitUpstreamResponded
		o.Result = ResultFailed
		o.HTTPStatus = AttemptStatus(status)
		o.Terminal = true
		o.BusinessFrameSent = false
		return o
	}
	if status >= 500 && status <= 599 {
		// 5xx 已响应未见业务帧，需冷却
		o.Commit = CommitUpstreamResponded
		o.Result = ResultFailed
		o.HTTPStatus = AttemptStatus(status)
		o.Terminal = true
		o.BusinessFrameSent = false
		return o
	}
	o.Commit = CommitResponseStarted
	o.Result = ResultSuccess
	o.HTTPStatus = 200
	o.Terminal = true
	o.BusinessFrameSent = true
	return o
}

func wsHealthForOutcome(o AttemptOutcome) *AttemptHealthEvent {
	// 按 HTTP 状态与结果映射冷却类型，客户端取消不计冷却
	var kind rule.Kind
	switch {
	case o.Result == ResultSuccess:
		kind = rule.KindOK
	case o.HTTPStatus == 429:
		kind = rule.Kind429
	case o.HTTPStatus >= 400 && o.HTTPStatus < 500:
		kind = rule.Kind4xx
	case o.HTTPStatus >= 500:
		kind = rule.Kind5xx
	case o.HTTPStatus == 0:
		kind = rule.KindNetwork // 读/心跳/拨号网络失败
	default:
		kind = rule.KindNetwork
	}
	return &AttemptHealthEvent{Kind: kind}
}

func (p *Proxy) reportWSOutcome(ctx context.Context, outcome AttemptOutcome, sel *scheduler.Selection, reqID string, groupID int64, reqModel string, start time.Time, u usageTuple, ttft *int64) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	ttftCopy := ttft
	if ttft != nil {
		v := *ttft
		ttftCopy = &v
	}
	status := int(outcome.HTTPStatus)
	et := domain.ErrNone
	switch outcome.Result {
	case ResultSuccess:
		status = 200
		et = domain.ErrNone
	case ResultClientCancel:
		status = 200
		et = domain.ErrAbort
		if outcome.Commit == CommitNotSent {
			status = statusClientClosedRequest
		}
	case ResultFailed:
		if outcome.IsMalformed {
			et = domain.Err4xx
			status = int(outcome.HTTPStatus)
			if status == 0 {
				status = 200
			}
		} else if outcome.HTTPStatus == 429 {
			status = 429
			et = domain.Err429
		} else if outcome.HTTPStatus >= 500 {
			status = int(outcome.HTTPStatus)
			et = domain.Err5xx
		} else if outcome.HTTPStatus >= 400 {
			status = int(outcome.HTTPStatus)
			et = domain.Err4xx
		} else if outcome.HTTPStatus == 0 {
			if outcome.Commit == CommitSentAmbiguous {
				status = 200
				et = domain.ErrAbort
			} else {
				status = 0
				et = domain.ErrNetwork
			}
		}
	}
	health := wsHealthForOutcome(outcome)
	if outcome.Result == ResultClientCancel {
		health = nil
	}
	// observer 唯一拥有释放与健康上报：release 拥有 finish/记录，markHealth 拥有冷却上报
	markHealth := func(o AttemptOutcome, ev AttemptHealthEvent) {
		p.sched.MarkResult(o.AccountID, ev.Kind, ev.ResetAt, int(o.HTTPStatus), ev.ErrorMessage, string(o.MappedModel))
	}
	appendFlow := func(o AttemptOutcome) {}
	release := func() {
		l := logWithCtx(ctx, p.buildLog(reqID, groupID, outcome.AccountID, reqModel, string(outcome.MappedModel), domain.FormatOpenAIResponsesWS, status, et, u, start))
		if ttftCopy != nil {
			l.TTFTMS = ttftCopy
		}
		p.finish(sel, l)
	}
	observer := NewAttemptObserver(nil, markHealth, appendFlow, release)
	if outcome.Result == ResultClientCancel {
		return observer.Cancel(outcome)
	}
	return observer.Complete(outcome, health)
}
