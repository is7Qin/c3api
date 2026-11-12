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
	if sel.CredentialType.IsCodex() {
		return CallerCodexWS
	}
	return CallerResponsesWS
}

func wsDispatchedBase(sel *scheduler.Selection, reqModel string, start time.Time) AttemptOutcome {
	lat := max(time.Since(start).Milliseconds(), 0)
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
	}
	return AttemptOutcome{
		ID:               AttemptID("attempt-ws-1"),
		RouteClassID:     rc,
		QualityClassID:   qc,
		Fingerprint:      fp,
		TemplateID:       sel.TemplateID,
		AccountID:        sel.AccountID,
		RequestedModel:   req,
		MappedModel:      mapped,
		CallerCategory:   cat,
		OperationTag:     OperationTag(string(domain.OpResponsesWS)),
		Ordinal:          1,
		IdentityRevision: 1, // synthetic placeholder K; not part of the continuation (I,K) comparison (see AttemptOutcome.IdentityRevision)
		Lane:             LanePrimary,
		Generation:       1,
		Timing:           AttemptTiming{LatencyMS: lat},
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

// wsOutcomeForClientAbort 客户端侧结束的观测事实。sent = 是否真的写出过业务帧
// （relayWS 的真实下行计数）：旧实现把它写成构造常量 true，等于把「一帧都没发出
// 去」也记成已交付。
func wsOutcomeForClientAbort(base AttemptOutcome, sent bool, u usageTuple, ttft *int64) AttemptOutcome {
	o := base
	o.Result = ResultClientCancel
	o.HTTPStatus = 0
	o.Terminal = true
	// 取消态允许 not_sent（Validate 对 client_cancel 放行 not_sent + terminal）：
	// 一帧未写出就是「没发」，不必伪装成已交付。
	o.Commit = CommitResponseStarted
	if !sent {
		o.Commit = CommitNotSent
	}
	o.BusinessFrameSent = sent // 写过业务帧仍不计冷却（客户端结束）
	o.Timing.TTFTMS = ttft
	o.Usage = wsUsageFromTuple(u)
	return o
}

// wsOutcomeForUpstreamError 上游侧失败的观测事实。
//
// ⚠ 结构性限制（spec §4）：AttemptOutcome.Validate 对「failed + status 0 + terminal」
// 只允许 sent_ambiguous / response_started / client_committed，且这三者都**要求**
// BusinessFrameSent=true —— 「上游已响应但一帧业务字节都没发出去」在类型上无法
// 表达。因此类型层保持 sent_ambiguous + BusinessFrameSent=true（观测与重试矩阵的
// 既有语义），**日志列的交付态**由 commitStateOfWSDelivery 按真实下行计数折算
// （一帧未写 ⇒ upstream_responded ⇒ 不计费）。
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

func (p *Proxy) reportWSOutcome(ctx context.Context, outcome AttemptOutcome, sel *scheduler.Selection, reqID string, groupID int64, reqModel string, start time.Time, u usageTuple, ttft *int64, cause error) error {
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
	// 归因唯一来源（I1）：入站 WS 与 chat/codex 三条路径共用同一套判定，出口不再
	// 自拼类别（旧实现把「上游发错误帧后正常关闭」记成 200+abort）。
	var health *AttemptHealthEvent
	switch outcome.Result {
	case ResultSuccess:
		status = 200
		et = domain.ErrNone
		health = &AttemptHealthEvent{Kind: rule.KindOK}
	case ResultClientCancel:
		// 客户端侧结束：200 + abort（未写出任何业务帧则 499），不投递健康事件。
		status = 200
		et = domain.ErrAbort
		if !outcome.BusinessFrameSent {
			status = statusClientClosedRequest
		}
	case ResultFailed:
		if outcome.IsMalformed {
			// 协议形状失败（网关判定）：4xx 语义，状态取 outcome 真值（0 ⇒ 200）。
			et = domain.Err4xx
			status = int(outcome.HTTPStatus)
			if status == 0 {
				status = 200
			}
			health = &AttemptHealthEvent{Kind: rule.Kind4xx}
		} else {
			f := classifyStreamFailure(cause)
			status, et, health = f.Status, f.ET, streamFailureHealth(f)
		}
	}
	// Single owner observation (quality + bounded flow) + health marking; the
	// finish owns the lease release exactly once.
	p.observeDispatchOutcome(ctx, outcome, health)
	commit := commitStateOfWSDelivery(outcome.Result, outcome.BusinessFrameSent)
	l := withCommit(logWithCtx(ctx, p.buildLog(reqID, groupID, outcome.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponsesWS, status, et, u, start)), commit)
	// 中止/失败行必须留因（cause 为分类阶段快照的真实错误：上游关闭帧、上游流内
	// 错误载荷、读写网络错误、context.Canceled）。成功行 cause 恒 nil ⇒ NULL。
	if outcome.Result != ResultSuccess {
		l.ErrorMessage = causeMessage(cause)
	}
	if ttftCopy != nil {
		l.TTFTMS = ttftCopy
	}
	p.finish(sel, l)
	return nil
}
