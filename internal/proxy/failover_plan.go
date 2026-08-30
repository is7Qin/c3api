// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

func (p *Proxy) normalizedAttempts() int {
	n := p.cfg.FailoverAttempts
	if n <= 0 {
		return 3
	}
	if n > 8 {
		return 8
	}
	return n
}

func (p *Proxy) selectWithPlan(groupID int64, format domain.RequestFormat, model string, identity scheduler.AttemptPlanIdentity) (*scheduler.Selection, *scheduler.AttemptPlan, error) {
	if p.sched == nil {
		return nil, nil, scheduler.ErrGroupNotFound
	}
	route := scheduler.RouteRefFor(groupID, string(format), model)
	return p.selectWithPlanForRoute(route, groupID, format, model, identity)
}

func (p *Proxy) selectWithPlanForRoute(route scheduler.RouteRef, groupID int64, format domain.RequestFormat, model string, identity scheduler.AttemptPlanIdentity) (*scheduler.Selection, *scheduler.AttemptPlan, error) {
	if p.sched == nil {
		return nil, nil, scheduler.ErrGroupNotFound
	}
	identity.MaxAttempts = uint8(p.normalizedAttempts())
	plan, err := p.sched.NewAttemptPlan(identity, route)
	if err != nil {
		sel, selErr := p.sched.Select(groupID, format, model)
		return sel, nil, selErr
	}
	sel, _, err := p.sched.ReserveAttempt(plan)
	if err != nil {
		return nil, plan, err
	}
	return sel, plan, nil
}

func callerCategoryFor(st attemptState, format domain.RequestFormat, selectFormat domain.RequestFormat) CallerCategory {
	// Map attemptState to CallerCategory for retry matrix; choose based on selectFormat/client format
	// Use selectFormat where converted, otherwise format
	eff := format
	if selectFormat != "" {
		eff = selectFormat
	}
	switch eff {
	case domain.FormatOpenAIChat:
		return CallerChat
	case domain.FormatOpenAIResponses:
		return CallerResponses
	case domain.FormatOpenAIResponsesWS:
		return CallerResponsesWS
	case domain.FormatAnthropic:
		return CallerAnthropic
	case domain.FormatOpenAIImages:
		if st.caller != nil {
			// images vs images_codex distinguished by credential type at call time, but matrix treats both as retryable same;
			// choose generic images for now
			return CallerImages
		}
		return CallerImages
	case domain.FormatOpenAISearch:
		return CallerSearch
	default:
		// fallback
		return CallerChat
	}
}

func outcomeForPlanRetry(code int, callErr error, ctx context.Context, cat CallerCategory) AttemptOutcome {
	// Build minimal AttemptOutcome for CanRetry decision; only not_sent and ordinary 429 are retryable
	// Use placeholder dispatched metadata valid for CanRetry
	base := AttemptOutcome{
		ID: "attempt-1", RouteClassID: "rc1", QualityClassID: "qc1", Fingerprint: "fp1",
		TemplateID: 1, AccountID: 1, RequestedModel: "gpt-4o", MappedModel: "gpt-4o",
		CallerCategory: cat, OperationTag: "chat_completions", Ordinal: 1,
		Lane: LanePrimary, Generation: 1, LifecycleRevision: 1,
	}
	if base.CallerCategory == "" {
		base.CallerCategory = CallerChat
	}
	// client cancel -> not retryable (matrix forbids)
	if code == 0 && ctx != nil && ctx.Err() != nil {
		base.Commit = CommitNotSent
		base.Result = ResultClientCancel
		base.HTTPStatus = 0
		base.Terminal = true
		return base
	}
	if code == 0 && callErr != nil {
		base.Commit = CommitNotSent
		base.Result = ResultFailed
		base.HTTPStatus = 0
		base.BusinessFrameSent = false
		base.Terminal = false
		base.HardContinuation = false
		return base
	}
	if code == http.StatusTooManyRequests {
		base.Commit = CommitUpstreamResponded
		base.Result = ResultFailed
		base.HTTPStatus = 429
		base.BusinessFrameSent = false
		base.Terminal = false
		base.HardContinuation = false
		return base
	}
	// other codes: construct terminal failed outcome which CanRetry will reject
	if code >= 500 && code <= 599 {
		base.Commit = CommitUpstreamResponded
		base.Result = ResultFailed
		base.HTTPStatus = AttemptStatus(code)
		base.Terminal = true
		return base
	}
	if code >= 400 && code < 500 {
		base.Commit = CommitUpstreamResponded
		base.Result = ResultFailed
		base.HTTPStatus = AttemptStatus(code)
		base.Terminal = true
		return base
	}
	// committed etc -> treat as terminal
	base.Commit = CommitResponseStarted
	base.Result = ResultFailed
	base.HTTPStatus = AttemptStatus(code)
	base.Terminal = true
	base.BusinessFrameSent = true
	return base
}

func (p *Proxy) shouldRetryWithPlan(ctx context.Context, code int, callErr error, st attemptState, format domain.RequestFormat, selectFormat domain.RequestFormat) bool {
	cat := callerCategoryFor(st, format, selectFormat)
	o := outcomeForPlanRetry(code, callErr, ctx, cat)
		// Use the typed retry matrix where raw status is insufficient.
	return CanRetry(cat, o)
}

// selectNextWithPlan advances the request-local plan: a compiled plan's
// verdict is final (ErrAttemptsExhausted / ErrNoAvailable propagate to the
// exhaustion path; reservation rejects already consumed no attempt inside
// ReserveAttempt). Only plan-less (legacy) callers fall through to Select.
func (p *Proxy) selectNextWithPlan(plan *scheduler.AttemptPlan, groupID int64, selectFormat domain.RequestFormat, model string) (*scheduler.Selection, error) {
	if plan != nil {
		sel, _, err := p.sched.ReserveAttempt(plan)
		if err != nil {
			return nil, err
		}
		return sel, nil
	}
	return p.sched.Select(groupID, selectFormat, model)
}

var _ = domain.ErrAuth
